# GEMINI.md

This file provides guidance to Gemini CLI when working in this repository.
For the canonical reference, see [CLAUDE.md](CLAUDE.md) — this file mirrors that content.

## Repository Overview

Infrastructure-as-code for jdwlabs cluster provisioning. Terraform + Talos Linux configurations for bare-metal Kubernetes clusters.

### Structure

- `terraform/` — Terraform modules and root config
- `clusters/` — Per-cluster node and network definitions
- `bootstrap/` — `talops` CLI: full cluster lifecycle (bootstrap, reconcile, infra deploy/destroy, up/down)
- `scenarios/` — Operational runbooks

## Development Commands

```bash
terraform init                    # Initialize working directory (needs remote-state creds — see docs/secrets.md)
terraform validate                # Validate HCL
terraform plan -out=tfplan        # Preview changes (never auto-apply)
terraform show tfplan             # Review plan
kubectl get nodes                 # Node status (read-only)
kubectl get pods -A               # Pod status (read-only)
```

## Agent Contract

- `terraform apply` and `terraform destroy` are NEVER run autonomously
- `kubectl apply` / `kubectl delete` are out of scope — use ArgoCD
- Read-only `kubectl get/describe/logs` are safe
- Never modify state files directly
- Produce plans and stop — human applies
- Never push to remote — stage and commit only
