# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working in this repository.

## Repository Overview

Infrastructure-as-code for jdwlabs cluster provisioning. Contains Terraform modules, Talos Linux cluster configurations, and bootstrap tooling.

### Structure

- `terraform/` — Terraform modules and root configuration for cluster infrastructure
- `clusters/` — Per-cluster node definitions, network, and storage configuration
- `bootstrap/` — `talops` CLI: full cluster lifecycle (bootstrap, reconcile, status, reset, infra deploy/destroy, up/down)
- `scenarios/` — Step-by-step runbooks for common operational tasks
- `docs/` — Architecture and operations documentation

### Tech Stack

- **Provisioning:** Terraform
- **OS:** Talos Linux
- **Orchestration:** Kubernetes

## Development Commands

### Terraform

```bash
terraform init                    # Initialize working directory (run once per env)
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

## AI Agent Contract

- `terraform apply` is NEVER run autonomously — produce a plan, stop, and await human approval
- `terraform destroy` is NEVER run autonomously under any circumstances
- `kubectl apply` and `kubectl delete` are out of scope — use ArgoCD via the `deployments` repo
- Read-only `kubectl get`, `kubectl describe`, `kubectl logs` are safe
- Never modify `.tfstate` files directly — they are managed by the Terraform backend
- Never push to remote — stage and commit only

## References

- Talos Linux docs: https://www.talos.dev/latest/
- Terraform docs: https://developer.hashicorp.com/terraform/docs
