package jira

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// ErrReadOnly is returned by a write attempted on a read-only client, so a
// dry run cannot post even through a code path that forgot to check.
var ErrReadOnly = errors.New("jira: write refused on a read-only (dry-run) client")

type Client struct {
	Base     string
	Email    string
	Token    string
	HTTP     *http.Client
	ReadOnly bool
}

type Issue struct {
	Key    string `json:"key"`
	Fields struct {
		Summary     string          `json:"summary"`
		Description json.RawMessage `json:"description"`
		Labels      []string        `json:"labels"`
		Created     string          `json:"created"`
	} `json:"fields"`
}

type Comment struct {
	ID     string `json:"id"`
	Author struct {
		AccountID string `json:"accountId"`
	} `json:"author"`
	Body json.RawMessage `json:"body"`
}

// CreateInput is a new issue under a parent, with an ADF description.
type CreateInput struct {
	Project     string
	IssueType   string
	Parent      string
	Summary     string
	Labels      []string
	Description Node
}

// StatusError is a non-2xx answer.
type StatusError struct {
	Method, Path string
	Status       int
	Body         string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.Path, e.Status, e.Body)
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	if method != http.MethodGet && c.ReadOnly {
		return ErrReadOnly
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, body)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.Email, c.Token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	httpc := c.HTTP
	if httpc == nil {
		httpc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := string(raw)
		if len(msg) > 200 {
			msg = msg[:200]
		}
		// The path alone: the query carries the whole JQL, which would bury
		// the status in the one line a failed run prints.
		p, _, _ := bytes.Cut([]byte(path), []byte("?"))
		return &StatusError{Method: method, Path: string(p), Status: resp.StatusCode, Body: msg}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode %s %s: %w", method, path, err)
		}
	}
	return nil
}

// Search runs JQL through /search/jql, following nextPageToken until isLast
// or max issues. The older /search endpoint answers 410 Removed.
func (c *Client) Search(ctx context.Context, jql string, max int) ([]Issue, error) {
	var all []Issue
	token := ""
	for {
		q := url.Values{
			"jql":        {jql},
			"fields":     {"summary,description,labels,created"},
			"maxResults": {strconv.Itoa(max - len(all))},
		}
		if token != "" {
			q.Set("nextPageToken", token)
		}
		var page struct {
			Issues        []Issue `json:"issues"`
			NextPageToken string  `json:"nextPageToken"`
			IsLast        bool    `json:"isLast"`
		}
		if err := c.do(ctx, http.MethodGet, "/rest/api/3/search/jql?"+q.Encode(), nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Issues...)
		if page.IsLast || page.NextPageToken == "" || len(all) >= max {
			if len(all) > max {
				all = all[:max]
			}
			return all, nil
		}
		token = page.NextPageToken
	}
}

// Comments returns every comment on an issue, paging by startAt.
func (c *Client) Comments(ctx context.Context, key string) ([]Comment, error) {
	var all []Comment
	for {
		var page struct {
			Comments []Comment `json:"comments"`
			Total    int       `json:"total"`
		}
		path := fmt.Sprintf("/rest/api/3/issue/%s/comment?startAt=%d&maxResults=100", url.PathEscape(key), len(all))
		if err := c.do(ctx, http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Comments...)
		if len(page.Comments) == 0 || len(all) >= page.Total {
			return all, nil
		}
	}
}

// Myself is the account id the credentials act as.
func (c *Client) Myself(ctx context.Context) (string, error) {
	var me struct {
		AccountID string `json:"accountId"`
	}
	if err := c.do(ctx, http.MethodGet, "/rest/api/3/myself", nil, &me); err != nil {
		return "", err
	}
	return me.AccountID, nil
}

func (c *Client) CreateIssue(ctx context.Context, in CreateInput) (string, error) {
	fields := map[string]any{
		"project":     map[string]string{"key": in.Project},
		"issuetype":   map[string]string{"name": in.IssueType},
		"parent":      map[string]string{"key": in.Parent},
		"summary":     in.Summary,
		"labels":      in.Labels,
		"description": in.Description,
	}
	var out struct {
		Key string `json:"key"`
	}
	if err := c.do(ctx, http.MethodPost, "/rest/api/3/issue", map[string]any{"fields": fields}, &out); err != nil {
		return "", err
	}
	return out.Key, nil
}

func (c *Client) AddComment(ctx context.Context, key string, body Node) error {
	return c.do(ctx, http.MethodPost, "/rest/api/3/issue/"+url.PathEscape(key)+"/comment", map[string]any{"body": body}, nil)
}
