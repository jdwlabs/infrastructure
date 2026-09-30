package audittest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
)

// BotAccount is the account id the fake's /myself answers.
const BotAccount = "audit-bot"

// Issue is one fake ticket; Description and comment bodies are raw ADF.
type Issue struct {
	Key         string
	Labels      []string
	Created     string
	Description json.RawMessage
	Comments    []Comment
}

type Comment struct {
	Author string
	Body   json.RawMessage
}

// Jira is a fake of the Jira Cloud REST v3 endpoints the audit uses. Issues
// are held newest first, the order the audit's JQL asks for.
type Jira struct {
	Server *httptest.Server

	mu           sync.Mutex
	Issues       []*Issue
	SearchStatus int
	WriteStatus  int
	methods      []string
	created      []json.RawMessage
	nextKey      int
}

func NewJira(t *testing.T) *Jira {
	t.Helper()
	j := &Jira{nextKey: 100}
	j.Server = httptest.NewServer(http.HandlerFunc(j.serve))
	t.Cleanup(j.Server.Close)
	return j
}

// Calls lists "METHOD path" for every request, in order.
func (j *Jira) Calls() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]string(nil), j.methods...)
}

// Posts counts the write requests received.
func (j *Jira) Posts() int {
	n := 0
	for _, c := range j.Calls() {
		if !strings.HasPrefix(c, "GET ") {
			n++
		}
	}
	return n
}

// Created returns the raw `fields` of every issue created, in order.
func (j *Jira) Created() []json.RawMessage {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]json.RawMessage(nil), j.created...)
}

func (j *Jira) find(key string) *Issue {
	for _, i := range j.Issues {
		if i.Key == key {
			return i
		}
	}
	return nil
}

var labelRe = regexp.MustCompile(`labels = "?([A-Za-z0-9-]+)"?`)

func (j *Jira) serve(w http.ResponseWriter, r *http.Request) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.methods = append(j.methods, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && path == "/rest/api/3/search":
		w.WriteHeader(http.StatusGone)
	case r.Method == http.MethodGet && path == "/rest/api/3/search/jql":
		if j.SearchStatus != 0 {
			w.WriteHeader(j.SearchStatus)
			return
		}
		m := labelRe.FindStringSubmatch(r.URL.Query().Get("jql"))
		var issues []map[string]any
		for _, i := range j.Issues {
			if m != nil && slices.Contains(i.Labels, m[1]) {
				issues = append(issues, map[string]any{"key": i.Key, "fields": map[string]any{
					"summary": "s", "labels": i.Labels, "created": i.Created, "description": i.Description,
				}})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"issues": issues, "isLast": true})
	case r.Method == http.MethodGet && path == "/rest/api/3/myself":
		_, _ = fmt.Fprintf(w, `{"accountId":%q}`, BotAccount)
	case strings.HasPrefix(path, "/rest/api/3/issue/") && strings.HasSuffix(path, "/comment"):
		key := strings.TrimSuffix(strings.TrimPrefix(path, "/rest/api/3/issue/"), "/comment")
		issue := j.find(key)
		if issue == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodGet {
			var cs []map[string]any
			for _, c := range issue.Comments {
				cs = append(cs, map[string]any{"id": "1", "author": map[string]string{"accountId": c.Author}, "body": c.Body})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"comments": cs, "total": len(cs)})
			return
		}
		if j.WriteStatus != 0 {
			w.WriteHeader(j.WriteStatus)
			return
		}
		var in struct {
			Body json.RawMessage `json:"body"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &in)
		issue.Comments = append(issue.Comments, Comment{Author: BotAccount, Body: in.Body})
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"2"}`))
	case r.Method == http.MethodPost && path == "/rest/api/3/issue":
		if j.WriteStatus != 0 {
			w.WriteHeader(j.WriteStatus)
			return
		}
		var in struct {
			Fields struct {
				Labels      []string        `json:"labels"`
				Description json.RawMessage `json:"description"`
			} `json:"fields"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &in)
		var raw struct {
			Fields json.RawMessage `json:"fields"`
		}
		_ = json.Unmarshal(b, &raw)
		j.created = append(j.created, raw.Fields)
		j.nextKey++
		key := fmt.Sprintf("AUDIT-%d", j.nextKey)
		j.Issues = append([]*Issue{{Key: key, Labels: in.Fields.Labels, Created: "2026-09-28T06:00:00.000+0000", Description: in.Fields.Description}}, j.Issues...)
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"key":%q}`, key)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}
