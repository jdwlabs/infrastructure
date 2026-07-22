# Runbook: Worker rebalance onto pve5 (scale out, retire small workers)

Status: DEFERRED (2026-07-21) — do not execute. The worker-capacity problem
this plan targeted was resolved by right-sizing (honest requests, QoS floors,
priorityClasses), relocating monitoring to the pve5 worker, and storage
cleanup: the biggest worker runs ~16% live, cluster requests ~19% of
allocatable. The remaining hot spot is the control planes, which this
worker-only plan cannot help — see `scenarios/cp-memory-resize.md`. The
former CI-node reservation on pve5 is void (self-hosted ARC runners are
dormant; CI is exclusively GitHub-hosted), leaving pve5 earmarked for the
GPU/AI node. Kept as reference for a future genuine worker-capacity need;
the guardrails below remain the standard for any such execution.

If ever executed: every `terraform apply` in this runbook is run by a
human. The agent contract forbids autonomous applies. Workers only — this
runbook never touches a control-plane node.

## Why

Placement, not total capacity, is the cluster's binding constraint. Cluster
live memory use is ~22Gi total, yet the three 4Gi workers run 60–77% memory
(~1–1.6Gi schedulable headroom each) while pve5 — the largest host, already
running one 64Gi worker at ~11% — sits mostly idle. Pods with ordinary 1–2Gi
requests cannot land on the small workers, so the scheduler is starved for
placement options on a cluster that is three-quarters empty.

Fix: add two larger workers on pve5, then retire two of the three small
workers one at a time, keeping one for physical-host spread. Workers are
cattle with node-local storage — replacement is offline VM create/destroy;
Longhorn rebuilds replicas onto the new disks.

## Sources of truth — resolve identity live

- **Worker specs** (`talos_worker_configuration`): the SOPS-encrypted
  `terraform/terraform.tfvars.enc.yaml` is authoritative. Hydrate with
  `talops secrets hydrate`, edit the plaintext working copy, let `talops`
  auto-seal. `terraform/terraform.tfvars.example` documents the shape only.
- **Host inventory (HBOM)**: query Proxmox live —
  `pvesh get /nodes` and `pvesh get /cluster/resources --type vm` — for
  host RAM, free memory, and existing VM allocations. Doc tables go stale.
- **Node identity**: never trust vmids or IPs written in docs (DHCP-lease
  flips have broken this cluster before). Before every step, re-derive the
  mapping live: `kubectl get nodes -o wide` (k8s node ↔ IP),
  `talosctl get members` (Talos view), and Proxmox guest-agent info
  (VM ↔ IP). Refer to workers by their tfvars `vm_name`; resolve the rest
  at execution time.

## Topology

Sizes below come from the encrypted tfvars at time of writing — re-derive
before executing.

Before:

| vm_name         | host | memory | note                         |
|-----------------|------|--------|------------------------------|
| talos-worker-01 | pve1 | 16Gi   |                              |
| talos-worker-02 | pve2 | 4Gi    | 60–77% used, retire          |
| talos-worker-03 | pve3 | 4Gi    | 60–77% used, retire          |
| talos-worker-04 | pve4 | 4Gi    | kept for host spread         |
| talos-worker-05 | pve5 | 64Gi   | ~11% used; right-size last   |

After (Option A):

| vm_name         | host | memory | cores | disk  |
|-----------------|------|--------|-------|-------|
| talos-worker-01 | pve1 | 16Gi   | (as-is) | (as-is) |
| talos-worker-04 | pve4 | 4Gi    | (as-is) | (as-is) |
| talos-worker-05 | pve5 | 32Gi   | (as-is) | (as-is) |
| talos-worker-06 | pve5 | 16Gi   | 6     | 200G  |
| talos-worker-07 | pve5 | 16Gi   | 6     | 200G  |

New-worker sizing rationale:

