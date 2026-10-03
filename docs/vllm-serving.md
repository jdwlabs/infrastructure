# vLLM Serving — Operator Guide

`vllm-inference` (`terraform/gpu-node.tf`, RTX 5090 passthrough on `pve5`) is
outside the Talos cluster, the same way the HAProxy VM is: Terraform
provisions the VM shell, and `talops` converges what runs on it through
`internal/hostconverge` — the same write/backup/validate/activate/verify/
rollback sequence documented in [ARCHITECTURE.md §7](ARCHITECTURE.md) ("Host
Convergence"). The GPU host's bring-up — the network gotcha, GPU passthrough,
and the NVIDIA driver install — lives in
[scenarios/ai-sre-agent-runbook.md](../scenarios/ai-sre-agent-runbook.md)
(§3 for the driver). This document covers operating what runs on the host
day to day: what serves, how to change it, and how to read drift. The
[weekly model audit](#weekly-model-audit), which suggests what to change it
to, is at the end.

## What serves and who depends on it

The single source of truth for what the GPU host runs is
[`inference/vllm/serving.yaml`](../inference/vllm/serving.yaml), committed to
this repo. `talops vllm apply --confirm` is the only thing that acts on it —
merging an edit to that file changes nothing by itself, exactly as the
comment at the top of the file says.

Three consumers reach the server directly, and all lose it for the duration
of an `apply`'s restart and health check:

- `@server` answers (`jdw-deployments` `minecraft-fwb` `llm.model`)
- LiteLLM's `sre-investigator-local` route (`platform`
  `tenants/platform/services/litellm/values.yaml`)
- LiteLLM's `pr-reviewer` route (the same `platform` file)

Each names the model in every request, and the server refuses a name it does
not serve with a 404. All three request `local-chat`, the server's only
name, so a model change in `serving.yaml` needs no edit in either consumer
repo. Renaming it again follows
[Renaming the served model](#renaming-the-served-model).

`talops vllm plan` names these same three consumers whenever it reports a
change, so a plan doubles as the announcement list for a change window.

## `serving.yaml` fields and validation

The committed [`serving.yaml`](../inference/vllm/serving.yaml) is the worked
example: it sets every field below.

| Field | Required | Rule | Why |
|---|---|---|---|
| `image` | yes | `<repo>:<tag>@sha256:<64 hex>`, lowercase OCI reference charset | The digest pins what runs; the tag is what Renovate reads. |
| `model.repo` | yes | `[A-Za-z0-9._:/=,+@-]+`, not starting with `-` | Ends up as a literal word on the Quadlet's `Exec=` line — a systemd command line that splits on whitespace and quotes and expands `%` and `$` — and as `vllm serve`'s first argument, where a leading `-` would make it a flag. |
| `model.revision` | yes | 40 hex characters | A branch moves; a commit pins what's downloaded. |
| `servedName` | yes | non-empty, same charset as `model.repo`, not starting with `-` | Identifies the model in client requests and is the name every response reports. It also lands unescaped in a Prometheus label the drift check emits. `--served-model-name` takes a list, so a leading `-` would end it and be read as the next option. |
| `servedAliases` | no | each matches the same charset and does not start with `-`; no duplicates; none equal to `servedName` | Further names the server answers to, so a consumer can keep requesting an old name while it moves to `servedName`. Rendered after `servedName` under the one `--served-model-name` flag. |
| `port` | no (default `8000`) | 1–65535 | |
| `args` | no | each element matches the same charset; JSON-valued vLLM args (anything needing a literal `{`/`}`/space) are unsupported for this reason | Every element is a literal `Exec=` word. |
| `healthGate.timeout` | no (default `10m`) | duration, must be `> 0` and `<= 10m` (the 15m rollback bound minus 5m for restore and restart, since a rollback re-runs this gate against the restored server) | Model load on this card is minutes, not seconds — the weights alone take a while to page onto the GPU. |

Unknown keys are rejected outright (`KnownFields(true)`), and `args` may not
set `--model`, `--revision`, `--port`, `--served-model-name`, or `--host`,
in either the `-` or the `_` spelling, which vLLM treats as the same flag,
or any shortened form of one (`--served-model-nam`, `--revisio`, `--mod`),
since vLLM's parser resolves an abbreviated long option to the full one —
talops's own rendered `Exec=` line already sets all five (`ExecArgs` in
`internal/vllm/render.go`): `model.repo` positionally, as `vllm serve`'s
first argument, and the rest as flags. A duplicate in `args` would either
conflict or silently lose to argument order — a second `--revision` could
load a revision other than the one `model.revision` pins and the applied
record reports.

A dotted option is judged by its root, the part before the first `.`:
vLLM's parser rewrites `--root.key=value` into `--root` with a JSON value,
so `--served-model-name.x=y` and `--mod.x=y` are rejected like the flags
they would set. A dotted option whose root is not one of the five, or a
shortened form of one, is allowed.

`talops` chdirs to the repo root before every command, and a relative
`--spec` is resolved **after** that chdir, so it's repo-root-relative.
`--tfvars` is the opposite: when you pass it explicitly, `AnchorToRepoRoot`
makes it absolute against your **invocation** directory *before* the chdir
happens (`app.go`) — left at its default (`terraform.tfvars`), it's found by
the ordinary tfvars search from the repo root once anchored, but an explicit
relative `--tfvars` means "relative to where you ran `talops` from," not
"relative to the repo." `apply` additionally requires the spec to be inside
this repo, tracked by git, and committed with no uncommitted changes
(`spec_outside_repo`, `spec_untracked`, `dirty_spec`) — an apply is recorded
on the host against the git commit it came from, so an uncommitted or
out-of-repo spec has no commit to record. Applying a commit that is not on
`origin/main` (as the checkout last fetched it — `apply` does not fetch), or
from a checkout with no `origin/main` at all, is allowed, since trying a
change before it merges is legitimate, but the report carries a note saying
so: the record then names a commit `main` may never contain, and a later
apply from `main` undoes the change.

### How vLLM treats several served names

`ExecArgs` renders `--served-model-name <servedName> <alias>...`, with
`servedName` first. In vLLM v0.24.0 (cited on `ExecArgs` in
`internal/vllm/render.go`):

- A request naming **any** of the names is accepted. Any other non-empty
  name gets a 404 `NotFoundError`. A request with no `model` at all is
  accepted too.
- `/v1/models` lists **every** name as its own entry, in that order, each
  with `root` set to `model.repo`.
- A response's `model` field is **always the first name**, `servedName`,
  whichever name the request used. A consumer still requesting an alias
  gets `local-chat` back in its responses.

## Renaming the served model

Consumers request the model by name, and a name the server does not serve
is a 404. So a rename moves one consumer at a time and never relies on two
repos changing at once:

1. **Add the alias.** In one PR, set `servedName` to the new name and add the
   old name to `servedAliases`. Apply it in an announced window like any
   other change. The server now answers to both names, the health gate
   checks that `/v1/models` lists both, and every consumer keeps working
   unchanged.
2. **Move the consumers.** Change each consumer to request the new name, in
   its own repo and on its own schedule. Nothing on the GPU host changes.
3. **Drop the alias.** Once every consumer in
   [the list above](#what-serves-and-who-depends-on-it) requests the new
   name — today all three: `@server` answers, LiteLLM's
   `sre-investigator-local` route and LiteLLM's `pr-reviewer` route — remove
   the old name from `servedAliases` and apply again. That is another
   restart, so it needs another announced window. Do not drop the alias
   before every consumer has moved: the gate probes `servedName`, so it
   would pass while a consumer that had not moved got 404s.

Do not change the model in the same PR as a rename. The weekly audit's
trial steps keep both `servedName` and `servedAliases` as they are for this
reason.

## Changing the model or version

1. Edit `inference/vllm/serving.yaml` in a PR and get it merged. Nothing on
   the host changes yet.
2. **Before doing anything else, sync your local checkout to the merge**:
   `git checkout main && git pull --ff-only`, then confirm `git log -1`
   shows the merge commit. `apply` reads the **local** spec file on disk and
   records the **local** `git rev-parse HEAD` against the host — not
   whatever merged on GitHub — so a stale local checkout applies stale
   content and records the wrong commit as having produced it.
3. In an announced window:
   - Announce the restart with `tools/mc announce` in `jdw-deployments` — the
     three consumers above lose the server for the length of the restart and
     health check.
   - Check LiteLLM's cloud quota has headroom, since the cloud tier is what
     picks up requests while the local tier is unavailable.
   - `talops vllm plan` — confirms what would change and who it interrupts,
     without touching the host. **Stop here** if `changed: false` (nothing to
     apply — the merge you expected isn't reflected, or you're not on the
     commit you think you are) or if the diff/`reasons` show something you
     didn't expect from the PR. Neither of those is a reason to proceed on
     the assumption `apply` will sort it out.
   - `talops vllm apply --confirm` — stages the new image/model while the
     previous server keeps serving, then stops the previous server, starts
     the new one, and checks it before reporting success.
   - Read the apply report's `phases` and `serving` — see the table below.

Every apply with `changed: true` restarts the server and runs the health
check, whatever `reasons` says — there is no lighter path for a change that
leaves the server's own definition alone. That includes two cases where
`serving.yaml` has not changed at all: re-running `apply` only to rewrite a
missing or stale `/etc/vllm/applied.json` (after `record_failed`, say), and
the drift check's script or units differing because this `talops` build
ships a newer one. Both go through the full swap, so both need the same
announced window as a model or version change.

Staging (the image pull and, if the model revision isn't already cached, the
model download) happens before anything is touched on the running server, so
a slow pull or a multi-gigabyte download never causes an outage by itself.
So does the pre-swap GPU check after it: one throwaway run of the pinned image
with `--device nvidia.com/gpu=all --entrypoint true`, which proves podman can
hand the new image the GPU before the previous server is stopped. It exits
as soon as its devices are set up, so it loads no model and takes no GPU
memory from the server still serving.
The outage is the restart itself: **the previous server is stopped and the
new one started before the health check ever runs** — the check decides
whether to keep the new server or roll back to the old one, not whether to
start the new one in the first place. `phases` times the restart and the
check (and the rollback's own re-check, `gate-rollback`, when one happens)
separately from staging and the GPU check (`gpu-check`).

The health check has three parts, all against the served name: `/v1/models`
lists it, and every one of `servedAliases`, with `root` equal to
`model.repo`; a 1-token chat completion
finishes with `stop` or `length`; and a request offering one `get_time`
tool comes back with at least one parsed `tool_calls` entry. That last
request sends `tool_choice: "auto"`, the way consumers do, because only
`auto` passes the model's reply through `--tool-call-parser`;
`"required"` would have vLLM constrain the output to the tool schema itself
and pass with a parser that parses nothing. With `auto` the model is free to
answer in prose, so the request's system and user messages tell it to call
`get_time`, and after a swap the check is retried until
`healthGate.timeout`. The tool-call probe itself also retries a prose miss
up to 3 times before failing (`checkToolCall`), so the single check on an
apply with nothing to change does not report `gate_failed` for a server
that would have passed on a second try.

### Apply outcomes: failure codes, `serving`, and exit codes

`rolledBack` is always printed (`report.go`'s `ReportApply` writes it
unconditionally as `%t`) — it's `false`, spelled out, not a dash, whenever
nothing was rolled back. `serving` is only ever a dash (`-`) when `apply`
never got far enough to call `vllm.Apply` at all — every preflight failure
below.

**Preflight failures** (`internal/app/vllm.go`) happen before SSH ever
reaches the host — `serving` reads `-` and `rolledBack` reads `false` for
every one of them. The first five apply to `status`/`plan` too; the rest are
`apply`-only, because they check the spec against git:

| Code | When | Action |
|---|---|---|
| `tfvars_not_found` | `terraform.tfvars` couldn't be located | `talops secrets status`, or point at it explicitly with `--tfvars`. |
| `tfvars_unreadable` | `terraform.tfvars` was found but couldn't be parsed | Check the file; hydrate the vault if it looks stale. |
| `vllm_host_unset` | No `--host`, and `gpu_vm_ip` is absent from tfvars | Pass `--host`, or add `gpu_vm_ip` to the vaulted tfvars (`talops secrets edit`). |
| `spec_unreadable` | `serving.yaml` couldn't be found (no `--spec`, repo root unresolvable) or couldn't be parsed/validated | Fix the path, or fix the validation error `spec.go` reports. |
| `ssh_auth_unconfigured` | No usable SSH key or agent for the GPU host | Set `gpu_vm_ssh_key_path` in the vaulted tfvars, pass `--ssh-key`, or run an SSH agent. `gpu_vm_ssh_key_path` takes precedence: when it is set, `--ssh-key` is ignored for this host. |
| `repo_root_unresolved` | *(apply only)* Couldn't find the repo's `terraform/` directory to check the spec against git | Run from inside the checkout, or pass `--spec` under it. |
| `spec_outside_repo` | *(apply only)* `--spec` resolves to a path outside the repo | Point `--spec` at a file under the repo root. |
| `spec_untracked` | *(apply only)* The spec isn't tracked by git | `git add` and commit it first. |
| `dirty_spec_unknown` | *(apply only)* `git status --porcelain` on the spec itself failed | Check git works in the repo, then re-run. |
| `dirty_spec` | *(apply only)* The spec has uncommitted changes | Commit it, then re-run. |
| `commit_unreadable` | *(apply only)* `git rev-parse HEAD` failed | Check git works in the repo, then re-run. |
| `confirm_required` | *(apply only)* `--confirm` wasn't passed | Usage error, **exit code 2** (every other row here and below exits 1). Re-run with `--confirm`. |

**Host-phase and swap outcomes** (`internal/vllm/converge.go`), once
`vllm.Apply` starts — from here, a pre-swap failure's `serving: previous` is
`ApplyResult`'s unwritten default, meaning "apply didn't touch the running
server," not "confirmed something is serving." On a host's very first apply
with nothing running yet, that default reads misleadingly; cross-check with
`talops vllm status`:

| Code / outcome | When | `serving` | `rolledBack` | What to do |
|---|---|---|---|---|
| (success, `changed: true`) | The swap happened and the new server passed its health check | `new` | `false` | Nothing to do — the change took. |
| (success, `changed: false`) | Host already matched `serving.yaml`; one health check ran against the untouched server, and nothing was restarted | `new` | `false` | Nothing to do. |
| `host_prereq_failed` | `EnsureHost` failed. The **running server is unchanged**, but `EnsureHost` may already have installed or downgraded packages, created directories, or regenerated the CDI specs before hitting the failure (`EnsureHost` in `internal/vllm/remote.go`) — those are host-state side effects, not a swap. If the installed-package query (`dpkg-query`) itself could not be run, it stops there with nothing installed. | `previous` | `false` | Commonly the NVIDIA Container Toolkit apt repo is missing, or no longer carries the pinned toolkit release — see [Host prerequisites](#host-prerequisites). Fix and re-run; re-running is safe, `EnsureHost` is idempotent. |
| `stage_failed` | The image pull or model download failed, or the apply was interrupted (Ctrl-C, SIGTERM) before the swap — the message then starts `cancelled before the swap`. **The running server is unchanged** — `Stage` never touches it, and an interrupt during host preparation, staging or the pre-swap GPU check stops before the swap even if the step in flight finished. | `previous` | `false` | For an interrupt, just re-run. Otherwise usually disk space: `Stage` needs 30 GiB free on `/var/lib/vllm` when the model revision isn't already staged, and 12 GiB free on `/var/lib/containers` every run (`internal/vllm/remote.go`). Free space, re-run. |
| `cdi_unresolvable` | The pre-swap GPU check failed, and podman's output names CDI: it could not give the staged image the GPU (`--device nvidia.com/gpu=all`). It runs after staging and before the swap, so **the running server is unchanged** — nothing was stopped, written or restarted. | `previous` | `false` | See [Troubleshooting: unresolvable CDI devices](#unresolvable-cdi-devices). Fix the CDI spec, re-run. |
| `gpu_check_failed` | The pre-swap GPU check's `podman run` failed for a reason podman did not attribute to CDI — SSH dropped, container storage or the OCI runtime failed — so the GPU could not be verified either way (`CheckGPU` in `internal/vllm/remote.go`). **The running server is unchanged**, exactly as for `cdi_unresolvable`. | `previous` | `false` | The message carries the underlying error; fix that (host access, `podman` itself), re-run. The CDI spec is not implicated. |
| `legacy_state_unknown` | Reading `vllm.service`'s active/enabled state failed — either an SSH/run error that came back with empty output, or a `systemctl` state talops doesn't recognise (`legacyStateError` in `internal/vllm/remote.go`). Can happen before staging (`changed` still `false`, via `pendingReasons`) or again right after staging succeeds on **any** apply with something to change (`changed: true`, the `ReadLegacy` call after `Stage` in `vllm.Apply`, `internal/vllm/converge.go`) — not only a host's first apply; every changed apply re-reads legacy state to build its `Activate` hook. | `previous` | `false` | Investigate the legacy unit's state by hand, then re-run. |
| `read_failed` | Couldn't read the installed Quadlet unit, the drift-check script/service/timer files, or `/etc/vllm/applied.json` while comparing against `serving.yaml` (`pendingReasons` in `internal/vllm/converge.go`) | `previous` | `false` | Check SSH/host access, re-run. |
| `config_invalid` | No health gate configured — a talops misconfiguration, not a host problem | `previous` | `false` | File an issue; this isn't something re-running fixes. |
| `converge_failed` (write path) | Writing, backing up, moving, or `chmod`-ing one of the files to install failed, **before `Activate` ever ran** (the install loop in `hostconverge.Apply` and its `abortInstall`, `internal/hostconverge/converge.go`) — `hostconverge` cleans up its own partial temp/installed files inline, but never calls `Activate`/`Verify` again, so this path is **not** a rollback. | `previous` | `false` | Nothing was activated; the running server was never touched. Check the failure reason (disk, permissions, SSH), re-run. |
| `converge_failed` (activate path) | The unit swap's `Activate` step itself failed (a `systemctl` command errored) — this **does** trigger `hostconverge`'s rollback (`rollBackAfter` in `internal/hostconverge/converge.go`), whose outcome `vllm.Apply` then maps to this code (the error mapping after `hostconverge.Apply` in `vllm.Apply`) | `previous` if rollback succeeded, `none` if rollback also failed | `true` | If `previous`: recovered automatically, nothing to do. If `none`: read the report's `help` — see [Rollback](#rollback). |
| `gate_failed`, `changed: false` | Nothing needed to change, but the health check against the already-matching host failed | `none` | `false` | Talops restarted nothing — the **running** server itself is unhealthy. Go to [Troubleshooting](#troubleshooting). |
| `gate_failed`, `changed: true` | The new server didn't pass the health check after the swap | `previous` if rollback succeeded, `none` if rollback also failed | `true` | Same as `converge_failed` (activate path)'s rolled-back cases, above. |
| `record_failed` | The new server is serving and passed the health check, but `/etc/vllm/applied.json` couldn't be written | `new` | `false` | **The server is serving correctly.** `status` and the drift check will report drift until you re-run `apply --confirm` to rewrite the record — and that re-run restarts the server and runs the health check again, so do it in an announced window. |

**Exit codes**: `0` on success, `2` only for `confirm_required`, `1` for
every other row in both tables above — including a fully successful, fully
re-verified rollback. A rolled-back apply exits `1` because the *attempted*
change didn't take, even though the host ends up healthy.

## Trialling a candidate model

A trial is an ordinary [model change](#changing-the-model-or-version) with a
measurement on each side and a decision at the end. It takes @server and the
local LiteLLM tiers down twice — once onto the candidate, once back or onward
— so both halves happen in announced windows.

1. **Pick the candidate** from the weekly audit ticket. Its trial steps are
   the args delta; prefer the largest `marginGiB`, because the estimate's
   overhead allowance was measured on a text-only model.
2. **Measure the incumbent first**, with production's settings, so the
   comparison is not against an older agent build. From the gameops repo's
   `minecraft/agent`, run `cmd/evalllm` six times with `LLM_BASE_URL` set to
   the host, `LLM_MODEL=local-chat`, and `LLM_MAX_TOKENS`, `LLM_TIMEOUT_MS`,
   `LLM_TOTAL_TIMEOUT_MS` and `MAX_TOOL_ROUNDS` copied from
   jdw-deployments `charts/minecraft-fwb/values.yaml` (`agent.llm`). Add
   `-wiki` runs too, because the wiki cases are part of the gate. evalllm's
   own defaults are not production's, and a baseline taken at the defaults
   compares the wrong thing.
3. **Swap** with a PR to `serving.yaml` applying the trial steps, then
   `talops vllm plan` and `talops vllm apply --confirm` in an announced
   window, exactly as for any model change. A model that thinks by default
   needs thinking turned off (`--default-chat-template-kwargs.enable_thinking=false`
   when its chat template honours it): @server answers in one short chat
   line, and thinking tokens spend that budget before the answer starts.
   A hybrid candidate, one with linear-attention layers, also needs
   `--max-num-seqs` sized to the consumers' concurrency. The audit's trial
   steps do not add it, and without it vLLM's default can run the card out
   of memory at startup (see [Known limits](#known-limits)).
4. **Measure the candidate** the same way, six runs plus the wiki runs, and
   run `inference/vllm/smoke-tool-call.sh` against the host and against
   LiteLLM's `sre-investigator-local` route (`BASE_URL`, `MODEL`, `API_KEY`;
   the key is LiteLLM's, so run that one from your own terminal, at a URL
   the script's header says it will send a key to).
5. **Decide against the gate.** The candidate stays only if all of these
   hold; otherwise revert the `serving.yaml` PR and apply again:
   - no evalllm case that passed for the incumbent loses two or more of its
     six passes;
   - the wiki cases pass at least as often as for the incumbent;
   - p95 latency stays within `LLM_TIMEOUT_MS`;
   - the SRE smoke check passes.
6. **Record it**: the comparison report goes into the gameops repo's
   `minecraft/agent/docs/eval/`, and the decision, keep or revert, goes on
   the trial ticket with links to both.

## Status and drift

`talops vllm status` is a three-way, read-only comparison: the local
`serving.yaml` **on disk** (labeled `git` in its own output, but it's read
straight off your working tree with no `git show` involved — an unpulled
merge is invisible to `status` exactly the same way it's invisible to
`plan`, see [step 2 above](#changing-the-model-or-version)), the record
`apply` last wrote to the host (`/etc/vllm/applied.json`), and the running
container's own live state, read directly (`ReadLive`, `converge.go`) plus
a live `/v1/models` query for the served names (`converge.go`) — it
genuinely inspects the container, not just files. It never runs the host
phase (`EnsureHost`) — only `apply` does — so `status` alone never installs
packages or regenerates the CDI specs.

```
vllm:
  host: 192.168.1.50
  drift: false
  legacy: false
fields[5]{name,git,applied,live}:
  imageDigest,sha256:251eba...,sha256:251eba...,sha256:251eba...
  modelRevision,c58857a...,c58857a...,n/a
  servedName,local-chat,local-chat,local-chat
  servedAliases,-,-,-
  argsHash,3f2a...,3f2a...,3f2a...
```

`servedName`'s `live` column is the **first** `/v1/models` entry, which is
the name responses report. It is not whichever entry matches git, because
with aliases a server can list git's name as an alias only.
`servedAliases`'s `live` column is the entries after the first that share
its `root`. Entries with another root are LoRA adapters and are left out.
Several aliases are space-separated, since a space can never be part of a
name. `-` means there are none. An `applied.json` written before aliases
existed has no `servedAliases` key and reads as `-`.

A read that fails reports a sentinel rather than a clean result, so a
partial failure never reads as "everything matches" — but the sentinels
aren't uniform across columns. `unknown` appears only in the `applied`
column (`ReadApplied` itself failing to read or parse
`/etc/vllm/applied.json`), or in the `live` column of `servedName` and
`servedAliases` in the rare case status runs with no models reader
configured at all. A live-container
read failure of any kind — SSH broken, `podman` broken, the container
simply not running — collapses to `(not running)` instead (`ReadLive`
treats every such failure as "not running," never as "unknown";
`ReadLive` in `internal/vllm/remote.go`). `(none)` means nothing has ever been applied in the
`applied` column; in the `live` column of `servedName` and `servedAliases` it
instead means `/v1/models` answered with an empty list — reachable, but
serving nothing (`converge.go`, `liveServedNames`). `unreachable` is
specific to those two `live` columns: the `/v1/models` query itself failed.
`(no repo digest)` can appear in `imageDigest`'s `live` column when the
container is running but reports no `RepoDigests` at all (`converge.go`).
`modelRevision`'s `live` column is always `n/a` **by
design**, not a gap: the revision is present in the container's argv, but
only as part of the hashed `argsHash`, not as its own inspectable value —
reading it back out of the downloaded model cache would report what's on
disk, not what's loaded. `status` exits non-zero whenever `drift: true` —
even when nothing else failed, since drift is what a caller acts on next.

The embedded drift-check timer writes three Prometheus metrics every 5
minutes (`vllm_serving_info`, `vllm_serving_drift{reason=...}`,
`vllm_serving_drift_check_timestamp_seconds`), read by three alerts defined
in the `platform` repo. That drift script runs entirely on the host and
never sees `serving.yaml` or git at all — it only ever compares the running
container against `/etc/vllm/applied.json`, the record `apply` itself
wrote. That's the real reason a merged-but-unapplied `serving.yaml` change
triggers no alert: the container hasn't changed, so it still matches the
applied record, and the drift reason stays `none`. `talops vllm status` and
`talops vllm plan` can reveal that gap — but only once your local checkout
actually has the merge (see [step 2](#changing-the-model-or-version)); both
read `serving.yaml` off disk, not off git, so an unpulled merge is as
invisible to them as it is to the alerts, for a different reason.

| Alert | Reads | First thing to check |
|---|---|---|
| `VllmDown` | `up{job="vllm-inference-server"}` — the scrape itself, not a drift metric | `talops vllm status` — see what, if anything, is running before assuming the whole host is down. |
| `VllmServingDrift` | `vllm_serving_drift{reason=...}`, written by the host's own drift-check script comparing the container against `applied.json` | **`talops vllm status`** — the alert's own description says so. For `reason=not_running`/`image`/`args`, go to [Troubleshooting](#troubleshooting) for host-side logs; for a stale-record mismatch, `talops vllm plan` then `talops vllm apply --confirm` converges it — a restart, even when only the record is stale. |
| `VllmDriftCheckStale` | `vllm_serving_drift_check_timestamp_seconds` absence or staleness | The timer itself, not the server: `systemctl status vllm-drift-check.timer` on the host. This also fires on plain **absence** of the metric, not only a stale timestamp — a rolled-back first apply removes both the timer's enablement link and the `.prom` file it wrote (`retireNewServer` in `internal/vllm/converge.go`), which reads identically to "the check stopped running." |

## Rollback

Rollback is automatic, not a separate command, and triggers on either an
`Activate` (the unit swap itself) failure or a failed health check —
whichever happens, `hostconverge` restores the previous Quadlet unit (or, on
a host's very first apply, the pre-Quadlet `vllm.service`) and re-verifies
it:

- **A later apply that fails** restores the previous Quadlet unit, restarts
  it, and re-runs the health check against it (`phases` names this attempt
  `gate-rollback`). `serving: previous` in the report means that succeeded.
  The rollback check looks for the previous server's own identity — the
  `servedName`, `servedAliases` and `model.repo` in the `applied.json` that
  was on the host when the apply started — not the one in the `serving.yaml`
  being applied. A change that renames the served model, adds or drops an
  alias, or swaps the model repo would otherwise fail the restored server's
  check even though it is healthy. A record written before aliases existed
  has none, which is correct for the server it describes.
  Only when there is no record does the rollback check use `serving.yaml`'s
  identity.

  This identity comes from the record, not from the Quadlet that rollback
  actually restores, and the two can disagree in two rare cases: after a
  `record_failed` apply, the installed unit is newer than the last record
  written; and a change to `port` is never captured in `applied.json` at
  all, so the rollback gate's `BaseURL` is still built from the new,
  rolled-back-from `spec.Port`. In either case a healthy restored server
  can fail `gate-rollback`, and the report says `serving: none` with
  "recover by hand" for a server that is actually up — check `talops vllm
  status` before trusting that verdict in those two cases.
- **The first apply on a host** has no previous Quadlet unit to restore, so
  rollback instead restores `vllm.service` to the exact active/enabled state
  it had before the apply started — the vLLM `Activate` hook (`activate` in
  `internal/vllm/converge.go`) detects the Quadlet is now absent and calls
  `retireNewServer`, which stops the new server and puts `vllm.service` back
  the way it found it. No record exists on a first apply, so the restored
  `vllm.service` is checked against `serving.yaml`'s `servedName`,
  `servedAliases` and `model.repo`; a first apply that also changes any of
  them — including adding an alias the legacy server does not answer to —
  rolls back to a server that fails that check and reports `serving: none`
  although the legacy server is running. If nothing was serving before (no legacy unit, or one
  that was enabled but stopped), there is nothing to roll back to, and the
  report says so (`serving: none`) rather than waiting out a health-check
  timeout against nothing.
- **A new server that won't stop** (it may still hold port 8000 and the GPU)
  gets its own help text telling the operator to stop `vllm-server` by hand
  before starting anything else, rather than attempting a rollback that would
  only fight the wedged process.

See the [failure-code table](#apply-outcomes-failure-codes-serving-and-exit-codes)
above for exactly which failure codes trigger which rollback outcome.

Manual rollback is the same procedure as any other change: revert the PR
that changed `serving.yaml`, merge, pull, and run `talops vllm apply
--confirm` again — there is no separate "undo" path, because the spec file
is already the record of every previous state.

## Troubleshooting

`talops vllm status` reads the container's current state — running or not,
which image digest, the served name — but never a log line or an error
message; `plan`/`apply` only compare installed files and the applied
record. None of the three inspect *why* a container is unhealthy beyond the
health gate's own three HTTP checks. A container that's up but misbehaving,
or that crashed and is mid-`Restart=always` retry, needs the host itself:

```bash
# On the vllm-inference VM:
systemctl status vllm-server      # the unit's own state and recent log lines
journalctl -u vllm-server         # full log, including a JIT/model-load failure's traceback
podman ps                         # is the container actually running right now?
podman logs vllm                  # the container's own stdout/stderr
```

**The container is named `vllm`; the systemd unit is `vllm-server.service`.**
`systemctl`/`journalctl` take the unit name; `podman ps`/`podman logs` take
the container name — mixing them up is the most common reason a
troubleshooting command comes back empty.

`Restart=always` in the Quadlet unit already covers a transient crash (an
OOM, for instance) by itself; if the container keeps crash-looping,
`journalctl -u vllm-server` is what tells you why — nothing in `talops`
diagnoses that for you, on purpose: a live crash loop is a host-state
problem, not a `serving.yaml` drift. It's never a download retrying,
though: the Quadlet sets `Environment=HF_HUB_OFFLINE=1`
(`internal/vllm/render.go`), so the running server never talks to
HuggingFace at all — every download happens once, during `Stage`, before
the server ever starts.

### Unresolvable CDI devices

`apply` fails with `cdi_unresolvable`, or `journalctl -u vllm-server`
shows the error below. (`gpu_check_failed` is the same pre-swap GPU check
failing without podman naming CDI; start from the error in its message instead.)

```
Error: setting up CDI devices: unresolvable CDI devices nvidia.com/gpu=all
```

Podman found no usable definition of `nvidia.com/gpu=all`. It reads every
spec in `/etc/cdi` and `/var/run/cdi` (the latter wins for the same device
name), a spec it cannot parse contributes no devices, and why it could not
parse one only shows at debug log level. Ask podman, with the image the
pre-swap GPU check uses (`image:` in `inference/vllm/serving.yaml`, already pulled by
`Stage`):

```bash
# On the vllm-inference VM:
sudo podman --log-level=debug run --rm --entrypoint true \
  --device nvidia.com/gpu=all '<image from serving.yaml>' 2>&1 | grep -i cdi
grep -H cdiVersion /etc/cdi/nvidia.yaml /var/run/cdi/nvidia.yaml
dpkg-query -W nvidia-container-toolkit nvidia-container-toolkit-base \
  libnvidia-container-tools libnvidia-container1
```

- **`failed to parse CDI Spec ... json: unknown field "additionalGids"`**,
  with `cdiVersion: 0.7.0`: the spec came from nvidia-ctk 1.19 or later,
  which podman 4.9 cannot read — the reason the toolkit is pinned (see
  [Host prerequisites](#host-prerequisites)). Re-running `talops vllm apply
  --confirm` moves the toolkit back onto the pin and regenerates both specs.
  If the packages already read `1.18.2-1` but a spec still says `0.7.0`,
  something regenerated it with another binary since; delete
  `/etc/vllm/cdi-generated-for` so the next apply regenerates both.
- **No spec at all, or no `nvidia.com/gpu=all` in it**: the driver is not
  loaded, or nvidia-ctk could not see a GPU when it ran.
  `nvidia-smi` must work first; then delete `/etc/vllm/cdi-generated-for`
  and re-run apply.

## Host prerequisites

Two things `talops vllm apply` does **not** do for you, because they are
host-level setup, not something `serving.yaml` should own:

- **The NVIDIA driver.** Install it per
  [scenarios/ai-sre-agent-runbook.md](../scenarios/ai-sre-agent-runbook.md)
  (§3). `talops vllm apply` reads the installed driver's version
  (`nvidia-smi --query-gpu=driver_version`) but never installs or upgrades
  it.
- **The NVIDIA Container Toolkit's apt repository.** `talops` deliberately
  does not add apt repositories from code — `EnsureHost` installs the
  toolkit packages itself, but only once the repository offers them. The
  runbook doesn't cover this step; add the repository once, by
  hand, on the VM:

  ```bash
  curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey | \
    sudo gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
  curl -s -L https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list | \
    sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' | \
    sudo tee /etc/apt/sources.list.d/nvidia-container-toolkit.list
  sudo apt-get update
  ```

  Without it, `apply` fails with `host_prereq_failed` and a message pointing
  back at this section rather than a bare `apt-get` error. The same happens,
  with a message naming the version, if the repository stops carrying the
  pinned release below.

**The toolkit is pinned at 1.18.2-1, and held.** `EnsureHost` installs
`nvidia-container-toolkit`, `nvidia-container-toolkit-base`,
`libnvidia-container-tools` and `libnvidia-container1` at exactly
`toolkitVersion` (`internal/vllm/remote.go`), downgrading them if apt has
moved them past it, and `apt-mark hold`s all four so an unattended upgrade
cannot move them between applies. A host whose installed version or hold
differs is reinstalled on the next apply. That reinstall names only those
four: `podman` and `prometheus-node-exporter` are installed when missing
and never upgraded, so moving the toolkit does not also move podman or
restart node-exporter. The reason is the host's podman:
Ubuntu 24.04 ships podman 4.9.3, whose CDI parser rejects the spec that
nvidia-ctk 1.19 and later generate (`cdiVersion: 0.7.0`, with an
`additionalGids` field it does not know), so every GPU container fails with
`unresolvable CDI devices` — see [Troubleshooting](#unresolvable-cdi-devices).
1.18.x is the newest release that still writes `cdiVersion: 0.5.0`.

To unpin later: first get a podman on the host whose vendored CDI library
parses CDI 0.7.0 (check with `podman --log-level=debug` against a spec a
newer nvidia-ctk generated, as in Troubleshooting, without installing that
toolkit), then raise `toolkitVersion` to the new release by PR. The next
apply moves the held packages (`--allow-change-held-packages`), holds them at
the new version and regenerates both CDI specs. Removing the pin entirely
means dropping the hold too (`sudo apt-mark unhold` the four packages), or
they stay at whatever version they were last held at.

**CDI regeneration** needs no manual step, but only happens as part of
`apply`'s host phase — `status` never runs it. `EnsureHost` records the
driver and toolkit versions it last generated the CDI specs for
(`/etc/vllm/cdi-generated-for`, `<driver>|<toolkit>`), and whenever either
differs it regenerates `/etc/cdi/nvidia.yaml` (`nvidia-ctk cdi generate`) and
restarts the toolkit's own `nvidia-cdi-refresh.service`, which rewrites
`/var/run/cdi/nvidia.yaml`. It also regenerates them whenever it reinstalls
or re-holds the toolkit, even if the record already matched: it deletes the
record before that reinstall and writes it again only after both specs are
regenerated, so an apply that fails in between is finished by the next one.
Both have to be regenerated: podman reads both,
and the `/var/run/cdi` one wins for the same device name whenever podman can
parse it, so a stale one there would shadow a fresh `/etc/cdi` one. The
service is kept rather than disabled because it is also what regenerates
`/var/run/cdi/nvidia.yaml` at boot and after a driver upgrade, with the
pinned nvidia-ctk. A record from before the
toolkit was part of the key (`/etc/vllm/cdi-driver-version`) is simply
ignored, so the first apply after upgrading talops regenerates both specs
once; the old file can be deleted by hand.
A driver upgrade therefore takes effect the next time `talops vllm apply`
runs — and even then, only the CDI specs are regenerated by a no-op apply
(`changed: false`); the running container is **not** restarted, so it keeps
its old device injection until its own next restart (the following apply
that actually changes something, or a manual restart).

**node-exporter's textfile directory** is checked, never rewritten. Ubuntu's
`prometheus-node-exporter` already reads `/var/lib/prometheus/node-exporter`,
where the drift check writes its metrics, and ships `ARGS=""`, so a stock
host gets no note. Only an `ARGS` line in
`/etc/default/prometheus-node-exporter` that sets
`--collector.textfile.directory` to another directory produces a
`host: warning:` note on the apply report.

> The pinned `vllm/vllm-openai:v0.24.0` image's own CUDA compatibility check
> (`NVIDIA_REQUIRE_CUDA`) lists driver bands only up to 575, while this host
> runs 580. CDI-based GPU injection is expected to bypass that check; if a
> future apply fails with an `nvidia-container-cli` requirement error, the
> fix is adding `Environment=NVIDIA_DISABLE_REQUIRE=1` to `quadletTemplate`
> in `bootstrap/internal/vllm/render.go`, by PR, then released and applied
> normally. A hand edit of the installed Quadlet unit is not a fix: the next
> `plan`/`apply` reads it as drift from what `serving.yaml` renders, and
> `apply` reverts it back to the unmodified template.

## First apply (migration from the legacy `vllm.service`)

The very first `talops vllm apply --confirm` against a host still running
the pre-Quadlet, hand-installed `vllm.service` is a one-time migration, not
an ordinary model/version change, and its Terraform side comes first — the
VM has to be ready for the migration before `talops vllm apply` is ever run
against it.

1. **The Terraform side comes first, and it's a separate, human-gated
   plan.** Before the VM is ready for this migration:
   - Sync to the merged base — `git fetch origin && git checkout main &&
     git pull --ff-only` — not just a fetch: a fetch alone updates your
     remote-tracking ref but doesn't change what your working tree (and
     therefore `terraform plan`) actually reads, so the checkout and
     fast-forward have to happen too.
   - `cd terraform && terraform plan -out=tfplan`, reviewed by a **human**
     directly against the gate below. This is deliberately the raw
     `terraform` binary, not `talops infra plan`: that command only prints
     resource counts and `type.name` (`internal/app/infra.go`'s
     `displayInfraPlanSummary`) — never attribute-level hunks, never the
     `Plan: N to add...` line, never "forces replacement" — so the gate
     below cannot be evaluated from its output at all. (It also runs
     `terraform fmt` without `-check`, so a plan through it can silently
     reformat files first.) `talops infra plan` is not a substitute for
     this review.

   > **Plan gate**: gpu_inference must show ONLY `~
   > reboot_after_update = true -> false`, plus `+
   > proxmox_virtual_environment_file.gpu_cloud_init`; overall `Plan: 1 to
   > add, 1 to change, 0 to destroy`; refuse on any initialization hunk,
   > user_data_file_id/user_account line, forces replacement, other
   > attribute, or untraceable change; the apply must emit no "a reboot is
   > required" warning. Re-check against freshly fetched origin/main first.

   - Once the plan matches exactly, a **human** runs `terraform apply
     tfplan` — never `talops infra deploy`/`apply`, and never
     autonomously — and watches *that apply's own output* for the "a
     reboot is required" warning. The warning comes from
     `reboot_after_update = false` meeting a change to one of the
     attributes that flag actually gates
     (`cpu`/`memory`/`hostpci`/`vga`/`machine`/`initialization`) — see
     [Rebuild](#rebuild). This plan doesn't touch any of those (the VM's
     `initialization` block is untouched thanks to `ignore_changes`), so no
     such warning is expected; if one appears, stop, because it means the
     apply reached the running VM's config after all.
2. **Prerequisite: the GPU host must already be reachable by address.**
   `gpu_vm_ip` (and `gpu_vm_ssh_key_path`, if the default `--ssh-key`/an SSH
   agent doesn't reach this host; once set it takes precedence over
   `--ssh-key`) has to already be in the vaulted tfvars —
   add it with `talops secrets edit` if it isn't. `vllm_host_unset` only
   fires when **neither** `--host` **nor** `gpu_vm_ip` resolves a host
   (`app/vllm.go:74-83`), so passing `--host` on every invocation is a
   substitute for the tfvars entry, not just a fallback — but without one
   or the other, the first `talops vllm apply` fails immediately with
   `vllm_host_unset`, before touching anything.
3. **What the migration itself does.** `EnsureHost` installs and holds the
   NVIDIA Container Toolkit at its pinned release (downgrading a toolkit
   the host already has at a newer one — see [Host
   prerequisites](#host-prerequisites)), installs `podman` and
   `prometheus-node-exporter` if either is missing, and
   generates both CDI specs for it. `Stage` pulls the pinned image and
   downloads the model, and the pre-swap GPU check runs that image once
   with the GPU; a host whose CDI spec podman cannot read stops there with
   `cdi_unresolvable`, the legacy server still serving. The swap then
   disables and stops the legacy `vllm.service` and starts the new
   `vllm-server.service` Quadlet unit — before the health check runs,
   exactly like every later apply. If the health check fails, rollback
   restores `vllm.service` to whatever
   active/enabled state it had going in (see [Rollback](#rollback)),
   **not** to a Quadlet unit, since none existed yet.
4. **Window procedure.** Same as [Changing the model or
   version](#changing-the-model-or-version) — announce, check LiteLLM cloud
   quota, `talops vllm plan` (it will show `legacy vllm.service is active or
   enabled and is replaced by the Quadlet unit` among its reasons), then
   `talops vllm apply --confirm`.
5. **Legacy cleanup, after a clean week.** `talops` never removes the
   legacy unit or its venv itself, and nothing in it ever re-enables or
   restarts `vllm.service` once the first apply's swap has succeeded: the
   forward `Activate` path disables and stops it (if present) during that
   same swap, and every later `apply`/`status` only *reads* its state
   (`LegacyPresent`/`ReadLegacy`) to report it, never writes to it. So
   removal is safe as soon as that first swap has succeeded — a clean week
   of stable serving afterward is about operator confidence, not a
   technical requirement. Remove it by hand, on the VM:

   ```bash
   sudo systemctl disable --now vllm.service   # idempotent if already stopped/disabled
   sudo rm -f /etc/systemd/system/vllm.service
   sudo rm -rf /etc/systemd/system/vllm.service.d
   sudo systemctl daemon-reload
   sudo rm -rf /home/vllm/vllm-env             # the old uv venv
   ```

   The old venv's Hugging Face cache (`/home/vllm/.cache/huggingface`) is
   worth cleaning up separately — it's tens of GB, entirely redundant with
   the Quadlet's own cache under `/var/lib/vllm/hf`, and `talops` never
   touches it either.

## Rebuild

Recovering from a full VM loss (or a deliberate Terraform `-replace`) is
three steps, not one. The two Terraform mechanisms below are separate and
easy to conflate — one governs whether a config change reaches the running
VM at all, the other governs whether the GPU-host cloud-init snippet is ever
used:

1. **Terraform** recreates the VM shell.
   - `reboot_after_update = false` on `gpu_inference` means a routine change
     to `cpu`/`memory`/`hostpci`/`vga`/`machine`/`initialization` lands as a
     **pending PVE config change plus a warning** on `terraform apply`,
     rather than the provider rebooting the VM immediately to apply it. A
     clean `Apply complete!` that's followed by a reboot-required warning is
     still a pending change, not a no-op — nothing on the running VM moved
     until an operator reboots it.
   - `lifecycle { ignore_changes = [initialization[0].user_data_file_id] }`
     is the separate mechanism that keeps the cloud-init snippet itself from
     ever reaching this already-running guest. A later edit to the
     cloud-init template **does** plan a change — the snippet file resource
     (`proxmox_virtual_environment_file.gpu_cloud_init`) is itself
     `ForceNew`, so editing it plans a replacement of that resource. What
     `ignore_changes` spares is only the VM: because the VM's
     `user_data_file_id` is ignored, `plan`/`apply` never updates the VM
     resource to point at the new snippet, so the already-running guest
     never picks up the edit — only the file resource in Terraform state
     changes, until this VM is actually replaced (`terraform apply
     -replace=...`), at which point it's built fresh from whatever the
     snippet then contains. See the comments in `terraform/gpu-node.tf` for
     the mechanism in full.
2. **Driver install** follows
   [scenarios/ai-sre-agent-runbook.md](../scenarios/ai-sre-agent-runbook.md)
   §3 — the GPU driver is host-level, not something Terraform or talops
   manages. The NVIDIA Container Toolkit apt repository (above, in [Host
   prerequisites](#host-prerequisites)) is a separate manual step the
   runbook doesn't cover.
3. **`talops vllm apply --confirm`** does the rest: `EnsureHost` installs
   `podman`/`prometheus-node-exporter` and the pinned, held NVIDIA Container
   Toolkit and generates the CDI specs, `Stage` re-pulls the image and
   re-downloads the model (a fresh disk has no staged marker), and the
   usual converge/health-check/record sequence brings the host to what
   `serving.yaml` defines.

## Weekly model audit

`talops vllm audit` lists newly released models that could replace the one
`serving.yaml` serves, and files the list to Jira.
`.github/workflows/model-audit.yml` runs it every Monday at 06:00 UTC.

### What it does and does not do

It does:

- read `serving.yaml` for the incumbent repo, revision, vLLM tag,
  `--tool-call-parser`, `--max-model-len` and `--gpu-memory-utilization`,
  plus the flags the fit estimate and trial steps depend on (see
  [Known limits](#known-limits));
- read vLLM's tool-parser registry (`vllm/tool_parsers/__init__.py` at
  `serving.yaml`'s tag) from GitHub. The run fails if that file is
  unreadable, has fewer than 10 parsers, or lacks the incumbent's parser;
- read vLLM's model registry (`vllm/model_executor/models/registry.py` at
  the same tag) from GitHub. Every table `_VLLM_MODELS` spreads is read, and
  each key indented four spaces is an architecture. The run fails if that
  file is unreadable, a table is missing or unclosed, it yields fewer than
  100 architectures, or it lacks `LlamaForCausalLM`;
- list the models each allow-listed org created in the last `windowDays`,
  plus the top `trendingN` trending `text-generation` repos created in the
  last `trendingWindowDays`. Trending repos are flagged **unvetted** and rank
  after allow-listed ones;
- reject, with a named reason, anything that fails any of these checks:
  - `pipeline_tag` must be `text-generation` or `image-text-to-text`;
  - the licence must be allowed;
  - a gated repo needs `HF_TOKEN`;
  - the repo must not be the incumbent or already on a `model-audit`
    ticket. The last 26 tickets are read, descriptions and comments both.
    This check runs before enrichment, so it spends no requests;
  - it must be instruction-tuned;
  - a parser rule must match, and that parser must exist in the registry;
  - one of `config.json`'s `architectures` must be in the model registry,
    else it is rejected as `architecture <first listed> not in vLLM <tag>`,
    or `architecture missing` when the list is absent or empty. vLLM's
    fallback to the Transformers backend is not counted;
  - it must fit the GPU;
- file one Jira ticket per ISO week, labelled `model-audit` and
  `model-audit-<YYYY-Www>`, under `jira.parent`:
  - a same-week re-run comments only the candidates not already on that
    ticket;
  - a week with no candidates adds one no-change comment to the newest
    `model-audit` ticket. It skips the comment when that ticket is this
    week's own, or when the audit's account already commented this week.

Each candidate carries trial steps: an args delta against `serving.yaml`,
written as a starting point. `servedName` and `servedAliases` are never
changed in them, because consumers request the model by those names; when
there are aliases, the steps say to keep them. A candidate whose architecture is
a multimodal `*ForConditionalGeneration` wrapper also gets
`add --language-model-only`, unless `serving.yaml` already sets it. That
flag skips building the vision tower and profiling its encoder at startup,
neither of which the memory estimate prices.

Each candidate also carries `marginGiB`, the budget minus the estimated
total: how far the one-constant `overheadGiB` can be wrong before the model
stops fitting.

It does not:

- contact the GPU host, decrypt the vault, or edit `serving.yaml`;
- open a PR or trial a model. A human decides whether to trial one, then
  follows [Changing the model or version](#changing-the-model-or-version);
- judge model quality. A small model that fits and has a parser rule is
  reported like any other;
- report a new revision of a repo already reported.

Every read failure either shows in the report or stops the run. A failed org
or trending read, or an org cut short by the budget, is a `skipped` row; a
candidate the budget left unread is rejected as `not enriched: request
budget spent`. A run stops with exit 1 and files nothing when:

- either registry, `serving.yaml` or `audit.yaml` cannot be used
  (`registry_*` for the tool-parser registry, `model_registry_*` for the
  model registry);
- every allow-listed org query fails, or the discovery budget runs out
  before any org is read;
- the Jira history search fails, because filing without dedupe would repeat
  candidates;
- the run is interrupted (`SIGINT`/`SIGTERM`) during discovery,
  enrichment or filing's Jira reads, with code `cancelled`. An interrupt
  during a registry read or the Jira history search is reported as
  `registry_unreadable`, `model_registry_unreadable` or `jira_read_failed`
  instead.

A failed Jira create or comment also exits 1, as `jira_write_failed`.

Run it locally with `talops vllm audit --dry-run`. A dry run performs every
read and no write. When the `JIRA_*` variables are set, that includes Jira's
reads, and the report's `jira.action` says what would happen (`would-create`,
`would-comment` or `none`). Without them, dedupe is reported as skipped.
Without `--dry-run`, a missing `JIRA_*` variable exits 1 before any request.
`--json` prints the report as one JSON object.

### `inference/vllm/audit.yaml`

It is decoded strictly: an unknown key or an out-of-range value fails the run
as `config_unreadable`. `contextTokens` and the GPU memory utilization are
not here; they come from `serving.yaml`, so the two files cannot disagree.

| Field | Meaning | Allowed |
|---|---|---|
| `windowDays` | how far back an allow-listed org's new repos are read, by creation date. Set to 14, deliberately twice the weekly cadence, so a late cron start or one failed week loses nothing; repos already reported are dropped by dedupe before enrichment, so the cost is roughly double the enrichment requests | 1–90 |
| `trendingWindowDays` | how old a trending repo may be and still be reported | 1–365 |
| `trendingN` | how many trending repos are read (one page) | 1–100 |
| `maxCandidates` | candidates reported; the rest are rejected as `over maxCandidates (N)` | 1–50 |
| `maxRequests` | hard cap on Hub and GitHub attempts, retries included; Jira is not counted | 1–500 (the Hub's anonymous limit is 500 per 5 minutes per IP) |
| `discoveryRequests` | the share of `maxRequests` the two registry reads and the listings may spend; enrichment gets the rest | 1 to `maxRequests`−1 |
| `gpuMemMiB` | the card's total memory. The budget is `gpuMemMiB / 1024 × --gpu-memory-utilization` | 1024–1048576 |
| `overheadGiB` | constant added to weights and KV cache in the fit estimate | 0–64 |
| `orgs` | the allow-listed Hub organisations | at least one, no duplicates |
| `licenses.allow` | accepted licence ids (`cardData.license`, else a `license:` tag) | |
| `licenses.allowNames` | accepted `license_name` values when the licence is `other` | |
| `parsers` | ordered rules, first match wins: `modelType`, optional `nameRegex` on the repo id, `parser`, optional `extraArgs` | at least one rule; `nameRegex` must compile |
| `jira.project`, `jira.parent`, `jira.issueType` | where the weekly ticket is created; `parent` must be an issue in `project` | |

A parser rule matches `config.json`'s `model_type`, or `text_config.model_type`
for multimodal wrappers. **A rule is a claim, not a verified fact.** It says
the family's chat format matches that vLLM parser. The audit only checks that
the parser exists in the registry. A live tool call during the model trial is
what proves the claim. `extraArgs` go into the trial steps word for word, so
the Llama 3 rule's `--chat-template=<model's tool chat template>` is a
reminder for the person running the trial, not a real path.

`model_type` alone does not identify a chat format. MiniCPM5, for example,
ships as `llama` but needs the `minicpm5` parser, and Hermes 3 is `llama` but
needs `hermes`. So the `llama` rule only claims Llama 3.1 and 3.2 repo names,
and any rule gated by `nameRegex` must come before a broader rule for the same
`model_type`. A repo that no rule claims is reported as `no parser rule`,
which is safer than trialling it with the wrong parser.

A mapped family can still be rejected as `fit unknown` when the fit
estimate cannot price its layers. See [Known limits](#known-limits).

A family on the allow-list with no rule shows up every week as `no parser
rule for <model_type>`. To add one, add a rule in a PR. A config test checks
that the committed rules still resolve the incumbent to `serving.yaml`'s
`--tool-call-parser`. `serving.yaml` does not carry a `model_type`, so a PR
that swaps the model also records the new repo's in that test's
`servedModelTypes` (`internal/modelaudit/config_test.go`).

### Secrets

The workflow reads four repository secrets:

- `HF_TOKEN`: optional. It is sent only to the Hub, never to GitHub. Without
  it, gated repos are rejected as `gated (...): licence acceptance and
  HF_TOKEN required`. A gated repo is only readable if the token's account
  has accepted that repo's licence on the Hub.
- `JIRA_BASE_URL`, `JIRA_EMAIL`, `JIRA_API_TOKEN`: required for a real run.
  The account needs to be able to search, create and comment in
  `jira.project`.

The owner sets them in their own terminal, never through an agent session,
so no token reaches a transcript. `gh secret set` prompts for the value, so
nothing lands in shell history:

```bash
gh secret set HF_TOKEN
gh secret set JIRA_BASE_URL
gh secret set JIRA_EMAIL
gh secret set JIRA_API_TOKEN
```

### Rollout

A scheduled run is never a dry run, and it exits 1 if the `JIRA_*` secrets
are missing. Do these steps in order:

1. Merge the workflow.
2. Set the four secrets.
3. Run the workflow manually with `dry_run=true` (the dispatch default):
   `gh workflow run model-audit.yml -f dry_run=true`. Download the artifact
   and read it. `report.json` is the full report, and `rejected.tsv` has one
   line per rejected repo with its reason.
4. Run it once with `dry_run=false` to file the first report:
   `gh workflow run model-audit.yml -f dry_run=false`.
5. From then on the Monday cron runs unattended.

To fetch an artifact:

1. `gh run list --workflow model-audit.yml --json databaseId,status,conclusion`
2. `gh run download <databaseId>`

Artifacts are kept for 14 days.

### Keeping it running

**GitHub disables scheduled workflows after 60 days without repository
activity.** When that happens, the weekly Jira ticket or comment simply stops
arriving. Nothing fails. Re-enable the workflow from its page under the
Actions tab, or with `gh workflow enable model-audit.yml`, then dispatch one
run to confirm.

**Artifacts in this public repository are public.** Anyone can download the
report and rejection table while they are retained. They hold public Hub
metadata. The exception is a failed run's `error.msg`: a Jira connection
error that never reached an HTTP status carries the full request URL, and
that URL includes the `JIRA_BASE_URL` host.

### Known limits

- **`fit unknown`**: this is a rejection, never an assumed fit. The estimate
  is `weights + KV cache and state + overheadGiB`, for one sequence of
  `--max-model-len` tokens, priced per layer the way vLLM sizes it at
  `serving.yaml`'s tag:
  - a `full_attention` layer holds K and V for every token;
  - a Qwen3.5 or Qwen3-Next `linear_attention` (Gated DeltaNet) layer holds
    one fixed-size state, whatever the context: a conv state and a recurrent
    state. The recurrent state is float32 when a Qwen3.5 multimodal wrapper's
    `mamba_ssm_dtype` says so;
  - a MiMo-V2 sliding layer (`hybrid_layer_pattern` entry `1`) holds
    `sliding_window_size − 1 + max_num_batched_tokens` tokens, capped at
    `--max-model-len`, not just the window, because a chunked-prefill step
    keeps the previous window beside the new chunk.
    `max_num_batched_tokens` is `serving.yaml`'s
    `--max-num-batched-tokens`, else vLLM's default for the card: 2048 below
    70 GiB, 8192 at or above it.

  It cannot price these, so it rejects them as `fit unknown`:
  - MLA (`kv_lora_rank` set, as in the DeepSeek families);
  - any other sliding-window attention in use (`sliding_window` set and
    `use_sliding_window` not `false`), such as gpt-oss or Gemma 3;
  - any other `layer_types` entry, such as Granite 4-H's `mamba`, or
    `linear_attention` outside Qwen3.5 and Qwen3-Next;
  - a `layer_types` or `hybrid_layer_pattern` whose length is not
    `num_hidden_layers`, or a pattern entry other than `0` or `1`;
  - a model with linear-attention or sliding layers when `serving.yaml` sets
    a flag those formulas assume is at its default: `--enable-prefix-caching`,
    `--no-enable-chunked-prefill`, `--mamba-cache-dtype` or
    `--mamba-ssm-cache-dtype` other than `auto`, or `--speculative-config`.
    The reason names the flag. Full-attention models are priced the same
    either way;
  - a config missing a field the formula needs. A Qwen3.5 or Qwen3-Next
    config without `layer_types` counts as missing, although vLLM would fill
    in a default from `full_attention_interval`;
  - weights that are not root-level `*.safetensors`.

  Qwen3's `sliding_window: null` with `use_sliding_window: false` is
  estimated normally.
- **The estimate is approximate.**
  - The KV cache and conv state are priced at 2 bytes per element, so an
    FP8 KV cache is over-counted.
  - Block rounding and hybrid group padding are left out. They are small
    (0.01 GiB for Qwen3.8), but can exceed one block per layer when a
    model's layer counts per type are not multiples of each other.
  - `overheadGiB` is one constant for every model, calibrated on the
    text-only `QuantTrio/Qwen3-Coder-30B-A3B-Instruct-AWQ`. A multimodal
    model adds startup memory it does not cover: encoder profiling, sampler
    warm-up over a larger vocabulary, and CUDA graphs. A `marginGiB` below
    about 2 GiB on a multimodal model is tight; trial it with
    `--language-model-only` and watch the startup log.
  - The estimate prices one sequence, but vLLM v0.24.0 serves up to 256
    concurrently by default (`--max-num-seqs`) and at startup captures CUDA
    graphs up to batch `min(2 × max_num_seqs, 512)`, which the estimate does
    not price. On 2026-10-02 `nvidia/Qwen3.8-27B-NVFP4`, a hybrid
    linear-attention model, loaded 19.08 GiB of weights and then ran the
    31.4 GiB card out of memory in CUDA-graph profiling, even with
    `--language-model-only`. With `--max-num-seqs=8`, enough for these
    consumers, it started with 28.4 GiB in use. Trial a hybrid candidate
    with `--max-num-seqs` sized to the consumers' concurrency.
  - The text-only model calibrates at 15.66 GiB weights + 3.00 GiB KV cache +
    3 GiB overhead = 21.66 GiB, against a 28.66 GiB budget.
    `RedHatAI/Qwen3.8-27B-NVFP4` calibrates at 22.20 GiB weights + 2.14 GiB
    (2.00 GiB for its 16 full-attention layers, 0.14 GiB of state for its 48
    linear-attention layers) + 3 GiB = 27.35 GiB.
  - Tune `overheadGiB` against observed usage.
- **Repos without `config.json`**, such as GGUF-only and adapter repos, are
  rejected as `enrich failed: 404 (no config.json)`. Each one still costs
  enrichment requests.
