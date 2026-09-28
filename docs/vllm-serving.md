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
day to day: what serves, how to change it, and how to read drift.

## What serves and who depends on it

The single source of truth for what the GPU host runs is
[`inference/vllm/serving.yaml`](../inference/vllm/serving.yaml), committed to
this repo. `talops vllm apply --confirm` is the only thing that acts on it —
merging an edit to that file changes nothing by itself, exactly as the
comment at the top of the file says.

Two consumers reach the server directly, and both lose it for the duration of
an `apply`'s restart and health check:

- `@server` answers (`jdw-deployments` `minecraft-fwb` `agent.llm`)
- LiteLLM's `sre-investigator-local` route (`platform` `litellm`)

`talops vllm plan` names these same two consumers whenever it reports a
change, so a plan doubles as the announcement list for a change window.

## `serving.yaml` fields and validation

```yaml
image: docker.io/vllm/vllm-openai:v0.24.0@sha256:251eba5cc7c12fed0b75da22a9240e582b1c9e39f6fbc064f86781b963bd814f
model:
  repo: QuantTrio/Qwen3-Coder-30B-A3B-Instruct-AWQ
  revision: c58857a7f41c0920f73d1b56678640f9c02017d7
servedName: qwen/qwen3-coder-30b-a3b
port: 8000
args:
  - --quantization=awq_marlin
  - --max-model-len=32768
  - --enable-auto-tool-choice
  - --tool-call-parser=qwen3_xml
healthGate:
  timeout: 10m
```

| Field | Required | Rule | Why |
|---|---|---|---|
| `image` | yes | `<repo>:<tag>@sha256:<64 hex>`, lowercase OCI reference charset | The digest pins what runs; the tag is what Renovate reads. |
| `model.repo` | yes | `[A-Za-z0-9._:/=,+@-]+`, not starting with `-` | Ends up as a literal word on the Quadlet's `Exec=` line — a systemd command line that splits on whitespace and quotes and expands `%` and `$` — and as `vllm serve`'s first argument, where a leading `-` would make it a flag. |
| `model.revision` | yes | 40 hex characters | A branch moves; a commit pins what's downloaded. |
| `servedName` | yes | non-empty, same charset as `model.repo` | Identifies the model in client requests, and lands unescaped in a Prometheus label the drift check emits. |
| `port` | no (default `8000`) | 1–65535 | |
| `args` | no | each element matches the same charset; JSON-valued vLLM args (anything needing a literal `{`/`}`/space) are unsupported for this reason | Every element is a literal `Exec=` word. |
| `healthGate.timeout` | no (default `10m`) | duration, must be `> 0` | Model load on this card is minutes, not seconds — the AWQ 30B-A3B MoE weights alone take a while to page onto the GPU. |

Unknown keys are rejected outright (`KnownFields(true)`), and `args` may not
set `--model`, `--revision`, `--port`, `--served-model-name`, or `--host` —
talops's own rendered `Exec=` line already sets all five (`ExecArgs` in
`internal/vllm/render.go`): `model.repo` positionally, as `vllm serve`'s
first argument, and the rest as flags. A duplicate in `args` would either
conflict or silently lose to argument order — a second `--revision` could
load a revision other than the one `model.revision` pins and the applied
record reports.

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
out-of-repo spec has no commit to record.

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
     two consumers above lose the server for the length of the restart and
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

