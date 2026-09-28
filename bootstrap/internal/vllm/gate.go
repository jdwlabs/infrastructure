package vllm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// maxGateBodyBytes bounds how much of a response this reads: the gate talks
// to a remote server it doesn't control, and an unbounded read is an
// unbounded memory commitment to whatever that server sends back.
const maxGateBodyBytes = 1 << 20 // 1 MiB

// maxToolCallAttempts bounds how many times checkToolCall retries a prose
// miss: enough to absorb sampling flake without letting a parser that is
// truly broken take long to fail deterministically.
const maxToolCallAttempts = 3

// HealthGate decides whether a vLLM server is serving the model it was
// asked to serve: present in /v1/models, able to complete, able to call a
// tool. Zero value BaseURL is invalid; HTTP and Poll default on use.
type HealthGate struct {
	BaseURL string        // http://<host>:<port>
	HTTP    *http.Client  // per-request timeout 30s when nil
	Poll    time.Duration // 5s when zero
}

// ModelEntry is one /v1/models list entry.
type ModelEntry struct {
	ID   string // data[].id: the served name a client requests
	Root string // data[].root: the model repo actually loaded under that name
}

// Wait polls Check until it succeeds or s.HealthGate.Timeout elapses,
// sleeping g.pollInterval() between attempts. Each attempt runs under
// context.WithTimeout(ctx, remaining) so a single hung request can't run
// past the overall deadline on its own — the HTTP client's own timeout is
// only a backstop, not what bounds an attempt. It never sleeps past the
// deadline: the last poll runs, and if the remaining budget is shorter than
// the poll interval, Wait's own timer — not a full poll sleep — is what
// fires next. On ctx cancellation it returns immediately with the last
// check error joined with ctx.Err().
//
// An attempt cut short by its own per-attempt deadline carries no
// information beyond "it didn't finish in time" — it says nothing about
// why the server was unhealthy. So that timeout alone never overwrites a
// prior attempt's substantive failure (a wrong model, a missing tool
// call): Wait keeps the last such error and reports it alongside the
// overall timeout, so an operator sees what was actually wrong, not just
// that time ran out.
func (g HealthGate) Wait(ctx context.Context, s Spec) error {
	deadline := time.Now().Add(s.HealthGate.Timeout)

	var lastErr error         // the most recent attempt's raw error; used as-is only on external ctx cancellation
	var prevSubstantive error // the most recent error not solely due to the per-attempt deadline expiring

	for {
		attemptCtx, cancel := context.WithTimeout(ctx, time.Until(deadline))
		lastErr = g.Check(attemptCtx, s)
		timedOut := errors.Is(lastErr, context.DeadlineExceeded) && attemptCtx.Err() != nil
		cancel()

		if lastErr == nil {
			return nil
		}
		if !timedOut {
			prevSubstantive = lastErr
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			timeoutErr := fmt.Errorf("health gate timed out after %s", s.HealthGate.Timeout)
			if prevSubstantive != nil {
				return errors.Join(prevSubstantive, timeoutErr)
			}
			return errors.Join(lastErr, timeoutErr)
		}

		wait := g.pollInterval()
		if wait > remaining {
			wait = remaining
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(lastErr, ctx.Err())
		case <-timer.C:
		}
	}
}

// Check runs one attempt of all three checks, in order, returning the first
// failure. Each failure names the check that failed and the observed value.
func (g HealthGate) Check(ctx context.Context, s Spec) error {
	entries, err := g.Models(ctx)
	if err != nil {
		return err
	}

	var found *ModelEntry
	for i := range entries {
		if entries[i].ID == s.ServedName {
			found = &entries[i]
			break
		}
	}
	if found == nil {
		return fmt.Errorf("models check: no entry with id %q among %d model(s)", s.ServedName, len(entries))
	}
	if found.Root != s.Model.Repo {
		return fmt.Errorf("models check: entry %q has root %q, want %q", s.ServedName, found.Root, s.Model.Repo)
	}

	if err := g.checkCompletion(ctx, s); err != nil {
		return err
	}
	if err := g.checkToolCall(ctx, s); err != nil {
		return err
	}
	return nil
}

