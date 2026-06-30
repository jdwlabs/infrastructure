# AI-SRE Alert Relay — Design Spec

> Sub-design of the cluster AI-SRE agent. See
> `2026-06-29-cluster-ai-sre-agent-design.md` for the parent architecture.

## Problem

HolmesGPT (deployed in `ai-sre`) is an *investigation engine*: given an alert it
returns a root-cause analysis over HTTP, but it has **no output sinks**. It does
not create Jira issues, post to Discord, or open PRs. Those outputs are normally
provided by the Robusta runner, which this design deliberately avoids (no SaaS,
no telemetry — the `robusta` toolset was disabled for this reason).

The relay is the missing "mouth": it turns a fired alert into an investigation
and fans the result out to humans (Discord, Jira) and, when safe, into a
human-gated remediation PR.

## Goals

- Receive Alertmanager webhooks, drive a Holmes investigation, deliver outputs.
- Outputs: Discord message, deduplicated Jira issue, and a best-effort GitHub PR.
- Smallest possible runtime footprint (cluster is memory-constrained; budget is
  tight). This is the dominant constraint and drives the language/packaging choice.
- Pure GitOps deployment; no manual `kubectl`/`helm`.
- Provider-agnostic: all LLM calls go through the existing LiteLLM gateway.

## Non-goals (v1)

- Auto-merging PRs. Every PR is human-reviewed and human-merged.
- Mutating the cluster directly (no `kubectl apply/delete/edit` from the relay).
- A web UI or persistent database. State is reconstructable from Jira/GitHub.

## Decisions

| Decision | Choice | Why |
|----------|--------|-----|
| Language | **Go** | Static binary, ~8–15Mi RSS, no interpreter floor. Memory is the hard constraint. |
| Image | **`scratch`/distroless**, multi-stage build | ~10MB image, minimal attack surface. |
| Source repo | **apps** | Main tooling source; already has the image build/publish pipeline. |
| Image name | `jdwlabs/ai-sre-relay` | Matches the `jdwlabs/*` registry convention. |
| Deploy repo | **platform** | Tenant envelope (`ai-sre` namespace) deploys the published image. |
| Outputs | Discord + Jira + gated PR | Full detect→explain→remediate. |
| LLM access | via LiteLLM gateway only | Keeps the backend swappable. |

## Architecture

```
Alertmanager --POST /webhook--> [relay :8080] --POST /api/investigate--> Holmes
                                    |<------- root-cause analysis -------|
                                    |
                                    |  (optional) POST /v1/chat/completions
                                    |------------> LiteLLM (structured patch) ----+
                                    |<-------------- {file, content} -------------+
                                    |
                                    |--> Discord webhook   (always)
                                    |--> Jira REST         (always; deduped)
                                    +--> GitHub API        (only if confident patch)
```

The relay is a single always-on Deployment (1 replica). It is stateless;
deduplication state lives in Jira (issue labels), not in the relay.

## Components

Each is a small, independently-testable Go package with a narrow interface.

### 1. `webhook` — HTTP ingress
- **Does:** serves `POST /webhook` (Alertmanager v4 payload) and `GET /healthz`.
- **Interface:** decodes the payload into `[]Alert`, filters to firing alerts,
  hands each to the `pipeline`. Returns `200` quickly; investigation runs async
  so Alertmanager is not held open.
- **Depends on:** `pipeline`.

### 2. `holmes` — investigation client
- **Does:** calls Holmes `POST /api/investigate` with the alert context.
- **Interface:** `Investigate(ctx, Alert) (Analysis, error)` where `Analysis`
  carries the prose root cause + suggested fix.
- **Depends on:** Holmes service URL (`holmes-holmes.ai-sre.svc`).

### 3. `patch` — structured remediation generator
- **Does:** a second, tightly-constrained LLM call (through LiteLLM) that turns
  the prose analysis into a machine-readable patch.
- **Interface:** `Generate(ctx, Analysis) (*Patch, error)`. `Patch` =
  `{repo, file_path, new_content, branch, rationale, confidence}`. Returns
  `nil` (no error) when no confident patch is possible.
- **Contract:** prompt demands strict JSON; the package validates the schema and
  the `confidence` threshold. Anything malformed or low-confidence → `nil`.
- **Depends on:** LiteLLM (`litellm.ai-sre.svc:4000/v1`), a virtual key.

### 4. `discord` — notifier
- **Does:** `POST` a formatted message to the Discord webhook.
- **Interface:** `Notify(ctx, Alert, Analysis, *PRLink) error`.

