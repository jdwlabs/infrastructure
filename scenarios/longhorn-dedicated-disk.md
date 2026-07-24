# Runbook: Move Longhorn data onto dedicated disks

Status: PLANNED — every `terraform apply`, `talosctl apply-config`, and
`kubectl patch` in this runbook is executed by a human. The agent contract
forbids autonomous applies. The PR carrying this runbook is safe to merge:
nothing changes on the cluster until the sequence below is walked.

## Why

Longhorn's only disk on every worker is `/var/lib/longhorn` on the EPHEMERAL
(root) partition — the same filesystem that holds container images, logs, and
everything else under `/var`. Two consequences:

- **Eviction coupling.** Replica growth eats kubelet's `nodefs` headroom; a
  full root partition triggers node-pressure eviction (or worse, the kernel
  OOM path this cluster has already been burned by) with Longhorn as both
  cause and casualty.
- **Wipe coupling.** Any operation that wipes EPHEMERAL (Talos
  `upgrade --wipe`, reinstall, disk replacement) silently destroys every
  replica on the node instead of just the OS.

Moving replica data to a dedicated per-VM disk decouples storage from the OS
lifecycle. Capacity is roughly preserved (see sizing table); the root
partition keeps its size and simply stops carrying Longhorn data.

## Mechanism

Talos 1.13 user volumes (this cluster runs v1.13.4, talosctl v1.13.4). Each
worker gets a `UserVolumeConfig` document via its per-node patch
`clusters/core/patches/node-{300..304}.yaml`; `talops` appends per-node
patches after the role template when generating node configs
(`resolveNodePatch` in `bootstrap/internal/talos/config.go`), and
`talosctl machineconfig patch` appends new documents — the worker role
template already proves multi-doc append with its `EthernetConfig` docs.
`TemplateHash` covers per-node patches, so `talops status` /
`talops reconcile --dry-run` will report all five workers drifted until
regenerated and applied.

Talos provisions the volume as a GPT partition filling the matched disk
(bounded by `minSize`/`maxSize` — v1.13 validation *requires* the bounds;
the official guide's bare `diskSelector` example fails `talosctl validate`,
found during prep), formats it xfs (default), mounts it at
`/var/mnt/longhorn` (user volumes always mount at `/var/mnt/<name>`), and
propagates the mount into kubelet automatically — no `kubelet.extraMounts`
needed. This is the pattern the official Talos Longhorn guide prescribes
(<https://docs.siderolabs.com/kubernetes-guides/csi/longhorn>; user-volume
reference: <https://docs.siderolabs.com/talos/v1.13/configure-your-talos-cluster/storage-and-disk-management/disk-management/user>).

**Disk selector.** Each patch matches
`disk.transport == "virtio" && !system_disk && disk.size ≈ <N> GiB`
(±1 GiB band). Size is the discriminator, so the Proxmox disk MUST be created
at exactly the agreed size — `scsi1`, same `local-lvm` datastore, 100 GiB on
workers 02–04 (root 150 GiB), 200 GiB on workers 01/05 (root 300 GiB). The
bands cannot collide with the root disk, and the `virtio` transport check
excludes Longhorn's own iSCSI volume attachments (verified live: root disk
shows `transport: virtio`, Longhorn attachments show `iscsi`).

**Longhorn side.** Per node, after the mount exists: add a new disk entry at
`/var/mnt/longhorn` to the `nodes.longhorn.io` CR and retire the old default
disk (evict → remove). Longhorn settings live in the platform repo; nothing
in this repo changes Longhorn itself.

## Live state (verified 2026-07-24 — always re-derive before applying)

Cluster: 14 volumes, 132 Gi provisioned, ~31.5 Gi actual data, 3 replicas
each. Settings: over-provisioning 150%, minimal-available 20%, default-disk
reserve 30%. Control planes run **zero** Longhorn components (no
`nodes.longhorn.io` CRs, no manager/instance-manager pods on CPs — verified
live), so vmids 200–202 are exempt from this runbook.