- 16Gi each ≈ 4–5× the schedulable capacity of a small worker; the pair
  (32Gi) exceeds the entire cluster's current live use (~22Gi).
- 6 cores each is conservative on the 16C/32T host; CPU is not the
  constraint.
- 200G disk exceeds the 158.7GB the small workers expose to Longhorn, so
  every replica evicted from a retiring node fits on a new one.

pve5 co-tenancy budget (the reason Phase 3 is mandatory, not optional):
pve5 is ~123GB OS-visible and also carries the GPU inference VM (32Gi
dedicated) plus future CI-runner and GPU/AI-node plans. Reserve ≥48GB for
that co-tenant tier, capping Talos worker allocation on pve5 at ~75GB.
After Phase 3 the workers total 64Gi (32+16+16) — under the cap. Between
Phase 1 and Phase 3 the host is nominally over-allocated (64+32+32+16Gi
host/co-tenant demand vs 123GB); tolerable only because actual usage is a
fraction of that — do not park there.

### Option B — add-only, keep worker-05 at 64Gi (rejected)

Permanently over-allocates pve5 and consumes the co-tenant reservation;
the first serious memory consumer on worker-05 or the GPU VM turns nominal
overcommit into host OOM.

### Option C — retire all three small workers (rejected)

Leaves worker hosts = pve1 + pve5 only. Longhorn's 3-replica placement is
per-node, so replica spread would stop implying multi-host durability, and
a pve5 failure would take most of the worker pool.

## Preconditions — hard gates

Re-verify **all** of these immediately before every phase step, not once at
the start:

1. **Longhorn clean**: zero degraded or faulted volumes, no rebuilds in
   flight — `kubectl -n longhorn-system get volumes.longhorn.io` shows
   every attached volume `healthy`.
2. **etcd quorum intact**: `talosctl -n <cp-ip> etcd status` healthy 3/3,
   single leader (worker churn shifts load; never proceed on a degraded
   control plane).
3. **≥3 schedulable workers at all times**: count Ready, uncordoned
   workers excluding the node about to be drained; must be ≥3.
4. **Replica safety**: the node about to be drained must not hold the only
   healthy replica of any volume. Check
   `kubectl -n longhorn-system get replicas.longhorn.io -o wide` grouped
   by volume; do not rely on the drain to catch this (Longhorn's
   instance-manager PDB should block, but verify first — never force past
   it).
5. **Cluster quiet**: ArgoCD apps Healthy/Synced, no in-flight upgrades,
   Vault unsealed with the auto-unseal CronJob green.
6. **Host-spread guard (before retiring the second small worker)**: with
   three workers on pve5, Longhorn's per-node anti-affinity can place all
   three replicas of a volume on one physical host. Label workers with
   `topology.kubernetes.io/zone=<proxmox-host>` and set Longhorn's replica
   zone anti-affinity (platform repo) so 3-replica volumes span pve1 /
   pve4 / pve5. Do not retire the second small worker until this is in
   place.

## Sequence

One node per step; re-run all gates and soak ~15 minutes between steps.

### Phase 1 — add talos-worker-06 and talos-worker-07 (pve5)

1. `talops secrets hydrate`, then append two entries to
   `talos_worker_configuration` in `terraform/terraform.tfvars`:
   `node_name = "pve5"`, `vm_name = "talos-worker-06"` / `"talos-worker-07"`,
   `cpu_cores = 6`, `memory = 16384`, `disk_size = 200`, and a `vmid` for
   each chosen as the next free IDs in the worker range — verify against
   both the current tfvars and live Proxmox (`pvesh get /cluster/resources
   --type vm`) before picking.
2. `terraform plan` — review: exactly two `create` actions, both
   `proxmox_virtual_environment_vm.worker`, nothing else.
