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
├── infra           # Terraform wrapper commands
│   ├── deploy
│   ├── destroy
│   ├── plan
│   ├── status
│   └── cleanup
└── prune-nodes     # Cleanup stale K8s node objects
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
│   ├── kubectl/           # Kubernetes management
│   ├── logging/           # Structured logging
│   ├── state/             # State management
│   ├── talos/             # Talos management
│   ├── terraform/         # Terraform management
│   └── types/             # Core type definitions
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
    VMID       VMID      // Immutable identifier
    IP         net.IP    // May change (DHCP)
    ConfigHash string    // For drift detection
    MAC        string    // For IP rediscovery
    LastSeen   time.Time
    Role       Role
    RemovedAt  *time.Time // Audit trail
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

**Dynamic Reconfiguration**:
- `talops` SSHs to HAProxy host
- Generates new backend configuration using VMIDs as server names
- Reloads HAProxy gracefully
- Health checks ensure traffic only routes to ready nodes

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

### 7. Configuration Management

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

**Key Configuration Sources**:
- `terraform.tfvars` - VM specifications, Proxmox connection
- Environment variables - Secrets, overrides (see table above)
- CLI flags - Runtime behavior (dry-run, auto-approve, etc.)

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
