package hub

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newClient(srv *httptest.Server, b *Budget, waits *[]time.Duration) *Client {
	return &Client{
		HubBase: srv.URL,
		RawBase: srv.URL,
		HTTP:    srv.Client(),
		Budget:  b,
		Sleep: func(_ context.Context, d time.Duration) error {
			if waits != nil {
				*waits = append(*waits, d)
			}
			return nil
		},
	}
}

func TestBudgetCapsDiscoveryButLetsEnrichmentSpendTheRest(t *testing.T) {
	b := NewBudget(3, 1)
	require.NoError(t, b.take(Discovery))
	assert.ErrorIs(t, b.take(Discovery), ErrBudget)
	require.NoError(t, b.take(Enrichment))
	require.NoError(t, b.take(Enrichment))
	assert.ErrorIs(t, b.take(Enrichment), ErrBudget)
	assert.Equal(t, 3, b.Used())
}

func TestGetRetriesServerErrorsThreeTimesAndCountsEveryAttempt(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	var waits []time.Duration
	b := NewBudget(100, 50)
	c := newClient(srv, b, &waits)

	_, err := c.ToolParserRegistry(context.Background(), "v0.24.0")

	var se *StatusError
	require.ErrorAs(t, err, &se)
	assert.Equal(t, http.StatusBadGateway, se.Status)
	assert.Equal(t, int32(4), hits.Load())
	assert.Equal(t, 4, b.Used())
	assert.Equal(t, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}, waits)
}

func TestGetHonoursRetryAfterButCapsItAtSixtySeconds(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch hits.Add(1) {
		case 1:
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
		case 2:
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			_, _ = w.Write([]byte("ok"))
		}
	}))
	defer srv.Close()
	var waits []time.Duration
	c := newClient(srv, NewBudget(100, 50), &waits)

	body, err := c.ToolParserRegistry(context.Background(), "v0.24.0")

	require.NoError(t, err)
	assert.Equal(t, "ok", string(body))
	assert.Equal(t, []time.Duration{7 * time.Second, 60 * time.Second}, waits)
}

func TestGetDoesNotRetryAClientError(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := newClient(srv, NewBudget(100, 50), nil)

	_, err := c.ConfigJSON(context.Background(), "meta-llama/Llama-3.1-8B-Instruct", "abc")

	var se *StatusError
	require.ErrorAs(t, err, &se)
	assert.Equal(t, http.StatusUnauthorized, se.Status)
	assert.Equal(t, int32(1), hits.Load())
}

func TestGetStopsAtTheBudgetMidRetry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := newClient(srv, NewBudget(10, 2), nil)

	_, err := c.ToolParserRegistry(context.Background(), "v0.24.0")

	assert.True(t, errors.Is(err, ErrBudget))
}

func TestTokenGoesToTheHubAndNeverToGitHub(t *testing.T) {
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.URL.Path+"|"+r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := newClient(srv, NewBudget(100, 50), nil)
	c.Token = "hf_test"

	_, err := c.ToolParserRegistry(context.Background(), "v0.24.0")
	require.NoError(t, err)
	_, err = c.ModelRegistry(context.Background(), "v0.24.0")
	require.NoError(t, err)
	_, err = c.ConfigJSON(context.Background(), "Org/M", "abc")
	require.NoError(t, err)

	assert.Equal(t, []string{
		"/vllm-project/vllm/v0.24.0/vllm/tool_parsers/__init__.py|",
		"/vllm-project/vllm/v0.24.0/vllm/model_executor/models/registry.py|",
		"/Org/M/resolve/abc/config.json|Bearer hf_test",
	}, auth)
}