3. **HUMAN**: `terraform apply` the reviewed plan.
4. `talops reconcile` — joins the new nodes and updates HAProxy backends.
5. Verify join: both nodes `Ready` in `kubectl get nodes`; present in
   `talosctl get members`; Longhorn shows both nodes schedulable with
   ~200G disks (`kubectl -n longhorn-system get nodes.longhorn.io`).
6. Confirm the changed tfvars got re-sealed (`talops secrets status`).

### Phase 2 — retire two small workers, one at a time

Default order: talos-worker-02, then talos-worker-03 (keeping worker-04
preserves a 4th worker host and frees pve2/pve3 memory for a future
control-plane stretch). Swap the survivor if live host health says
otherwise. Per node W:

1. Re-verify all gates (gate 6 is mandatory before the **second**
   retirement).
2. `kubectl cordon <W>` then
   `kubectl drain <W> --ignore-daemonsets --delete-emptydir-data --timeout=5m`.
   If the drain blocks on the Longhorn instance-manager PDB, that is the
   replica-safety gate working — stop and evict first, never override.
3. Longhorn evict: in `nodes.longhorn.io/<W>` set
   `spec.allowScheduling: false` and `spec.evictionRequested: true`; wait
   until the node holds **zero** replicas and gate 1 (all volumes healthy)
   passes again.
4. Remove W's entry from `talos_worker_configuration` in the hydrated
   tfvars.
5. `terraform plan` — review: exactly one `destroy` of W, nothing else.
6. **HUMAN**: `terraform apply` the reviewed plan.
7. Clean up: `kubectl delete node <W>`; confirm the Longhorn node object
   is gone (delete it if orphaned); `talops reconcile` to update HAProxy
   and bootstrap state.
8. Soak ~15 minutes: no Pending pods, Longhorn clean, new workers
   absorbing load. Then next node.

### Phase 3 — right-size talos-worker-05 (64Gi → 32Gi)

Required to close the pve5 co-tenancy budget (see above). Do this last so
worker-06/-07 exist to absorb the monitoring stack during the drain.

1. Re-verify all gates.
2. `kubectl cordon` + drain worker-05 (it prefers the monitoring stack via
   soft affinity — expect Prometheus/Grafana/Loki to reschedule; verify
   they land and go Ready before continuing).
3. Edit hydrated tfvars: worker-05 `memory` 65536 → 32768.
4. `terraform plan -target 'proxmox_virtual_environment_vm.worker["talos-worker-05"]'`
   — review: memory-only in-place update; expect a VM power-cycle on apply.
5. **HUMAN**: `terraform apply`. Wait for node `Ready`, then
   `kubectl uncordon`.
6. Let the monitoring stack drift back (soft affinity); verify Prometheus
   healthy and scraping.

## Abort criteria

- Any gate fails mid-sequence → stop; do not start the next node.
- A new worker is not `Ready`/joined within 10 minutes of apply → stop;
  investigate via `talosctl` before retiring anything (the small workers
  stay until their replacements are proven).
- Drain stuck >10 minutes on Pending reschedules → `kubectl uncordon`,
  investigate capacity, resume only when placement succeeds.
- Longhorn eviction stalls or any volume goes degraded → pause; wait for
  rebuild to settle; if it does not, re-enable scheduling on the node and
  investigate before retrying.
- etcd healthy members < 3 at any point → stop all worker operations until
  the control plane is healthy again.

## Post-checks

- `kubectl top nodes`: no worker above ~60% memory; scheduler no longer
  placement-bound (no Pending pods with memory-fit events).
- Longhorn dashboard: all volumes healthy; replica spread covers at least
  three physical hosts (pve1 / pve4 / pve5), not three VMs on one host.
- Proxmox: pve5 allocation leaves the ≥48GB co-tenant reservation intact
  for the CI-runner and GPU/AI-node plans.
- Grafana capacity dashboard reflects the new allocatable totals.
- Update any docs that enumerate the worker fleet (e.g. reboot-order lists
  in `docs/OPERATIONS.md`) and confirm `talops status` shows no drift.
