# Infrastructure

[![Bootstrap](https://github.com/jdwlabs/infrastructure/actions/workflows/bootstrap.yml/badge.svg?branch=main)](https://github.com/jdwlabs/infrastructure/actions/workflows/bootstrap.yml)
[![Terraform](https://github.com/jdwlabs/infrastructure/actions/workflows/terraform.yml/badge.svg?branch=main)](https://github.com/jdwlabs/infrastructure/actions/workflows/terraform.yml)

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

# 2. Build the bootstrap tool
cd bootstrap && make build

# 3. Provision and bootstrap
./build/talops up
```

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
```
