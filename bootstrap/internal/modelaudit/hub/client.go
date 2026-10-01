package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultHubBase = "https://huggingface.co"
	DefaultRawBase = "https://raw.githubusercontent.com"

	maxRetries    = 3
	maxRetryAfter = 60 * time.Second
	maxBodyBytes  = 32 << 20
)

// ListExpand is every list field used downstream. expand replaces the
// default field set rather than adding to it, so a field missing here is
// silently absent from every item.
var ListExpand = []string{"sha", "createdAt", "lastModified", "downloads", "pipeline_tag", "tags", "gated", "cardData"}

// StatusError is a non-2xx answer after retries.
type StatusError struct {
	URL    string
	Status int
}

func (e *StatusError) Error() string { return fmt.Sprintf("GET %s: HTTP %d", e.URL, e.Status) }

type Client struct {
	HubBase string
	RawBase string
	// Token is sent to the Hub only, never to GitHub.
	Token  string
	HTTP   *http.Client
	Budget *Budget
	// Sleep waits between retries; tests replace it so backoff costs no time.
	Sleep func(context.Context, time.Duration) error
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) get(ctx context.Context, phase Phase, rawURL string, hubAuth bool) ([]byte, http.Header, error) {
	sleep := c.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	httpc := c.HTTP
	if httpc == nil {
		httpc = &http.Client{Timeout: 30 * time.Second}
	}
	backoff := time.Second
	for attempt := 0; ; attempt++ {
		if err := c.Budget.take(phase); err != nil {
			return nil, nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, nil, err
		}
		if hubAuth && c.Token != "" {
			req.Header.Set("Authorization", "Bearer "+c.Token)
		}
		resp, err := httpc.Do(req)
		if err != nil {
			return nil, nil, err
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if readErr != nil {
				return nil, nil, readErr
			}
			return body, resp.Header, nil
		}
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		if !retryable || attempt == maxRetries {
			return nil, nil, &StatusError{URL: rawURL, Status: resp.StatusCode}
		}
		wait := backoff
		if ra, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && ra >= 0 {
			wait = time.Duration(ra) * time.Second
		}
		// A server-chosen wait could otherwise outlast the workflow's
		// ten-minute job timeout on its own.
		if wait > maxRetryAfter {
			wait = maxRetryAfter
		}
		if err := sleep(ctx, wait); err != nil {
			return nil, nil, err
		}
		backoff *= 2
	}
}

// Gated is "" for an open repo, else the Hub's gating mode ("auto", "manual").
type Gated string

func (g *Gated) UnmarshalJSON(b []byte) error {
	var asBool bool
	if err := json.Unmarshal(b, &asBool); err == nil {
		if asBool {
			*g = "true"
		} else {
			*g = ""
		}
		return nil
	}
	var asString string
	if err := json.Unmarshal(b, &asString); err != nil {
		return fmt.Errorf("gated: %w", err)
	}
	*g = Gated(asString)
	return nil
}

// FlexString accepts a string, null, or a list of strings (first wins):
// model-card metadata is free-form YAML, and cards write license as either.
type FlexString string

func (f *FlexString) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = FlexString(s)
		return nil
	}
	var list []string
	if err := json.Unmarshal(b, &list); err == nil {
		if len(list) > 0 {
			*f = FlexString(list[0])
		}
		return nil
	}
	*f = ""
	return nil
}

type CardData struct {
	License     FlexString `json:"license"`
	LicenseName FlexString `json:"license_name"`
	PipelineTag FlexString `json:"pipeline_tag"`
}

type ListItem struct {
	ID           string    `json:"id"`
	SHA          string    `json:"sha"`
	CreatedAt    time.Time `json:"createdAt"`
	LastModified time.Time `json:"lastModified"`
	Downloads    int64     `json:"downloads"`
	PipelineTag  string    `json:"pipeline_tag"`
	Tags         []string  `json:"tags"`
	Gated        Gated     `json:"gated"`
	CardData     CardData  `json:"cardData"`
}

// ListURL builds a /api/models query with ListExpand appended.
func (c *Client) ListURL(q url.Values) string {
	q = cloneValues(q)
	for _, e := range ListExpand {
		q.Add("expand[]", e)
	}
	return c.HubBase + "/api/models?" + q.Encode()
}