func TestListPageDecodesTheRecordedShapeAndFollowsLink(t *testing.T) {
	const page = `[{"_id":"x","id":"google/gemma-3-27b-it","sha":"005ad3404e59d6023443cb575daa05336842228a","gated":"manual","pipeline_tag":"image-text-to-text","cardData":{"license":"gemma","license_name":null,"pipeline_tag":"image-text-to-text"},"createdAt":"2025-03-01T19:10:19.000Z","downloads":12,"tags":["license:gemma"]},
{"_id":"y","id":"Org/listed-license","sha":"1111111111111111111111111111111111111111","gated":false,"cardData":{"license":["mit","apache-2.0"]},"createdAt":"2026-09-25T00:00:00.000Z"}]`
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Header().Set("Link", `<https://huggingface.co/api/models?author=google&cursor=abc>; rel="next"`)
		_, _ = w.Write([]byte(page))
	}))
	defer srv.Close()
	c := newClient(srv, NewBudget(100, 50), nil)

	items, next, err := c.ListPage(context.Background(), c.ListURL(url.Values{"author": {"google"}}))

	require.NoError(t, err)
	assert.Equal(t, ListExpand, gotQuery["expand[]"])
	assert.Equal(t, "https://huggingface.co/api/models?author=google&cursor=abc", next)
	require.Len(t, items, 2)
	assert.Equal(t, Gated("manual"), items[0].Gated)
	assert.Equal(t, FlexString("gemma"), items[0].CardData.License)
	assert.Equal(t, "", string(items[0].CardData.LicenseName))
	assert.Equal(t, time.Date(2025, 3, 1, 19, 10, 19, 0, time.UTC), items[0].CreatedAt)
	assert.Equal(t, Gated(""), items[1].Gated)
	assert.Equal(t, FlexString("mit"), items[1].CardData.License)
	assert.Equal(t, "", items[1].PipelineTag)
}

func TestTreeFollowsTheCursorAcrossPages(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") == "" {
			w.Header().Set("Link", "<"+srv.URL+r.URL.Path+`?recursive=true&cursor=2>; rel="next"`)
			_, _ = w.Write([]byte(`[{"type":"file","path":"a.safetensors","size":1}]`))
			return
		}
		_, _ = w.Write([]byte(`[{"type":"file","path":"b.safetensors","size":2}]`))
	}))
	defer srv.Close()
	b := NewBudget(100, 50)
	c := newClient(srv, b, nil)

	tree, err := c.Tree(context.Background(), "Org/M", "abc")

	require.NoError(t, err)
	assert.Equal(t, []TreeEntry{{"file", "a.safetensors", 1}, {"file", "b.safetensors", 2}}, tree)
	assert.Equal(t, 2, b.Used())
}

func TestHasChatTemplateAcceptsEitherShape(t *testing.T) {
	for raw, want := range map[string]bool{
		`"{% for m in messages %}"`:           true,
		`[{"name":"default","template":"x"}]`: true,
		`""`:                                  false,
		`null`:                                false,
		`[]`:                                  false,
		``:                                    false,
	} {
		var m ModelInfo
		m.Config.TokenizerConfig.ChatTemplate = []byte(raw)
		assert.Equal(t, want, m.HasChatTemplate(), raw)
	}
}

// The recorded page is Qwen's newest three repos on 2026-09-29, fetched with
// ListExpand: pipeline_tag is absent (not null) when unset, and gated is a
// bare false for an open repo.
func TestListPageDecodesARecordedPage(t *testing.T) {
	page, err := os.ReadFile("../audittest/testdata/list-page-qwen.json")
	require.NoError(t, err)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(page)
	}))
	defer srv.Close()
	c := newClient(srv, NewBudget(100, 50), nil)

	items, next, err := c.ListPage(context.Background(), c.ListURL(url.Values{"author": {"Qwen"}}))

	require.NoError(t, err)
	assert.Empty(t, next)
	require.Len(t, items, 3)
	first := items[0]
	assert.Equal(t, "Qwen/Qwen-Image-2.1-PE-I2I", first.ID)
	assert.Equal(t, "72927bc08afc99b7888ceb7d7d51a12db3700bbd", first.SHA)
	assert.Equal(t, time.Date(2026, 9, 20, 8, 46, 47, 0, time.UTC), first.CreatedAt)
	assert.Equal(t, int64(9551), first.Downloads)
	assert.Equal(t, "", first.PipelineTag)
	assert.Equal(t, Gated(""), first.Gated)
	assert.Equal(t, FlexString("other"), first.CardData.License)
	assert.Equal(t, FlexString("qwen-research"), first.CardData.LicenseName)
	assert.Equal(t, "text-to-image", items[1].PipelineTag)
	assert.Contains(t, items[2].Tags, "license:other")
}

func TestModelRegistryDrawsOnTheDiscoveryBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	b := NewBudget(10, 1)
	c := newClient(srv, b, nil)

	_, err := c.ModelRegistry(context.Background(), "v0.24.0")
	require.NoError(t, err)
	_, err = c.ModelRegistry(context.Background(), "v0.24.0")
	assert.ErrorIs(t, err, ErrBudget, "discovery's share is spent although the total is not")
}
