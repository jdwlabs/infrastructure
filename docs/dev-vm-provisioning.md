# Dev VM Provisioning — Design

Status: **design only, nothing applied.** No Terraform resource exists yet;
this document is the plan for `terraform/dev-vm-node.tf`.

A dedicated, always-on Proxmox VM to be the full daily-driver dev environment
for jdwlabs work — SSH + VS Code Remote-SSH from the Windows workstation,
git/build/Claude Code sessions run on the VM, not locally. Reproducible from
git, and movable between Proxmox hosts without a rebuild.

## 1. Why

Today all interactive dev work (editing, builds, Claude Code sessions, git)
runs on the Windows workstation. That ties the environment to one physical
machine, gives no 24/7 runway for background/overnight agent work, and has no
story for hardware failure other than starting over.

## 2. Precedents in this repo

- `terraform/gpu-node.tf` — non-Talos Ubuntu VM from a cloud image
  (`proxmox_virtual_environment_download_file`, `content_type = "import"`,
  avoids the provider's node-SSH importdisk path), flat vars (single VM, not
  a list — that shape is reserved for HA fleets like `haproxy_vms`).
- `terraform/haproxy-node.tf` design (`docs/haproxy-vm-provisioning.md`) —
  cloud-init kept to day-0 OS/user/base-package bootstrap only; anything that
  would drift (real service config) is pushed post-boot through a separate,
  reviewable path instead of living in cloud-init.
- `docs/host-addressing.md` — DHCP pool spans roughly `.64`–`.253`; the vLLM
  GPU VM sits at `.50` and haproxy's cutover VM at `.55`, both deliberately
  below the pool floor to avoid DHCP collision. This VM follows the same
  placement class.

## 3. Requirements

1. **Reproducible day-0**: VM shell, OS, base packages, static IP, SSH access
   recreatable from git + vault alone.
2. **Full daily driver**: sized for IDE (Remote-SSH) + Nx/pnpm builds + Docker
   + multiple concurrent Claude Code agent sessions, not just light editing.
3. **Movable between Proxmox hosts** with minutes-or-less downtime, not a
   restore-from-backup exercise.
4. **Off the control-plane hosts** (pve2/pve3/pve4) — same rule already
   applied to the GPU and HAProxy VMs.
5. **Human-gated mutation**: VM create/destroy goes through
   `terraform plan` → human `terraform apply`, same as every other VM here.
6. **Own backup**, independent of cross-host mobility — protects against
   breaking the dev environment itself, not just host loss.

Non-goals: HA/auto-failover, public exposure (LAN SSH only), multi-user/shared
box.

## 4. Storage & mobility — decision

**Put this VM's disk on `truenas-vmdisks`, the TrueNAS-backed NFS storage
that already exists cluster-wide.** With the disk on shared storage reachable
from every pve host, moving the VM is `qm migrate <vmid> <target-node>
--online` (or the Proxmox UI) — only VM RAM state transfers, since the disk
doesn't move. Seconds of downtime.

**Correction (2026-08-09):** this section originally proposed adding a new
NFS storage backend as a Phase 0 human prerequisite (`pvesm add nfs ...`).
Live Proxmox API verification during Phase 0 execution found that work
already done: `truenas-vmdisks` (dataset `storage/proxmox`, content
`iso,images,vztmpl`) is active on every node, `shared: true`, currently
empty. It's already documented in `docs/ARCHITECTURE.md` §"Tier 2 — TrueNAS
NFS", separate from the `truenas-nfs` k8s PVC tier (`storage/k8s/vols`) and
`truenas-backup` (`storage/backup`, covers §6). This VM is simply the first
thing to use it. Nothing left to provision here — §8 Phase 0's storage item
is done, not outstanding.

### Alternatives considered

- **ZFS storage replication** (disk stays local-per-node, Proxmox replicates
  to a target on a schedule): no NAS dependency, but introduces replication
  lag (minutes) and a "promote replica, start VM" procedure instead of a
  single migrate command. Rejected — the NAS is already a proven dependency
  for this cluster (RWX PVCs), and live migration is materially simpler to
  operate correctly under pressure than a replica-promotion runbook.
- **Backup/restore only** (`vzdump` to NAS, restore on the new host to move):
  simplest, but real downtime (minutes) and loses everything since the last
  backup. Rejected as the *mobility* mechanism, but this is exactly what
  §6 uses for the separate backup requirement.

## 5. Design

### 5.1 Terraform: `terraform/dev-vm-node.tf`

Flat vars (single VM — not a list; this box has no HA requirement):

```hcl
dev_vm_node             = "pve5"
dev_vm_name             = "devbox"
dev_vm_id               = 111
dev_vm_cores            = 8
dev_vm_memory           = 32768   # 32GB
dev_vm_disk_size        = 300     # 300GB, on the new NFS datastore
dev_vm_ip               = "192.168.1.56/24"
dev_vm_gateway          = "192.168.1.254"
dev_vm_user             = "dev-admin"
dev_vm_ssh_public_key   = "ssh-ed25519 AAAA..."   # same key already used for the GPU VM
dev_vm_storage_pool     = "truenas-vmdisks"       # existing cluster-wide NFS storage, see §4
```

Placement: **pve5**, not pve1 as originally planned. Phase 0's pre-flight
(2026-08-09, live Proxmox API query) found pve1 has only **28.2GiB total
RAM**, with `minecraft-server` (4GB), `haproxy-1` (1GB), and
`talos-worker-01` (16GB) already running — ~21GiB allocated, ~5.5GiB free.
An 8c/32GB VM cannot fit there at all, regardless of what else is idle.
pve2/pve3/pve4 are excluded per requirement #4 (control plane) and are
undersized anyway (~12.6GiB each). **pve5** is the only remaining
non-control-plane host: 123.5GiB total, ~42GiB free at check time — enough
for this VM with headroom, though it already carries the GPU inference VM
and is a live K8s worker, so it is not as idle a box as pve1 was assumed to
be. **Pre-flight before `apply`**: re-check pve5's free memory, since it
carries live cluster workload that can shift.