// Models fetches /v1/models and parses each data[] entry's id and root.
func (g HealthGate) Models(ctx context.Context) ([]ModelEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.BaseURL+"/v1/models", nil)
	if err != nil {
		return nil, fmt.Errorf("models check: build request: %w", err)
	}

	resp, err := g.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("models check: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGateBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("models check: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models check: status %s: %s", resp.Status, bytes.TrimSpace(body))
	}

	var parsed struct {
		Data []struct {
			ID   string `json:"id"`
			Root string `json:"root"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("models check: parse response: %w", err)
	}

	entries := make([]ModelEntry, len(parsed.Data))
	for i, d := range parsed.Data {
		entries[i] = ModelEntry{ID: d.ID, Root: d.Root}
	}
	return entries, nil
}

// checkCompletion asks for a 1-token completion and requires a
// finish_reason of stop or length — either is a real generation, as
// opposed to an error or an empty response.
func (g HealthGate) checkCompletion(ctx context.Context, s Spec) error {
	reqBody := map[string]any{
		"model":      s.ServedName,
		"max_tokens": 1,
		"messages": []map[string]string{
			{"role": "user", "content": "ok"},
		},
	}

	finishReason, _, err := g.chatCompletion(ctx, reqBody)
	if err != nil {
		return fmt.Errorf("completion check: %w", err)
	}
	if finishReason != "stop" && finishReason != "length" {
		return fmt.Errorf("completion check: finish_reason %q, want stop or length", finishReason)
	}
	return nil
}

// checkToolCall asks the model to use a trivial tool and requires at least
// one parsed tool_calls entry back — proof the server's tool-calling parser
// works end to end, not just that the model itself is up.
//
// tool_choice is "auto" because that is what consumers send, and only auto
// hands the model's free-form reply to --tool-call-parser to extract the
// calls; "required" makes vLLM constrain the output to the tool schema
// itself, which passes even with a parser that can parse nothing. With auto
// the model may answer in prose instead of calling the tool even on a
// healthy server, so a single miss here retries up to maxToolCallAttempts
// times before failing — Check runs once on a no-op apply, so without its
// own retry a prose reply there would report a healthy server as down. Only
// a prose miss (HTTP 200, no tool_calls) is retried; a transport, non-200
// or parse error returns immediately, since none of those are the flake
// this guards against.
func (g HealthGate) checkToolCall(ctx context.Context, s Spec) error {
	reqBody := map[string]any{
		"model":      s.ServedName,
		"max_tokens": 64,
		"messages": []map[string]string{
			{"role": "system", "content": "You cannot know the time yourself. When asked for it, call get_time and reply with nothing else."},
			{"role": "user", "content": "What time is it? Call get_time."},
		},
		"tools": []map[string]any{
			{
				"type": "function",
				"function": map[string]any{
					"name":        "get_time",
					"description": "Get the current time",
					"parameters": map[string]any{
						"type":       "object",
						"properties": map[string]any{},
					},
				},
			},
		},
		"tool_choice": "auto",
	}

	var lastMiss error
	for attempt := 1; attempt <= maxToolCallAttempts; attempt++ {
		if attempt > 1 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}

		_, toolCalls, err := g.chatCompletion(ctx, reqBody)
		if err != nil {
			return fmt.Errorf("tool call check: %w", err)
		}
		if len(toolCalls) > 0 {
			return nil
		}
		lastMiss = fmt.Errorf("tool call check: model answered in prose, no tool_calls in response (attempt %d/%d)", attempt, maxToolCallAttempts)
	}
	return lastMiss
}

// chatCompletion POSTs reqBody to /v1/chat/completions and returns
// choices[0].finish_reason and choices[0].message.tool_calls.
func (g HealthGate) chatCompletion(ctx context.Context, reqBody map[string]any) (finishReason string, toolCalls []json.RawMessage, err error) {
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", nil, fmt.Errorf("encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.BaseURL+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.client().Do(req)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGateBodyBytes))
	if err != nil {
		return "", nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("status %s: %s", resp.Status, bytes.TrimSpace(body))
	}

	var parsed struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				ToolCalls []json.RawMessage `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", nil, fmt.Errorf("parse response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return "", nil, fmt.Errorf("no choices in response")
	}

	return parsed.Choices[0].FinishReason, parsed.Choices[0].Message.ToolCalls, nil
}

func (g HealthGate) client() *http.Client {
	if g.HTTP != nil {
		return g.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (g HealthGate) pollInterval() time.Duration {
	if g.Poll > 0 {
		return g.Poll
	}
	return 5 * time.Second
}