| VM (vmid) | Host | K8s node | IP | Root disk | LH max | Reserved | Scheduled | Replicas | New disk |
|-----------|------|----------|----|-----------|--------|----------|-----------|----------|----------|
| worker-01 (300) | pve1 | talos-4h8-zy6 | 192.168.1.87 | 300 GiB | 297.7 Gi | 89.3 Gi | 122.0 Gi | 13 | **200 GiB** |
| worker-02 (301) | pve2 | talos-k3y-y3e | 192.168.1.165 | 150 GiB | 147.8 Gi | 44.3 Gi | 25.0 Gi | 1 | **100 GiB** |
| worker-03 (302) | pve3 | talos-2qd-v0u | 192.168.1.78 | 150 GiB | 147.8 Gi | 44.3 Gi | 92.0 Gi | 10 | **100 GiB** |
| worker-04 (303) | pve4 | talos-g1i-e3h | 192.168.1.130 | 150 GiB | 147.8 Gi | 44.3 Gi | 89.0 Gi | 11 | **100 GiB** |
| worker-05 (304) | pve5 | talos-lx0-6a4 | 192.168.1.163 | 300 GiB | 297.7 Gi | 89.3 Gi | 73.0 Gi | 6 | **200 GiB** |

Sizing: new schedulable capacity per node (reserve 0 on a dedicated disk) is
100/200 Gi vs today's effective 103.5/208.4 Gi (max − 30% reserve) — near
parity, sized against 132 Gi provisioned / ~32 Gi actual with headroom.
Disks are thin-provisioned on `local-lvm`, so host allocation grows with
actual data, not disk size.

## Preconditions — hard gates, all must pass

1. **All volumes healthy.** `kubectl -n longhorn-system get volumes.longhorn.io`
   — every volume `robustness: healthy`, no rebuilds in flight. As of the
   live capture, `pvc-a0152d45…` and `pvc-df6c5715…` showed `unknown`
   (detached volumes report this) — confirm they are intentionally detached
   before starting; eviction of detached-volume replicas is slower and must
   be watched to completion.
2. **Eviction headroom.** The node being migrated must fit its scheduled
   replicas on the other four. Free scheduling capacity
   `= (max − reserved) × 150% − scheduled`; live: 4h8 190.6 Gi,
   k3y 130.3 Gi, 2qd 63.3 Gi, g1i 66.3 Gi, lx0 239.6 Gi. Worst case
   (worker-01, 122 Gi scheduled) fits with room to spare, but each evicted
   replica must land on a node not already holding a sibling — with 3
   replicas across 5 nodes there are always exactly 2 candidates, so
   re-check this gate live before every node, not once.
3. **Proxmox thin-pool space.** On the target host, confirm `local-lvm` can
   absorb the new disk's worst case (`pvesm status` / `lvs` — pve2–4 run
   tighter than pve1/5).
4. **Configs regenerated and diffed.** `talops secrets hydrate`, regenerate
   node configs (`talops reconcile --dry-run` shows all five workers
   drifted), then diff old vs new `clusters/core/nodes/node-worker-<vmid>.yaml`:
   the only delta must be the appended `UserVolumeConfig` document. Any
   other delta → stop and investigate.
5. **Versions.** `talosctl version` client v1.13.x against nodes v1.13.4.

## Sequence — one worker at a time

Order: **worker-02 (301) first** — 1 replica, cheapest pilot — then
302 → 303 → 300 → 304. Never touch two workers in the same pass; wait for
full health between nodes.

Per worker W (vmid V, node N, IP X, size S):

1. Re-verify gates 1–3.
2. Read the node's current disk map and note the default disk's key
   (observed live as `default-disk-080400000000` on all five, but re-derive):
   `kubectl -n longhorn-system get nodes.longhorn.io N -o jsonpath='{.spec.disks}'`
3. **HUMAN** — disable scheduling on the old disk, then request eviction
   (Longhorn requires scheduling off before eviction):
   `kubectl -n longhorn-system patch nodes.longhorn.io N --type merge -p '{"spec":{"disks":{"<default-disk-key>":{"allowScheduling":false,"evictionRequested":true}}}}'`
4. Wait until the old disk carries zero replicas and every volume is healthy
   again — abort criteria below if this stalls:
   `kubectl -n longhorn-system get nodes.longhorn.io N -o jsonpath='{.status.diskStatus.<default-disk-key>.scheduledReplica}'`
   must be empty; `kubectl -n longhorn-system get volumes.longhorn.io` all
   healthy.
5. Edit `terraform/terraform.tfvars`: add `data_disk_size = <S-in-GiB>` to
   this worker's entry only (tfvars values are not committed — this runbook
   records the intent; `talops` auto-seals the vault copy).
