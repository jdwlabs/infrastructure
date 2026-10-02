package vllm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testSpec builds the Spec Check/Wait compare against. The gate dials
// g.BaseURL, not s.Port, so the port here is a placeholder.
func testSpec(timeout time.Duration) Spec {
	return Spec{
		ServedName: "qwen/qwen3-coder-30b-a3b",
		Model:      Model{Repo: "QuantTrio/Qwen3-Coder-30B-A3B-Instruct-AWQ"},
		Port:       8000,
		HealthGate: Gate{Timeout: timeout},
	}
}

// chatResponse renders a minimal /v1/chat/completions body. toolCalls, when
// non-empty, is embedded verbatim as the tool_calls JSON array.
func chatResponse(finishReason, toolCalls string) string {
	tc := toolCalls
	if tc == "" {
		tc = "null"
	}
	return `{"choices":[{"finish_reason":"` + finishReason + `","message":{"tool_calls":` + tc + `}}]}`
}

const sampleToolCall = `[{"id":"call_1","type":"function","function":{"name":"get_time","arguments":"{}"}}]`

// healthyHandler serves /v1/models with a matching entry, a 1-token
// completion that stops, and a tool call on every chat request — the
// handler doesn't distinguish the plain-completion and tool-call requests
// bodies, so it always answers with a tool call, which the plain-completion
// check ignores.
func healthyHandler(servedName, repo string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]string{{"id": servedName, "root": repo}},
			})
		case "/v1/chat/completions":
			_, _ = w.Write([]byte(chatResponse("stop", sampleToolCall)))
		default:
			http.NotFound(w, r)
		}
	}
}

func TestCheckPassesOnHealthyServer(t *testing.T) {
	srv := httptest.NewServer(healthyHandler("qwen/qwen3-coder-30b-a3b", "QuantTrio/Qwen3-Coder-30B-A3B-Instruct-AWQ"))
	defer srv.Close()

	g := HealthGate{BaseURL: srv.URL}
	s := testSpec(time.Minute)
	err := g.Check(context.Background(), s)
	assert.NoError(t, err)
}

func TestCheckFailsNamingRootOnMismatch(t *testing.T) {
	srv := httptest.NewServer(healthyHandler("qwen/qwen3-coder-30b-a3b", "wrong/repo"))
	defer srv.Close()

	g := HealthGate{BaseURL: srv.URL}
	s := testSpec(time.Minute)
	err := g.Check(context.Background(), s)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "root")
	assert.Contains(t, err.Error(), "wrong/repo")
}

func TestCheckFailsNamingToolCallWhenAbsent(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"id": "qwen/qwen3-coder-30b-a3b", "root": "QuantTrio/Qwen3-Coder-30B-A3B-Instruct-AWQ"}},
		})
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		// Every chat request, tool-call or plain, answers with no tool_calls —
		// that's the one thing under test here.
		_, _ = w.Write([]byte(chatResponse("stop", "")))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	g := HealthGate{BaseURL: srv.URL}
	s := testSpec(time.Minute)
	err := g.Check(context.Background(), s)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tool call")
}

// toolCallOnlyHandler routes the plain-completion check (no "tools" key)
// straight to a passing reply, and hands every request that does carry
// "tools" to fn — isolating the tool-call probe's own retries from the
// unrelated completion check that shares the same endpoint.
func toolCallOnlyHandler(fn func(w http.ResponseWriter, body map[string]any)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, ok := body["tools"]; !ok {
			_, _ = w.Write([]byte(chatResponse("stop", "")))
			return
		}
		fn(w, body)
	}
}

func modelsHandler(servedName, repo string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"id": servedName, "root": repo}},
		})
	}
}

