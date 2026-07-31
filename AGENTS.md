# AGENTS.md

Canonical context for AI agents (Claude Code, OpenAI Codex, Gemini CLI, GitHub Copilot, and others) working in this repository. `CLAUDE.md` and `GEMINI.md` are thin pointers to this file — make edits here.

## What This Repo Is

jdwlabs `infrastructure` defines the physical and virtual infrastructure for jdwlabs clusters using Terraform and Talos Linux. It provisions nodes, networking, and storage — but does NOT manage what runs on the cluster (that is the `platform` and `deployments` repos).

## Key Concepts

- **Talos Linux:** Immutable, API-driven OS for Kubernetes nodes. No SSH — all management via the Talos API (`talosctl`)
- **Terraform:** Declarative infrastructure. Changes require `terraform plan` review before `terraform apply`
- **State:** Terraform state is stored remotely (S3-compatible MinIO; credentials vaulted in `terraform/backend-credentials.enc.yaml` — see `docs/secrets.md`) — never edit `.tfstate` files directly
- **Separation of concerns:** This repo provisions the cluster; `platform` configures what runs on it

## Repository Structure

- `terraform/` — flat Terraform config (providers, variables, control/worker node definitions)
- `bootstrap/` — `talops` CLI: full cluster lifecycle (bootstrap, reconcile, status, reset, infra deploy/destroy, up/down, secrets)
- `clusters/<name>/` — per-cluster runtime state created by `talops`. Plaintext working files (`secrets/`, `nodes/`, `state/`) are gitignored; the SOPS+age encrypted vault is the shared source of truth (see `docs/secrets.md`)
- `scenarios/` — step-by-step runbooks for operational tasks, plus scaling-test fixtures
- `docs/` — architecture and operations documentation

## Development Commands

### Terraform

```bash
terraform init                    # Initialize working directory (needs remote-state creds — see docs/secrets.md)
terraform validate                # Validate HCL syntax and configuration
terraform plan -out=tfplan        # Preview changes — always review before applying
terraform show tfplan             # Human-readable plan review
```

### Cluster inspection (read-only — safe for agents)

```bash
kubectl get nodes                 # Node status
kubectl get pods -A               # All pod status across namespaces
kubectl describe node <name>      # Node details and conditions
kubectl logs <pod> -n <ns>        # Pod logs
```

## Common Tasks

### Add a new cluster

1. Create directory under `clusters/<cluster-name>/`
2. Define node configuration following an existing cluster as template
3. Run `terraform validate` and `terraform plan`
4. Review the plan output fully before handing off for human apply

### Troubleshooting a node

See `scenarios/` for step-by-step runbooks for common failure modes (node not joining, disk issues, network partition).

## Secrets

Sensitive artifacts (`terraform.tfvars`, the Talos secrets bundle, `talosconfig`, bootstrap
state) are stored as SOPS+age encrypted `*.enc.yaml` files committed to git — the shared,
versioned source of truth. Plaintext working copies are gitignored and regenerated on demand.
`talops` auto-hydrates before a command and auto-seals changed plaintext after (disable with
`TALOPS_NO_AUTOSEAL=1`). Manage with `talops secrets {status,hydrate,seal,lock,edit,add-device}`.
Terraform state lives in a remote S3 (MinIO) backend; its credentials are vaulted in
`terraform/backend-credentials.enc.yaml` and hydrated manually — `talops` does not manage
that file. See `docs/secrets.md`.

## Code & Manifest Comments

Never put a Jira ticket ID (`JDWLABS-*`) or PR/issue number in a comment in
any file here — Terraform HCL and Talos machine-config YAML included.
Traceability lives in the commit message and PR description; comments
should explain *why* the config is what it is so they stay meaningful
after the ticket closes.

## Tooling Traps

RTK's filtered output is **not** the tool's output — it summarises, truncates, and
prints its own status lines. Every `rtk` row below is that one root cause. Run
anything you intend to act on through `rtk proxy <cmd>` and read the raw result.