6. `terraform plan -target 'proxmox_virtual_environment_vm.worker["<W-vm-name>"]'`
   — review: must be **update in-place** adding only `scsi1`. Any
   destroy/replace → ABORT, do not apply.
7. **HUMAN**: `terraform apply` the plan. SCSI disks hot-plug; no VM restart.
   Verify Talos sees it: `talosctl -n X get disks` — new disk of exactly S,
   `transport: virtio`, alongside the root disk.
8. **HUMAN**: `talosctl -n X apply-config --file clusters/core/nodes/node-worker-V.yaml`
   — a new volume document applies without reboot. If talosctl reports a
   reboot is required anyway: `kubectl cordon N`, `kubectl drain N
   --ignore-daemonsets --delete-emptydir-data --timeout=5m`, reboot, wait
   Ready, `kubectl uncordon N` — the node holds no replicas at this point,
   so the reboot risks nothing in Longhorn.
9. Verify the volume: `talosctl -n X get volumestatus u-longhorn` (phase
   `ready`, size ≈ S — if the partition came out smaller than expected,
   stop before letting Longhorn schedule onto it),
   `talosctl -n X get mountstatus` shows `/var/mnt/longhorn` (xfs),
   `talosctl -n X get discoveredvolumes` shows the new partition.
10. **HUMAN** — register the new disk with Longhorn (reserve 0: the disk is
    dedicated, and the global 20% minimal-available floor still applies):
    `kubectl -n longhorn-system patch nodes.longhorn.io N --type merge -p '{"spec":{"disks":{"longhorn-data":{"path":"/var/mnt/longhorn","diskType":"filesystem","allowScheduling":true,"evictionRequested":false,"storageReserved":0,"tags":[]}}}}'`
    Wait for `status.diskStatus.longhorn-data` conditions Ready and
    Schedulable, `storageMaximum` ≈ S.
11. **HUMAN** — remove the old (now replica-free) default disk; merge-patch
    null deletes the key:
    `kubectl -n longhorn-system patch nodes.longhorn.io N --type merge -p '{"spec":{"disks":{"<default-disk-key>":null}}}'`
12. Soak: all volumes healthy, new replicas landing on `longhorn-data` over
    time (Longhorn does not auto-rebalance; parity returns as volumes churn
    or via manual rebalancing later). Then next worker.

## Abort criteria

- **Eviction stalls** (a replica stuck rebuilding > 30 min, or a volume
  `degraded` that isn't actively rebuilding): clear
  `evictionRequested`/re-enable scheduling on the old disk to restore the
  status quo, investigate capacity/anti-affinity before retrying.
- **Any volume `faulted`, or two volumes `degraded` simultaneously**: stop
  the whole procedure, restore scheduling, let Longhorn heal fully.
- **Terraform plan shows anything but an in-place `scsi1` add**: do not
  apply; reconcile provider state first.
- **`u-longhorn` volume not `ready` or wrong size after apply**: leave
  Longhorn untouched (do not run step 10), fix the disk/selector mismatch —
  the likely cause is a disk not created at exactly S.
- Node NotReady after any step → standard node triage before continuing;
  replicas are elsewhere by design.

## Post-checks

- All five `nodes.longhorn.io` show a single `longhorn-data` disk at
  `/var/mnt/longhorn`, Ready + Schedulable; no disk at `/var/lib/longhorn`.
- All volumes healthy; total schedulable capacity ≈ 700 Gi.
- `talops status` — no config drift on any worker.
- Root-partition usage on every worker dropped by its former replica
  footprint (`kubectl -n longhorn-system get nodes.longhorn.io` used to be
  the measure; now compare EPHEMERAL usage via `talosctl -n X get
  volumestatus` / node exporter).

## Follow-ups (separate changes, different repos)

- **platform repo**: Longhorn `defaultSettings.defaultDataPath` is still
  `/var/lib/longhorn/`; change to `/var/mnt/longhorn` so any future worker
  gets its default disk on the dedicated volume, not EPHEMERAL.
- The old `/var/lib/longhorn/longhorn-disk.cfg` stays behind on EPHEMERAL on
  each node (no shell on Talos to delete it). Harmless unless a default disk
  is ever re-created at that path — it would adopt the stale disk UUID; wipe
  the directory during the next node reinstall.
