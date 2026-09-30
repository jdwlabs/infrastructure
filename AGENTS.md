# AGENTS.md

Instructions for AI agents in this repo. `CLAUDE.md` and `GEMINI.md` import this file; edit here.

## Scope

This repo provisions Proxmox VMs (Terraform) and runs the Talos cluster's lifecycle (`talops`, the Go CLI in `bootstrap/`). What runs *on* the cluster belongs to the `platform` and `deployments` repos.

- `talops` also converges two hosts outside the cluster through `internal/hostconverge`: the HAProxy load balancer (`talops haproxy`) and the vLLM GPU host (`talops vllm`).
- devbox2 and the dev VMs are neither Talos nodes nor converged by talops — they are cloud-init plus a runbook (`docs/devbox2-provisioning.md`, `docs/dev-vm-provisioning.md`).
- Talos nodes have no SSH; manage them through the Talos API (`talosctl`).
- `talops vllm audit` touches neither host nor vault but files Jira tickets for replacement models (a weekly workflow runs it); locally use `--dry-run`, which files nothing. Details: `docs/vllm-serving.md`, "Weekly model audit".

## Hard limits

Nothing here takes effect until a human runs the apply the merged change describes. Agents produce the plan and stop.

- NEVER run `terraform apply` or `terraform destroy`, or the `talops` commands that wrap them or change live hosts (`up`, `down`, `bootstrap`, `reset`, `prune-nodes`, `infra deploy|destroy`, `reconcile` without `--plan`, `upgrade-k8s --apply`, `haproxy apply`, `vllm apply`), or `talosctl apply-config`. Use the read-only forms: `terraform plan`, `talops infra plan`, `talops reconcile --plan`, `talops upgrade-k8s` (previews by default), `talops {haproxy,vllm} plan|status`.
- `kubectl apply`/`delete` are out of scope — workloads belong to ArgoCD via `deployments`. `kubectl get|describe|logs` are fine for investigation. (`.claude/settings.json` denies the mutating forms for Claude Code.)
- Never edit `.tfstate`; state lives in the remote MinIO backend.
- Never commit decrypted secrets — only the SOPS+age `*.enc.yaml` vault is tracked.
- Never `git push --force`. Pushing a feature branch for a PR is fine.

## Secrets and state

- `terraform.tfvars`, the Talos secrets bundle, `talosconfig` and bootstrap state are SOPS+age `*.enc.yaml` files in git — the shared source of truth. Plaintext copies under `clusters/<name>/` and `terraform/` are gitignored and regenerated.
- `talops` auto-hydrates before a command and auto-seals changed plaintext after (`TALOPS_NO_AUTOSEAL=1` disables it). Manage with `talops secrets {status,hydrate,seal,lock,edit,add-device}`.
- `terraform init`/`plan` need the MinIO backend credentials from `terraform/backend-credentials.enc.yaml`, hydrated manually — `talops` does not manage that file. Details: `docs/secrets.md`.

## Required checks

Branch rulesets live in `.github/rulesets/` and are applied by hand with `apply.sh` after merge. Read that script's header before renaming, merging or removing any CI job — the wrong order makes PRs permanently unmergeable.

## Building talops from a worktree

`go build` in `bootstrap/` can fail inside a worktree with `error obtaining VCS status`; add `-buildvcs=false`. Under RTK the failure is hidden behind a success line — see `docs/agent-tooling-traps.md` for this and other misleading tool output (`rtk`, `gh`, `kubectl`, `curl` on Windows).

## Evidence

Ticket evidence older than about a week, or from another investigation, is a hypothesis. Re-check it against live state, state the scope you searched before claiming something is absent ("all N nodes", not one sample), and record a disproved premise on the ticket instead of working around it.
