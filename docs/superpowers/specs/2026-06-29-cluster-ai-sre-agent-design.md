# Cluster AI-SRE Agent — Design

**Status:** Approved design, pre-implementation
**Date:** 2026-06-29
**Repo:** infrastructure (with implementation also touching platform)

## Summary

An always-running, provider-agnostic AI agent that monitors the Talos/Kubernetes
cluster, performs root-cause analysis on incidents, and assists resolution. It
adopts an existing CNCF SRE-agent harness (HolmesGPT) rather than building one,
routes all LLM calls through a provider-agnostic gateway (LiteLLM) so the model
backend (Claude, Codex/OpenAI, Kimi, Ollama, or self-hosted vLLM on the local
RTX 5090) is a swappable config choice, and is capped at human-gated remediation
(detect+explain and PR-based) for v1.

The recurring high-cost incidents this targets are infrastructure-layer faults
whose *root cause* is slow to find, not hard to fix: VXLAN cross-node overlay
blackholes, etcd quorum loss from control-plane IP flips, and memory-starvation
OOM events. The goal is to cut mean-time-to-resolution by making root-cause
analysis fast and consistent.

## Goals

- Continuous, low-cost cluster monitoring with AI root-cause analysis.
- Provider-agnostic LLM backing — swap Claude / Codex / Kimi / Ollama / self-host
  via configuration, no agent code changes.
- Use the local RTX 5090 (on pve5) as a free inference tier for high-volume,
  low-stakes work; reserve frontier (Claude) for hard reasoning.
- Human-gated remediation only in v1: detect+explain and PR-based fixes.
- Reuse existing operational surfaces: Jira (record), GitHub (PRs), Discord
  (notifications), Prometheus/Alertmanager (triggers), ArgoCD (apply).

## Non-Goals (v1)

- Autonomous remediation. Deferred to a later phase, limited to a small
  whitelist of low-risk actions, evidence-gated. **Never** etcd / control-plane.
- A bespoke agent harness. Adopt HolmesGPT; the custom-brain option is held only
  as an escape hatch for fault classes the adopted loop demonstrably cannot
  handle.
- Multi-cluster orchestration.

## Background / Decisions

The two candidate postures evaluated:

- **A — Adopt + configure:** HolmesGPT operator + LiteLLM gateway, extended with
  custom toolsets and runbooks.
- **B — Adopt detector, build brain:** k8sgpt for detection + a custom
  Claude-SDK agent for bespoke logic.

**Decision: A is the spine.** HolmesGPT is far more extensible than B assumed —
custom YAML toolsets execute arbitrary (Jinja2-templated) shell, so `talosctl` /
`etcdctl` become first-class tools; custom runbooks ingest the existing
`.remember/` incident knowledge; MCP servers plug in anything else. This gives
the same extension surface a custom agent would, without owning the agent loop,
context compaction, self-critique, and retry logic that the harness already
solves. B's custom brain survives only as a documented, evidence-driven escape
hatch for a specific fault class if Holmes's loop proves insufficient.

**Why a harness at all:** an LLM alone is a stateless token predictor. The
harness — agent loop (ReAct: Thought→Action→Observation), tool registry, context
management, guardrails, memory — is what turns it into a system that can act on a
live cluster. Industry consensus is that the harness matters more than the model;
adopting a mature one is the high-leverage choice.

## Architecture

```
 ┌─────────────────── Talos cluster ───────────────────┐
 │  Alertmanager ──webhook──► HolmesGPT (operator pod)  │
 │  CronJob (hourly) ───────►   │  ReAct loop           │
 │                               │  custom toolsets:     │
 │                               │   kubectl, talosctl,  │
 │                               │   etcdctl, promQL     │
 │                               │  runbooks (.remember) │
 │                               ▼                       │
 │                          LiteLLM gateway (pod)        │
 │                          + Postgres + Redis           │
 │     outputs:             routing ┬──────────┐         │
 │   ├─ Jira issue (root-cause+fix) │          │         │
 │   ├─ GitHub PR (remediation)     │          │         │
 │   └─ Discord ping                │          │         │
 └──────────────────────────────────┼──────────┼────────┘
                                     │          │
                              Claude API   vLLM @ pve5 VM
                              (hard RCA)   (5090, cheap/scan)
                                     └──fallback─┘
```

### Components

Each component has one clear purpose, a well-defined interface, and is
independently testable.

1. **HolmesGPT operator** — the harness/brain. Runs the ReAct loop, loads
   runbooks, dispatches toolsets. *Adopted, not built.* Interface: receives an
   alert/scan trigger, emits a root-cause + suggested action. Depends on LiteLLM
   for inference and on toolsets for cluster access.

2. **LiteLLM gateway** — the provider-agnostic socket. A self-hosted proxy
   exposing one OpenAI-compatible endpoint; all LLM calls go through it. Backend
   selection, routing, fallback, per-key budgets, and cost tracking live here.
   Depends on Postgres (keys/spend) and Redis (routing state). Interface: an
   OpenAI-format API; clients are backend-agnostic.

3. **vLLM on pve5 VM** — the local inference tier. Ubuntu VM with the RTX 5090
   passed through via PCIe, running vLLM, exposed on the LAN. Used for the
   high-volume / low-stakes tier (drift scans, simple triage). Interface: an
   OpenAI-compatible endpoint registered as a LiteLLM backend. (LXC with a shared
   GPU is a lighter alternative; VM chosen for clean isolation of a dedicated
   inference box.)