Staging (the image pull and, if the model revision isn't already cached, the
model download) happens before anything is touched on the running server, so
a slow pull or a multi-gigabyte download never causes an outage by itself.
The outage is the restart itself: **the previous server is stopped and the
new one started before the health check ever runs** — the check decides
whether to keep the new server or roll back to the old one, not whether to
start the new one in the first place. `phases` times the restart and the
check (and the rollback's own re-check, `gate-rollback`, when one happens)
separately from staging.

The health check has three parts, all against the served name: `/v1/models`
lists it with `root` equal to `model.repo`; a 1-token chat completion
finishes with `stop` or `length`; and a request offering one `get_time`
tool comes back with at least one parsed `tool_calls` entry. That last
request sends `tool_choice: "auto"`, the way consumers do, because only
`auto` passes the model's reply through `--tool-call-parser`;
`"required"` would have vLLM constrain the output to the tool schema itself
and pass with a parser that parses nothing. With `auto` the model is free to
answer in prose, so the request's system and user messages tell it to call
`get_time`, and after a swap the check is retried until
`healthGate.timeout`. The single check on an apply with nothing to change
is not retried, so a model that answers in prose once reads as
`gate_failed` there; re-running `apply` checks again.

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
| `ssh_auth_unconfigured` | No usable SSH key or agent for the GPU host | Set `gpu_vm_ssh_key_path` in the vaulted tfvars, pass `--ssh-key`, or run an SSH agent. |
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
| `host_prereq_failed` | `EnsureHost` failed. The **running server is unchanged**, but `EnsureHost` may already have installed packages, created directories, or regenerated the CDI spec before hitting the failure (`remote.go:64-114`) — those are host-state side effects, not a swap. | `previous` | `false` | Commonly the NVIDIA Container Toolkit apt repo is missing — see [Host prerequisites](#host-prerequisites). Fix and re-run; re-running is safe, `EnsureHost` is idempotent. |
| `stage_failed` | The image pull or model download failed, or the apply was interrupted (Ctrl-C, SIGTERM) before the swap — the message then starts `cancelled before the swap`. **The running server is unchanged** — `Stage` never touches it, and an interrupt during host preparation or staging stops before the swap even if the step in flight finished. | `previous` | `false` | For an interrupt, just re-run. Otherwise usually disk space: `Stage` needs 30 GiB free on `/var/lib/vllm` when the model revision isn't already staged, and 12 GiB free on `/var/lib/containers` every run (`internal/vllm/remote.go`). Free space, re-run. |
| `legacy_state_unknown` | Reading `vllm.service`'s active/enabled state failed — either an SSH/run error that came back with empty output, or a `systemctl` state talops doesn't recognise (`remote.go:407-412`). Can happen before staging (`changed` still `false`, via `pendingReasons`) or again right after staging succeeds on **any** apply with something to change (`changed: true`, `converge.go:263`) — not only a host's first apply; every changed apply re-reads legacy state to build its `Activate` hook. | `previous` | `false` | Investigate the legacy unit's state by hand, then re-run. |
| `read_failed` | Couldn't read the installed Quadlet unit, the drift-check script/service/timer files, or `/etc/vllm/applied.json` while comparing against `serving.yaml` (`converge.go:160-178`) | `previous` | `false` | Check SSH/host access, re-run. |
| `config_invalid` | No health gate configured — a talops misconfiguration, not a host problem | `previous` | `false` | File an issue; this isn't something re-running fixes. |
| `converge_failed` (write path) | Writing, backing up, moving, or `chmod`-ing one of the files to install failed, **before `Activate` ever ran** (`hostconverge/converge.go:79-145`, `abortInstall`) — `hostconverge` cleans up its own partial temp/installed files inline, but never calls `Activate`/`Verify` again, so this path is **not** a rollback. | `previous` | `false` | Nothing was activated; the running server was never touched. Check the failure reason (disk, permissions, SSH), re-run. |
| `converge_failed` (activate path) | The unit swap's `Activate` step itself failed (a `systemctl` command errored) — this **does** trigger `hostconverge`'s rollback (`hostconverge/converge.go:161-166`), whose outcome `vllm.Apply` then maps to this code (`vllm/converge.go:314-320`) | `previous` if rollback succeeded, `none` if rollback also failed | `true` | If `previous`: recovered automatically, nothing to do. If `none`: read the report's `help` — see [Rollback](#rollback). |
| `gate_failed`, `changed: false` | Nothing needed to change, but the health check against the already-matching host failed | `none` | `false` | Talops restarted nothing — the **running** server itself is unhealthy. Go to [Troubleshooting](#troubleshooting). |
| `gate_failed`, `changed: true` | The new server didn't pass the health check after the swap | `previous` if rollback succeeded, `none` if rollback also failed | `true` | Same as `converge_failed` (activate path)'s rolled-back cases, above. |
| `record_failed` | The new server is serving and passed the health check, but `/etc/vllm/applied.json` couldn't be written | `new` | `false` | **The server is serving correctly.** `status` and the drift check will report drift until you re-run `apply --confirm` to rewrite the record. |

**Exit codes**: `0` on success, `2` only for `confirm_required`, `1` for
every other row in both tables above — including a fully successful, fully
re-verified rollback. A rolled-back apply exits `1` because the *attempted*
change didn't take, even though the host ends up healthy.

## Status and drift

`talops vllm status` is a three-way, read-only comparison: the local
`serving.yaml` **on disk** (labeled `git` in its own output, but it's read
straight off your working tree with no `git show` involved — an unpulled
merge is invisible to `status` exactly the same way it's invisible to
`plan`, see [step 2 above](#changing-the-model-or-version)), the record
`apply` last wrote to the host (`/etc/vllm/applied.json`), and the running
container's own live state, read directly (`ReadLive`, `converge.go`) plus
a live `/v1/models` query for the served name (`converge.go`) — it
genuinely inspects the container, not just files. It never runs the host
phase (`EnsureHost`) — only `apply` does — so `status` alone never installs
packages or regenerates the CDI spec.

```
vllm:
  host: 192.168.1.50
  drift: false
  legacy: false
fields[4]{name,git,applied,live}:
  imageDigest,sha256:251eba...,sha256:251eba...,sha256:251eba...
  modelRevision,c58857a...,c58857a...,n/a
  servedName,qwen/qwen3-coder-30b-a3b,qwen/qwen3-coder-30b-a3b,qwen/qwen3-coder-30b-a3b
  argsHash,3f2a...,3f2a...,3f2a...
```

A read that fails reports a sentinel rather than a clean result, so a
partial failure never reads as "everything matches" — but the sentinels
aren't uniform across columns. `unknown` appears only in the `applied`
column (`ReadApplied` itself failing to read or parse
`/etc/vllm/applied.json`), or in `servedName`'s `live` column in the rare
case status runs with no models reader configured at all. A live-container
read failure of any kind — SSH broken, `podman` broken, the container
simply not running — collapses to `(not running)` instead (`ReadLive`
treats every such failure as "not running," never as "unknown";
`remote.go:272-275`). `(none)` means nothing has ever been applied in the
`applied` column; in `servedName`'s `live` column it instead means
`/v1/models` answered with an empty list — reachable, but serving nothing
(`converge.go`, `liveServedName`). `unreachable` is specific to
`servedName`'s `live` column: the `/v1/models` query itself failed.
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
| `VllmServingDrift` | `vllm_serving_drift{reason=...}`, written by the host's own drift-check script comparing the container against `applied.json` | **`talops vllm status`** — the alert's own description says so. For `reason=not_running`/`image`/`args`, go to [Troubleshooting](#troubleshooting) for host-side logs; for a stale-record mismatch, `talops vllm plan` then `talops vllm apply --confirm` converges it. |
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
  `servedName` and `model.repo` in the `applied.json` that was on the host
  when the apply started — not the one in the `serving.yaml` being applied.
  A change that renames the served model or swaps the model repo would
  otherwise fail the restored server's check even though it is healthy.
  Only when there is no record does the rollback check use `serving.yaml`'s
  identity.
- **The first apply on a host** has no previous Quadlet unit to restore, so
  rollback instead restores `vllm.service` to the exact active/enabled state
  it had before the apply started — the vLLM `Activate` hook (`activate` in
  `internal/vllm/converge.go`) detects the Quadlet is now absent and calls
  `retireNewServer`, which stops the new server and puts `vllm.service` back
  the way it found it. No record exists on a first apply, so the restored
  `vllm.service` is checked against `serving.yaml`'s `servedName` and
  `model.repo`; a first apply that also changes either of them rolls back to
  a server that fails that check and reports `serving: none` although the
  legacy server is running. If nothing was serving before (no legacy unit, or one
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
  `nvidia-container-toolkit` package itself, but only once its apt candidate
  exists. The runbook doesn't cover this step; add the repository once, by
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
  back at this section rather than a bare `apt-get` error.

**CDI regeneration** needs no manual step, but only happens as part of
`apply`'s host phase — `status` never runs it. `EnsureHost` records the
driver version it last generated `/etc/cdi/nvidia.yaml` for
(`/etc/vllm/cdi-driver-version`), and regenerates the CDI spec
(`nvidia-ctk cdi generate`) whenever the installed driver's version differs.
A driver upgrade therefore takes effect the next time `talops vllm apply`
runs — and even then, only the CDI spec is regenerated by a no-op apply
(`changed: false`); the running container is **not** restarted, so it keeps
its old device injection until its own next restart (the following apply
that actually changes something, or a manual restart).

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
   agent doesn't reach this host) has to already be in the vaulted tfvars —
   add it with `talops secrets edit` if it isn't. `vllm_host_unset` only
   fires when **neither** `--host` **nor** `gpu_vm_ip` resolves a host
   (`app/vllm.go:74-83`), so passing `--host` on every invocation is a
   substitute for the tfvars entry, not just a fallback — but without one
   or the other, the first `talops vllm apply` fails immediately with
   `vllm_host_unset`, before touching anything.
3. **What the migration itself does.** `EnsureHost` installs `podman`,
   `nvidia-container-toolkit`, and `prometheus-node-exporter`, and generates
   the CDI spec for the first time. `Stage` pulls the pinned image and
   downloads the model. The swap then disables and stops the legacy
   `vllm.service` and starts the new `vllm-server.service` Quadlet unit —
   before the health check runs, exactly like every later apply. If the
   health check fails, rollback restores `vllm.service` to whatever
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
   `podman`/`nvidia-container-toolkit`/`prometheus-node-exporter` and
   generates the CDI spec, `Stage` re-pulls the image and re-downloads the
   model (a fresh disk has no staged marker), and the usual converge/health-check/
   record sequence brings the host to what `serving.yaml` defines.
