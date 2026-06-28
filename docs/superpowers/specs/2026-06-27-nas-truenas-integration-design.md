# NAS (TrueNAS) Integration into Proxmox + Talos Kubernetes

**Status:** Approved
**Date:** 2026-06-27
**Scope:** Cross-repo — TrueNAS (ops), Proxmox (ops), `infrastructure` (Talos/cluster), `platform` (k8s GitOps)

## Problem

The 5-node Proxmox cluster (`pve-cluster-1`) has **no shared storage** — only
`local` and `local-lvm` per node. Kubernetes persistent storage is served
entirely by Longhorn, whose replicas live on the same local NVMe that backs the
Talos VMs. There is:

- No off-VM-disk home for backups, ISOs, or VM templates.
- No path to live-migrate VMs between Proxmox nodes (requires shared storage).
- No large-capacity tier for bulk/RWX workloads — Longhorn is fast but capacity
  is bounded by per-node local disks.

A new TrueNAS box (35.1 TiB usable RAIDZ2) is available. Goal: integrate it as
general-purpose cluster storage without disrupting the existing Longhorn-backed
workloads.

## Verified Hardware (audited live via SSH — not assumed)

### Proxmox `pve-cluster-1` (PVE 9.2.3, 5 nodes, quorate)

| Node | IP | CPU / RAM | Local disk | NIC | Guests |
|------|-----|-----------|-----------|-----|--------|
| pve1 | .200 | Ryzen7 8845HS / 28G | 1TB Lexar NVMe | 1GbE (+1 spare nic) | haproxy-0 (100), talos-worker-01 (300), minecraft (1000) |
| pve2 | .201 | Ryzen5 3550H / 12G | 512G AirDisk | 1GbE | talos-cp-01 (200), talos-worker-02 (301) |
| pve3 | .202 | Ryzen5 3550H / 12G | 512G AirDisk | 1GbE | talos-cp-02 (201), talos-worker-03 (302) |
| pve4 | .203 | Ryzen5 3550H / 12G | 512G AirDisk | 1GbE | talos-cp-03 (202), talos-worker-04 (303) |
| pve5 | .204 | **Ryzen9 9950X (16C/32T) / 128G** | **2TB Samsung 9100 PRO (PCIe5) — IDLE/empty** | 1GbE | none |

- Talos topology: 3 control planes + 4 workers across pve1–pve4.
- `vmbr0` reports 10000Mb — this is a Linux bridge cosmetic value, **not real**.
  Real uplink (`nic0`) is **1GbE everywhere**.
- **pve5 is by far the most capable node** (16C/32T, 128G RAM, 2TB PCIe5 NVMe)
  yet runs no guests — significantly underused (flagged; see Out of Scope).

### TrueNAS `.205` (SCALE 25.10.4 Community)

- Pool `storage`: RAIDZ2, 4× 18.2 TiB Seagate IronWolf Pro HDD, 72.8 TiB raw →
  **35.1 TiB usable**. Healthy, empty.
- `boot-pool`: Samsung 990 PRO 1TB NVMe (boot only).
- NIC `enp7s0`: **1GbE**. No SLOG, no L2ARC.
- User `truenas_admin`: zsh, sudo ALL but **NOPASSWD is empty** → sudo prompts
  over non-tty SSH. Read-only commands (`zpool`/`zfs list`, `midclt call
  pool.query`) work without sudo.

## Decision

### Two-tier storage

- **Longhorn stays the default StorageClass.** Fast, on-node, replicated; backs
  latency-sensitive and self-replicating workloads (e.g. Vault on
  `longhorn-single`). No change to existing PVCs.
- **TrueNAS = NFS secondary tier**, opt-in via a non-default StorageClass. For
  bulk capacity, RWX, and backups.

### NFS-only (no iSCSI, no SMB)

