# Infrastructure

[![Bootstrap](https://github.com/jdwlabs/infrastructure/actions/workflows/bootstrap.yml/badge.svg?branch=main)](https://github.com/jdwlabs/infrastructure/actions/workflows/bootstrap.yml)
[![Terraform](https://github.com/jdwlabs/infrastructure/actions/workflows/terraform.yml/badge.svg?branch=main)](https://github.com/jdwlabs/infrastructure/actions/workflows/terraform.yml)
[![License](https://img.shields.io/badge/License-PolyForm%20NonCommercial%201.0-blue)](https://polyformproject.org/licenses/noncommercial/1.0.0/)

Talos Kubernetes cluster provisioning on Proxmox - Terraform for VMs, Go tool for bootstrap and lifecycle management.

## Structure

```
terraform/    Proxmox VM definitions (providers, variables, control/worker nodes)
bootstrap/    Go CLI tool - cluster bootstrap, reconciliation, infrastructure management
docs/         Architecture documentation
```

## Quick Start

```bash
# 1. Configure terraform
cp terraform/terraform.tfvars.example terraform/terraform.tfvars
# Edit terraform.tfvars with your Proxmox credentials and cluster settings

# 2. Set up the secret vault (SOPS + age) — see docs/secrets.md
age-keygen -o ~/.config/sops/age/keys.txt
talops secrets add-device <your-age-public-key>   # existing repo: git pull instead

# 3. Build the bootstrap tool
cd bootstrap && make build

# 4. Provision and bootstrap
./build/talops up
```

Secrets (`terraform.tfvars`, the Talos secrets bundle, `talosconfig`, and bootstrap state)
are stored as SOPS+age encrypted files committed to git and shared across machines. See
[docs/secrets.md](docs/secrets.md) for setup, onboarding a new device, and revocation.

## Commands

```
talops up                    Provision VMs + bootstrap cluster
talops down                  Drain + destroy cluster
talops bootstrap             Initial cluster deployment
talops reconcile             Reconcile cluster with terraform.tfvars
talops reconcile --plan      Preview changes without applying
talops status                Show cluster status
talops reset                 Reset cluster state
talops infra deploy          Deploy/update infrastructure (Terraform)
talops infra destroy         Destroy infrastructure
talops infra plan            Preview infrastructure changes
talops infra status          Show infrastructure state
talops infra cleanup         Remove generated Terraform files
talops secrets status        Show vault recipients and artifact state
talops secrets add-device    Authorize a device's age key and re-key the vault
talops secrets hydrate/seal  Decrypt vault to working files / encrypt back
```

## Demo

https://github.com/user-attachments/assets/2cd27971-e04d-49a2-a943-11a4b5760b81
