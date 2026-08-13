# HAProxy VM Provisioning — Design

Status: **Phase 1 applied, Phase 2 in the repo.** `terraform/haproxy-node.tf`
was applied and the rebuild/cutover in `scenarios/haproxy-vm-rebuild.md`
executed on 2026-08-09 (commit `568b368`) — the production load balancer is
`haproxy-1`, Terraform-managed, not the hand-built `haproxy-0` this document
originally described as still live. That commit updated the vaulted tfvars
and bootstrap-state but not this document's own status line, which is the
correction being made here now (found while re-verifying live state for
JDWLABS-285, a separate, DNS-focused ticket — recorded here because this is
where the stale claim lived, not because that ticket's work touches
provisioning). `talops haproxy status` against `192.168.1.199` is the live
source of truth if this drifts again. Phase 3 (keepalived) remains
unscheduled — see §7.

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

- `internal/haproxy`: `Stats()` reads backend health over SSH from the runtime
  socket (`show stat` → CSV → parse) and `Diff()` compares the freshly rendered
  config against the deployed file. Column positions come from the CSV header
  rather than fixed indices — HAProxy inserts columns between releases, and a
  misaligned `status` column would report health belonging to another field.
- Record the pushed config hash in bootstrap-state. It is a hint about what
  talops last pushed, not the deployed config: it cannot see an edit made on
  the host, so drift is always confirmed against the live file when SSH is
  available and reported as `unknown` when it is not.
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

## 7. Open questions — resolved or deferred

1. **HA pair now or later? — DEFERRED, owner: repo maintainer.** Not decided
   here. The rebuild below **has** now been executed (2026-08-09), so the
   input this deferral was waiting on — a measured outage window rather than
   an estimated one — exists if someone captures it from that cutover's
   timestamps; it was not captured at the time, so the window is still
   effectively unmeasured. Revisit by pulling the stop/start timestamps from
   that day rather than re-running a cutover just to measure it. Nothing in
   phases 1–2 forecloses the pair — the
   tfvars list takes a second element, `net.ipv4.ip_nonlocal_bind` is already
   set by cloud-init so a VIP bind needs no rebuild, and `--host` already
   targets an instance by address rather than assuming one. Revisit once the
   rebuild has run once and the real window is known.

2. **Snippets datastore — RESOLVED, no work needed.** Every node's `local`
   directory storage already advertises the `snippets` content type
   (`content=backup,iso,vztmpl,snippets,import`), verified against live
   Proxmox. The NFS option is rejected regardless: putting the snippet on the
   NAS would make a load-balancer rebuild depend on the NAS being up, and a
   rebuild is exactly the situation where that assumption is least safe.

3. **Placement — RESOLVED: pve1.** Control planes sit on pve2, pve3, and pve4,
   so the design's own sketch (`node_name = "pve3"`) contradicted its
   requirement that one host failure must not take a control plane *and* the
   load balancer. pve1 runs no control plane and already hosts the existing
   load balancer. pve5 is the equivalent alternative for a future peer.

4. **SSH user cutover — RESOLVED: bundle it with the swap.** Cloud-init creates
   `haproxy-admin` and sets `disable_root: true`, so keeping `root` in tfvars
   would leave the config push authenticating as a user the new VM does not
   have. The runbook's cutover step changes `haproxy_login_user` in the same
   tfvars edit as the address. Keeping `root` for a smaller diff was rejected:
   it trades a one-line change for a root login on the host that fronts the
   API.

5. **OS choice — RESOLVED: Ubuntu 24.04.** It matches the GPU VM precedent, so
   there is one cloud-image path in this repo rather than two. Image size is
   not a constraint worth a second OS family for a 10 GiB single-purpose VM.

6. **`talops up` ordering — DEFERRED, owner: repo maintainer.** Unchanged by
   this work and deliberately not bundled: `talops up` is the full-cluster
   bootstrap path, and adding a bounded wait to its HAProxy preflight is a
   change to cluster bootstrap, not to load-balancer provisioning. Until then
   the ordering is a documented sequence, not an automated one — the runbook
   provisions and verifies the load balancer before anything depends on it.
   The scope of the eventual fix is known: the reconcile preflight must
   tolerate the ~60 s cloud-init window when the VM was created in the same
   apply.
