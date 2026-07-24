# HAProxy VM Provisioning — Design

Status: **proposed** (design only — no implementation yet)

Automating the provisioning of the HAProxy load-balancer VM that fronts the
Kubernetes API, Talos API, and cluster ingress. Today the VM is the only piece
of the cluster's infrastructure that is not reproducible from this repo.

## 1. Current state

### What exists

| Layer | How it is managed today |
|---|---|
| HAProxy **VM** (single, `192.168.1.199`) | **Manual.** Created by hand in the Proxmox UI (OS install, package install, static IP, SSH key). No Terraform resource, no runbook, no record of its build. |
| `haproxy.cfg` | **Fully automated** by `talops`. `bootstrap/internal/haproxy/config.go` renders the config from cluster state (CP backends for 6443/50000, ingress NodePort backends for 80/443, stats on 9000, optional admin CIDR allowlist). |
| Config push | `bootstrap/internal/haproxy/client.go` SSHes to the VM (`haproxy_login_user`, currently `root`), writes via base64, backs up, validates (`haproxy -c`), installs, reloads — with automatic rollback on validation failure and retry on SSH connection errors. |
| Reconciliation | `talops bootstrap`/`reconcile` regenerate and push the config whenever CP/worker membership or IPs change (ARP-based IP rediscovery already handles DHCP churn). SSH connectivity to the VM is a preflight check. |
| Identity | `haproxy_ip` in `terraform.tfvars` (SOPS-vaulted) is consumed **only** by `talops` — Terraform provisions nothing for it. DNS (`cluster.jdwlabs.com`) points at the IP; `talosconfig` endpoints and kubeconfig go through it. |

### What this means

- **Day-1+ operations are solved.** Backend drift when nodes change is already
  reconciled automatically. This design does not need to re-solve config
  management — it must *preserve* it.
- **Day-0 is the gap.** If the VM dies (disk loss, accidental delete, Proxmox
  host failure), there is no automated or even documented path to rebuild it.
  Recovery would be an ad-hoc manual rebuild while the cluster API and all
  ingress are dark.
- The VM is a single point of failure for the API endpoint *and* all HTTP(S)
  ingress. (Workloads keep running during an outage; nothing can reach them.)

### Precedents in this repo

- All Talos VMs and the GPU inference VM are declared in flat Terraform using
  the `bpg/proxmox` provider (`~> 0.99.0`), applied by a human via
  `talops infra deploy` / `terraform apply` (autonomous apply is prohibited by
  repo policy).
- `terraform/gpu-node.tf` already demonstrates the exact pattern needed here:
  a non-Talos Ubuntu VM built from a cloud image
  (`proxmox_virtual_environment_download_file` with `content_type = "import"`)
  plus a cloud-init `initialization` block (static IP, user, SSH key).
- Secrets (tfvars, SSH material config) flow through the SOPS+age vault;
  `talops` auto-hydrates/seals.

## 2. Requirements

1. **Reproducible day-0**: the VM must be recreatable from git + vault alone —
   VM shell, OS, packages, static IP, SSH access, and a valid initial
   `haproxy.cfg` — with no console interaction.
2. **Idempotent & reviewable**: re-running provisioning against an existing,
   healthy VM is a no-op; changes are visible as a plan before apply.
3. **Preserve existing config reconciliation**: node add/remove and CP IP
   changes must keep flowing through the current `talops reconcile` path
   unchanged.
4. **Human-gated mutations**: consistent with repo policy, anything that
   creates/destroys VMs goes through a plan → human apply gate. Config-only
   pushes (already automated today) stay automated.
5. **Static identity**: the VM keeps a static IP (DHCP flips on the CP nodes
   have already caused outages; the LB must never join that class of problem).
6. **AXI-compliant surface**: any new `talops` subcommands follow AXI (TOON
   stdout, structured errors to stdout, exit 0/1, no prompts, `help[]` lines).
7. **No new secret stores**: SSH keys and stats credentials stay in the
   existing tfvars/SOPS flow.
8. **Leave a door open for HA** (keepalived VIP pair) without redesign.

Non-goals: replacing HAProxy (kube-vip et al. were already rejected — see
ARCHITECTURE.md "Why HAProxy Instead of kube-vip?"); managing DNS.

## 3. Options

### Option A — talops-native provisioning (Proxmox API + cloud-init)

`talops haproxy provision` talks to the Proxmox HTTP API directly (new Go
client): clone/create VM, attach cloud-init drive, start, wait for SSH.

