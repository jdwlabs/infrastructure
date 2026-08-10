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

**Add the existing TrueNAS NAS (`192.168.1.205`, 35TB RAIDZ2 — already serving
k8s RWX PVCs) as an NFS storage backend in Proxmox's datacenter config, and
put this VM's disk on it.** With the disk on shared storage reachable from
every pve host, moving the VM is `qm migrate <vmid> <target-node> --online`
(or the Proxmox UI) — only VM RAM state transfers, since the disk doesn't
move. Seconds of downtime.

This is a Proxmox-level config change (`pvesm add nfs ...` or the Datacenter
UI), not a Terraform resource — the `bpg/proxmox` provider manages VMs and
files on existing storage, not the storage backends themselves. It is a
one-time human prerequisite, done once for the whole cluster.

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
dev_vm_node             = "pve1"
dev_vm_name             = "devbox"
dev_vm_id               = 111
dev_vm_cores            = 8
dev_vm_memory           = 32768   # 32GB
dev_vm_disk_size        = 300     # 300GB, on the new NFS datastore
dev_vm_ip               = "192.168.1.56/24"
dev_vm_gateway          = "192.168.1.254"
dev_vm_user             = "dev-admin"
dev_vm_ssh_public_key   = "ssh-ed25519 AAAA..."   # same key already used for gpu/haproxy
dev_vm_storage_pool     = "nfs-nas"               # new Proxmox storage id from §4
```

Placement: **pve1**. No control plane there; already hosts `talos-worker-01`
(vmid 300) and the planned `haproxy-1` (vmid 110, `.55`) — consistent
placement class, no new precedent. **Pre-flight**: confirm pve1 has headroom
for +8c/32GB/300GB before `apply` (not assumed from this document alone).

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
| 0 | Human: NFS storage backend added to Proxmox (§4); pve1 capacity check; `.56`/vmid `111` confirmed free | `pvesm status` shows the new datastore on every node; ARP scan clean |
| 1 | `terraform/dev-vm-node.tf` + vars + tfvars entry | `terraform plan` clean; human `terraform apply` |
| 2 | Cloud-init verified (SSH reachable, guest agent running) | `qm agent 111 ping` OK |
| 3 | Bootstrap script run (§5.3): tooling installed | `docker`, `gh`, `claude`, `kubectl`, `talosctl`, `sops` all resolve |
| 4 | Credential/dotfile sync from Windows | age hydrate works on VM; `gh auth status` OK |
| 5 | Backup job configured (§6) | first `vzdump` run completes, visible on NAS |
| 6 | Migration proven | `qm migrate 111 pve5 --online` and back; downtime measured; runbook written (`scenarios/dev-vm-migrate.md`) |

## 9. Open questions

1. **Exact NFS export path/options on TrueNAS** — reuse the existing RWX PVC
   export or a new dedicated one for Proxmox VM images? Deferred to Phase 0
   execution; either works, a dedicated export keeps VM-image traffic
   separate from k8s PVC traffic for easier troubleshooting.
2. **Target migration test host** — pve5 assumed in §8 (has headroom, already
   proven as a non-CP host); confirm at Phase 6 rather than locking it here.