| Symptom | Cause | Fix |
|---|---|---|
| A node/object's owning controller is unclear from `kubectl get -o json` | `managedFields` (which manager set which field) is hidden by default | Add `--show-managed-fields` |
| `rtk go build -o <path>` prints `Go build: Success`, exits 1, and writes no binary | RTK's success line doesn't reflect the Go toolchain result; inside a git worktree the real error is `error obtaining VCS status: exit status 128`, which RTK suppresses. `bootstrap/` is a Go module (`talops`) and the `.worktrees/talops-*` trees are in active use, so this is the normal build path here — not an edge case | `rtk proxy go build ...` to see the real output; add `-buildvcs=false` when building `bootstrap/` from a worktree |
| `gh pr edit` fails on every PR in this org | `gh` resolves the org through a GraphQL **query** that requires the `read:org` scope, and the active `GITHUB_TOKEN` (`ghp_...`) lacks it — it fails before any mutation is attempted (`the 'login' field requires ... ['read:org']`) | `unset GITHUB_TOKEN` so `gh` falls back to the keyring `gho_` OAuth token, which already carries `read:org`. Fallback if that token is unavailable: `gh api -X PATCH repos/<owner>/<repo>/pulls/<n> --input payload.json` |
| `gh run watch <n>` errors or watches nothing | It takes the run's **databaseId**, not the run number shown in the UI or in a `gh run list` number column | Resolve it first — `gh run list --json databaseId,number,headBranch` — and pass the `databaseId` |
| `.status.containerStatuses[].image` disagrees with the pod spec — a bare `sha256:…` with no repo, or a digest that matches nothing you deployed | That field carries the **config** digest, reported under whichever reference resolved first; `.imageID` carries the repo plus the **manifest** digest. Sampled live: `.image` was `sha256:9700374b…` with no repo while `.imageID` was `docker.io/jdwlabs/ai-sre-relay@sha256:f42b749b…` — two different digests for one running container | Read `.imageID`, never `.status…image`, when verifying which image is running. If the repo names still disagree, compare config/layer digests rather than concluding the wrong image is deployed |
| `curl --cacert <ca>.pem https://host` reports HTTP 000 on Windows | HTTP 000 only means no HTTP response arrived; every transport-level failure reports it, so the status alone cannot tell them apart. Windows curl's Schannel backend **does** honour `--cacert` and fails loudly when the bundle is wrong | Read curl's **exit code**, not the HTTP status: 60 = certificate verification failed, 77 = CA bundle could not be loaded, 7 = connection refused. `-w '%{http_code}'` alone will mislead you |

## Verify Before You Start

Ticket evidence more than ~a week old (or gathered in a different investigation) is a hypothesis, not ground truth. Before acting on it:

- Re-confirm the ticket's premises against live state — don't build on a stale finding
- State the scope you searched before claiming something is absent ("checked all N nodes", "every cluster in `clusters/`") — one sample is not the whole set
- A disproved premise is a valuable result: record it on the ticket, don't quietly work around it

## Constraints

- `terraform apply` is NEVER run autonomously — produce a plan, stop, and await human approval
- `terraform destroy` is NEVER run autonomously under any circumstances
- `kubectl apply` and `kubectl delete` are out of scope — cluster workload management belongs to ArgoCD (via the `deployments` repo)
- Read-only `kubectl get`, `kubectl describe`, `kubectl logs` are safe for investigation
- Never modify `.tfstate` files directly — they are managed by the Terraform backend
- Never commit decrypted plaintext secrets; only the encrypted `*.enc.yaml` vault is tracked
- Never push to `main`, and never `git push --force` anywhere — a feature branch may be pushed so the change can go through a pull request, which is how every other repository here works and how changes actually reach `main`
- Pushing a branch is not applying it: nothing in this repository takes effect until a human runs the `terraform apply` or `talosctl apply-config` that the merged change describes

## References

- Talos Linux docs: https://www.talos.dev/latest/
- Terraform docs: https://developer.hashicorp.com/terraform/docs
