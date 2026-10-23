# AI-SRE Alert Relay Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a tiny Go service that turns Alertmanager webhooks into HolmesGPT investigations and fans the result out to Discord, Jira, and (when confident) a human-gated GitHub PR.

**Architecture:** Stateless single-replica Deployment. `webhook` ingress → async `pipeline` per firing alert → `holmes.Investigate` → optional `patch.Generate` (via LiteLLM) → `jira.Upsert` (deduped) → optional `github.OpenPR` → `discord.Notify`. Each output is independent; a failure in one never suppresses the others. Dedup state lives in Jira issue labels, not the relay.

**Tech Stack:** Go 1.23 stdlib only (`net/http`, `encoding/json`) — no third-party deps. Distroless container. Nx `@nx-go/nx-go` executors. Deployed GitOps via the platform tenant envelope.

**Source spec:** `docs/superpowers/specs/2026-06-30-ai-sre-alert-relay-design.md`

## Global Constraints

- **Go floor:** `go 1.23` (matches `apps/go.work`). Module path: `apps/backend/ai-sre-relay`.
- **Stdlib only:** no third-party Go modules. `go.mod` has no `require` block. This keeps the image ~10MB and the attack surface minimal.
- **Nx tags (mandatory):** every `project.json` needs `type:app`, `scope:ai-sre-relay`, `framework:go`.
- **Image:** `jdwlabs/ai-sre-relay`, distroless `nonroot`, multi-arch `linux/amd64,linux/arm64`.
- **Resources:** requests `cpu: 10m` / `memory: 16Mi`; limit `memory: 32Mi`. No CPU limit.
- **LLM access:** only via LiteLLM gateway `http://litellm.ai-sre.svc.cluster.local:4000/v1`. Never call a provider directly.
- **Read-only cluster:** the relay never executes returned content and never mutates the cluster. It only writes patch content into a PR file for human review.
- **No silent failures:** every dropped output is logged at `ERROR` with the alert `fingerprint` and cause. (Global project constraint from the parent AI-SRE design.)
- **Secrets:** all via Vault + ESO (`ClusterSecretStore` `vault`). None committed to git.
- **Worktree drive (apps repo):** apps worktrees MUST be created on `F:` (`export WT_BASE=F:/Dev/worktrees`) — pnpm hard links cannot cross NTFS volumes. See apps `CLAUDE.md`.
- **Plan location:** this plan lives in the **infrastructure** repo (its `docs/superpowers` is committed). The apps repo `docs/superpowers` is gitignored — do NOT copy this plan there.
- **PR-review-before-merge:** global mandatory policy applies to every PR this work produces.

---

## File Structure

**apps repo** (`F:\Dev\projects\personal\jdwlabs\apps`), new Nx project under `apps/backend/ai-sre-relay/`:

| File | Responsibility |
|------|----------------|
| `go.mod` | Module `apps/backend/ai-sre-relay`, `go 1.23`, no deps. |
| `project.json` | Nx targets (mirror `servicediscovery`): build/serve/test/lint/tidy/version/build-image. |
| `Dockerfile` | Distroless final stage, consumes Nx `dist/` output. |
| `Dockerfile.local` | Self-contained multi-stage build for local `--load`. |
| `types.go` (`package relay`) | Domain types shared across packages: `Alert`, `Analysis`, `Patch`, `PRLink`, `IssueKey`. |
| `holmes.go` | `HolmesClient.Investigate` — calls Holmes `/api/investigate`. |
| `patch.go` | `PatchGenerator.Generate` — constrained LiteLLM call → validated `*Patch` or `nil`. |
| `discord.go` | `DiscordNotifier.Notify` — POST formatted message to webhook. |
| `jira.go` | `JiraClient.Upsert` — create-or-comment with fingerprint dedup. |
| `github.go` | `GitHubClient.OpenPR` — branch+commit+PR via REST, no clone. |
| `pipeline.go` | `Pipeline.Handle` — orchestrates the six steps per alert. |
| `webhook.go` | HTTP ingress: `POST /webhook`, `GET /healthz`; decodes payload, filters firing, dispatches async. |
| `main.go` | Wire config from env, construct clients, start server, graceful shutdown. |
| `*_test.go` | Table-driven unit tests per package against `httptest` fakes. |

**platform repo** (worktree `~/worktrees/platform/feat/cluster-ai-sre-agent`), new service under `tenants/platform/services/ai-sre-relay/`:

| File | Responsibility |
|------|----------------|
| `postInstall/ai-sre-relay.yaml` | Raw `Deployment` + `Service` manifests (mirror `litellm-redis` rawManifests pattern). |
| `postInstall/externalsecret.yaml` | ESO pulling `{DISCORD_WEBHOOK_URL, JIRA_*, GITHUB_TOKEN, LITELLM_KEY}` from Vault. |
| `tenants/platform/tenant.yaml` (modify) | Add `ai-sre-relay` application entry, `rawManifests: true`, `syncWave: 6`. |
| `.../kube-prometheus-stack/postInstall/alertmanager-config-externalsecret.yaml` (modify) | Repoint `holmes` receiver webhook from `holmes-holmes/api/alertmanager` to the relay `/webhook`. |

---

## Preconditions (operator / verification, not code)

- [ ] **P1: apps worktree exists on F:.** From the apps repo: `gwta feat/ai-sre-relay` (creates `F:\Dev\worktrees\apps\feat\ai-sre-relay`). Confirm `node_modules/.bin` is populated after `pnpm install --frozen-lockfile`.
- [ ] **P2: Confirm Holmes `/api/investigate` shape against the deployed 0.34.0 image.** Port-forward and curl (used to lock the `holmes.go` request/response structs in Task 3):
  ```bash
  kubectl -n ai-sre port-forward svc/holmes-holmes 8000:80 &
  curl -s -XPOST localhost:8000/api/investigate -H 'Content-Type: application/json' \
    -d '{"source":"prometheus","title":"test","description":"probe","subject":{"namespace":"default"},"context":{}}' | jq 'keys'
  ```
  Expected: a JSON object containing an `analysis` string field. If the field name differs, adjust `holmesResponse` in Task 3 before implementing.

---

## Phase A — Go relay (apps repo)

### Task 1: Nx Go project scaffold + walking skeleton

**Files:**
- Create: `apps/backend/ai-sre-relay/go.mod`
- Create: `apps/backend/ai-sre-relay/project.json`
- Create: `apps/backend/ai-sre-relay/main.go`
- Create: `apps/backend/ai-sre-relay/webhook.go`
- Create: `apps/backend/ai-sre-relay/webhook_test.go`
- Modify: `go.work` (repo root) — add the module to the `use` block.

**Interfaces:**
- Produces: `newRouter(p handler) *http.ServeMux` where `handler` is `interface{ Handle(ctx context.Context, a Alert) error }`; `GET /healthz` → `200 "ok"`. Later tasks replace the stub `Alert`/handler.

- [ ] **Step 1: Write the failing test** (`webhook_test.go`)

```go
package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

type stubHandler struct{ calls int }

func (s *stubHandler) Handle(_ context.Context, _ Alert) error { s.calls++; return nil }

func TestHealthz(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	newRouter(&stubHandler{}).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", rr.Code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd F:/Dev/worktrees/apps/feat/ai-sre-relay/apps/backend/ai-sre-relay && go test ./... -run TestHealthz -v`
Expected: FAIL — `undefined: newRouter` / `undefined: Alert`.

- [ ] **Step 3: Write minimal implementation**

`go.mod`:
```
module apps/backend/ai-sre-relay

go 1.23
```

