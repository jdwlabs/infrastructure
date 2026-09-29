package jira

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSearchPagesByNextPageTokenAndSendsFields(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/rest/api/3/search/jql", r.URL.Path)
		user, pass, ok := r.BasicAuth()
		assert.True(t, ok)
		assert.Equal(t, "bot@example.com", user)
		assert.Equal(t, "tok", pass)
		q := r.URL.Query()
		assert.Equal(t, "summary,description,labels,created", q.Get("fields"))
		queries = append(queries, q.Get("nextPageToken")+"|"+q.Get("maxResults"))
		if q.Get("nextPageToken") == "" {
			_, _ = w.Write([]byte(`{"issues":[{"key":"A-3"},{"key":"A-2"}],"nextPageToken":"p2","isLast":false}`))
			return
		}
		_, _ = w.Write([]byte(`{"issues":[{"key":"A-1"}],"isLast":true}`))
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, Email: "bot@example.com", Token: "tok", HTTP: srv.Client()}

	issues, err := c.Search(context.Background(), "project = A", 26)

	require.NoError(t, err)
	require.Len(t, issues, 3)
	assert.Equal(t, "A-1", issues[2].Key)
	assert.Equal(t, []string{"|26", "p2|24"}, queries)
}

func TestSearchStopsAtMax(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"issues":[{"key":"A-3"},{"key":"A-2"}],"nextPageToken":"p2","isLast":false}`))
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client()}

	issues, err := c.Search(context.Background(), "project = A", 2)

	require.NoError(t, err)
	assert.Len(t, issues, 2)
}

func TestSearchErrorNamesPathAndStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"errorMessages":["boom"]}`))
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client()}

	_, err := c.Search(context.Background(), "project = A", 26)

	var se *StatusError
	require.ErrorAs(t, err, &se)
	assert.Equal(t, 500, se.Status)
	assert.Equal(t, "/rest/api/3/search/jql", se.Path)
	assert.NotContains(t, err.Error(), "project")
}

func TestCommentsPagesByStartAt(t *testing.T) {
	var starts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/rest/api/3/issue/A-1/comment", r.URL.Path)
		starts = append(starts, r.URL.Query().Get("startAt"))
		if r.URL.Query().Get("startAt") == "0" {
			_, _ = w.Write([]byte(`{"comments":[{"id":"1","author":{"accountId":"me"},"body":{"type":"doc"}}],"total":2}`))
			return
		}
		_, _ = w.Write([]byte(`{"comments":[{"id":"2","author":{"accountId":"you"}}],"total":2}`))
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client()}

	cs, err := c.Comments(context.Background(), "A-1")

	require.NoError(t, err)
	require.Len(t, cs, 2)
	assert.Equal(t, "me", cs[0].Author.AccountID)
	assert.Equal(t, []string{"0", "1"}, starts)
}

func TestCreateIssueSendsParentLabelsAndADF(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/rest/api/3/issue", r.URL.Path)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		b, _ := io.ReadAll(r.Body)
		assert.NoError(t, json.Unmarshal(b, &got))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"1","key":"A-9"}`))
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client()}

	key, err := c.CreateIssue(context.Background(), CreateInput{
		Project: "A", IssueType: "Task", Parent: "A-1", Summary: "s",
		Labels: []string{"model-audit"}, Description: Doc(Paragraph(Text("hi"))),
	})

	require.NoError(t, err)
	assert.Equal(t, "A-9", key)
	fields := got["fields"].(map[string]any)
	assert.Equal(t, map[string]any{"key": "A-1"}, fields["parent"])
	assert.Equal(t, map[string]any{"key": "A"}, fields["project"])
	assert.Equal(t, map[string]any{"name": "Task"}, fields["issuetype"])
	assert.Equal(t, []any{"model-audit"}, fields["labels"])
	desc := fields["description"].(map[string]any)
	assert.Equal(t, "doc", desc["type"])
	assert.Equal(t, 1.0, desc["version"])
}

func TestReadOnlyClientRefusesWritesWithoutSendingThem(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client(), ReadOnly: true}

	_, err := c.CreateIssue(context.Background(), CreateInput{})
	assert.ErrorIs(t, err, ErrReadOnly)
	assert.ErrorIs(t, c.AddComment(context.Background(), "A-1", Doc()), ErrReadOnly)
	assert.Zero(t, hits)
}
