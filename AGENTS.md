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

## Constraints

- `terraform apply` is NEVER run autonomously — produce a plan, stop, and await human approval
- `terraform destroy` is NEVER run autonomously under any circumstances
- `kubectl apply` and `kubectl delete` are out of scope — cluster workload management belongs to ArgoCD (via the `deployments` repo)
- Read-only `kubectl get`, `kubectl describe`, `kubectl logs` are safe for investigation
- Never modify `.tfstate` files directly — they are managed by the Terraform backend
- Never commit decrypted plaintext secrets; only the encrypted `*.enc.yaml` vault is tracked
- Never push to remote — stage and commit only

## References

- Talos Linux docs: https://www.talos.dev/latest/
- Terraform docs: https://developer.hashicorp.com/terraform/docs