// TestCheckToolCallRetriesAProseMissThenSucceeds pins the fix for review
// finding D: a single prose reply used to fail the whole check, which made
// Apply's no-change path (a single Check, no Wait) report a healthy server
// as down whenever the tool call was missed once.
func TestCheckToolCallRetriesAProseMissThenSucceeds(t *testing.T) {
	var attempts atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", modelsHandler("qwen/qwen3-coder-30b-a3b", "QuantTrio/Qwen3-Coder-30B-A3B-Instruct-AWQ"))
	mux.HandleFunc("/v1/chat/completions", toolCallOnlyHandler(func(w http.ResponseWriter, body map[string]any) {
		if attempts.Add(1) < 3 {
			_, _ = w.Write([]byte(chatResponse("stop", "")))
			return
		}
		_, _ = w.Write([]byte(chatResponse("stop", sampleToolCall)))
	}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	g := HealthGate{BaseURL: srv.URL}
	s := testSpec(time.Minute)
	err := g.Check(context.Background(), s)
	assert.NoError(t, err)
	assert.Equal(t, int32(3), attempts.Load())
}

func TestCheckToolCallFailsAfterThreeProseAnswers(t *testing.T) {
	var attempts atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", modelsHandler("qwen/qwen3-coder-30b-a3b", "QuantTrio/Qwen3-Coder-30B-A3B-Instruct-AWQ"))
	mux.HandleFunc("/v1/chat/completions", toolCallOnlyHandler(func(w http.ResponseWriter, body map[string]any) {
		attempts.Add(1)
		_, _ = w.Write([]byte(chatResponse("stop", "")))
	}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	g := HealthGate{BaseURL: srv.URL}
	s := testSpec(time.Minute)
	err := g.Check(context.Background(), s)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "prose")
	assert.Equal(t, int32(3), attempts.Load())
}

// TestCheckToolCallDoesNotRetryAnHTTPError pins that only a prose miss (HTTP
// 200, no tool_calls) is retried — a broken parser or a down server fails
// on the first attempt, exactly like every other check here.
func TestCheckToolCallDoesNotRetryAnHTTPError(t *testing.T) {
	var attempts atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", modelsHandler("qwen/qwen3-coder-30b-a3b", "QuantTrio/Qwen3-Coder-30B-A3B-Instruct-AWQ"))
	mux.HandleFunc("/v1/chat/completions", toolCallOnlyHandler(func(w http.ResponseWriter, body map[string]any) {
		attempts.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	g := HealthGate{BaseURL: srv.URL}
	s := testSpec(time.Minute)
	err := g.Check(context.Background(), s)
	require.Error(t, err)
	assert.Equal(t, int32(1), attempts.Load())
}

func TestToolCallCheckLeavesTheChoiceToTheParser(t *testing.T) {
	var toolReq map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"id": "qwen/qwen3-coder-30b-a3b", "root": "QuantTrio/Qwen3-Coder-30B-A3B-Instruct-AWQ"}},
		})
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
			return
		}
		if _, ok := body["tools"]; ok {
			toolReq = body
		}
		_, _ = w.Write([]byte(chatResponse("stop", sampleToolCall)))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	require.NoError(t, HealthGate{BaseURL: srv.URL}.Check(context.Background(), testSpec(time.Minute)))

	require.NotNil(t, toolReq, "the gate sent no request with tools")
	assert.Equal(t, "auto", toolReq["tool_choice"], "only auto routes the reply through --tool-call-parser, as consumers' requests do")
	msgs, ok := toolReq["messages"].([]any)
	require.True(t, ok)
	require.Len(t, msgs, 2)
	assert.Equal(t, "system", msgs[0].(map[string]any)["role"])
	assert.Equal(t, "user", msgs[1].(map[string]any)["role"])
	var text strings.Builder
	for _, m := range msgs {
		text.WriteString(m.(map[string]any)["content"].(string))
	}
	assert.Contains(t, text.String(), "get_time", "with auto, the prompt is what makes the model call the tool")
}

func TestWaitSucceedsAfterServerTurnsHealthy(t *testing.T) {
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n < 3 {
			http.Error(w, "loading", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"id": "qwen/qwen3-coder-30b-a3b", "root": "QuantTrio/Qwen3-Coder-30B-A3B-Instruct-AWQ"}},
		})
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(chatResponse("stop", sampleToolCall)))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	g := HealthGate{BaseURL: srv.URL, Poll: 10 * time.Millisecond}
	s := testSpec(time.Second)
	err := g.Wait(context.Background(), s)
	assert.NoError(t, err)
	assert.GreaterOrEqual(t, int(calls.Load()), 3)
}

func TestWaitReturnsLastCheckErrorWhenTimeoutElapses(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "loading", http.StatusServiceUnavailable)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	g := HealthGate{BaseURL: srv.URL, Poll: 10 * time.Millisecond}
	s := testSpec(50 * time.Millisecond)
	start := time.Now()
	err := g.Wait(context.Background(), s)
	elapsed := time.Since(start)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "models")
	assert.Less(t, elapsed, time.Second, "Wait must not sleep past the deadline")
}

func TestWaitStopsPromptlyOnContextCancellation(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "loading", http.StatusServiceUnavailable)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	g := HealthGate{BaseURL: srv.URL, Poll: 10 * time.Millisecond}
	s := testSpec(time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)

	start := time.Now()
	err := g.Wait(ctx, s)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Contains(t, err.Error(), "models")
	assert.Less(t, elapsed, 500*time.Millisecond, "Wait must stop promptly on cancellation, not run out the hour timeout")
}

func TestCheckFailsNamingModelsWhenServedNameAbsent(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"id": "some/other-model", "root": "Some/OtherRepo"}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	g := HealthGate{BaseURL: srv.URL}
	s := testSpec(time.Minute)
	err := g.Check(context.Background(), s)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "models")
	assert.Contains(t, err.Error(), s.ServedName)
}