- **Pros**: single tool owns the whole lifecycle; no Terraform coupling;
  could later fold into `talops up`.
- **Cons**:
  - Duplicates what Terraform already does for every other VM in the fleet —
    a second, hand-rolled idempotency/state layer (VM exists? disk resized?
    NIC changed?) that Terraform gives us for free.
  - talops currently has **no Proxmox HTTP API client at all** (its Proxmox
    access is SSH `qm`/ARP for discovery); this is a large new surface with
    auth, error, and drift semantics to own forever.
  - Splits infrastructure state: every other VM is in the Terraform remote
    state; this one would live in bootstrap-state JSON.
  - Violates the spirit of the human-gated `terraform plan/apply` flow — we
    would have to rebuild an equivalent plan/approve gate by hand.

### Option B — Terraform (existing flat config) + cloud-init; talops orchestrates (recommended)

Declare the VM in `terraform/haproxy-node.tf` following the `gpu-node.tf`
pattern: Ubuntu cloud image import + cloud-init. Day-0 configuration
(packages, user, hardening, placeholder `haproxy.cfg`) rides in a cloud-init
user-data snippet uploaded via `proxmox_virtual_environment_file`. The human
applies via the existing `talops infra deploy` gate. After first boot,
`talops reconcile` pushes the real generated config exactly as it does today.
A new read/config-only `talops haproxy` command group adds visibility and a
standalone config-push path.

- **Pros**:
  - Matches every repo convention: flat Terraform, bpg provider, cloud-init
    precedent, remote state, human-gated apply, SOPS-vaulted tfvars.
  - Idempotency, drift detection, and plan/review are inherited from
    Terraform — near-zero new state machinery in talops.
  - VM count is trivially parameterizable (list of VM objects), which is the
    HA door-opener.
  - Smallest new code surface: one `.tf` file, one cloud-init template, one
    Cobra command group that mostly reuses `internal/haproxy`.
- **Cons**:
  - Two-step day-0 (human `infra deploy`, then `reconcile`) — mitigated by
    `talops up`, which already runs both.
  - Cloud-init snippets need a snippets-enabled datastore (`local-lvm`
    cannot hold them — see open questions).

### Option C — stay manual, add a runbook

Document the current VM's build in `scenarios/haproxy-vm-rebuild.md`.

- **Pros**: zero code; captures tribal knowledge immediately.
- **Cons**: not idempotent, not tested until the day it is needed, drifts
  from reality, recovery time stays human-speed during a full-ingress
  outage. Fails requirements 1–2.

### Tradeoffs summary

| | A: talops-native | B: Terraform + cloud-init | C: manual runbook |
|---|---|---|---|
| Idempotency / drift | build ourselves | inherited from Terraform | none |
| Plan/approve gate | build ourselves | existing `infra deploy` flow | n/a |
| New code in talops | large (Proxmox API client) | small (command group) | none |
| State location | bootstrap-state JSON (split) | Terraform remote state (unified) | nowhere |
| Repo convention fit | weak | strong (`gpu-node.tf` precedent) | weak |
| HA extension | manual loop logic | `for_each` over VM list | copy-paste |
| Recovery confidence | medium | high (exercised by plan on every change) | low |

## 4. Recommendation

**Option B.** Terraform owns the VM shell (as it does for all nine other VMs),
cloud-init owns first-boot OS configuration, and talops keeps owning the
`haproxy.cfg` lifecycle it already handles well. A thin `talops haproxy`
command group adds the missing observability and a standalone config-push
entry point. Option A re-implements Terraform inside talops for one VM;
Option C fails the reproducibility requirement outright.

A minimal runbook still gets written (migration + rebuild steps), but as the
*driver* of the automated path, not a substitute for it.

## 5. Design

### 5.1 Terraform: `terraform/haproxy-node.tf`

New tfvars (vaulted like everything else):

```hcl
haproxy_vms = [
  {
    node_name = "pve3"          # avoid co-locating with a majority of CPs
    vm_name   = "haproxy-1"
    vmid      = 110
    cpu_cores = 2
    memory    = 1024
    disk_size = 10
    ip        = "192.168.1.199/24"
  },
]
haproxy_gateway        = "192.168.1.254"
haproxy_ssh_public_key = "ssh-ed25519 AAAA..."
```

Resources (per VM, `for_each` over `haproxy_vms`):