4. **Custom toolsets** — teach Holmes the cluster's infra layer. YAML toolsets
   wrapping `talosctl`, `etcdctl`, and promQL queries, plus the built-in kubectl
   toolset. **Read-only verbs only in v1** (`get`, `describe`, `member list`,
   query) — no `delete`, `edit`, or `reset`.

5. **Output adapters** — Jira issue create/comment (durable record + suggested
   fix), GitHub PR (remediation, linked to the Jira issue), Discord webhook
   (instant notification, same channel alerts already use).

6. **Triggers** — Alertmanager webhook (event-driven, primary) and an hourly
   CronJob (drift scan, secondary, routed to the free local tier).

## Data Flow

### Incident flow (event-driven, primary)

1. Fault fires (OOM, etcd quorum drop, VXLAN blackhole) → Alertmanager rule →
   webhook → HolmesGPT.
2. Holmes searches for a matching runbook from the `.remember/` knowledge base.
3. ReAct loop gathers data via toolsets (`kubectl describe`, `talosctl`,
   `etcdctl member list`, promQL), correlates, and forms a root cause.
4. LLM calls are routed by LiteLLM: hard root-cause → Claude; simple → 5090.
5. Output: Jira issue (root-cause + suggested fix) + Discord ping. If the fix is
   GitOps-expressible, a GitHub PR is opened and linked to the Jira issue.
6. A human reviews and merges the PR (or not); ArgoCD applies. Auto-remediation
   stays off in v1.

### Drift flow (scheduled, secondary)

Hourly CronJob → light Holmes scan → routed to the 5090 (free) → escalates to
Jira/Discord only on a finding; silent otherwise.

## Failure Handling

The agent must fail safe during the exact incidents it exists to help with.

- **Read-only blast radius (v1).** Toolsets expose only query verbs.
  Remediation leaves the system only as a PR a human merges. The guardrail is
  enforced at the tool layer, not by trusting the model.
- **LLM provider down** → LiteLLM fallback chain (Claude↔5090). No single
  provider outage stops the agent.
- **pve5 offline** (it is a workstation node and may reboot) → local tier drops;
  routing falls back to Claude. Non-event.
- **Cluster API unreachable** (the real outages, e.g. etcd quorum loss) →
  Holmes's own tools fail; it reports degraded state from cached/external
  signals and must **not** fabricate a remediation.
- **Cost runaway** → LiteLLM per-key budget cap as a hard ceiling.

## Testing Strategy

Replay-driven. Past incidents recorded in `.remember/` are the regression set:
each becomes a case asking "did the agent reach the documented root cause, and
faster?" This is the only honest test for an SRE agent — real outages can't be
scheduled.

Per-phase verification gates are defined in the build sequence below.

## Build Sequence (phased, tracer-bullet)

Each phase ships a working vertical slice, verified before the next.

**Phase 0 — Gateway foundation.** Deploy LiteLLM (pod + Postgres + Redis) via
GitOps; Claude key from Vault.
*Gate:* curl the gateway → Claude responds; budget cap enforced.

**Phase 1 — Holmes detect+explain, Claude-only, manual trigger.** Deploy
HolmesGPT pointed at LiteLLM; built-in toolsets only.
*Gate:* `holmes ask` against a replayed past fault produces a sane root cause
(stdout only).

**Phase 2 — A-CHECKPOINT: event-driven + outputs, Claude-only.** Alertmanager
webhook → Holmes; wire Jira + Discord adapters; PR remediation on.
*Gate:* a real/synthetic alert produces a Jira issue + Discord ping + PR where
applicable. This is the "verify with A" milestone — a full, shippable pipeline
with no local model yet.

**Phase 3 — Custom toolsets (infra faults).** Add YAML toolsets for `talosctl`,
`etcdctl`, promQL; load `.remember/` runbooks, prioritized by incident history
(VXLAN blackhole, etcd IP-flip, hugepages OOM).
*Gate:* replaying the etcd IP-flip and VXLAN cases reaches the documented root
cause and cites the runbook.

**Phase 4 — B: 5090 local tier.** Build the pve5 GPU VM (Ubuntu + PCIe
passthrough + vLLM); register as a LiteLLM backend; route drift-scan + triage to
local, hard RCA to Claude; enable the hourly CronJob.
*Gate:* drift scan runs on the 5090 (Claude scan spend = $0); killing the Claude
key falls back to local; rebooting pve5 falls back to Claude.

### Deferred (not v1)

- **Phase 5 — auto-remediate whitelist:** a small set of low-risk actions
  (e.g. restart an OOM pod), evidence-gated, never touching etcd/control-plane.
- **Custom Claude-SDK brain:** escape hatch, only if Holmes's loop proves
  insufficient for a specific fault class.

## Open Risks / Notes

- Talos + GPU passthrough is deliberately avoided; the GPU VM lives on pve5
  *outside* Talos to protect the already-fragile cluster (CP nodes run hot,
  recurring etcd quorum incidents).
- Model quality: local 32B-class models trail frontier Claude at agentic
  tool-calling and multi-step reasoning; tiered routing exists precisely to keep
  the hard reasoning on Claude.
- Spec spans two repos (infrastructure for the GPU VM, platform for in-cluster
  manifests); the implementation plan must sequence cross-repo work.