func cloneValues(q url.Values) url.Values {
	out := url.Values{}
	for k, v := range q {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// ListPage fetches one page of models and the next page's URL ("" at the end).
func (c *Client) ListPage(ctx context.Context, pageURL string) ([]ListItem, string, error) {
	body, hdr, err := c.get(ctx, Discovery, pageURL, true)
	if err != nil {
		return nil, "", err
	}
	var items []ListItem
	if err := json.Unmarshal(body, &items); err != nil {
		return nil, "", fmt.Errorf("decode model list: %w", err)
	}
	return items, nextLink(hdr.Get("Link")), nil
}

var nextLinkRe = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="next"`)

func nextLink(h string) string {
	if m := nextLinkRe.FindStringSubmatch(h); m != nil {
		return m[1]
	}
	return ""
}

// ModelInfo is the /revision answer at a pinned sha.
type ModelInfo struct {
	ID     string   `json:"id"`
	Tags   []string `json:"tags"`
	Config struct {
		ModelType       string `json:"model_type"`
		TokenizerConfig struct {
			// A string, or a list of named templates; only emptiness matters.
			ChatTemplate json.RawMessage `json:"chat_template"`
		} `json:"tokenizer_config"`
	} `json:"config"`
}

// HasChatTemplate reports a non-empty chat template in either of its shapes.
func (m ModelInfo) HasChatTemplate() bool {
	raw := strings.TrimSpace(string(m.Config.TokenizerConfig.ChatTemplate))
	return raw != "" && raw != "null" && raw != `""` && raw != "[]"
}

type TreeEntry struct {
	Type string `json:"type"`
	Path string `json:"path"`
	Size int64  `json:"size"`
}

func repoPath(id string) string {
	parts := strings.Split(id, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func (c *Client) ModelInfo(ctx context.Context, id, sha string) (ModelInfo, error) {
	u := fmt.Sprintf("%s/api/models/%s/revision/%s?expand[]=config&expand[]=tags", c.HubBase, repoPath(id), url.PathEscape(sha))
	body, _, err := c.get(ctx, Enrichment, u, true)
	if err != nil {
		return ModelInfo{}, err
	}
	var m ModelInfo
	if err := json.Unmarshal(body, &m); err != nil {
		return ModelInfo{}, fmt.Errorf("decode model info: %w", err)
	}
	return m, nil
}

// Tree lists every file at sha, following the Link cursor across pages.
func (c *Client) Tree(ctx context.Context, id, sha string) ([]TreeEntry, error) {
	next := fmt.Sprintf("%s/api/models/%s/tree/%s?recursive=true", c.HubBase, repoPath(id), url.PathEscape(sha))
	var all []TreeEntry
	for next != "" {
		body, hdr, err := c.get(ctx, Enrichment, next, true)
		if err != nil {
			return nil, err
		}
		var page []TreeEntry
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("decode tree: %w", err)
		}
		all = append(all, page...)
		next = nextLink(hdr.Get("Link"))
	}
	return all, nil
}

// ConfigJSON fetches the repo's full config.json at sha.
func (c *Client) ConfigJSON(ctx context.Context, id, sha string) (map[string]any, error) {
	u := fmt.Sprintf("%s/%s/resolve/%s/config.json", c.HubBase, repoPath(id), url.PathEscape(sha))
	body, _, err := c.get(ctx, Enrichment, u, true)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("decode config.json: %w", err)
	}
	if m == nil {
		return nil, errors.New("decode config.json: not an object")
	}
	return m, nil
}

// ToolParserRegistry fetches vllm/tool_parsers/__init__.py at a vLLM tag.
func (c *Client) ToolParserRegistry(ctx context.Context, tag string) ([]byte, error) {
	u := fmt.Sprintf("%s/vllm-project/vllm/%s/vllm/tool_parsers/__init__.py", c.RawBase, url.PathEscape(tag))
	body, _, err := c.get(ctx, Discovery, u, false)
	return body, err
}

// ModelRegistry fetches vllm/model_executor/models/registry.py at a vLLM tag.
func (c *Client) ModelRegistry(ctx context.Context, tag string) ([]byte, error) {
	u := fmt.Sprintf("%s/vllm-project/vllm/%s/vllm/model_executor/models/registry.py", c.RawBase, url.PathEscape(tag))
	body, _, err := c.get(ctx, Discovery, u, false)
	return body, err
}