Identity: vmid `111` (next free after haproxy's `110`), IP `192.168.1.56/24`
— adjacent to haproxy's `.55`, in the same below-`.64` reserved block as the
GPU VM's `.50`. **Pre-flight**: verify both are still unclaimed (ARP scan)
before `apply`.

Resources, following the `gpu-node.tf` pattern:

- `proxmox_virtual_environment_download_file` — Ubuntu 24.04 cloud image,
  `content_type = "import"`, same API-import path (avoids needing an
  ssh-agent reachable from the Windows workstation).
- `proxmox_virtual_environment_vm` — disk on `var.dev_vm_storage_pool`
  (the new NFS datastore, not `local`/`local-lvm`), virtio NIC on `vmbr0`,
  `agent { enabled = true }`, `on_boot = true`, cloud-init `initialization`
  block (static IP, user, SSH key), tags `["dev", "workstation"]`.

### 5.2 Cloud-init (day-0 only)

Matches the haproxy design's rule: cloud-init carries only what's needed for
the VM to exist and be reachable, not the full dev toolchain — a second,
drifting config-management path is worse than a short manual bootstrap step.

```yaml
#cloud-config
hostname: devbox
users:
  - name: dev-admin
    groups: [sudo]
    shell: /bin/bash
    sudo: "ALL=(ALL) NOPASSWD:ALL"
    ssh_authorized_keys:
      - ${ssh_public_key}
package_update: true
packages:
  - qemu-guest-agent
  - git
  - build-essential
  - ca-certificates
runcmd:
  - systemctl enable --now qemu-guest-agent
```

### 5.3 Post-boot bootstrap (manual, one-time)

SSH in once cloud-init completes, then:

1. Install dev tooling: Docker Engine, Node/pnpm (nvm), `gh` CLI, Claude Code
   CLI, `kubectl`/`talosctl`/`helm`, `sops`/`age`.
2. Sync credentials/dotfiles from the Windows workstation: age key (the
   existing dual-recipient re-key setup already anticipates a second
   machine), SSH keys, git config, shell dotfiles.
3. Fresh auth on the VM rather than copying tokens: `gh auth login`, Claude
   Code login.
4. Connect from the workstation via VS Code Remote-SSH.

No new automation proposed here — this is a documented manual sequence, not a
script committed to this repo, since it's a one-time personal-environment
setup rather than reproducible infrastructure.

## 6. Backup

Nightly `vzdump` to NAS-backed storage via a Proxmox Backup Job (Datacenter
UI), ~7-day retention. Proxmox-level scheduled job, not a Terraform resource
— same category of one-time human config as §4's storage backend. This is
independent of §4's live-migration storage: it protects against breaking the
dev environment itself (bad `rm`, botched upgrade), not host loss.

## 7. Networking & security

LAN-only. SSH inbound from the workstation's subnet; no port-forward, no
public exposure — same posture as every other VM in this repo. Outbound
internet needed for package registries (apt, npm/pnpm, Docker Hub, GitHub).

## 8. Implementation phases

| Phase | Deliverable | Exit criteria |
|---|---|---|
| 0 | Node capacity check; `.56`/vmid `111` confirmed free; NFS storage backend | **Done 2026-08-09**: pve1 ruled out (28.2GiB total, ~5.5GiB free), pve5 selected (~42GiB free); vmid `111` and `.56` confirmed unclaimed; `truenas-vmdisks` NFS storage already existed cluster-wide (§4 correction) — no new backend needed |
| 1 | `terraform/dev-vm-node.tf` + vars + tfvars entry | Done: `terraform validate`/`fmt` clean, `dev_vm_ssh_public_key` in vault. `terraform plan` clean; human `terraform apply` still pending |
| 2 | Cloud-init verified (SSH reachable, guest agent running) | `qm agent 111 ping` OK |
| 3 | Bootstrap script run (§5.3): tooling installed | `docker`, `gh`, `claude`, `kubectl`, `talosctl`, `sops` all resolve |
| 4 | Credential/dotfile sync from Windows | age hydrate works on VM; `gh auth status` OK |
| 5 | Backup job configured (§6) | first `vzdump` run completes, visible on NAS |
| 6 | Migration proven | `qm migrate 111 <target> --online` and back; downtime measured; runbook written (`scenarios/dev-vm-migrate.md`) — target host TBD, see open question 2 |

## 9. Open questions

1. ~~Exact NFS export path/options on TrueNAS~~ — **resolved 2026-08-09**:
   `truenas-vmdisks` (dataset `storage/proxmox`) already exists as a
   dedicated export, separate from the k8s PVC tier (`storage/k8s/vols`).
   No action needed; see §4.
2. **Target migration test host** — with the VM now living on pve5 (§5.1),
   the Phase 6 round-trip needs a different target. pve1 is the only other
   non-CP host but currently has no headroom (§5.1); this needs pve1 freed
   up (e.g. relocating `minecraft-server`) or a fresh capacity check before
   Phase 6, not locked here.
3. **pve1's API proxy to pve5 returns "no route to host"** — found while
   verifying pve5's storage during Phase 0 (querying `pve5` through pve1's
   API works for cluster-wide/cached data but a live per-node proxy call
   failed; querying pve5's own API directly at `192.168.1.204:8006` worked
   fine). Unclear if transient or a standing issue with pve1↔pve5
   connectivity specifically — worth checking before relying on pve1 as the
   API entry point for pve5-targeted operations (e.g. `talops`, migration).
