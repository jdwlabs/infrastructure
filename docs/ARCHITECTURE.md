# Architecture

This document describes the architecture of the Talos Kubernetes infrastructure provisioning system, covering the design decisions, component interactions, and operational flows.

## Overview

This project provides a complete infrastructure-as-code solution for deploying and managing Talos Linux Kubernetes clusters on Proxmox VE. It combines Terraform for VM provisioning with a custom Go CLI tool (`talops`) for cluster lifecycle management.

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              User Interface                                 │
│                         (CLI: talops, Terraform)                            │
└─────────────────────────────────────────────────────────────────────────────┘
                                      │
                                      ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                           Orchestration Layer                               │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐     │
│  │   Command    │  │   Config     │  │    State     │  │   Logging    │     │
│  │   Handler    │  │   Manager    │  │   Manager    │  │   Session    │     │
│  │   (Cobra)    │  │              │  │              │  │   (Zap)      │     │
│  └──────────────┘  └──────────────┘  └──────────────┘  └──────────────┘     │
└─────────────────────────────────────────────────────────────────────────────┘
                                      │
                    ┌─────────────────┼─────────────────┐
                    ▼                 ▼                 ▼
┌──────────────────────┐ ┌──────────────────┐ ┌──────────────────────┐
│   Infrastructure     │ │   Bootstrap      │ │   Reconciliation     │
│     Management       │ │   Engine         │ │   Engine             │
│  ┌────────────────┐  │ │  ┌────────────┐  │ │  ┌────────────────┐  │
│  │  Terraform     │  │ │  │  Talos     │  │ │  │  Three-Way     │  │
│  │  Controller    │  │ │  │  Client    │  │ │  │  State Diff    │  │
│  └────────────────┘  │ │  └────────────┘  │ │  └────────────────┘  │
│  ┌────────────────┐  │ │  ┌────────────┐  │ │  ┌────────────────┐  │
│  │  Proxmox API   │  │ │  │  Config    │  │ │  │  etcd Quorum   │  │
│  │  Client        │  │ │  │  Generator │  │ │  │  Safety        │  │
│  └────────────────┘  │ │  └────────────┘  │ │  └────────────────┘  │
└──────────────────────┘ └──────────────────┘ └──────────────────────┘
           │                       │                       │
           ▼                       ▼                       ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                           Proxmox Infrastructure                            │
│                                                                             │
│   ┌─────────────┐    ┌─────────────┐    ┌─────────────┐    ┌─────────────┐  │
│   │  Control    │    │  Control    │    │  Control    │    │   Worker    │  │
│   │  Plane 1    │    │  Plane 2    │    │  Plane 3    │    │   Node 1    │  │
│   │  (VMID 201) │    │  (VMID 202) │    │  (VMID 203) │    │  (VMID 211) │  │
│   └──────┬──────┘    └──────┬──────┘    └──────┬──────┘    └──────┬──────┘  │
│          │                  │                  │                  │         │
│          └──────────────────┼──────────────────┘                  │         │
│                             │                                     │         │
│                             ▼                                     ▼         │
│                    ┌─────────────────┐                   ┌─────────────┐    │
│                    │   HAProxy LB    │                   │   Worker    │    │
│                    │  (192.168.1.x)  │                   │   Node 2    │    │
│                    └────────┬────────┘                   │  (VMID 212) │    │
│                             │                            └─────────────┘    │
│                             │                                               │
│                             ▼                                               │
│                    ┌─────────────────┐                                      │
│                    │  Kubernetes     │                                      │
│                    │  API Endpoint   │                                      │
│                    │  (Port 6443)    │                                      │
│                    └─────────────────┘                                      │
└─────────────────────────────────────────────────────────────────────────────┘
```

## Core Components

### 1. Terraform Layer

**Purpose**: Declarative infrastructure provisioning on Proxmox VE.

**Responsibilities**:
- Proxmox VM creation with Talos ISO
- Resource allocation (CPU, memory, disk)
- Network interface configuration
- MAC address assignment (used for IP rediscovery)

**Integration Point**: The `talops` tool reads `terraform.tfvars` to understand the desired state and extracts VMIDs and MAC addresses for node identification.

### 2. Bootstrap Tool (`talops`)

**Language**: Go 1.26
**Framework**: Cobra for CLI, Zap for logging

#### 2.1 Command Structure

```
talops
├── up              # Full provisioning + bootstrap
├── down            # Graceful shutdown + destroy
├── bootstrap       # Initial cluster creation only
├── reconcile       # State reconciliation with safety
│   └── --plan      # Preview changes
├── status          # Cluster health overview
├── reset           # Local state cleanup
├── prune-nodes     # Cleanup stale K8s node objects
├── upgrade-k8s     # Upgrade Kubernetes, prune-guarded and previewed by default
├── infra           # Terraform wrapper commands
│   ├── deploy
│   ├── destroy
│   ├── plan
│   ├── status
│   └── cleanup
├── haproxy         # Inspect/converge the HAProxy load balancer (outside the cluster)
│   ├── status
│   ├── plan
│   └── apply
├── vllm            # Inspect/converge the vLLM GPU host (outside the cluster)
│   ├── status
│   ├── plan
│   └── apply --confirm
└── secrets         # SOPS+age vault management
    ├── status
    ├── hydrate
    ├── seal
    ├── lock
    ├── edit
    └── add-device
