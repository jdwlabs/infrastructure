# jdwlabs Homelab — Cluster Overview

A small, self-hosted Kubernetes platform run GitOps-style. This is a
high-level overview; operational detail (addresses, exact capacity, serials)
is kept private.

## Stack

- **Hypervisor:** Proxmox VE — a 5-node cluster
- **OS:** Talos Linux — immutable, API-driven, no SSH/shell
- **Orchestration:** Kubernetes — 3 control-plane nodes + worker nodes (as VMs)
- **GitOps:** ArgoCD — watches the platform repo; merges to `main` auto-sync
- **Provisioning:** Terraform (Proxmox) + a `talops` lifecycle CLI

## Platform services

Vault (secrets) · External Secrets Operator · cert-manager (DNS-01) ·
nginx Gateway Fabric · Longhorn (block storage) · CloudNativePG (Postgres) ·
Actions Runner Controller (self-hosted CI) · Prometheus / Grafana / Loki
(observability).

## Capacity (approximate)

- ~70+ CPU threads and ~190 GB RAM across the cluster
- Several TB of local NVMe, with a NAS expansion planned for bulk/backup
- A dedicated **GPU node (NVIDIA, 32 GB)** for AI inference / model serving

## Storage strategy (tiered)

| Tier | Use | Backing |
|------|-----|---------|
| Ephemeral | CI scratch, caches | node-local (local-path) |
| Replicated block | databases, app state | Longhorn |
| Bulk / shared / backup | media, datasets, DR | NAS (NFS / S3) — planned |

## AI / GPU

GPU-accelerated workloads run on the dedicated GPU node via PCI passthrough
to a Kubernetes worker, scheduled through the NVIDIA device plugin (model
serving, e.g. Ollama / vLLM).

## Design principles

- **GitOps everything** — desired state in git, reconciled by ArgoCD.
- **Immutable nodes** — Talos config is declarative; no manual host changes.
- **Tiered storage** — match durability/perf to the workload, don't put
  throwaway data on replicated block.
- **Least-privilege, per-tenant** — namespaces, quotas, network policies,
  and scoped ArgoCD projects per tenant.

> Detailed inventory, capacity budgets, and network specifics are maintained
> privately.