// TestWaitPreservesSubstantiveErrorAcrossATimedOutAttempt pins a second
// review finding: once a per-attempt deadline bounds each Check, a later
// attempt that only fails because that per-attempt context expired must
// not paper over an earlier attempt's real diagnosis (wrong model) with a
// bare "context deadline exceeded". The server answers the first request
// with a wrong root, then hangs on every subsequent request until that
// request's own context ends — so the final error must still carry both
// the original model-mismatch detail and the fact that the gate timed out.
func TestWaitPreservesSubstantiveErrorAcrossATimedOutAttempt(t *testing.T) {
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]string{{"id": "qwen/qwen3-coder-30b-a3b", "root": "wrong/repo"}},
			})
			return
		}
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	g := HealthGate{BaseURL: srv.URL, Poll: 20 * time.Millisecond}
	s := testSpec(300 * time.Millisecond)

	err := g.Wait(context.Background(), s)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "root")
	assert.Contains(t, err.Error(), "timed out")
}

// TestWaitBoundsAHangingRequestToTheOverallDeadline pins the fix for a gap
// found in review: a request that never returns was bounded only by the
// HTTP client's own timeout, which can far outlive s.HealthGate.Timeout.
// The server here blocks on its own request context instead of ever
// responding, so the only thing that can end the attempt is Wait attaching
// a per-attempt deadline to the context it hands Check.
func TestWaitBoundsAHangingRequestToTheOverallDeadline(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	g := HealthGate{
		BaseURL: srv.URL,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
		Poll:    10 * time.Millisecond,
	}
	s := testSpec(200 * time.Millisecond)

	start := time.Now()
	err := g.Wait(context.Background(), s)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Less(t, elapsed, time.Second, "Wait must not wait out the HTTP client's 30s timeout")
	assert.True(t,
		strings.Contains(err.Error(), "deadline") || strings.Contains(err.Error(), "timeout"),
		"error should name the timeout/deadline, got: %v", err)
}

func TestModelsParsesIDAndRoot(t *testing.T) {
	srv := httptest.NewServer(healthyHandler("qwen/qwen3-coder-30b-a3b", "QuantTrio/Qwen3-Coder-30B-A3B-Instruct-AWQ"))
	defer srv.Close()

	g := HealthGate{BaseURL: srv.URL}
	entries, err := g.Models(context.Background())
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "qwen/qwen3-coder-30b-a3b", entries[0].ID)
	assert.Equal(t, "QuantTrio/Qwen3-Coder-30B-A3B-Instruct-AWQ", entries[0].Root)
}

func TestModelsFailsOnUnreachableServer(t *testing.T) {
	g := HealthGate{BaseURL: "http://127.0.0.1:1"}
	_, err := g.Models(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "models")
}

// aliasedModelsHandler answers /v1/models the way vLLM does for several
// --served-model-name values: one entry per name, in order, all rooted at
// the one model repo. Every chat request's model is recorded.
func aliasedModelsHandler(entries []map[string]string, models *[]string, mu *sync.Mutex) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": entries})
		case "/v1/chat/completions":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			*models = append(*models, fmt.Sprint(body["model"]))
			mu.Unlock()
			_, _ = w.Write([]byte(chatResponse("stop", sampleToolCall)))
		default:
			http.NotFound(w, r)
		}
	}
}

func aliasedTestSpec() Spec {
	s := testSpec(time.Minute)
	s.ServedName = "local-chat"
	s.ServedAliases = []string{"qwen/qwen3-coder-30b-a3b"}
	return s
}

func TestCheckPassesWhenEveryAliasIsServedAndProbesUnderServedName(t *testing.T) {
	s := aliasedTestSpec()
	var models []string
	var mu sync.Mutex
	srv := httptest.NewServer(aliasedModelsHandler([]map[string]string{
		{"id": "local-chat", "root": s.Model.Repo},
		{"id": "qwen/qwen3-coder-30b-a3b", "root": s.Model.Repo},
	}, &models, &mu))
	defer srv.Close()

	require.NoError(t, HealthGate{BaseURL: srv.URL}.Check(context.Background(), s))
	require.NotEmpty(t, models)
	for _, m := range models {
		assert.Equal(t, "local-chat", m, "the completion and tool-call probes request servedName, the name every consumer is moving to")
	}
}

// An alias is what keeps a not-yet-moved consumer working, so a server that
// does not answer to one has broken that consumer, however healthy the
// servedName probes look.
func TestCheckFailsWhenAnAliasIsNotServed(t *testing.T) {
	s := aliasedTestSpec()
	var models []string
	var mu sync.Mutex
	cases := map[string][]map[string]string{
		"alias absent": {
			{"id": "local-chat", "root": s.Model.Repo},
		},
		"alias rooted at another model": {
			{"id": "local-chat", "root": s.Model.Repo},
			{"id": "qwen/qwen3-coder-30b-a3b", "root": "Some/OtherRepo"},
		},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(aliasedModelsHandler(entries, &models, &mu))
			defer srv.Close()

			err := HealthGate{BaseURL: srv.URL}.Check(context.Background(), s)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "models check")
			assert.Contains(t, err.Error(), "qwen/qwen3-coder-30b-a3b")
		})
	}
}
