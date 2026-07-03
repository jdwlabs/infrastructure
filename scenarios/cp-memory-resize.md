# Runbook: Control-plane memory resize (4G → 6G)

Status: PLANNED — every `terraform apply` in this runbook is executed by a
human. The agent contract forbids autonomous applies.

## Why

All three control-plane VMs run 4G (3.98Gi visible, ~2.7Gi allocatable after
etcd/kubelet/OS reserve) and sit chronically at 90–110% memory. etcd runs as a
Talos host service, so pressure lands on the host, not a pod: one CP has
already OOM-thrashed and needed a hard reboot, and quorum regularly rides on
2/3. Target: 6G per CP (~55% steady-state), funded by shrinking the co-located
worker on each Proxmox host — net-zero host allocation, no new hardware.

## Current layout (verified 2026-07-03, Proxmox API + terraform state)

| Host | RAM total | free | VMs (vmid, RAM, k8s node, IP) |
|------|-----------|------|-------------------------------|
| pve1 | 28G | 5.4G | worker-01 (300, 16G, talos-4h8-zy6, .87) |
| pve2 | 13G | 1.4G | cp-01 (200, 4G, talos-oam-s4g, .241) + worker-02 (301, 6G, talos-k3y-y3e, .165) |
| pve3 | 13G | 1.6G | cp-02 (201, 4G, talos-6iz-oey, .98) + worker-03 (302, 6G, talos-2qd-v0u, .78) |
| pve4 | 13G | 1.7G | cp-03 (202, 4G, talos-fow-vbk, .125) + worker-04 (303, 6G, talos-g1i-e3h, .130) |
| pve5 | 123G | 97.7G | worker-05 (304, 64G, talos-lx0-6a4, .163) |

pve2/3/4 cannot absorb a straight +2–4G CP bump (≤1.7G free). But each hosts
exactly one 6G worker whose workload now fits elsewhere: the monitoring stack
prefers worker-05 (soft node affinity, `workload.jdwlabs.io/monitoring`), and
worker-01/-05 have ample headroom.

## Target (Option A — recommended)

Per host on pve2/3/4: worker 6G → 4G, control plane 4G → 6G.
Host allocation unchanged (10G); free memory stays ~1.5G.

- Worker at 4G is sufficient post-monitoring-drain (steady use 1.8–2.4Gi,
  heaviest singletons moved to worker-05).
- CP at 6G puts etcd + control plane at ~45–60% instead of 90–110%.
- Stretch: CP 7G fits inside the remaining ~1.4G host free memory if 6G proves
  insufficient — same procedure, second pass, no worker change.

### Option B (not recommended now)

Migrate worker VMs 301–303 to pve5 (offline migrate; local-lvm storage) and
raise CPs to 8–10G. Rejected for now: 4 of 5 workers would share one physical
host, so Longhorn's 3-replica spread across worker VMs stops implying
3-host durability, and a pve5 failure takes most of the worker pool.

### Option C (rejected)

Delete workers 302–304 and consolidate. Leaves exactly 3 workers — the
minimum for Longhorn 3-replica placement — with zero rebuild headroom.

## Preconditions — hard gates, all must pass

1. **CP IP stability.** A CP reboot previously came back on a different DHCP
   lease and broke the etcd peer mesh (full API outage, 2026-06-28). Before
   any reboot: create/verify DHCP reservations (or Talos static IPs) for the
   MACs of vmids 200/201/202, and confirm lease = current IP
   (.241 / .98 / .125). Do not proceed on hope.
2. **etcd healthy 3/3**: `talosctl -n <cp-ip> etcd status` on all three —
   no alarms, single leader, low DB fragmentation.
3. **etcd snapshot taken**: `talosctl -n <cp-ip> etcd snapshot ./etcd-$(date +%F).db`.
4. **Cluster quiet**: ArgoCD apps Healthy/Synced; no in-flight upgrades;
   Vault unsealed with the auto-unseal CronJob green (a rescheduled vault pod
   must re-unseal itself).

## Sequence — one host at a time: pve2 → pve3 → pve4

Never touch two hosts in the same pass; quorum must stay ≥2/3 throughout.

Per host X (worker W, control plane C):

1. `kubectl cordon <W>` then
   `kubectl drain <W> --ignore-daemonsets --delete-emptydir-data --timeout=5m`.
   Pods reschedule to worker-01/-05. Longhorn: wait for volume rebuilds to
   settle (`kubectl get volumes.longhorn.io -n longhorn-system` all Healthy).
2. Edit `terraform/terraform.tfvars`: W's `memory` 6144 → 4096.
3. `terraform plan -target 'proxmox_virtual_environment_vm.worker["<W-vm-name>"]'`
   — review: memory-only, in-place update. Expect the VM to power-cycle on
   apply (no memory hotplug).
4. **HUMAN**: `terraform apply` the plan. Wait for node Ready, then
   `kubectl uncordon <W>`.
5. Re-verify gate 2 (etcd 3/3) before touching the CP.
6. Edit tfvars: C's `memory` 4096 → 6144.
7. `terraform plan -target 'proxmox_virtual_environment_vm.controlplane["<C-vm-name>"]'`
   — review as above.
8. **HUMAN**: `terraform apply`. The CP reboots. Watch, in order:
   - VM boots and gets the **same IP** (gate 1 — if the IP changed, STOP:
     fix the reservation, restore the lease, do not continue),
   - node Ready, etcd member rejoined and healthy on all three
     (`talosctl etcd status`), kube-apiserver answering via
     cluster.jdwlabs.com (haproxy .199).
9. Soak 15 minutes. Check CP memory sits well under 70%. Then next host.

## Abort criteria

- etcd healthy members < 2, or a rebooted CP absent from the mesh after
  10 minutes → stop; recover per the 2026-06-28 incident procedure (hard
  reset of affected CP, re-derive node↔IP mapping live, fix lease first).
- Any CP comes back on a different IP → stop immediately (see gate 1).
- Drained worker's pods stuck Pending → uncordon, investigate capacity
  before resuming.

## Post-checks

- Grafana capacity dashboard: CP panels show new allocatable; utilization
  ~45–60%.
- `kubectl top nodes`: all three CPs < 70% memory.
- Update `clusters/core/state` expectations: next `talops status` run should
  show no config drift (tfvars memory values are not committed — this runbook
  records the intent).
