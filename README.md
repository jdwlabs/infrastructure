# Infrastructure

[![Bootstrap](https://github.com/jdwlabs/infrastructure/actions/workflows/bootstrap.yml/badge.svg?branch=main)](https://github.com/jdwlabs/infrastructure/actions/workflows/bootstrap.yml)
[![Terraform](https://github.com/jdwlabs/infrastructure/actions/workflows/terraform.yml/badge.svg?branch=main)](https://github.com/jdwlabs/infrastructure/actions/workflows/terraform.yml)
[![License](https://img.shields.io/badge/License-PolyForm%20NonCommercial%201.0-blue)](https://polyformproject.org/licenses/noncommercial/1.0.0/)

Proxmox infrastructure for jdwlabs: Terraform for the VMs, and `talops` to provision the Talos cluster and operate the hosts outside it.

## Structure

```
terraform/    Proxmox VM definitions (providers, variables, control/worker nodes)
bootstrap/    Go CLI tool (talops) - cluster bootstrap, reconciliation, infrastructure management
clusters/     Per-cluster runtime state created by talops (plaintext working files are
              gitignored; the SOPS+age encrypted vault is the shared source of truth)
inference/    Serving definitions for hosts outside the cluster (vllm/serving.yaml)
scenarios/    Step-by-step operational runbooks and scaling-test fixtures
docs/         Architecture and operations documentation
```

## Quick Start

```bash
# 1. Configure terraform
cp terraform/terraform.tfvars.example terraform/terraform.tfvars
# Edit terraform.tfvars with your Proxmox credentials and cluster settings

# 2. Set up the secret vault (SOPS + age) — see docs/secrets.md
age-keygen -o ~/.config/sops/age/keys.txt
talops secrets add-device <your-age-public-key>   # existing repo: git pull instead

# 3. Terraform remote state credentials (MinIO) — see docs/secrets.md
sops -d terraform/backend-credentials.enc.yaml   # export as AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY

# 4. Build the bootstrap tool
cd bootstrap && make build

# 5. Provision and bootstrap
./build/talops up
```

Secrets (`terraform.tfvars`, the Talos secrets bundle, `talosconfig`, and bootstrap state)
are stored as SOPS+age encrypted files committed to git and shared across machines. Terraform
state lives in a remote S3 (MinIO) backend; its credentials are vaulted in
`terraform/backend-credentials.enc.yaml` and hydrated manually. See
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
talops prune-nodes           Delete NotReady K8s node objects not in the desired state
talops upgrade-k8s           Upgrade Kubernetes, previewed by default (--apply to run it)
talops infra deploy          Deploy/update infrastructure (Terraform)
talops infra destroy         Destroy infrastructure
talops infra plan            Preview infrastructure changes
talops infra status          Show infrastructure state
talops infra cleanup         Remove generated Terraform files
talops haproxy status        Health of the load-balancer VM, service, config drift, and backends
talops haproxy plan          Diff rendered config against deployed, without touching it
talops haproxy apply         Push the rendered config (validated, rolled back on failure)
talops vllm status           Compare serving.yaml, the applied record and the running container
talops vllm plan             Diff the rendered Quadlet unit against the one installed, without touching it
talops vllm apply --confirm  Restart the server to match serving.yaml (health-gated, rolled back on failure)
talops secrets status        Show vault recipients and artifact state
talops secrets hydrate/seal  Decrypt vault to working files / encrypt back
talops secrets lock          Seal the vault and remove plaintext working copies
talops secrets edit          Edit a secret in $EDITOR and re-seal it
talops secrets add-device    Authorize a device's age key and re-key the vault
talops version               Print the talops version
```

## Demo

https://github.com/user-attachments/assets/2cd27971-e04d-49a2-a943-11a4b5760b81
