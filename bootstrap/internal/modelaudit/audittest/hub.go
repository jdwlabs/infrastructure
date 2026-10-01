// Package audittest serves recorded Hugging Face, GitHub raw and Jira
// answers from httptest servers, so audit tests at every layer share one fake
// instead of each hand-rolling its own.
package audittest

import (
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

//go:embed testdata
var testdata embed.FS

// Fixture returns a recorded file from testdata.
func Fixture(name string) []byte {
	b, err := testdata.ReadFile("testdata/" + name)
	if err != nil {
		panic(err)
	}
	return b
}

// Now is the fixed clock every audit test runs at: Monday 2026-09-28, ISO
// week 2026-W40.
var Now = time.Date(2026, 9, 28, 6, 0, 0, 0, time.UTC)

// Model is one repo the fake lists and can enrich.
type Model struct {
	ID          string
	SHA         string
	CreatedAt   time.Time
	Downloads   int64
	PipelineTag string
	License     string
	Gated       string
	// Revision, Tree and Config are the enrichment answers; nil serves the
	// recorded incumbent's.
	Revision, Tree, Config []byte
	// EnrichStatus, when non-zero, answers every enrichment request with it.
	EnrichStatus int
	// ConfigStatus, when non-zero, answers only config.json with it, the way
	// the Hub answers a GGUF-only repo.
	ConfigStatus int
}

// ListJSON renders the list-endpoint shape recorded from
// /api/models?expand[]=... (fields the audit does not read are dropped).
func (m Model) ListJSON() map[string]any {
	card := map[string]any{"license": nilIfEmpty(m.License), "license_name": nil, "pipeline_tag": nil}
	var gated any = false
	if m.Gated != "" {
		gated = m.Gated
	}
	out := map[string]any{
		"_id": "x", "id": m.ID, "sha": m.SHA, "gated": gated, "downloads": m.Downloads,
		"createdAt":    m.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		"lastModified": m.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		"tags":         []string{"transformers", "safetensors", "conversational"},
		"cardData":     card,
	}
	if m.PipelineTag != "" {
		out["pipeline_tag"] = m.PipelineTag
	}
	return out
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Hub is a fake of huggingface.co and raw.githubusercontent.com on one server.
type Hub struct {
	Server *httptest.Server

	mu sync.Mutex
	// Orgs maps an author to its pages, newest first.
	Orgs map[string][][]Model
	// OrgStatus answers every list request for that org with the status.
	OrgStatus      map[string]int
	Trending       []Model
	TrendingStatus int
	Registry       []byte
	RegistryStatus int
	// ModelRegistry is vllm/model_executor/models/registry.py.
	ModelRegistry       []byte
	ModelRegistryStatus int
	requests            []string
	auth                []string
}

// NewHub starts a fake serving the recorded v0.24.0 registries and no models.
func NewHub(t *testing.T) *Hub {
	t.Helper()
	h := &Hub{
		Orgs:          map[string][][]Model{},
		OrgStatus:     map[string]int{},
		Registry:      Fixture("tool_parsers_v0.24.0.py"),
		ModelRegistry: Fixture("model_registry_v0.24.0.py"),
	}
	h.Server = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.Server.Close)
	return h
}

// Requests lists every path+query served, in order.
func (h *Hub) Requests() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.requests...)
}

// AuthHeaders lists every Authorization header received, in order.
func (h *Hub) AuthHeaders() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.auth...)
}

func (h *Hub) model(id string) (Model, bool) {
	for _, pages := range h.Orgs {
		for _, p := range pages {
			for _, m := range p {
				if m.ID == id {
					return m, true
				}
			}
		}
	}
	for _, m := range h.Trending {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}

func (h *Hub) serve(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.requests = append(h.requests, r.URL.RequestURI())
	h.auth = append(h.auth, r.Header.Get("Authorization"))
	path := r.URL.Path
	switch {
	case strings.HasPrefix(path, "/vllm-project/vllm/") && strings.HasSuffix(path, "/vllm/tool_parsers/__init__.py"):
		if h.RegistryStatus != 0 {
			w.WriteHeader(h.RegistryStatus)
			return
		}
		_, _ = w.Write(h.Registry)
	case strings.HasPrefix(path, "/vllm-project/vllm/") && strings.HasSuffix(path, "/vllm/model_executor/models/registry.py"):
		if h.ModelRegistryStatus != 0 {
			w.WriteHeader(h.ModelRegistryStatus)
			return
		}
		_, _ = w.Write(h.ModelRegistry)
	case path == "/api/models":
		h.serveList(w, r)
	case strings.HasPrefix(path, "/api/models/"):
		h.serveEnrich(w, strings.TrimPrefix(path, "/api/models/"))
	case strings.Contains(path, "/resolve/") && strings.HasSuffix(path, "/config.json"):
		id := strings.TrimPrefix(path[:strings.Index(path, "/resolve/")], "/")
		m, ok := h.model(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		if st := max(m.EnrichStatus, m.ConfigStatus); st != 0 {
			w.WriteHeader(st)
			return
		}
		_, _ = w.Write(orDefault(m.Config, "incumbent-config.json"))
	default:
		http.NotFound(w, r)
	}
}

func orDefault(b []byte, fixture string) []byte {
	if b != nil {
		return b
	}
	return Fixture(fixture)
}

func (h *Hub) serveList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var page []Model
	if author := q.Get("author"); author != "" {
		if st := h.OrgStatus[author]; st != 0 {
			w.WriteHeader(st)
			return
		}
		pages := h.Orgs[author]
		n, _ := strconv.Atoi(q.Get("cursor"))
		if n < len(pages) {
			page = pages[n]
		}
		if n+1 < len(pages) {
			next := r.URL.Query()
			next.Set("cursor", strconv.Itoa(n+1))
			w.Header().Set("Link", fmt.Sprintf(`<%s/api/models?%s>; rel="next"`, h.Server.URL, next.Encode()))
		}
	} else {
		if h.TrendingStatus != 0 {
			w.WriteHeader(h.TrendingStatus)
			return
		}
		page = h.Trending
	}
	out := make([]map[string]any, 0, len(page))
	for _, m := range page {
		out = append(out, m.ListJSON())
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (h *Hub) serveEnrich(w http.ResponseWriter, rest string) {
	var id, kind string
	for _, k := range []string{"/revision/", "/tree/"} {
		if i := strings.Index(rest, k); i >= 0 {
			id, kind = rest[:i], k
		}
	}
	m, ok := h.model(id)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if m.EnrichStatus != 0 {
		w.WriteHeader(m.EnrichStatus)
		return
	}
	if kind == "/revision/" {
		_, _ = w.Write(orDefault(m.Revision, "incumbent-revision.json"))
		return
	}
	_, _ = w.Write(orDefault(m.Tree, "incumbent-tree.json"))
}