`types.go` (minimal stub — Task 2 fills it out):
```go
package relay

// Alert is one firing Alertmanager alert. Fields added in Task 2.
type Alert struct {
	Fingerprint string `json:"fingerprint"`
}
```

`webhook.go`:
```go
package relay

import (
	"context"
	"net/http"
)

// handler is the pipeline seam; webhook depends only on this narrow interface.
type handler interface {
	Handle(ctx context.Context, a Alert) error
}

func newRouter(h handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}
```

`main.go`:
```go
package main

// Entry point; wiring added in Task 9. Package main lives alongside the
// library package via go:build until Task 9 — for now, a no-op main so
// `nx build` produces a binary.

func main() {}
```

Note: Go forbids two package names in one dir. Keep `main.go` as `package relay` with a `main`-less build during Phase A, OR place `main.go` under `cmd/`. **Decision:** put the executable in the package root as `package relay` is not valid for `func main`. Instead, during Phase A name the entry file `main.go` with `package main` and move all library files to a subpackage is overkill. **Chosen layout:** single `package main` for everything (like `servicediscovery`, which is all `package main`). Rename `package relay` → `package main` in every file above. Tests stay `package main`.

**Apply now:** change `package relay` to `package main` in `types.go`, `webhook.go`, `webhook_test.go`. Delete the stub `main.go` body conflict by making `main.go`:
```go
package main

func main() {}
```

`project.json` (mirror `servicediscovery`, swap names):
```json
{
  "name": "ai-sre-relay",
  "$schema": "../../../node_modules/nx/schemas/project-schema.json",
  "projectType": "application",
  "sourceRoot": "apps/backend/ai-sre-relay",
  "tags": ["type:app", "scope:ai-sre-relay", "framework:go"],
  "targets": {
    "build": {
      "executor": "@nx-go/nx-go:build",
      "options": {
        "main": ".",
        "outputPath": "../../../dist/apps/backend/ai-sre-relay/ai-sre-relay"
      }
    },
    "serve": { "executor": "@nx-go/nx-go:serve", "options": { "main": "." } },
    "test": { "executor": "@nx-go/nx-go:test" },
    "lint": { "executor": "@nx-go/nx-go:lint" },
    "tidy": { "executor": "@nx-go/nx-go:tidy" },
    "build-image": {
      "executor": "nx:run-commands",
      "options": {
        "commands": [
          {
            "command": "docker buildx build --platform linux/amd64,linux/arm64 -f {projectRoot}/Dockerfile -t jdwlabs/ai-sre-relay:latest -t jdwlabs/ai-sre-relay:{version} --push .",
            "forwardAllArgs": false
          }
        ],
        "parallel": false
      }
    },
    "local-build-image": {
      "executor": "nx:run-commands",
      "options": {
        "commands": [
          {
            "command": "docker buildx build -f {projectRoot}/Dockerfile.local -t jdwlabs/ai-sre-relay:latest --load .",
            "forwardAllArgs": false
          }
        ],
        "parallel": false
      }
    },
    "version": {
      "executor": "@jscutlery/semver:version",
      "options": {
        "push": true,
        "preset": "conventionalcommits",
        "postTargets": ["format", "build-image"]
      }
    },
    "format": { "command": "bash scripts/format.sh" },
    "download": { "command": "go mod download" }
  }
}
```

Add to `go.work` `use (...)` block: `./apps/backend/ai-sre-relay`.

- [ ] **Step 4: Run test to verify it passes**

Run: `cd apps/backend/ai-sre-relay && go test ./... -run TestHealthz -v`
Expected: PASS.

Also: `pnpm exec nx reset && pnpm exec nx run ai-sre-relay:build` → produces the dist binary.

- [ ] **Step 5: Commit**

```bash
git add apps/backend/ai-sre-relay go.work
git commit -m "feat(ai-sre-relay): scaffold nx go project with healthz"
```

---

### Task 2: Domain types

**Files:**
- Modify: `apps/backend/ai-sre-relay/types.go`
- Test: `apps/backend/ai-sre-relay/types_test.go`

**Interfaces:**
- Produces: `Alert`, `Analysis`, `Patch`, `PRLink`, `IssueKey` used by every downstream package.

- [ ] **Step 1: Write the failing test** (`types_test.go`)

```go
package main

import (
	"encoding/json"
	"testing"
)

func TestAlertDecodesFingerprintAndLabels(t *testing.T) {
	const payload = `{
	  "fingerprint": "abc123",
	  "status": "firing",
	  "labels": {"alertname": "KubePodCrashLooping", "namespace": "prod", "severity": "warning"},
	  "annotations": {"description": "pod restarting"},
	  "startsAt": "2026-06-30T00:00:00Z"
	}`
	var a Alert
	if err := json.Unmarshal([]byte(payload), &a); err != nil {
		t.Fatal(err)
	}
	if a.Fingerprint != "abc123" || a.Labels["alertname"] != "KubePodCrashLooping" {
		t.Fatalf("bad decode: %+v", a)
	}
	if a.Name() != "KubePodCrashLooping" || a.Namespace() != "prod" || a.Severity() != "warning" {
		t.Fatalf("accessor mismatch: %+v", a)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestAlertDecodes -v`
Expected: FAIL — `Alert.Labels` / `Alert.Name` undefined.

- [ ] **Step 3: Write minimal implementation** (`types.go`)

```go
package main

// Alert is one firing alert from an Alertmanager v4 webhook.
type Alert struct {
	Fingerprint string            `json:"fingerprint"`
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    string            `json:"startsAt"`
}

func (a Alert) Name() string      { return a.Labels["alertname"] }
func (a Alert) Namespace() string { return a.Labels["namespace"] }
func (a Alert) Severity() string  { return a.Labels["severity"] }

// Analysis is Holmes' prose root-cause output.
type Analysis struct {
	RootCause string // full markdown analysis
}

// Patch is a machine-readable, single-file remediation proposal.
type Patch struct {
	Repo       string  `json:"repo"`        // "owner/name"
	FilePath   string  `json:"file_path"`   // path within repo
	NewContent string  `json:"new_content"` // full new file contents
	Branch     string  `json:"branch"`
	Rationale  string  `json:"rationale"`
	Confidence float64 `json:"confidence"` // 0..1
}

// IssueKey is a Jira issue identifier, e.g. "JDWLABS-123".
type IssueKey string

// PRLink is a rendered GitHub pull request URL.
type PRLink string
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run TestAlertDecodes -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add apps/backend/ai-sre-relay/types.go apps/backend/ai-sre-relay/types_test.go
git commit -m "feat(ai-sre-relay): add domain types"
```

---

### Task 3: Holmes investigation client

**Files:**
- Create: `apps/backend/ai-sre-relay/holmes.go`
- Test: `apps/backend/ai-sre-relay/holmes_test.go`

**Interfaces:**
- Consumes: `Alert`, `Analysis`.
- Produces: `NewHolmesClient(baseURL string, hc *http.Client) *HolmesClient`; `(*HolmesClient).Investigate(ctx, Alert) (Analysis, error)`.

> Lock the request/response structs against the Task P2 curl output before implementing. The code below assumes the response carries an `analysis` string.

- [ ] **Step 1: Write the failing test** (`holmes_test.go`)

```go
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHolmesInvestigate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/investigate" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"analysis": "pod OOMKilled; memory limit too low"}`))
	}))
	defer srv.Close()

	a := Alert{Fingerprint: "fp1", Labels: map[string]string{"alertname": "KubePodCrashLooping", "namespace": "prod"}, Annotations: map[string]string{"description": "restarting"}}
	got, err := NewHolmesClient(srv.URL, srv.Client()).Investigate(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if got.RootCause != "pod OOMKilled; memory limit too low" {
		t.Fatalf("RootCause = %q", got.RootCause)
	}
}

func TestHolmesInvestigateServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := NewHolmesClient(srv.URL, srv.Client()).Investigate(context.Background(), Alert{}); err == nil {
		t.Fatal("want error on 500, got nil")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestHolmes -v`
Expected: FAIL — `undefined: NewHolmesClient`.

- [ ] **Step 3: Write minimal implementation** (`holmes.go`)

```go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

type HolmesClient struct {
	baseURL string
	hc      *http.Client
}

func NewHolmesClient(baseURL string, hc *http.Client) *HolmesClient {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &HolmesClient{baseURL: baseURL, hc: hc}
}

// holmesRequest matches Holmes 0.34.0 /api/investigate. subject carries the
// k8s object hints; context is reserved for extra grounding data.
type holmesRequest struct {
	Source      string            `json:"source"`
	Title       string            `json:"title"`
	Description string            `json:"description"`
	Subject     map[string]string `json:"subject"`
	Context     map[string]string `json:"context"`
}

type holmesResponse struct {
	Analysis string `json:"analysis"`
}

func (c *HolmesClient) Investigate(ctx context.Context, a Alert) (Analysis, error) {
	body, _ := json.Marshal(holmesRequest{
		Source:      "prometheus",
		Title:       a.Name(),
		Description: a.Annotations["description"],
		Subject:     map[string]string{"namespace": a.Namespace(), "name": a.Labels["pod"], "kind": a.Labels["kind"]},
		Context:     map[string]string{"severity": a.Severity(), "fingerprint": a.Fingerprint},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/investigate", bytes.NewReader(body))
	if err != nil {
		return Analysis{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return Analysis{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Analysis{}, fmt.Errorf("holmes: status %d", resp.StatusCode)
	}
	var hr holmesResponse
	if err := json.NewDecoder(resp.Body).Decode(&hr); err != nil {
		return Analysis{}, fmt.Errorf("holmes: decode: %w", err)
	}
	return Analysis{RootCause: hr.Analysis}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run TestHolmes -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add apps/backend/ai-sre-relay/holmes.go apps/backend/ai-sre-relay/holmes_test.go
git commit -m "feat(ai-sre-relay): add holmes investigation client"
```

---

### Task 4: Patch generator (LiteLLM, schema + confidence gate)

**Files:**
- Create: `apps/backend/ai-sre-relay/patch.go`
- Test: `apps/backend/ai-sre-relay/patch_test.go`

**Interfaces:**
- Consumes: `Analysis`, `Patch`.
- Produces: `NewPatchGenerator(baseURL, apiKey, model string, minConfidence float64, hc *http.Client) *PatchGenerator`; `(*PatchGenerator).Generate(ctx, Analysis) (*Patch, error)`. Returns `(nil, nil)` when no confident patch is possible.

- [ ] **Step 1: Write the failing test** (`patch_test.go`)

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeLiteLLM returns an OpenAI-shaped chat completion whose message content
// is the given string (the model's raw reply).
func fakeLiteLLM(t *testing.T, content string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing/wrong auth: %q", r.Header.Get("Authorization"))
		}
		resp := map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": content}}}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestPatchGenerateValid(t *testing.T) {
	patch := `{"repo":"jdwlabs/platform","file_path":"values.yaml","new_content":"limits:\n  memory: 512Mi\n","branch":"fix/oom","rationale":"raise limit","confidence":0.9}`
	srv := fakeLiteLLM(t, patch)
	defer srv.Close()
	g := NewPatchGenerator(srv.URL, "test-key", "claude-sonnet", 0.7, srv.Client())
	got, err := g.Generate(context.Background(), Analysis{RootCause: "OOM"})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Repo != "jdwlabs/platform" || got.Confidence != 0.9 {
		t.Fatalf("bad patch: %+v", got)
	}
}

func TestPatchGenerateLowConfidenceReturnsNil(t *testing.T) {
	srv := fakeLiteLLM(t, `{"repo":"x/y","file_path":"a","new_content":"b","branch":"c","rationale":"d","confidence":0.3}`)
	defer srv.Close()
	got, err := NewPatchGenerator(srv.URL, "test-key", "m", 0.7, srv.Client()).Generate(context.Background(), Analysis{})
	if err != nil || got != nil {
		t.Fatalf("want (nil,nil), got (%+v,%v)", got, err)
	}
}

func TestPatchGenerateMalformedReturnsNil(t *testing.T) {
	srv := fakeLiteLLM(t, "sorry, I cannot produce a patch")
	defer srv.Close()
	got, err := NewPatchGenerator(srv.URL, "test-key", "m", 0.7, srv.Client()).Generate(context.Background(), Analysis{})
	if err != nil || got != nil {
		t.Fatalf("want (nil,nil), got (%+v,%v)", got, err)
	}
}

var _ = fmt.Sprint // keep fmt imported if unused in a trimmed build
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestPatch -v`
Expected: FAIL — `undefined: NewPatchGenerator`.

- [ ] **Step 3: Write minimal implementation** (`patch.go`)

```go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type PatchGenerator struct {
	baseURL       string
	apiKey        string
	model         string
	minConfidence float64
	hc            *http.Client
}

func NewPatchGenerator(baseURL, apiKey, model string, minConfidence float64, hc *http.Client) *PatchGenerator {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &PatchGenerator{baseURL: baseURL, apiKey: apiKey, model: model, minConfidence: minConfidence, hc: hc}
}

const patchSystemPrompt = `You are an SRE assistant. Given a root-cause analysis, propose AT MOST ONE single-file GitOps change that fixes it. Respond with ONLY a JSON object, no prose, matching:
{"repo":"owner/name","file_path":"...","new_content":"<full file>","branch":"fix/...","rationale":"...","confidence":0.0-1.0}
If no safe single-file change exists, respond with exactly: {"confidence":0}`

type openAIRequest struct {
	Model    string          `json:"model"`
	Messages []openAIMessage `json:"messages"`
}
type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type openAIResponse struct {
	Choices []struct {
		Message openAIMessage `json:"message"`
	} `json:"choices"`
}

// Generate returns (nil, nil) for the common "no confident patch" case; an
// error only for transport/decode failures of the LiteLLM call itself.
func (g *PatchGenerator) Generate(ctx context.Context, an Analysis) (*Patch, error) {
	reqBody, _ := json.Marshal(openAIRequest{
		Model: g.model,
		Messages: []openAIMessage{
			{Role: "system", Content: patchSystemPrompt},
			{Role: "user", Content: an.RootCause},
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+"/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+g.apiKey)
	resp, err := g.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("litellm: status %d", resp.StatusCode)
	}
	var or openAIResponse
	if err := json.NewDecoder(resp.Body).Decode(&or); err != nil {
		return nil, fmt.Errorf("litellm: decode: %w", err)
	}
	if len(or.Choices) == 0 {
		return nil, nil
	}
	content := strings.TrimSpace(or.Choices[0].Message.Content)

	// Malformed / refusal → no patch (not an error). Only accept strict JSON.
	var p Patch
	if err := json.Unmarshal([]byte(content), &p); err != nil {
		return nil, nil
	}
	if p.Confidence < g.minConfidence || p.Repo == "" || p.FilePath == "" || p.NewContent == "" {
		return nil, nil
	}
	return &p, nil
}
```

Remove the `var _ = fmt.Sprint` line from the test if `fmt` ends up used; otherwise drop the `fmt` import from the test. Run `gofmt`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run TestPatch -v`
Expected: PASS (all three cases).

- [ ] **Step 5: Commit**

```bash
git add apps/backend/ai-sre-relay/patch.go apps/backend/ai-sre-relay/patch_test.go
git commit -m "feat(ai-sre-relay): add litellm patch generator with confidence gate"
```

---

### Task 5: Discord notifier

**Files:**
- Create: `apps/backend/ai-sre-relay/discord.go`
- Test: `apps/backend/ai-sre-relay/discord_test.go`

**Interfaces:**
- Consumes: `Alert`, `Analysis`, `PRLink`, `IssueKey`.
- Produces: `NewDiscordNotifier(webhookURL string, hc *http.Client) *DiscordNotifier`; `(*DiscordNotifier).Notify(ctx, Alert, Analysis, IssueKey, *PRLink) error`.

- [ ] **Step 1: Write the failing test** (`discord_test.go`)

```go
package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDiscordNotifyIncludesLinks(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	pr := PRLink("https://github.com/jdwlabs/platform/pull/9")
	err := NewDiscordNotifier(srv.URL, srv.Client()).Notify(context.Background(),
		Alert{Labels: map[string]string{"alertname": "KubePodCrashLooping"}},
		Analysis{RootCause: "OOM"}, IssueKey("JDWLABS-123"), &pr)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := body["content"].(string)
	if !strings.Contains(content, "KubePodCrashLooping") || !strings.Contains(content, "JDWLABS-123") || !strings.Contains(content, "pull/9") {
		t.Fatalf("content missing fields: %q", content)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestDiscord -v`
Expected: FAIL — `undefined: NewDiscordNotifier`.

- [ ] **Step 3: Write minimal implementation** (`discord.go`)

```go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

type DiscordNotifier struct {
	webhookURL string
	hc         *http.Client
}

func NewDiscordNotifier(webhookURL string, hc *http.Client) *DiscordNotifier {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &DiscordNotifier{webhookURL: webhookURL, hc: hc}
}

func (d *DiscordNotifier) Notify(ctx context.Context, a Alert, an Analysis, issue IssueKey, pr *PRLink) error {
	msg := fmt.Sprintf("**AI-SRE — %s**\n%s", a.Name(), truncate(an.RootCause, 1500))
	if issue != "" {
		msg += fmt.Sprintf("\nJira: %s", issue)
	}
	if pr != nil {
		msg += fmt.Sprintf("\nPR: %s", *pr)
	}
	body, _ := json.Marshal(map[string]string{"content": msg})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.webhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("discord: status %d", resp.StatusCode)
	}
	return nil
}

// truncate keeps Discord's 2000-char content ceiling safe.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run TestDiscord -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add apps/backend/ai-sre-relay/discord.go apps/backend/ai-sre-relay/discord_test.go
git commit -m "feat(ai-sre-relay): add discord notifier"
```

---

### Task 6: Jira upsert with fingerprint dedup

**Files:**
- Create: `apps/backend/ai-sre-relay/jira.go`
- Test: `apps/backend/ai-sre-relay/jira_test.go`

**Interfaces:**
- Consumes: `Alert`, `Analysis`, `IssueKey`.
- Produces: `NewJiraClient(baseURL, email, token, projectKey string, hc *http.Client) *JiraClient`; `(*JiraClient).Upsert(ctx, Alert, Analysis) (IssueKey, error)`.
- Dedup label format: `amfp-<fingerprint>`.

- [ ] **Step 1: Write the failing test** (`jira_test.go`)

```go
package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJiraUpsertCreatesWhenNoDuplicate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/rest/api/3/search"):
			_ = json.NewEncoder(w).Encode(map[string]any{"issues": []any{}})
		case r.Method == http.MethodPost && r.URL.Path == "/rest/api/3/issue":
			raw, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(raw), "amfp-fp1") {
				t.Errorf("create missing dedup label: %s", raw)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"key": "JDWLABS-500"})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	key, err := NewJiraClient(srv.URL, "e@x", "tok", "JDWLABS", srv.Client()).
		Upsert(context.Background(), Alert{Fingerprint: "fp1", Labels: map[string]string{"alertname": "X"}}, Analysis{RootCause: "y"})
	if err != nil || key != "JDWLABS-500" {
		t.Fatalf("key=%q err=%v", key, err)
	}
}

func TestJiraUpsertCommentsWhenDuplicate(t *testing.T) {
	commented := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/rest/api/3/search"):
			_ = json.NewEncoder(w).Encode(map[string]any{"issues": []map[string]string{{"key": "JDWLABS-77"}}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comment"):
			commented = true
			w.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	key, err := NewJiraClient(srv.URL, "e@x", "tok", "JDWLABS", srv.Client()).
		Upsert(context.Background(), Alert{Fingerprint: "fp1"}, Analysis{RootCause: "y"})
	if err != nil || key != "JDWLABS-77" || !commented {
		t.Fatalf("key=%q err=%v commented=%v", key, err, commented)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestJira -v`
Expected: FAIL — `undefined: NewJiraClient`.

- [ ] **Step 3: Write minimal implementation** (`jira.go`)

```go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

type JiraClient struct {
	baseURL    string
	email      string
	token      string
	projectKey string
	hc         *http.Client
}

func NewJiraClient(baseURL, email, token, projectKey string, hc *http.Client) *JiraClient {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &JiraClient{baseURL: baseURL, email: email, token: token, projectKey: projectKey, hc: hc}
}

func (j *JiraClient) label(a Alert) string { return "amfp-" + a.Fingerprint }

func (j *JiraClient) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var r *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	} else {
		r = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, j.baseURL+path, r)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(j.email, j.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return j.hc.Do(req)
}

// Upsert creates a new issue for the alert, or comments on the existing open
// issue that carries the fingerprint dedup label.
func (j *JiraClient) Upsert(ctx context.Context, a Alert, an Analysis) (IssueKey, error) {
	jql := fmt.Sprintf(`project = %s AND labels = "%s" AND statusCategory != Done`, j.projectKey, j.label(a))
	resp, err := j.do(ctx, http.MethodGet, "/rest/api/3/search?jql="+url.QueryEscape(jql), nil)
	if err != nil {
		return "", err
	}
	var search struct {
		Issues []struct {
			Key string `json:"key"`
		} `json:"issues"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&search)
	resp.Body.Close()

	if len(search.Issues) > 0 {
		key := search.Issues[0].Key
		cResp, err := j.do(ctx, http.MethodPost, fmt.Sprintf("/rest/api/3/issue/%s/comment", key), adf(an.RootCause))
		if err != nil {
			return "", err
		}
		defer cResp.Body.Close()
		if cResp.StatusCode >= 300 {
			return "", fmt.Errorf("jira comment: status %d", cResp.StatusCode)
		}
		return IssueKey(key), nil
	}

	create := map[string]any{
		"fields": map[string]any{
			"project":     map[string]string{"key": j.projectKey},
			"issuetype":   map[string]string{"name": "Bug"},
			"summary":     "AI-SRE: " + a.Name(),
			"description": adfDoc(an.RootCause),
			"labels":      []string{j.label(a), "ai-sre"},
		},
	}
	cResp, err := j.do(ctx, http.MethodPost, "/rest/api/3/issue", create)
	if err != nil {
		return "", err
	}
	defer cResp.Body.Close()
	if cResp.StatusCode >= 300 {
		return "", fmt.Errorf("jira create: status %d", cResp.StatusCode)
	}
	var created struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(cResp.Body).Decode(&created); err != nil {
		return "", err
	}
	return IssueKey(created.Key), nil
}

// adf wraps text as an Atlassian Document Format comment body.
func adf(text string) map[string]any {
	return map[string]any{"body": adfDoc(text)}
}

func adfDoc(text string) map[string]any {
	return map[string]any{
		"type":    "doc",
		"version": 1,
		"content": []any{map[string]any{
			"type":    "paragraph",
			"content": []any{map[string]any{"type": "text", "text": text}},
		}},
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run TestJira -v`
Expected: PASS (create + comment cases).

- [ ] **Step 5: Commit**

```bash
git add apps/backend/ai-sre-relay/jira.go apps/backend/ai-sre-relay/jira_test.go
git commit -m "feat(ai-sre-relay): add jira upsert with fingerprint dedup"
```

---

### Task 7: GitHub PR opener (REST, no clone)

**Files:**
- Create: `apps/backend/ai-sre-relay/github.go`
- Test: `apps/backend/ai-sre-relay/github_test.go`

**Interfaces:**
- Consumes: `Patch`, `IssueKey`, `PRLink`.
- Produces: `NewGitHubClient(apiBase, token string, hc *http.Client) *GitHubClient`; `(*GitHubClient).OpenPR(ctx, Patch, IssueKey) (PRLink, error)`.
- Flow: get default branch ref SHA → create branch → get file SHA (if exists) → PUT contents → create PR. Only called when patch is non-nil.

- [ ] **Step 1: Write the failing test** (`github_test.go`)

```go
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGitHubOpenPR(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/git/ref/heads/main"):
			_ = json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": "basesha"}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git/refs"):
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/contents/"):
			w.WriteHeader(http.StatusNotFound) // new file
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/contents/"):
			raw, _ := io.ReadAll(r.Body)
			var m map[string]any
			_ = json.Unmarshal(raw, &m)
			if _, err := base64.StdEncoding.DecodeString(m["content"].(string)); err != nil {
				t.Errorf("content not base64: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			_ = json.NewEncoder(w).Encode(map[string]string{"html_url": "https://github.com/jdwlabs/platform/pull/9"})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	p := Patch{Repo: "jdwlabs/platform", FilePath: "values.yaml", NewContent: "limits:\n  memory: 512Mi\n", Branch: "fix/oom", Rationale: "raise", Confidence: 0.9}
	link, err := NewGitHubClient(srv.URL, "ghtok", srv.Client()).OpenPR(context.Background(), p, "JDWLABS-500")
	if err != nil {
		t.Fatal(err)
	}
	if link != "https://github.com/jdwlabs/platform/pull/9" {
		t.Fatalf("link=%q", link)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestGitHub -v`
Expected: FAIL — `undefined: NewGitHubClient`.

- [ ] **Step 3: Write minimal implementation** (`github.go`)

```go
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
)

type GitHubClient struct {
	apiBase string // https://api.github.com (override in tests)
	token   string
	hc      *http.Client
}

func NewGitHubClient(apiBase, token string, hc *http.Client) *GitHubClient {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &GitHubClient{apiBase: apiBase, token: token, hc: hc}
}

func (g *GitHubClient) req(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var buf *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		buf = bytes.NewReader(b)
	} else {
		buf = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.apiBase+path, buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	return g.hc.Do(req)
}

// OpenPR creates a branch off default (main), writes the single patched file,
// and opens a PR. Base = "main"; the branch name comes from the patch.
func (g *GitHubClient) OpenPR(ctx context.Context, p Patch, issue IssueKey) (PRLink, error) {
	repoPath := "/repos/" + p.Repo

	// 1. base ref SHA
	refResp, err := g.req(ctx, http.MethodGet, repoPath+"/git/ref/heads/main", nil)
	if err != nil {
		return "", err
	}
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	_ = json.NewDecoder(refResp.Body).Decode(&ref)
	refResp.Body.Close()
	if ref.Object.SHA == "" {
		return "", fmt.Errorf("github: empty base sha")
	}

	// 2. create branch (ignore 422 already-exists)
	brResp, err := g.req(ctx, http.MethodPost, repoPath+"/git/refs", map[string]string{
		"ref": "refs/heads/" + p.Branch, "sha": ref.Object.SHA,
	})
	if err != nil {
		return "", err
	}
	brResp.Body.Close()

	// 3. existing file SHA (needed to update; absent => new file)
	var existingSHA string
	fResp, err := g.req(ctx, http.MethodGet, repoPath+"/contents/"+p.FilePath+"?ref="+p.Branch, nil)
	if err != nil {
		return "", err
	}
	if fResp.StatusCode == http.StatusOK {
		var f struct {
			SHA string `json:"sha"`
		}
		_ = json.NewDecoder(fResp.Body).Decode(&f)
		existingSHA = f.SHA
	}
	fResp.Body.Close()

	// 4. put contents
	put := map[string]any{
		"message": fmt.Sprintf("fix(ai-sre): %s (%s)", p.Rationale, issue),
		"content": base64.StdEncoding.EncodeToString([]byte(p.NewContent)),
		"branch":  p.Branch,
	}
	if existingSHA != "" {
		put["sha"] = existingSHA
	}
	pResp, err := g.req(ctx, http.MethodPut, repoPath+"/contents/"+p.FilePath, put)
	if err != nil {
		return "", err
	}
	pResp.Body.Close()
	if pResp.StatusCode >= 300 {
		return "", fmt.Errorf("github put contents: status %d", pResp.StatusCode)
	}

	// 5. open PR
	prBody := fmt.Sprintf("Automated AI-SRE remediation for %s.\n\n%s\n\n**Human review required — do not auto-merge.**", issue, p.Rationale)
	prResp, err := g.req(ctx, http.MethodPost, repoPath+"/pulls", map[string]string{
		"title": fmt.Sprintf("fix(ai-sre): %s [%s]", p.Rationale, issue),
		"head":  p.Branch,
		"base":  "main",
		"body":  prBody,
	})
	if err != nil {
		return "", err
	}
	defer prResp.Body.Close()
	if prResp.StatusCode >= 300 {
		return "", fmt.Errorf("github create pr: status %d", prResp.StatusCode)
	}
	var pr struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(prResp.Body).Decode(&pr); err != nil {
		return "", err
	}
	return PRLink(pr.HTMLURL), nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run TestGitHub -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add apps/backend/ai-sre-relay/github.go apps/backend/ai-sre-relay/github_test.go
git commit -m "feat(ai-sre-relay): add github pr opener"
```

---

### Task 8: Pipeline orchestrator

**Files:**
- Create: `apps/backend/ai-sre-relay/pipeline.go`
- Test: `apps/backend/ai-sre-relay/pipeline_test.go`

**Interfaces:**
- Consumes: all clients via narrow interfaces (so tests use fakes, not live servers).
- Produces: `Pipeline` with constructor `NewPipeline(h holmesInvestigator, pg patcher, j jiraUpserter, gh prOpener, d discordNotifier, log *slog.Logger) *Pipeline`; `(*Pipeline).Handle(ctx, Alert) error`.
- Ordering: investigate → patch → jira → (PR if patch) → discord (last, with all links). Each output failure is logged, not fatal (except Holmes, which is terminal but still notifies Discord).

- [ ] **Step 1: Write the failing test** (`pipeline_test.go`)

```go
package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

type fakeHolmes struct {
	an  Analysis
	err error
}

func (f fakeHolmes) Investigate(context.Context, Alert) (Analysis, error) { return f.an, f.err }

type fakePatcher struct{ p *Patch }

func (f fakePatcher) Generate(context.Context, Analysis) (*Patch, error) { return f.p, nil }

type fakeJira struct {
	key    IssueKey
	called bool
}

func (f *fakeJira) Upsert(context.Context, Alert, Analysis) (IssueKey, error) {
	f.called = true
	return f.key, nil
}

type fakeGH struct{ called bool }

func (f *fakeGH) OpenPR(context.Context, Patch, IssueKey) (PRLink, error) {
	f.called = true
	return "http://pr/1", nil
}

type fakeDiscord struct {
	called bool
	pr     *PRLink
}

func (f *fakeDiscord) Notify(_ context.Context, _ Alert, _ Analysis, _ IssueKey, pr *PRLink) error {
	f.called = true
	f.pr = pr
	return nil
}

func silentLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestPipelineHappyPathOpensPR(t *testing.T) {
	j, gh, d := &fakeJira{key: "JDWLABS-1"}, &fakeGH{}, &fakeDiscord{}
	p := NewPipeline(fakeHolmes{an: Analysis{RootCause: "x"}}, fakePatcher{p: &Patch{Repo: "a/b"}}, j, gh, d, silentLogger())
	if err := p.Handle(context.Background(), Alert{Fingerprint: "f"}); err != nil {
		t.Fatal(err)
	}
	if !j.called || !gh.called || !d.called || d.pr == nil {
		t.Fatalf("steps: jira=%v gh=%v discord=%v pr=%v", j.called, gh.called, d.called, d.pr)
	}
}

func TestPipelineNoPatchSkipsPR(t *testing.T) {
	gh, d := &fakeGH{}, &fakeDiscord{}
	p := NewPipeline(fakeHolmes{an: Analysis{RootCause: "x"}}, fakePatcher{p: nil}, &fakeJira{key: "JDWLABS-2"}, gh, d, silentLogger())
	_ = p.Handle(context.Background(), Alert{})
	if gh.called {
		t.Fatal("PR opened despite nil patch")
	}
	if !d.called || d.pr != nil {
		t.Fatal("discord should fire with nil PR link")
	}
}

func TestPipelineHolmesFailureStillNotifies(t *testing.T) {
	d := &fakeDiscord{}
	p := NewPipeline(fakeHolmes{err: errors.New("down")}, fakePatcher{}, &fakeJira{}, &fakeGH{}, d, silentLogger())
	_ = p.Handle(context.Background(), Alert{})
	if !d.called {
		t.Fatal("expected failure notice to Discord")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestPipeline -v`
Expected: FAIL — `undefined: NewPipeline`.

- [ ] **Step 3: Write minimal implementation** (`pipeline.go`)

```go
package main

import (
	"context"
	"log/slog"
)

type holmesInvestigator interface {
	Investigate(ctx context.Context, a Alert) (Analysis, error)
}
type patcher interface {
	Generate(ctx context.Context, an Analysis) (*Patch, error)
}
type jiraUpserter interface {
	Upsert(ctx context.Context, a Alert, an Analysis) (IssueKey, error)
}
type prOpener interface {
	OpenPR(ctx context.Context, p Patch, issue IssueKey) (PRLink, error)
}
type discordNotifier interface {
	Notify(ctx context.Context, a Alert, an Analysis, issue IssueKey, pr *PRLink) error
}

type Pipeline struct {
	holmes  holmesInvestigator
	patch   patcher
	jira    jiraUpserter
	github  prOpener
	discord discordNotifier
	log     *slog.Logger
}

func NewPipeline(h holmesInvestigator, pg patcher, j jiraUpserter, gh prOpener, d discordNotifier, log *slog.Logger) *Pipeline {
	return &Pipeline{holmes: h, patch: pg, jira: j, github: gh, discord: d, log: log}
}

// Handle runs one alert through the pipeline. Each output is independent: a
// failure is logged with the fingerprint and never suppresses later outputs.
func (p *Pipeline) Handle(ctx context.Context, a Alert) error {
	log := p.log.With("fingerprint", a.Fingerprint, "alert", a.Name())

	an, err := p.holmes.Investigate(ctx, a)
	if err != nil {
		// Holmes is terminal: nothing to fan out. Still tell humans so they
		// are not left blind.
		log.Error("holmes investigation failed", "err", err)
		if derr := p.discord.Notify(ctx, a, Analysis{RootCause: "⚠️ investigation failed: " + err.Error()}, "", nil); derr != nil {
			log.Error("discord failure notice failed", "err", derr)
		}
		return err
	}

	// Structured patch is best-effort; nil is the common case.
	var patch *Patch
	if pt, perr := p.patch.Generate(ctx, an); perr != nil {
		log.Error("patch generation failed", "err", perr)
	} else {
		patch = pt
	}

	var issue IssueKey
	if k, jerr := p.jira.Upsert(ctx, a, an); jerr != nil {
		log.Error("jira upsert failed", "err", jerr)
	} else {
		issue = k
	}

	var prLink *PRLink
	if patch != nil {
		if link, gerr := p.github.OpenPR(ctx, *patch, issue); gerr != nil {
			log.Error("github pr failed", "err", gerr)
		} else {
			prLink = &link
		}
	}

	if derr := p.discord.Notify(ctx, a, an, issue, prLink); derr != nil {
		log.Error("discord notify failed", "err", derr)
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run TestPipeline -v`
Expected: PASS (3 cases).

- [ ] **Step 5: Commit**

```bash
git add apps/backend/ai-sre-relay/pipeline.go apps/backend/ai-sre-relay/pipeline_test.go
git commit -m "feat(ai-sre-relay): add pipeline orchestrator"
```

---

### Task 9: Webhook ingress (async) + main wiring

**Files:**
- Modify: `apps/backend/ai-sre-relay/webhook.go` (add `/webhook`)
- Modify: `apps/backend/ai-sre-relay/webhook_test.go`
- Modify: `apps/backend/ai-sre-relay/main.go` (full wiring)

**Interfaces:**
- Consumes: `handler` (satisfied by `*Pipeline`).
- Produces: `POST /webhook` decodes Alertmanager v4 payload, filters `status == "firing"`, dispatches each alert to `handler.Handle` in a goroutine, returns `202` immediately.

- [ ] **Step 1: Write the failing test** (append to `webhook_test.go`)

```go
func TestWebhookDispatchesFiringAlerts(t *testing.T) {
	done := make(chan Alert, 2)
	h := handlerFunc(func(_ context.Context, a Alert) error { done <- a; return nil })

	const payload = `{"alerts":[
	  {"status":"firing","fingerprint":"f1","labels":{"alertname":"A"}},
	  {"status":"resolved","fingerprint":"f2","labels":{"alertname":"B"}}
	]}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	newRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("code = %d, want 202", rr.Code)
	}
	select {
	case got := <-done:
		if got.Fingerprint != "f1" {
			t.Fatalf("dispatched wrong alert: %s", got.Fingerprint)
		}
	case <-time.After(time.Second):
		t.Fatal("firing alert not dispatched")
	}
	select {
	case got := <-done:
		t.Fatalf("resolved alert should not dispatch: %s", got.Fingerprint)
	case <-time.After(100 * time.Millisecond):
	}
}
```

Add imports `strings`, `time` and this adapter to the test file:

```go
type handlerFunc func(context.Context, Alert) error

func (f handlerFunc) Handle(ctx context.Context, a Alert) error { return f(ctx, a) }
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestWebhookDispatches -v`
Expected: FAIL — `/webhook` returns 404 (route missing).

- [ ] **Step 3: Write minimal implementation** (`webhook.go` — add route)

```go
// in newRouter, after the healthz registration:
	mux.HandleFunc("POST /webhook", func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Alerts []Alert `json:"alerts"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		for _, a := range payload.Alerts {
			if a.Status != "firing" {
				continue
			}
			a := a
			// Async: return 202 fast so Alertmanager is not held open for the
			// full investigation (which can take tens of seconds).
			go func() {
				_ = h.Handle(context.Background(), a)
			}()
		}
		w.WriteHeader(http.StatusAccepted)
	})
```

Add `"encoding/json"` to `webhook.go` imports.

`main.go` (full wiring):
```go
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		slog.Error("missing required env var", "key", key)
		os.Exit(1)
	}
	return v
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	hc := &http.Client{Timeout: 90 * time.Second}

	holmes := NewHolmesClient(env("HOLMES_URL", "http://holmes-holmes.ai-sre.svc.cluster.local"), hc)
	patchGen := NewPatchGenerator(
		env("LITELLM_URL", "http://litellm.ai-sre.svc.cluster.local:4000/v1"),
		mustEnv("LITELLM_KEY"),
		env("LITELLM_MODEL", "claude-sonnet"),
		0.75, hc,
	)
	discord := NewDiscordNotifier(mustEnv("DISCORD_WEBHOOK_URL"), hc)
	jira := NewJiraClient(mustEnv("JIRA_URL"), mustEnv("JIRA_USERNAME"), mustEnv("JIRA_API_TOKEN"), env("JIRA_PROJECT", "JDWLABS"), hc)
	github := NewGitHubClient(env("GITHUB_API", "https://api.github.com"), mustEnv("GITHUB_TOKEN"), hc)

	pipeline := NewPipeline(holmes, patchGen, jira, github, discord, log)

	srv := &http.Server{Addr: ":" + env("PORT", "8080"), Handler: newRouter(pipeline)}

	go func() {
		log.Info("relay listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			log.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
```

- [ ] **Step 4: Run full test suite + build**

Run: `go test ./... -v && go vet ./...`
Expected: PASS, no vet warnings.
Run: `pnpm exec nx run ai-sre-relay:build`
Expected: builds the dist binary.

- [ ] **Step 5: Commit**

```bash
git add apps/backend/ai-sre-relay
git commit -m "feat(ai-sre-relay): add webhook ingress and wire main"
```

---

### Task 10: Container images

**Files:**
- Create: `apps/backend/ai-sre-relay/Dockerfile`
- Create: `apps/backend/ai-sre-relay/Dockerfile.local`
- Create: `apps/backend/ai-sre-relay/.dockerignore` (optional; mirror repo root if present)

**Interfaces:** none (packaging only).

- [ ] **Step 1: Write `Dockerfile`** (mirrors `servicediscovery`: consumes Nx `dist/` output)

```dockerfile
# Distroless final image; consumes the Nx build output at ./dist/...
# The CI build-image target runs after `nx build`, so dist/ is populated.
FROM gcr.io/distroless/base-debian12:nonroot AS runner

WORKDIR /app
COPY ./dist/apps/backend/ai-sre-relay/ai-sre-relay .

EXPOSE 8080
ENTRYPOINT ["/app/ai-sre-relay"]
```

- [ ] **Step 2: Write `Dockerfile.local`** (self-contained, for `local-build-image`)

```dockerfile
# Self-contained build for local iteration (no prior `nx build` needed).
FROM golang:1.23-bookworm AS build
WORKDIR /src
COPY apps/backend/ai-sre-relay/go.mod ./
RUN go mod download || true
COPY apps/backend/ai-sre-relay/ ./
RUN CGO_ENABLED=0 go build -o /out/ai-sre-relay .

FROM gcr.io/distroless/base-debian12:nonroot AS runner
WORKDIR /app
COPY --from=build /out/ai-sre-relay .
EXPOSE 8080
ENTRYPOINT ["/app/ai-sre-relay"]
```

- [ ] **Step 3: Build the local image to verify**

Run: `pnpm exec nx run ai-sre-relay:local-build-image`
Expected: image `jdwlabs/ai-sre-relay:latest` built and loaded.

- [ ] **Step 4: Smoke-test the container**

Run:
```bash
docker run --rm -d -p 8080:8080 \
  -e LITELLM_KEY=x -e DISCORD_WEBHOOK_URL=http://x -e JIRA_URL=http://x \
  -e JIRA_USERNAME=x -e JIRA_API_TOKEN=x -e GITHUB_TOKEN=x \
  --name relay-smoke jdwlabs/ai-sre-relay:latest
curl -sf localhost:8080/healthz && echo OK
docker rm -f relay-smoke
```
Expected: `ok` / `OK`.

- [ ] **Step 5: Commit**

```bash
git add apps/backend/ai-sre-relay/Dockerfile apps/backend/ai-sre-relay/Dockerfile.local
git commit -m "feat(ai-sre-relay): add container images"
```

> **Apps PR gate:** open a PR from `feat/ai-sre-relay` → apps `main`. CI runs lint/test/build; on merge, the `version` target bumps semver and `build-image` publishes `jdwlabs/ai-sre-relay:<version>`. Record the published tag — Task 12 pins it.

---

## Phase B — Deployment (platform repo)

> Work in `~/worktrees/platform/feat/cluster-ai-sre-agent`. These tasks are YAML/GitOps; the "tests" are `kubeconform`/ArgoCD dry-run and the live Phase-2 gate.

### Task 11: Relay ExternalSecret

**Files:**
- Create: `tenants/platform/services/ai-sre-relay/postInstall/externalsecret.yaml`

**Interfaces:** Vault path `holmes` (reuse; the operator seeds these keys) — `jira_url`, `jira_email`, `jira_api_token`, `github_token`, `discord_webhook_url`, `litellm_key`.

- [ ] **Step 1: Write the ExternalSecret**

```yaml
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: ai-sre-relay
  namespace: ai-sre
spec:
  refreshInterval: 1h
  secretStoreRef:
    name: vault
    kind: ClusterSecretStore
  target:
    name: ai-sre-relay
    creationPolicy: Owner
  data:
    - secretKey: LITELLM_KEY
      remoteRef: { key: holmes, property: litellm_key }
    - secretKey: DISCORD_WEBHOOK_URL
      remoteRef: { key: holmes, property: discord_webhook_url }
    - secretKey: JIRA_URL
      remoteRef: { key: holmes, property: jira_url }
    - secretKey: JIRA_USERNAME
      remoteRef: { key: holmes, property: jira_email }
    - secretKey: JIRA_API_TOKEN
      remoteRef: { key: holmes, property: jira_api_token }
    - secretKey: GITHUB_TOKEN
      remoteRef: { key: holmes, property: github_token }
```

- [ ] **Step 2: Validate**

Run: `kubeconform -strict tenants/platform/services/ai-sre-relay/postInstall/externalsecret.yaml` (or repo's manifest-lint target).
Expected: valid (ExternalSecret CRD schema).

- [ ] **Step 3: Operator prerequisite (HUMAN)**

Seed the Vault `holmes` secret with `jira_url`, `jira_email`, `jira_api_token`, `github_token`, `discord_webhook_url` (litellm_key already present). GitHub token: **PR scope only**, no admin/merge.

- [ ] **Step 4: Commit**

```bash
git add tenants/platform/services/ai-sre-relay/postInstall/externalsecret.yaml
git commit -m "feat(ai-sre): externalsecret for alert relay outputs"
```

---

### Task 12: Deployment + Service (rawManifests) + tenant entry

**Files:**
- Create: `tenants/platform/services/ai-sre-relay/postInstall/ai-sre-relay.yaml`
- Modify: `tenants/platform/tenant.yaml`

> rawManifests convention (confirmed against `litellm-redis`): plain multi-doc
> K8s YAML at `services/<name>/postInstall/<name>.yaml`. No wrapper key.

**Interfaces:** publishes `Service` `ai-sre-relay.ai-sre.svc.cluster.local:8080` — the Alertmanager target in Task 13.

- [ ] **Step 1: Write the manifests** (`postInstall/ai-sre-relay.yaml`)

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ai-sre-relay
  namespace: ai-sre
  labels: { app: ai-sre-relay }
spec:
  replicas: 1
  selector:
    matchLabels: { app: ai-sre-relay }
  template:
    metadata:
      labels: { app: ai-sre-relay }
    spec:
      securityContext:
        runAsNonRoot: true
        seccompProfile: { type: RuntimeDefault }
      containers:
        - name: relay
          # Pin to the tag published by the apps CI (Task 10 gate).
          image: jdwlabs/ai-sre-relay:<PUBLISHED_TAG>
          ports:
            - { name: http, containerPort: 8080 }
          envFrom:
            - secretRef: { name: ai-sre-relay }
          env:
            - { name: HOLMES_URL, value: "http://holmes-holmes.ai-sre.svc.cluster.local" }
            - { name: LITELLM_URL, value: "http://litellm.ai-sre.svc.cluster.local:4000/v1" }
            - { name: LITELLM_MODEL, value: "claude-sonnet" }
            - { name: JIRA_PROJECT, value: "JDWLABS" }
          readinessProbe:
            httpGet: { path: /healthz, port: http }
            initialDelaySeconds: 3
          livenessProbe:
            httpGet: { path: /healthz, port: http }
            initialDelaySeconds: 10
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: { drop: ["ALL"] }
          resources:
            requests: { cpu: 10m, memory: 16Mi }
            limits: { memory: 32Mi }
---
apiVersion: v1
kind: Service
metadata:
  name: ai-sre-relay
  namespace: ai-sre
spec:
  selector: { app: ai-sre-relay }
  ports:
    - { name: http, port: 8080, targetPort: http }
```

- [ ] **Step 2: Add the tenant.yaml application entry** (after the holmes entry, ~line 306)

```yaml
  # Wave 6: AI-SRE alert relay (webhook target; calls holmes wave 5, litellm wave 5)
  - name: ai-sre-relay
    chart: ""
    repo: ""
    revision: ""
    namespace: ai-sre
    rawManifests: true
    postInstall: true
    syncWave: 6
```

- [ ] **Step 3: Validate**

Run: `kubeconform -strict tenants/platform/services/ai-sre-relay/postInstall/ai-sre-relay.yaml` and the repo's tenant-render/lint target if present.
Expected: Deployment + Service valid.

- [ ] **Step 4: Commit**

```bash
git add tenants/platform/services/ai-sre-relay/postInstall/ai-sre-relay.yaml tenants/platform/tenant.yaml
git commit -m "feat(ai-sre): deploy alert relay deployment and service"
```

---

### Task 13: Repoint Alertmanager to the relay

**Files:**
- Modify: `tenants/platform/services/kube-prometheus-stack/postInstall/alertmanager-config-externalsecret.yaml`

**Interfaces:** the `holmes` receiver's webhook URL changes to the relay; the relay now drives Holmes. The raw `discord` receiver stays (immediate un-enriched notification alongside the relay's later enriched post).

- [ ] **Step 1: Change the receiver URL + rename for clarity**

Replace the `holmes` receiver block (current lines ~50-54):
```yaml
            - name: 'holmes'
              webhook_configs:
                # Chart fullname is '<release>-holmes' => service 'holmes-holmes'.
                - url: 'http://holmes-holmes.ai-sre.svc.cluster.local/api/alertmanager'
                  send_resolved: false
```
with:
```yaml
            - name: 'ai-sre'
              webhook_configs:
                # Relay receives the raw alert, drives a Holmes investigation,
                # and fans out to Discord + Jira + a gated PR.
                - url: 'http://ai-sre-relay.ai-sre.svc.cluster.local:8080/webhook'
                  send_resolved: false
```
And update the route receiver reference (current line ~34) from `receiver: 'holmes'` to `receiver: 'ai-sre'`.

- [ ] **Step 2: Validate**

Run: `kubeconform -strict tenants/platform/services/kube-prometheus-stack/postInstall/alertmanager-config-externalsecret.yaml`
Expected: valid. Confirm the embedded `alertmanager.yaml` still parses (no unbalanced templating) by eye — the `{{ .discord_webhook_url }}` ESO template and the nested `{{ template ... }}` escapes are unchanged.

- [ ] **Step 3: Commit**

```bash
git add tenants/platform/services/kube-prometheus-stack/postInstall/alertmanager-config-externalsecret.yaml
git commit -m "feat(ai-sre): route alertmanager webhooks to the relay"
```

---

## Phase-2 live gate (operator, after both PRs merge)

- [ ] **G1:** Relay pod `Running`, `readinessProbe` green: `kubectl -n ai-sre get pods -l app=ai-sre-relay`.
- [ ] **G2:** ExternalSecret synced: `kubectl -n ai-sre get externalsecret ai-sre-relay` → `SecretSynced`.
- [ ] **G3:** Synthetic alert drives the full pipeline. Fire a test `KubePodCrashLooping` (or `amtool` a fake alert into Alertmanager) and confirm: a Discord message with RCA, a new Jira issue labeled `amfp-<fingerprint>` + `ai-sre`, and — if a confident patch was produced — a PR marked "do not auto-merge". Re-fire the same alert; confirm Jira **comments** rather than creating a duplicate.
- [ ] **G4:** Kill Holmes (scale to 0) and fire an alert; confirm Discord gets an "investigation failed" notice and the relay logs an `ERROR` with the fingerprint (no crash).

---

## Self-Review

**Spec coverage:**
- Goals (Alertmanager→Holmes→Discord/Jira/PR): Tasks 3–9, 13 ✓
- Smallest footprint (Go/scratch): Tasks 1, 10; resources in Task 12 (10m/16Mi/32Mi) ✓
- Pure GitOps: Phase B, no manual kubectl (rawManifests + ESO) ✓
- Provider-agnostic via LiteLLM: Task 4 + main wiring ✓
- Components 1–7 (webhook/holmes/patch/discord/jira/github/pipeline): Tasks 9,3,4,5,6,7,8 ✓
- Error handling (independent outputs, Holmes terminal, patch non-terminal, no pile-up retries): Task 8 ✓
- Dedup in Jira labels: Task 6 ✓
- Security (PR-scope token, no exec, Vault/ESO): Tasks 7, 11, Global Constraints ✓
- Deployment (apps source, platform deploy, alertmanager reroute, resources): Tasks 10, 12, 13 ✓
- Testing (fakes per package, patch schema/confidence, synthetic integration): every task + G3 ✓

**Placeholder scan:** `<PUBLISHED_TAG>` in Task 12 is an intentional operator fill-in (the CI-published semver from Task 10's gate), not a plan gap. rawManifests wrapper is flagged "confirm against litellm-redis" — a verification step, not a missing value.

**Type consistency:** `Alert`, `Analysis`, `Patch`, `IssueKey`, `PRLink` defined in Task 2; client constructors and the pipeline interfaces (`holmesInvestigator`, `patcher`, `jiraUpserter`, `prOpener`, `discordNotifier`) use them consistently. `Notify` signature `(ctx, Alert, Analysis, IssueKey, *PRLink)` matches between Task 5 and Task 8. `Generate` returns `(*Patch, error)` in Tasks 4 and 8. `Upsert` returns `(IssueKey, error)` in Tasks 6 and 8. `OpenPR` returns `(PRLink, error)` in Tasks 7 and 8.