- `proxmox_virtual_environment_download_file` — Ubuntu 24.04 cloud image,
  `content_type = "import"` (same API-import path the GPU VM uses, avoiding
  the provider's node-SSH importdisk requirement).
- `proxmox_virtual_environment_file` — rendered cloud-init user-data snippet
  (see 5.2) on a snippets-enabled datastore.
- `proxmox_virtual_environment_vm` — cloud image disk, virtio NIC on
  `vmbr0`, `agent { enabled = true }`, `on_boot = true`,
  `initialization { ip_config { ... } user_data_file_id = ... }`,
  tags `["haproxy", "loadbalancer"]`.

The list-of-objects shape means a keepalived pair later is "add a second
element", not a refactor.

### 5.2 Cloud-init user-data (day-0 only)

Rendered from a `templatefile()` with the VM's hostname, user, and key.
Custom user-data replaces the Proxmox-generated config entirely, so it must
carry hostname and user creation itself:

```yaml
#cloud-config
hostname: ${hostname}
users:
  - name: haproxy-admin
    groups: [sudo]
    shell: /bin/bash
    sudo: "ALL=(ALL) NOPASSWD:ALL"
    ssh_authorized_keys:
      - ${ssh_public_key}
package_update: true
packages:
  - qemu-guest-agent
  - haproxy
runcmd:
  - systemctl enable --now qemu-guest-agent
  - systemctl enable haproxy
```

Deliberately **no** haproxy.cfg content here: the placeholder distro config
is enough for the service to exist; the first `talops reconcile` pushes the
real config through the existing validated/rollback path. Keeping cluster
topology out of cloud-init avoids a second config-rendering path that would
drift from `internal/haproxy/config.go`.

Note the user is `haproxy-admin`, not `root` — the SSH client already runs
everything through `sudo`, so only `haproxy_login_user` in tfvars changes.

### 5.3 talops changes

No change to the reconcile flow. Additions:

- `internal/haproxy`: add a `Status()` that reads backend health over SSH
  from the stats socket (`echo "show stat" | socat /run/haproxy/admin.sock -`
  → CSV → parse), and a `Diff()` that compares the freshly rendered config
  against the deployed file's hash.
- Record the deployed config hash in bootstrap-state so drift is detectable
  without SSH round-trips when nothing changed.
- New Cobra command group `talops haproxy` (below). VM *provisioning* is not
  a talops subcommand mutation — the VM is Terraform-managed, so
  `talops infra plan` / `infra deploy` (human-gated) covers it, and
  `talops haproxy status` reports the VM layer read-only.

### 5.4 Command surface (AXI)

All three commands: TOON on stdout, structured errors on stdout, exit 0
success / 1 error, never prompt, `--json` escape hatch, concise `--help`.

`talops haproxy status` — one-shot health of every layer (VM → SSH →
service → backends → config drift), with pre-computed aggregates:

```
haproxy:
  host: 192.168.1.199
  vm: {vmid: 110, node: pve3, state: running, source: terraform}
  ssh: ok
  service: active
  configDrift: false
backends[5]{name,addr,status,lastChk}:
  talos-cp-201,192.168.1.21:6443,UP,L4OK
  talos-cp-202,192.168.1.22:6443,UP,L4OK
  talos-cp-203,192.168.1.23:6443,UP,L4OK
  ingress-301,192.168.1.31:30543,UP,L4OK
  ingress-302,192.168.1.32:30543,UP,L4OK
backendsUp: 5/5
help[2]:
  talops haproxy plan  # preview config changes without touching the VM
  talops infra status  # VM-layer deployment state
```

Definitive empty/error states, e.g. VM missing:

```
haproxy:
  host: 192.168.1.199
  vm: {state: not_found, source: terraform}
error: {code: vm_not_provisioned, msg: "no HAProxy VM in Terraform state — run: talops infra plan"}
help[1]:
  talops infra plan  # review VM provisioning changes, then human-approved deploy
```

`talops haproxy plan` — render config from current cluster state, diff
against deployed, print unified diff (truncated with size hint if large),
exit 0 whether or not drift exists (`drift: true/false` is the signal):

```
haproxy:
  host: 192.168.1.199
  drift: true
diff: |
  ...unified diff, capped... (truncated, 2841 chars total — use --full)
help[1]:
  talops haproxy apply  # push this config (validated, auto-rollback)
```

`talops haproxy apply` — standalone config push using the exact
`Update()` path reconcile uses (validate → install → reload → rollback on
failure). Idempotent: no drift → no-op with explicit `changed: false`.
This gives operators/agents a config-only lever without running a full
reconcile.

### 5.5 High availability (phased, not in scope for first implementation)

Single-VM remains a SPOF for API + ingress. Two candidate end-states:

1. **Single VM, fast rebuild (phase 1 result)**: outage window = human apply
   (`infra deploy`) + first reconcile — minutes, from nothing. Cheapest;
   accepts brief ingress downtime on VM loss.
2. **keepalived VRRP pair (phase 3 option)**: two VMs on different Proxmox
   hosts, `haproxy_ip` becomes the VIP, instances get their own IPs.
   Changes required: cloud-init adds keepalived + VRRP config (priority,
   password from tfvars); `haproxy.cfg` binds must move from the VIP address
   to `*` or use `net.ipv4.ip_nonlocal_bind`; talops pushes config to **all**
   instances (loop over per-instance IPs) instead of one host.

The tfvars list shape, the `bind` strategy, and the multi-host push loop are
the only three touchpoints — all are called out above so phase 1 does not
paint us into a corner. Whether the pair is worth ~1 GiB RAM and two more
managed hosts on this cluster is an open question (below).

### 5.6 Migration of the existing VM

The live `.199` VM predates IaC. Two paths:

- **Blue-green rebuild (recommended)**: provision `haproxy-1` (new VMID) via
  the new Terraform with a temporary free IP; verify with
  `talops haproxy status --host <temp-ip>` + a manual backend smoke test;
  then stop the old VM, re-apply with `ip = 192.168.1.199` (or swap statically),
  run `talops reconcile`, confirm, delete the old VM after a soak period.
  DNS, talosconfig, and kubeconfig never change. Proves the rebuild path
  works — which is the entire point of this design.
- **`terraform import` of the live VM**: keeps history but imports years of
  hand-made drift, and never exercises day-0 — the recovery path stays
  unproven. Not recommended.

Expected API/ingress blip during cutover: seconds (bounded by the stop/start
and ARP settle), schedulable in a quiet window.

## 6. Implementation phases

| Phase | Deliverable | Exit criteria |
|---|---|---|
| 1 | `haproxy-node.tf` + cloud-init snippet + tfvars schema (+ vault update); snippets datastore enabled | `terraform plan` clean on a fresh VM definition; blue-green rebuild executed; old VM deleted; `talops reconcile` green against the new VM |
| 2 | `talops haproxy status\|plan\|apply` (AXI), stats-socket parsing, config-hash drift in bootstrap-state | Commands pass AXI review (TOON, exit codes, help[], no prompts); reconcile refactored to call the shared apply path; unit tests follow existing `client_test.go` mock-runner pattern |
| 3 (optional) | keepalived pair per 5.5 | VIP failover test: kill active VM, API+ingress recover < 5 s, `talops haproxy status` shows both instances |
| Docs | ARCHITECTURE.md HAProxy section updated; `scenarios/haproxy-vm-rebuild.md` runbook | Runbook is a thin driver of the automated path |

## 7. Open questions

1. **HA pair now or later?** Is ~1 GiB RAM + one more managed VM worth
   removing the ingress/API SPOF, given host RAM is already tight
   (CP resize just rebalanced memory)? Phase 3 is designed but not scheduled.
2. **Snippets datastore**: cloud-init user-data snippets cannot live on
   `local-lvm`. Enable `snippets` content on each node's `local` dir storage,
   or add it to the `truenas-vmdisks` NFS storage (cluster-wide, one place)?
   NFS is tidier but couples LB rebuild to the NAS being up.
3. **Placement**: which Proxmox host gets `haproxy-1` (and the phase 3 peer)?
   Should it avoid hosts running CP VMs so one host failure cannot take a CP
   *and* the LB?
4. **SSH user cutover**: switching `haproxy_login_user` from `root` to
   `haproxy-admin` changes a vaulted tfvars value at cutover time — bundle it
   with the blue-green swap, or keep `root` in cloud-init for a smaller diff?
5. **OS choice**: Ubuntu 24.04 (matches GPU VM precedent, larger image) vs
   Debian 13 (smaller, HAProxy-current). Default assumption: Ubuntu 24.04
   for consistency.
6. **Should `talops up` provision the LB before the Talos VMs?** Bootstrap
   ordering today assumes the LB already exists (SSH preflight). If the VM is
   in the same Terraform config, `infra deploy` creates it in the same apply —
   verify the reconcile preflight tolerates the ~60 s cloud-init window or
   add a bounded wait.