The network is 1GbE end-to-end (Proxmox uplinks, TrueNAS NIC). At ~118 MB/s
line rate the **network is the bottleneck, not the storage protocol**. iSCSI
adds block-device operational complexity (multipath, LUN management) for no
throughput gain on this link. SMB is unnecessary for a Linux/k8s consumer.
NFS gives RWX natively and is the simplest correct choice. Revisit only if the
link is upgraded (see Out of Scope: storage VLAN/bond).

### Do NOT migrate existing Talos VMs onto NFS

The Talos VM disks stay on local NVMe — they back Longhorn replicas, and moving
them to a 1GbE NFS store would cripple Longhorn I/O. NFS on Proxmox is for
ISOs, backups, templates, and *future* live-migratable VMs only.

## Architecture

```
                         1GbE LAN (192.168.1.0/24)
   ┌──────────────────────────┬───────────────────────────┐
   │                          │                           │
[Proxmox pve1..pve5]   [Talos workers (k8s)]        [TrueNAS .205]
   │ NFS client            │ democratic-csi             │ pool: storage (RAIDZ2, 35.1T)
   │                       │  (freenas-api-nfs)         │  ├─ storage/proxmox  → NFS share
   ├─ truenas-vmdisks ─────┼────────────────────────────┤  ├─ storage/backup   → NFS share
   │   (images,iso,vztmpl) │                            │  └─ storage/k8s      → NFS share
   └─ truenas-backup ──────┘   SC: truenas-nfs ─────────┘     + TrueNAS API key → Vault
       (backup)                (non-default, Retain, RWX+RWO)
```

## Components

### Tier 1 — TrueNAS (ops via SSH / `midclt`; some steps need interactive sudo)

Datasets under pool `storage`:

| Dataset | Purpose | Consumer |
|---------|---------|----------|
| `storage/proxmox` | VM disks / ISO / templates | Proxmox NFS storage `truenas-vmdisks` |
| `storage/backup` | Proxmox vzdump backups | Proxmox NFS storage `truenas-backup` |
| `storage/k8s` | Kubernetes PVs | democratic-csi `truenas-nfs` |

NFS shares: each dataset exported to `192.168.1.0/24`, `maproot=root` (required
so Proxmox and the CSI driver can manage ownership). Create a TrueNAS **API
key** for the democratic-csi driver; store it in Vault (see Secrets Flow).

### Tier 2 — Proxmox (cluster-wide NFS storage)

Two NFS storages added cluster-wide (visible on all 5 nodes):

| Storage ID | Backing dataset | Content types |
|------------|-----------------|---------------|
| `truenas-vmdisks` | `storage/proxmox` | `images`, `iso`, `vztmpl` |
| `truenas-backup` | `storage/backup` | `backup` |

Verify after add: vzdump backup writes to `truenas-backup`; a test VM on
`truenas-vmdisks` live-migrates between two pve nodes.

### Tier 3 — Kubernetes (platform repo GitOps — mirror the `longhorn` service)

New service `platform/tenants/platform/services/democratic-csi/`:

- `values.yaml` — democratic-csi Helm chart values, `freenas-api-nfs` driver
  pointed at TrueNAS API + `storage/k8s`. (Service follows the longhorn pattern:
  `values.yaml` + `postInstall/`, no wrapping `Chart.yaml`.)