### 5. `jira` — issue manager (with dedup)
- **Does:** create or update a Jira issue for the alert.
- **Interface:** `Upsert(ctx, Alert, Analysis, *PRLink) (IssueKey, error)`.
- **Dedup:** searches for an open issue labeled `amfp-<fingerprint>`; if found,
  adds a comment instead of creating a duplicate; else creates one labeled with
  the fingerprint.

### 6. `github` — PR opener
- **Does:** via the GitHub REST API (no local clone), create a branch, commit
  the patched file, open a PR linked to the Jira issue.
- **Interface:** `OpenPR(ctx, Patch, IssueKey) (PRLink, error)`.
- **Safety:** only invoked when `patch.Generate` returned a non-nil patch.

### 7. `pipeline` — orchestrator
- **Does:** sequences the above per alert: investigate → (try patch) → upsert
  Jira → (open PR if patch) → notify Discord with all links.
- **Interface:** `Handle(ctx, Alert) error`.
- **Ordering:** Jira before PR (so the PR can link the issue); Discord last (so
  it can include both links).

## Data flow (one firing alert)

1. Alertmanager `POST /webhook`; relay returns `200`, processes async.
2. `holmes.Investigate` → prose `Analysis`.
3. `patch.Generate` → `*Patch` or `nil`.
4. `jira.Upsert` → `IssueKey` (new or commented).
5. If `Patch != nil`: `github.OpenPR` → `PRLink`.
6. `discord.Notify` with analysis + issue + optional PR link.

## Error handling

- **Each output is independent.** A failure in one (e.g. GitHub down) must not
  suppress the others. Errors are logged with the alert fingerprint; the relay
  posts whatever it can. No silent failures — every dropped output is logged at
  `ERROR` with cause.
- **Holmes failure** is terminal for that alert: log and stop (nothing to fan
  out). Discord still gets a "investigation failed" notice so humans are not
  left blind.
- **Patch generation failure** is non-terminal: skip the PR, continue. This is
  the common, expected case — most alerts have no GitOps-expressible fix.
- **No retries that pile up.** At-most-once per alert delivery; Alertmanager
  re-sends on `repeat_interval`, and Jira dedup absorbs the repeats.

## Security / safety

- Relay holds a **GitHub token with PR scope only** (no admin, no merge). PRs
  are human-merged per the global PR-review policy.
- LiteLLM virtual key is scoped to the relay; rotated via Vault.
- Relay never executes returned content — it only writes it into a PR file for
  human review.
- All secrets via Vault + ESO (`ClusterSecretStore` `vault`); none committed.

## Deployment

- **apps repo:** Go source under `tools/ai-sre-relay/` (or repo convention),
  Dockerfile (multi-stage → scratch), CI builds + publishes
  `jdwlabs/ai-sre-relay:<tag>`.
- **platform repo:** under `tenants/platform/services/ai-sre-relay/` —
  Deployment + Service values, an ExternalSecret for
  `{DISCORD_WEBHOOK_URL, JIRA_*, GITHUB_TOKEN, LITELLM_KEY}`, application entry
  in `tenant.yaml` (`ai-sre`, syncWave 5).
- **Alertmanager:** webhook target changes from `holmes-holmes` to the relay
  (`ai-sre-relay.ai-sre.svc`). The relay calls Holmes.
- Resources: requests `10m`/`16Mi`, limit `32Mi`.

## Dependencies / risks

- **apps image pipeline (JDWLABS-72).** Building `jdwlabs/ai-sre-relay` depends
  on the apps publish pipeline being healthy. If still blocked, that must clear
  first — this is the critical path for the chosen Go packaging.
- **Holmes `/api/investigate` payload shape** must be confirmed against the
  deployed 0.34.0 image at implementation time.
- **PR quality.** Structured-patch generation is the riskiest feature; the
  confidence gate + human merge contain the blast radius. If quality is poor in
  practice, the PR step can be disabled by config without touching Discord/Jira.

## Testing

- Unit-test each package against a fake HTTP server (Holmes, Discord, Jira,
  GitHub, LiteLLM) — table-driven, no live calls.
- `patch` package: test schema validation + confidence gate with canned LLM
  responses (valid, malformed, low-confidence → expect `nil`).
- Integration: a synthetic `KubePodCrashLooping` alert drives the full pipeline
  in a kind/test namespace with stubbed outputs (the Phase 2 gate).

## Out of scope (later)

- Auto-merge / auto-remediation whitelist.
- Multiple Jira projects or Discord channels by severity.
- Persisting investigation history (Holmes operator could own this later).