```

#### 2.2 Internal Architecture

```
bootstrap/
├── cmd/                   # Cobra command definitions
│   └── root.go            # Root command + subcommand setup
├── internal/
│   ├── app/               # Application orchestration
│   ├── discovery/         # Node discovery
│   ├── haproxy/           # Load balancer management
│   ├── hostconverge/      # Backup/validate/activate/verify/rollback for hosts outside the cluster
│   ├── kubectl/           # Kubernetes management
│   ├── logging/           # Structured logging
│   ├── sshutil/           # Shared SSH dial, auth and known_hosts handling
│   ├── state/             # State management
│   ├── talos/             # Talos management
│   ├── terraform/         # Terraform management
│   ├── types/             # Core type definitions
│   └── vllm/              # vLLM GPU host: spec, render, converge, health gate
└── main.go                # Entry point
```

### 3. State Management System

The system implements a **three-way reconciliation** pattern:

```
┌──────────────────┐     ┌──────────────────┐     ┌──────────────────┐
│  DESIRED STATE   │     │  DEPLOYED STATE  │     │   LIVE STATE     │
│                  │     │                  │     │                  │
│  terraform.tfvars│◄───►│  clusters/*/     │◄───►│  Proxmox API     │
│                  │     │  state/          │     │  Talos API       │
│  • VMID list     │     │                  │     │  etcd members    │
│  • Specs         │     │  • Known IPs     │     │  K8s nodes       │
│  • Counts        │     │  • Config hashes │     │  • Current IPs   │
│                  │     │  • MAC addresses │     │  • Health status │
└──────────────────┘     └──────────────────┘     └──────────────────┘
         │                       │                       │
         └───────────────────────┼───────────────────────┘
                                 │
                                 ▼
                    ┌──────────────────────┐
                    │   RECONCILIATION     │
                    │      ENGINE          │
                    │                      │
                    │  1. Calculate diff   │
                    │  2. Safety checks    │
                    │  3. Execute plan     │
                    │  4. Update state     │
                    └──────────────────────┘
```

**State Files**:
- `clusters/{name}/state/bootstrap-state.json` - Persistent cluster state
- `clusters/{name}/state/infra-deploy-state.json` - Infrastructure deployment tracking
- `terraform.tfvars` - Source of truth for desired state

**Key Types** (from `internal/types/types.go`):

```go
// NodeSpec - What Terraform wants
type NodeSpec struct {
    VMID   VMID   // Unique VM identifier
    Name   string // VM name
    Node   string // Proxmox node (pve1, pve2, etc.)
    CPU    int
    Memory int    // MB
    Disk   int    // GB
    Role   Role   // control-plane or worker
}

// NodeState - What we know is deployed
type NodeState struct {
    VMID         VMID      // Immutable identifier
    IP           net.IP    // May change (DHCP)
    ConfigHash   string    // Drift detection: rendered node YAML at last apply
    TemplateHash string    // Drift detection: config-generation inputs (patch templates, base config) at last apply
    MAC          string    // For IP rediscovery
    LastSeen     time.Time
    Role         Role
    RemovedAt    *time.Time // Audit trail
}

// LiveNode - Current reality
type LiveNode struct {
    VMID         VMID
    IP           net.IP
    MAC          string
    Status       NodeStatus // discovered/joined/ready/not_found/rebooting
    TalosVersion string
    K8sVersion   string
	DiscoveredAt time.Time
}
```

### 4. Reconciliation Engine

The reconciliation engine is the core innovation of this system, providing safe, idempotent cluster management.

#### 4.1 ReconcilePlan Structure

```go
type ReconcilePlan struct {
    NeedsBootstrap      bool     // First-time setup required
    AddControlPlanes    []VMID   // New CP nodes to add
    AddWorkers          []VMID   // New workers to add
    RemoveControlPlanes []VMID   // CP nodes to remove (interactive)
    RemoveWorkers       []VMID   // Workers to remove (automatic)
    UpdateConfigs       []VMID   // Nodes needing reconfiguration
    NoOp                []VMID   // Unchanged nodes
}
```

#### 4.2 Safety Mechanisms

**etcd Quorum Protection**:

| Current CPs | Majority | Can Lose | Safe to Remove? |
|-------------|----------|----------|-----------------|
| 5           | 3        | 2        | Yes (→ 4)       |
| 4           | 3        | 1        | Yes (→ 3)       |
| 3           | 2        | 1        | ⚠️ Interactive only |
| 2           | 2        | 0        | ❌ Never        |
| 1           | 1        | 0        | ❌ Never        |

Control plane removal requires `--auto-approve` or interactive confirmation when quorum would be at risk.

**Worker Removal**: Automatic and safe - workers can be removed without cluster availability concerns.

### 5. IP Rediscovery System

**Problem**: Talos nodes reboot into secure mode and may receive new DHCP IPs, making static configuration stale.

**Solution**: ARP-based MAC-to-IP resolution

```
┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│   Node      │     │   Proxmox   │     │   talops    │
│   Reboots   │────►│   DHCP      │────►│   Scanner   │
│             │     │   Server    │     │             │
└─────────────┘     └─────────────┘     └──────┬──────┘
                                               │
                                               ▼
                                    ┌─────────────────────┐
                                    │  1. SSH to Proxmox  │
                                    │     run qm config   │
                                    │     to get MAC      │
                                    │  2. Read ARP table  │
                                    │     /proc/net/arp   │
                                    │  3. Match MAC→IP    │
                                    └─────────────────────┘
```

**Implementation**: `internal/discovery/scanner.go`
- SSHs to Proxmox host and runs `qm config <vmid>` to extract MAC addresses
- Reads the ARP table via `cat /proc/net/arp` on the Proxmox host
- Matches MAC addresses to IPs from the ARP table
- Updates `NodeState.IP` when changes detected

### 6. HAProxy Integration

**Purpose**: Provide a stable control plane endpoint that doesn't change when control plane nodes are replaced.

**Architecture**:

```
                         ┌─────────────────┐
                         │   HAProxy VM    │
                         │  (192.168.1.x)  │
                         │                 │
                         │  Frontend: 6443 │
                         │  Talos:   50000 │
                         │  Stats:    9000 │
                         └────────┬────────┘
                                  │
              ┌───────────────────┼───────────────────┐
              │                   │                   │
              ▼                   ▼                   ▼
    ┌─────────────────┐ ┌─────────────────┐ ┌─────────────────┐
    │  Control Plane  │ │  Control Plane  │ │  Control Plane  │
    │      VMID 201   │ │      VMID 202   │ │      VMID 203   │
    │   (192.168.1.10)│ │   (192.168.1.11)│ │   (192.168.1.12)│
    └─────────────────┘ └─────────────────┘ └─────────────────┘
```

**VM Provisioning**:

The VM itself is declared in `terraform/haproxy-node.tf` — an Ubuntu cloud
image plus a cloud-init snippet that creates the admin user, installs
`haproxy` and `qemu-guest-agent`, and pins the static address. Two properties
are deliberate:

- `haproxy_vms` defaults to an empty list, so a checkout provisions no load
  balancer until an operator adds an entry. The load balancer built by hand
  before this config existed is **not** in Terraform state; it is replaced via
  `scenarios/haproxy-vm-rebuild.md` rather than imported, because adopting it
  would leave Terraform reconciling a cloud-init drive onto the live API
  endpoint.
- The address is always static CIDR, enforced by a variable validation. A
  lease-dependent address on the host that DNS, every `talosconfig` endpoint,
  and kubeconfig resolve to is the same failure class that has already taken
  etcd down on the control-plane nodes.

Cloud-init deliberately ships **no** `haproxy.cfg`. The distro placeholder is
enough for the service to hold its ports; the real config arrives through the
push path below, so there is only ever one config-rendering path.

**High availability (keepalived VRRP group)**:

Two or more `haproxy_vms` entries make the load balancer a VRRP group, on
different Proxmox hosts and never a control-plane host. `haproxy_ip` becomes a
virtual address that keepalived moves to a surviving instance; each instance
has its own static address underneath it. One entry is a standalone load
balancer and behaves exactly as it did before the group existed.

```
        cluster.jdwlabs.com / kubeconfig / talosconfig
                           │
                 192.168.1.199 (virtual)
                 ┌─────────┴─────────┐
        haproxy-1 (pve1)      haproxy-2 (pve5)
        own address <A>  VRRP  own address <B>
                 └─────────┬─────────┘
                    control planes
```

- **Every instance binds the virtual address**, held or not
  (`net.ipv4.ip_nonlocal_bind`). The generated config is therefore identical
  on every instance, and a backup serves the moment the address arrives.
- **No preemption.** The address stays where it is when a failed instance
  returns, so a recovery costs clients nothing.
- **An instance is eligible only while HAProxy listens on `:6443`.** A freshly
  built one, still on the distro placeholder config, cannot claim the address.
- **`talops` addresses instances, never the virtual address.** Pushing to the
  virtual address would update only the holder.

What does and does not follow the address — DNS, the UDP rules, the Tailscale
subnet router — and why: [haproxy-vm-provisioning.md](haproxy-vm-provisioning.md)
§5.5. Bringing the pair up: `scenarios/haproxy-keepalived-cutover.md`.

**Dynamic Reconfiguration**:
- `talops` SSHs to each HAProxy instance
- Generates new backend configuration using VMIDs as server names
- Reloads HAProxy gracefully
- Health checks ensure traffic only routes to ready nodes

In a group the push goes to every instance and does not stop at a failure: an
unreachable instance is the case the group exists for, and the one still
serving is the one that needs the new backends. A push that reaches only some
instances is a `DivergenceError`. `reconcile` finishes the rest of its plan
and then exits non-zero naming the instance left behind; a push that fails on
every instance stops it, as a standalone failure always has. The preflight
likewise fails only when no instance answers.

**Inspecting and Converging the Load Balancer**:

`talops haproxy` is the read/config-only surface over the layers above. It
never provisions, destroys, or reconfigures the VM — that stays behind
`talops infra plan` and a human `terraform apply`.

| Command | Reads | Writes |
|---|---|---|
| `talops haproxy status` | VM (tfvars + Terraform state), SSH, service, config drift, per-backend health from the runtime socket | nothing |
| `talops haproxy plan` | rendered vs. deployed config | nothing |
| `talops haproxy apply` | as above | `/etc/haproxy/haproxy.cfg`, via the validated/auto-rollback path |

Three properties are load-bearing:

- **A layer that could not be read reports `unknown`, never a clean result.**
  An unreachable host and a healthy one must not produce the same report.
- **`status` names whether the address it targeted belongs to a VM this repo
  can rebuild** (`source: terraform`), one declared but not yet applied
  (`declared`), or one built outside it (`unmanaged`). That last value is the
  honest answer for the hand-built load balancer, and it flips only when the
  rebuild in `scenarios/haproxy-vm-rebuild.md` has actually happened.
- **`--host` targets an address other than `haproxy_ip`.** A replacement is
  verified on a temporary address before DNS, tfvars, or anything else moves.

For a VRRP group the same three commands work per instance:

- `status` prints one row per instance — VM, SSH, `haproxy`, `keepalived`,
  whether it holds the virtual address, config drift, backends up — plus the
  holder and the holder's backend table. It exits 1 unless exactly one
  instance holds the address and every instance is reachable with keepalived
  active: `virtual_address_split`, `virtual_address_unheld`,
  `instance_unreachable`, `keepalived_inactive`. A pair running on one leg is
  serving, and it is still a failing report.
- `plan` diffs each instance separately, so one lagging the other is visible.
- `apply` pushes to each instance that drifts and skips the ones already
  current, which makes a retry after a partial failure safe. If some instances
  took the config and others did not it fails with `group_divergent` and does
  not record the pushed hash.
- `--host <instance-address>` narrows any of them to one instance, still
  rendered against the virtual address.

`apply` shares its install path with `reconcile`, so a fix to one is a fix to
both, and it records the pushed config's hash in cluster state. That record is
a hint about what talops last pushed — it cannot see a change made on the host,
so drift is always confirmed against the live file when SSH is available. The
write/backup/validate/activate/verify/rollback mechanics behind `apply` are
`internal/hostconverge`, shared with the vLLM host below — see "Host
Convergence" (§7).

**Configuration Template**:

The generated config includes two backend - one for the Kubernetes API and one for the Talos API:

```haproxy
backend k8s-controlplane
    balance leastconn
    option tcp-check
    tcp-check connect port 6443
    default-server inter 5s fall 3 rise 2
    server talos-cp-201 192.168.1.10:6443 check
    server talos-cp-202 192.168.1.11:6443 check
    server talos-cp-203 192.168.1.12:6443 check
    
backend talos-controlplane
    balance leastconn
    option tcp-check
    tcp-check connect port 50000
    default-server inter 5s fall 3 rise 2
    server talos-cp-201 192.168.1.10:50000 check
    server talos-cp-202 192.168.1.11:50000 check
    server talos-cp-203 192.168.1.12:50000 check
```

### 7. Host Convergence

**Purpose**: One backup/validate/activate/verify/rollback sequence for every
host talops configures outside the Talos cluster, so HAProxy and the vLLM GPU
host share a single, well-tested path instead of each growing its own.

**Implementation**: `internal/hostconverge`. `Apply` takes a `Change` — the
files to install plus optional `Validate`, `Activate`, and `Verify` hooks —
and runs them in a fixed order:

1. **Write** each file to a temp path next to its destination, over SSH.
2. **Back up** the existing file (skipped for a file that doesn't exist yet —
   its absence is recorded instead, so restore knows to delete it rather
   than copy back a backup that was never made), then move the temp file
   into place.
3. **Validate** the new configuration (a syntax check, for example) before
   anything is activated. A validation failure restores the backup and stops.
4. **Activate** the change (a service reload or restart).
5. **Verify** the activated change is actually working.

When the `Change` has a `Verify` hook, a failure at `Activate` or `Verify`
restores every file `Apply` touched — copying back its backup for a file
that had one, and deleting a file that didn't exist before the attempt —
then re-runs `Activate`/`Verify` against the restored configuration.
`Result.RolledBack` and `Result.RollbackErr` tell the caller whether
recovery itself succeeded. A `Change` without `Verify` gets no rollback of
an `Activate` failure: the error is returned as-is and the new files stay
installed, so the guarantee that a failed change is undone holds only for a
caller that sets `Verify` (vLLM does; HAProxy does not — see below).
`Unchanged` compares the files a `Change` would install against what's on
disk (by content hash) without writing anything, and treats a read that
returns anything but a hash or "missing" as an error rather than as a
change; vLLM's `plan` and `apply` are its only callers (`pendingReasons` in
`internal/vllm/converge.go`), which is how they report installed-file drift
without installing anything — HAProxy's `plan` compares configs with its own
`Diff()` instead, and neither command group's `status` calls `Unchanged` at
all.

Content is limited to roughly 96 KiB per file (`MAX_ARG_STRLEN` for the `sh
-c` argument used to write it), which comfortably covers a Quadlet unit or an
haproxy.cfg but rules out shipping large files through this path.

**Two users, two configurations of the same sequence**:

| | Validate | Activate | Verify |
|---|---|---|---|
| HAProxy (`internal/haproxy`) | `haproxy -c` against the rendered config | reload the service | none — a reload that returns is trusted |
| vLLM (`internal/vllm`) | none — a Quadlet unit has no offline syntax check | `daemon-reload` + swap the systemd unit + restart | the health gate: model present, a real completion, a real tool call — see [docs/vllm-serving.md](vllm-serving.md) |

HAProxy's `Verify` is `nil`, so a failed `Activate` there is returned as-is
rather than triggering a rollback — matching the config push's historical
behavior before it moved onto `hostconverge`. A failed reload leaves the
new, `haproxy -c`-validated `haproxy.cfg` on disk; nothing re-checks which
configuration the running process holds, and the next reload or restart
loads the new file. vLLM always sets `Verify` (the health gate), so its
`Activate` failures do roll back, and a rollback re-activates and
re-verifies the previous Quadlet unit before reporting which server ended
up serving.

### 8. Configuration Management

**Config Precedence** (lowest to highest):
1. Default values (`types.DefaultConfig()`)
2. Environment variables (`CLUSTER_NAME`, `TERRAFORM_TFVARS`, etc.)
3. Terraform `.tfvars` file
4. Command-line flags

**Validation**: `Config.Validate()` ensures required fields are present before operations.

**Environment Variables**:

| Variable | Purpose |
|----------|---------|
| `CLUSTER_NAME` | Cluster identifier |
| `TERRAFORM_TFVARS` | Path to tfvars file |
| `CONTROL_PLANE_ENDPOINT` | Kubernetes API endpoint |
| `HAPROXY_IP` | HAProxy load balancer IP |
| `KUBERNETES_VERSION` | Target Kubernetes version |
| `TALOS_VERSION` | Target Talos version |
| `INSTALLER_IMAGE` | Talos installer image |
| `SECRETS_DIR` | Path to secrets directory |
| `SSH_KEY_PATH` | SSH key for Proxmox/HAProxy access |
| `HAPROXY_LOGIN_USER` | SSH user for HAProxy host |
| `HAPROXY_STATS_USER` | HAProxy stats page username |
| `HAPROXY_STATS_PASSWORD` | HAProxy stats page password |
| `ADMIN_ALLOWED_CIDRS` | Comma-separated source CIDRs allowed to reach k8s/talos apiserver frontends |

**Key Configuration Sources**:
- `terraform.tfvars` - VM specifications, Proxmox connection
- Environment variables - Secrets, overrides (see table above)
- CLI flags - Runtime behavior (dry-run, auto-approve, etc.)

## Storage Tiers

The cluster uses two distinct storage tiers that serve different workload profiles.
Keeping them separate preserves Longhorn performance while unlocking shared capacity
and RWX access for workloads that need it.

### Tier 1 — Longhorn (default, on-node NVMe, replicated)

Longhorn is the default StorageClass (`storageclass.kubernetes.io/is-default-class:
"true"`). It provisions volumes on the local NVMe disks inside the Talos VMs and
replicates them across worker nodes in-cluster.

- **Use for:** databases, caches, Vault, and any latency-sensitive workload.
- **Characteristics:** low latency (local NVMe); I/O does not leave the node for
  reads; replication provides in-cluster durability without depending on external
  infrastructure.
- **Existing PVCs are untouched** — this tier is the operational baseline and no
  migration away from it is planned or required.

Talos VM disks (`local-lvm` per node) also reside on local NVMe. They must remain
there: Longhorn replica I/O runs over the same path, so moving a Talos VM disk to
a network share would serialize all Longhorn traffic over a 1GbE link and cripple
cluster storage performance.

### Tier 2 — TrueNAS NFS (`truenas-nfs`, capacity/RWX, opt-in)

A TrueNAS box (`192.168.1.205`, 35.1 TiB RAIDZ2) provides a secondary NFS tier.
It is opt-in: workloads must explicitly set `storageClassName: truenas-nfs`.

- **Use for:** RWX (ReadWriteMany) shared mounts, bulk data, ISOs, container
  templates, and Proxmox backup archives.
- **Characteristics:** high raw capacity (35 TiB); RWX native via NFS; bounded at
  roughly 118 MB/s total (1GbE link shared by all NFS consumers); HDD latency.
- **Not suitable for** latency-sensitive workloads — the 1GbE/HDD profile is
  incompatible with database or Vault storage backends.

Kubernetes PVs are provisioned dynamically by the `democratic-csi` `freenas-api-nfs`
driver, which calls the TrueNAS API to create a child ZFS dataset and NFS export
per PVC under `storage/k8s/vols`. The StorageClass uses `reclaimPolicy: Retain`:
deleting a PVC leaves the PV and TrueNAS dataset in place and requires manual
cleanup (documented in `scenarios/truenas-nfs-storage.md`).

Proxmox gets two cluster-wide NFS storages backed by static NFS shares:

| Storage ID | Dataset | Content |
|-----------|---------|---------|
| `truenas-vmdisks` | `storage/proxmox` | `images`, `iso`, `vztmpl` |
| `truenas-backup` | `storage/backup` | `backup` (vzdump archives) |

VMs whose disks reside on `truenas-vmdisks` can be live-migrated between any of the
five Proxmox nodes without copying data — this is the key benefit over the per-node
`local-lvm` pools.

### Tier Selection Summary

| Need | StorageClass | Notes |
|------|-------------|-------|
| Default PVC (RWO, fast) | `longhorn` | Implicit — no annotation needed |
| RWX shared mount | `truenas-nfs` | Explicit `storageClassName` required |
| Bulk data / large volume | `truenas-nfs` | Explicit `storageClassName` required |
| Proxmox backup | `truenas-backup` (Proxmox storage) | Via `vzdump --storage truenas-backup` |
| Proxmox ISO / template | `truenas-vmdisks` (Proxmox storage) | Via Proxmox UI or `pvesm` |

## Operational Flows

### 1. Initial Bootstrap (`talops up`)

```
1. Parse terraform.tfvars
2. Run terraform apply (create VMs)
3. Discover node IPs (ARP scanning)
4. Generate Talos configs
5. Apply config to first control plane
6. Wait for etcd bootstrap
7. Join remaining control planes
8. Join workers
9. Configure HAProxy backends
10. Generate kubeconfig
11. Save bootstrap state
```

### 2. Scale Out (Add Nodes)

```
1. User updates terraform.tfvars (increase count)
2. Run terraform apply
3. talops reconcile detects new VMIDs
4. Discover IPs for new nodes
5. Generate join configs
6. Apply configs to new nodes
7. Update HAProxy with new backends
8. Update state file
```

### 3. Scale In (Remove Workers)

```
1. Detect worker VMIDs no longer in terraform.tfvars
2. Cordon node (prevent new workloads)
3. Drain node (move existing workloads)
4. Remove from Kubernetes
5. Remove from etcd (if CP)
6. Destroy VM via Terraform
7. Update HAProxy config
8. Update state file
```

### 4. Control Plane Replacement (Safe Removal)

```
1. Detect CP VMID no longer in terraform.tfvars
2. Check etcd quorum math
3. If quorum would be at risk:
   - Require --auto-approve OR interactive confirmation
4. Cordon and drain
5. Remove etcd member
6. Remove Kubernetes node
7. Destroy VM
8. Update HAProxy
9. Update state
```

## Design Decisions

### Why Go instead of Bash?

Original implementation was Bash-based, but migrated to Go for:
- **Type Safety**: VMID as distinct type prevents mixing with other integers
- **Error Handling**: Structured error types vs exit codes
- **Testing**: Unit tests for reconciliation logic
- **Maintainability**: Single binary vs script dependencies
- **Cross-Platform**: Windows/Git Bash compatibility issues

### Why Three-Way Reconciliation?

- **Terraform State**: Tells us what VMs exist
- **Local State**: Tracks what we've deployed and their IPs
- **Live State**: What actually responds on the network

This handles DHCP IP changes, manual VM modifications, and interrupted operations.

### Why VMID-Based Identification?

IPs change (DHCP), names can collide, but VMIDs are:
- Immutable (assigned at creation)
- Unique within Proxmox
- Present in both Terraform and Proxmox API

### Why HAProxy Instead of kube-vip?

- **Separation of Concerns**: Load balancer independent of cluster
- **Flexibility**: Can run on separate infrastructure
- **Observability**: Built-in stats page
- **Familiarity**: Standard HAProxy configuration

## Security Considerations

### 1. Secrets Management

- **SSH Keys**: Stored in `clusters/{name}/secrets/`, never committed
- **Proxmox Tokens**: Environment variables or tfvars (user responsibility)
- **Talos Secrets**: Generated per-cluster, stored in secrets dir
- **kubeconfig**: Fetched from Talos, stored in secrets dir

### 2. Network Security

- **SSH**: Key-based auth to Proxmox and HAProxy
- **Talos API**: Mutual TLS with generated certificates
- **Kubernetes API**: Via HAProxy load balancer, certificate auth

### 3. etcd Security

- etcd runs on control plane nodes only
- Peer-to-peer TLS encryption
- Client certificates for Kubernetes components

## Scalability Limits

| Resource | Current Limit | Notes |
|----------|---------------|-------|
| Control Planes | 5 | etcd performance degrades beyond 5-7 nodes |
| Workers | Unlimited | Limited by Proxmox resources |
| Clusters | Unlimited | Per-directory state isolation |
| DHCP Leases | Network-dependent | IP rediscovery handles churn |

## References

- [Talos Linux Documentation](https://www.talos.dev/)
- [Proxmox VE API](https://pve.proxmox.com/wiki/Proxmox_VE_API)
- [etcd Operations Guide](https://etcd.io/docs/v3.5/op-guide/)
- [Cobra CLI Framework](https://github.com/spf13/cobra)