- `postInstall/external-secret.yaml` — `ExternalSecret` (mirror longhorn's),
  `secretStoreRef` `vault` / `ClusterSecretStore`, syncing the TrueNAS API key
  from Vault into the driver's namespace.
- `postInstall/storageclass-truenas-nfs.yaml` — StorageClass `truenas-nfs`:
  - `is-default-class: "false"` (non-default — Longhorn stays default)
  - `reclaimPolicy: Retain`
  - access modes RWX + RWO
  - `allowVolumeExpansion: true`

Register the service in the platform tenant (`tenants/platform/tenant.yaml`)
so the `platform-services` ApplicationSet picks it up; ArgoCD syncs on merge.

## Secrets Flow

```
operator: create TrueNAS API key (midclt / UI)
   → write to Vault: kv/truenas-csi  field: api_key   (+ host, port)

ESO: <democratic-csi ns>/<secret>  ←  kv/truenas-csi
   (ClusterSecretStore: vault)

democratic-csi driver pod:
   reads synced secret → authenticates to TrueNAS API → provisions NFS PVs
```

Seeding path mirrors existing services. If the platform `platformctl bootstrap`
seed framework is the canonical secret entry point, add a `truenas-csi` seed
spec; otherwise write the key directly to Vault per existing ESO conventions.
**Confirm exact seed mechanism during implementation** (see longhorn/argo-cd ESO
precedent).

## Build Order (tracer-bullet vertical slices)

1. **TrueNAS base** — create 3 datasets + 3 NFS shares + API key. Verify an NFS
   mount from a Proxmox node by hand. (Doable now via SSH/`midclt`; sudo steps
   need an interactive session.) Store API key in Vault.
2. **Proxmox NFS** — add `truenas-vmdisks` + `truenas-backup` cluster-wide.
   Verify: vzdump backup lands on NAS; test VM live-migrates between nodes.
3. **k8s CSI** — add democratic-csi service in platform repo + ESO secret +
   `truenas-nfs` StorageClass. Verify: bind a test RWX PVC, write/read from two
   pods on different nodes.
4. **Docs + Jira** — runbook in `infrastructure/scenarios/`, update
   ARCHITECTURE.md storage section; file epic + subtasks.

## Acceptance Criteria

- [ ] TrueNAS pool `storage` has datasets `proxmox`, `backup`, `k8s`, each NFS-exported to `192.168.1.0/24` with `maproot=root`.
- [ ] TrueNAS API key stored in Vault (`kv/truenas-csi`), not in Git.
- [ ] Proxmox shows `truenas-vmdisks` and `truenas-backup` on all 5 nodes.
- [ ] A vzdump backup successfully writes to `truenas-backup`.
- [ ] A test VM on `truenas-vmdisks` live-migrates between two Proxmox nodes.
- [ ] Longhorn remains the default StorageClass; existing PVCs untouched.
- [ ] StorageClass `truenas-nfs` exists, non-default, `Retain`, RWX+RWO.
- [ ] A test RWX PVC on `truenas-nfs` is writable from pods on two different nodes.
- [ ] democratic-csi deployed via ArgoCD from the platform repo (no manual `kubectl apply`).
- [ ] Runbook added to `infrastructure/scenarios/`; ARCHITECTURE.md storage section updated.

## Files Changed

| Repo | Path | Change |
|------|------|--------|
| platform | `tenants/platform/services/democratic-csi/values.yaml` | New |
| platform | `tenants/platform/services/democratic-csi/postInstall/external-secret.yaml` | New |
| platform | `tenants/platform/services/democratic-csi/postInstall/storageclass-truenas-nfs.yaml` | New |
| platform | `tenants/platform/tenant.yaml` | Register democratic-csi service |
| infrastructure | `scenarios/<nas-runbook>.md` | New runbook |
| infrastructure | `docs/ARCHITECTURE.md` | Add storage tier section |

TrueNAS dataset/share/API-key creation and Proxmox NFS storage registration are
operational steps (no repo artifact) unless later codified — see Out of Scope.

## Out of Scope (flagged for follow-up)

- **pve5 idle capacity (Ryzen9 9950X / 128G / 2TB PCIe5 NVMe)** — the cluster's
  strongest node runs no guests. Candidate for a dedicated Longhorn fast tier,
  rehosting heavier Talos workers, or local-VM disk. Separate decision.
- **SLOG SSD on TrueNAS** — would accelerate sync writes (NFS default).
  Unnecessary at 1GbE; revisit with a faster link.
- **Storage VLAN / NIC bond** — the 1GbE link is the throughput ceiling. A
  dedicated storage network or LACP bond would lift it and could justify
  revisiting iSCSI. Out of scope for this integration.
- **Codifying TrueNAS/Proxmox steps as IaC** — currently manual ops; could move
  to Terraform/Ansible later.
