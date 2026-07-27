# Runbook: Talos node upgrade

Status: PLANNED — every `talosctl upgrade` in this runbook is executed by a
human. The agent contract forbids autonomous cluster mutation; agents may run
the read-only inspection commands here (`talosctl get`, `talosctl version`,
`talosctl etcd status`, `kubectl get`) but never the upgrade itself.

Scope: upgrading the **Talos OS** on existing nodes in place. Upgrading
Kubernetes is a separate operation (`talosctl upgrade-k8s`) and is out of
scope — see [Out of scope](#out-of-scope). For system design see
[ARCHITECTURE.md](ARCHITECTURE.md); for the VXLAN offload rollout that this
runbook depends on see [OPERATIONS.md](OPERATIONS.md).

## Why

Talos upgrades replace the node's system partition and reboot it. On this
cluster that means: a control plane leaves the etcd mesh for the duration of
the reboot, a worker's Longhorn replicas go stale until it returns, and every
in-place machine-config workaround has to re-land on the freshly booted
system. Three of those workarounds have already caused outages here, so the
sequencing and the gates below are not ceremony — each one maps to a specific
recorded failure.

The cluster is small enough that a rolling upgrade is entirely serial: one
node at a time, full health between nodes, abort on the first failure.

## Fleet

Derived from [scenarios/cp-static-ips.md](../scenarios/cp-static-ips.md)
(mapping verified 2026-07-23) and
[scenarios/cp-memory-resize.md](../scenarios/cp-memory-resize.md) (layout
verified 2026-07-03, with the resize applied since). **Re-derive this table
live before every upgrade** — `kubectl get nodes -o wide`,
`talosctl -n <ip> version` — and stop if anything disagrees.

| VM (vmid) | Proxmox host | K8s node | IP | Role |
|-----------|--------------|----------|----|------|
| cp-01 (200)     | pve2 | talos-oam-s4g | 192.168.1.241 | control plane |
| cp-02 (201)     | pve3 | talos-6iz-oey | 192.168.1.98  | control plane |
| cp-03 (202)     | pve4 | talos-fow-vbk | 192.168.1.125 | control plane |
| worker-01 (300) | pve1 | talos-4h8-zy6 | 192.168.1.87  | worker |
| worker-02 (301) | pve2 | talos-k3y-y3e | 192.168.1.165 | worker |
| worker-03 (302) | pve3 | talos-2qd-v0u | 192.168.1.78  | worker |
| worker-04 (303) | pve4 | talos-g1i-e3h | 192.168.1.130 | worker |
| worker-05 (304) | pve5 | talos-lx0-6a4 | 192.168.1.163 | worker |

Cluster endpoint is HAProxy at `cluster.jdwlabs.com` (.199); the LAN gateway
and DNS are `192.168.1.254` — **not** `.1`.

## Where the target version comes from

Three pins move together and must agree before any node is touched:

- `talos_version` and `installer_image` in `terraform/terraform.tfvars`
  (untracked; the tracked example is `terraform/terraform.tfvars.example`).
- `TALOS_VERSION` in `.github/workflows/bootstrap.yml`, which is the
  talosctl version CI installs.
- The `installer_image` in every `scenarios/scaling_tests/*.tfvars` fixture.

Renovate owns all of them and groups them into a single PR so the target
moves atomically — `renovate.json` states the intent explicitly:

> All Talos pins (tfvars, tfvars.example, CI talosctl install) are grouped
> into one PR so the cluster target version moves atomically. Talos upgrades
> are applied by a human runbook — the PR is a prompt, not an auto-rollout.

**A merged Renovate PR does not upgrade anything.** It only moves the pins.
As of this writing `terraform/terraform.tfvars.example` pins `v1.13.7`
(commit `aea6d07`) while the live nodes run `v1.13.4` — that gap is normal
and expected between the pin bump and the human rollout. Read the *live*
value from the hydrated `terraform/terraform.tfvars`, not the example.

The installer image is an Image Factory reference carrying a schematic ID,
e.g. the scaling-test fixtures use
`factory.talos.dev/nocloud-installer/b553b4a25d76e938fd7a9aaa7f887c06ea4ef75275e64f4630e6f8f739cf07df:v1.13.4`.
Across a patch upgrade **only the tag moves** — the schematic ID stays the
same, as recorded in the v1.12.13 → v1.13.4 bump (commit `576baff`):

> Cluster nodes upgrade v1.12.3 -> v1.13.4 (1.12 community support ended at
> 1.13.0). Image Factory schematic ID is unchanged; only tags move.

If a proposed upgrade also changes the schematic ID (new system extension,
new kernel args), that is a different, larger change — treat it as a
schematic migration, not a version bump, and diff the schematic first.

## Preconditions — hard gates, all must pass

1. **Version skew is legal.** `talosctl version` client tag must be >= the
   target node version, and the current node version must be within Talos's
   supported upgrade range for the target. Talos 1.12 lost community support
   at 1.13.0 (commit `576baff`), so a multi-minor jump may need an
   intermediate hop — confirm against the upstream release notes for the
   specific pair of versions. Do not assume N→N+2 works.
2. **CP IP stability.** DHCP reservations (or the static-IP patches at
   `clusters/core/patches/node-{200,201,202}.yaml`) are in place and each CP's
   configured address equals the address it currently holds. See
   [DHCP lease flips](#dhcp-lease-flips-on-control-plane-reboot). Verify with
   `talosctl -n <cp-ip> get addresses`. Do not proceed on hope.
3. **etcd healthy 3/3**:
   `talosctl -n 192.168.1.241,192.168.1.98,192.168.1.125 etcd status` — three
   members, single leader, no learners, no alarms.
4. **etcd snapshot taken**:
   `talosctl -n 192.168.1.241 etcd snapshot ./etcd-$(date +%F).db`.
   This is the only true rollback for a control-plane disaster.
5. **Control-plane memory headroom.** CPs must be at 6G and sitting well
   under 70% (`kubectl top nodes`). See
   [Control-plane memory pressure](#control-plane-memory-pressure).
6. **Longhorn healthy with rebuild headroom.**
   `kubectl -n longhorn-system get volumes.longhorn.io` all Healthy, no
   degraded replicas. With 3 replicas across 5 workers there are always
   exactly 2 candidate nodes for an evicted replica, so re-check this gate
   live before **every** worker, not once — the same caveat
   [scenarios/longhorn-dedicated-disk.md](../scenarios/longhorn-dedicated-disk.md)
   records for its own per-node loop.
7. **Cluster quiet**: ArgoCD apps Healthy/Synced; no in-flight infra changes;
   Vault unsealed with the auto-unseal CronJob green (a rescheduled Vault pod
   must re-unseal itself) — carried from
   [scenarios/cp-memory-resize.md](../scenarios/cp-memory-resize.md).
8. **Baseline captured.** Record, per node, what "healthy" looks like now so
   the post-checks have something to compare against:

   ```bash
   kubectl get nodes -o wide
   talosctl -n <ip> version
   talosctl -n <ip> get ethernetstatus flannel.1 -o yaml
   ```

## Sequence — control planes first, one node at a time

Order: **control planes ascending by VMID (200 → 201 → 202), then workers
ascending by VMID (300 → 301 → 302 → 303 → 304)**.

This ordering is not arbitrary — it is the plan the (unmerged) `talops
upgrade` command builds in `buildUpgradePlan`: control planes sorted by VMID,
then workers sorted by VMID, with per-node gates between. Control planes go
first so the control plane is running the new version before any worker
kubelet is; a worker that comes back on a newer Talos than its control plane
is the skew direction Talos does not support.

> Note the contrast with the VXLAN `EthernetConfig` rollout in
> [OPERATIONS.md](OPERATIONS.md), which goes **workers first**. That is a
> dynamic network-config apply with no reboot, so blast radius dominates. An
> upgrade reboots the node, so version skew dominates instead.

Never touch two nodes in the same pass. Quorum must stay >= 2/3 throughout.

### Per control plane C (IP X)

1. Re-verify gates 3 (etcd 3/3) and 5 (CP memory).
2. **HUMAN**:

   ```bash
   talosctl -n X upgrade --image <installer-image>:<target-tag>
   ```

   `--wait` defaults to true, so the command tracks progress and blocks until
   the node returns. `--drain` also defaults to true (cordon + evict, 5m
   timeout), so talosctl drains the node for you — no separate
   `kubectl drain` step is needed. Verify these defaults against your local
   client with `talosctl upgrade --help`; they were read from client v1.13.4.
3. Watch, in order:
   - the VM comes back on the **same IP** — if it changed, STOP and go to
     [DHCP lease flips](#dhcp-lease-flips-on-control-plane-reboot);
   - Talos API answers: `talosctl -n X version` reports the new tag;
   - etcd member rejoined: `talosctl -n X etcd status` and
     `talosctl -n <other-cp-ip> etcd members` — 3/3, one leader, same member
     ID as before;
   - `kubectl get nodes` shows the node Ready on the new version;
   - kube-apiserver answering through the cluster endpoint (haproxy .199).
4. Re-check the VXLAN offloads on this node before moving on:

   ```bash
   talosctl -n X get ethernetstatus flannel.1 -o yaml | grep -E 'tx-checksum-ip-generic|tx-generic-segmentation|tx-tcp-segmentation|tx-tcp6-segmentation|rx-gro'
   talosctl -n X get ethernetstatus eth0 -o yaml | grep -E 'tx-checksum-ip-generic|tx-generic-segmentation|tx-tcp-segmentation|tx-tcp6-segmentation|rx-gro'
   ```

   All five features must report `false`/off on **both** links. See
   [VXLAN tx-checksum offload](#vxlan-tx-checksum-offload-on-a-recreated-flannel1).
5. Soak 10–15 minutes. CP memory back under 70%. Then the next control plane.

### Per worker W (node N, IP X)

1. Re-verify gates 3 (etcd 3/3) and 6 (Longhorn healthy, rebuild headroom
   live).
2. **HUMAN**: `talosctl -n X upgrade --image <installer-image>:<target-tag>`
   — the built-in drain cordons and evicts before reboot.
3. Wait for the node to return: `talosctl -n X version` on the new tag,
   `kubectl get nodes` Ready.
4. Uncordon if talosctl left it cordoned: `kubectl uncordon N`. Confirm with
   `kubectl get node N` (no `SchedulingDisabled`).
5. Re-check the VXLAN offloads exactly as in CP step 4, and run the
   cross-node TCP verification from
   [OPERATIONS.md](OPERATIONS.md) — **both directions**, since the fault is
   sender-side.
6. **Wait for Longhorn to fully heal before the next worker**:
   `kubectl -n longhorn-system get volumes.longhorn.io` all Healthy, no
   rebuilding replicas. This gate is the one the `talops upgrade` branch
   treats as fatal rather than advisory — it aborts the whole run on
   "Longhorn volumes degraded after upgrading <ip>". Do the same by hand.

## Failure modes carried forward

These are not hypotheticals. Each has repo evidence and, in three cases, a
recorded outage.

### DHCP lease flips on control-plane reboot

**Evidence** — `clusters/core/patches/node-200.yaml`:

> DHCP lease flips on control-plane reboots have twice moved a CP to a new
> address and broken the etcd peer mesh (peer URLs pin the old IP), taking
> the API down.

and [scenarios/cp-static-ips.md](../scenarios/cp-static-ips.md):

> 2026-06-28: a CP moved .240 → .98; later recurrence: cp-03/fow-vbk moved
> .244 → .125. A reboot does **not** heal a stale peer URL; only a surgical
> `etcdctl member update` through the surviving quorum does.

An upgrade reboots every control plane, which is exactly the trigger. The
static-IP patches exist to remove the failure mode; confirm they are actually
applied (gate 2) rather than assuming.

**Repair, if a CP comes back on a different address** — rebooting it again
does *not* help. From `scenarios/cp-static-ips.md`:

1. From a healthy CP, list members: `talosctl -n <healthy-ip> etcd members`.
2. Update the stale member's peer URL against a **healthy** member endpoint.
   talosctl has no `member update`, so use `etcdctl` with the etcd client
   certs from the hydrated secrets bundle:

   ```bash
   etcdctl --endpoints=https://<healthy-ip>:2379 \
     member update <member-id> --peer-urls=https://<current-ip>:2380
   ```

3. The member rejoins without data loss. Verify `etcd status` 3/3, then fix
   the address problem (lease / reservation / static config) before
   continuing the upgrade.

The runbook calls this "the zero-downtime procedure that resolved both prior
outages".

### VXLAN tx-checksum offload on a recreated flannel.1

**Evidence** — `bootstrap/internal/talos/patches/control-plane.yaml`, above
the `EthernetConfig` documents:

> virtio_net leaves tx-checksum-ip-generic enabled on flannel's VXLAN link,
> so inner TCP checksums are never filled in after encapsulation; the
> receiving node's conntrack marks those packets INVALID and kube-proxy's
> nftables rules drop them, silently blackholing cross-node pod TCP while
> ICMP still passes. […] Talos re-applies this whenever the EthernetSpec
> reconciles, which covers boot-time creation of flannel.1; after flannel
> upgrades that recreate the link mid-life, re-check with
> `talosctl get ethernetstatus flannel.1`.

[OPERATIONS.md](OPERATIONS.md) states the residual gap plainly:

> the controller re-applies on spec changes and on its own restart/boot, but
> does not watch link recreation. […] After any flannel or Talos upgrade,
> re-check `talosctl get ethernetstatus flannel.1` per node.

The settings are in the machine config, so a normal reboot re-lands them.
The exposure is a **flannel/CNI version change** that recreates `flannel.1`
mid-life without a reboot — which a Talos upgrade can carry along. That is
why the per-node steps re-check both links every time rather than only after
the first node.

Symptom to watch for afterwards: cross-node pod TCP timing out while ICMP
between the same pods succeeds.

### Control-plane memory pressure

**Evidence** — [scenarios/cp-memory-resize.md](../scenarios/cp-memory-resize.md):

> All three control-plane VMs run 4G (3.98Gi visible, ~2.7Gi allocatable
> after etcd/kubelet/OS reserve) and sit chronically at 90–110% memory. etcd
> runs as a Talos host service, so pressure lands on the host, not a pod: one
> CP has already OOM-thrashed and needed a hard reboot, and quorum regularly
> rides on 2/3.

The resize to 6G was applied — commit `d385e68` "chore(terraform): CP resize
applied - CP 4G->6G, workers 6G->4G on pve2-4". Gate 5 exists to confirm the
CPs are still at 6G and still have headroom **before** an upgrade adds
rejoin/compaction load on top. A CP that is already at 90%+ will not
reliably survive rejoining the mesh.

Note the corollary from the same commit: workers on pve2/3/4 were cut 6G →
4G to fund it. Those three workers have the least headroom for rescheduled
pods, so the drain during their upgrade is the one most likely to leave pods
Pending.

### Removed extraManifests are never garbage-collected

**Evidence** — `bootstrap/internal/talos/patches/control-plane.yaml`:

> Talos never garbage-collects a removed entry, so clusters built before each
> removal keep the originals until deleted by hand.

and [scenarios/remove-talos-metrics-server.md](../scenarios/remove-talos-metrics-server.md):

> The extraManifests entry has been removed from the template. Talos does NOT
> garbage-collect resources created by removed extraManifests — the
> kube-system copy stays until deleted by hand.

Consequence for upgrades in both directions:

- An upgrade will **not** clean up anything a previous `extraManifests` entry
  created. If the template changed since the cluster was built, expect
  orphans to survive the upgrade untouched.
- Conversely, a **newly added** `extraManifests` entry does apply on the
  control planes. If an upgrade is bundled with a template change, apply and
  review that config change on its own first — do not discover a manifest
  change mid-upgrade.

The current template carries **no** `extraManifests` entries. Both former
entries now ship as GitOps releases that pin an image tag, because each raw
upstream URL served a rolling tag: `metrics-server` was removed in commit
`ace8290`, and `kubelet-serving-cert-approver` afterwards. Their orphans are
cleaned up by hand per
[remove-talos-metrics-server.md](../scenarios/remove-talos-metrics-server.md)
and
[remove-talos-cert-approver.md](../scenarios/remove-talos-cert-approver.md).

So an upgrade on this cluster has no manifest fetch bundled into it at all —
which is the point. Adding an entry back reintroduces that coupling.

## Rollback and recovery

Ordered cheapest to most invasive.

- **Single node, new version misbehaving**: `talosctl -n X rollback` reverts
  that node to the previous installation (it is a real talosctl subcommand;
  `talosctl rollback --help` confirms "Rollback a node to the previous
  installation"). This is the hint the `talops upgrade` branch emits on
  failure: *"use `talosctl rollback` on that node if needed"*. Roll back the
  failed node and stop the run — do not continue to the next node.
- **Node does not come back at all**: use the Proxmox console to see where
  boot stopped. The pattern used elsewhere in this repo for an unreachable
  node is console + `talosctl apply-config` with the last-known-good config
  from `clusters/core/nodes/`.
- **CP came back on a wrong IP**: the `etcdctl member update` repair above.
  Do not reboot again hoping it heals.
- **etcd healthy members < 2**: stop everything. Restore from the gate-4
  snapshot. Talos etcd recovery is a documented upstream procedure — follow
  the Talos docs for the running version rather than improvising, and treat
  it as an incident, not a step in this runbook.
- **Longhorn volumes degraded and not healing**: do not upgrade another
  worker. A degraded volume plus another worker reboot is how you lose the
  last good replica.

## Post-checks

- `kubectl get nodes -o wide` — all 8 nodes Ready, all on the new
  `KERNEL-VERSION` / `CONTAINER-RUNTIME`, none `SchedulingDisabled`.
- `talosctl -n <all-ips> version` — every node reports the target tag.
- `talosctl -n <cp-ips> etcd status` — 3/3, one leader, no alarms.
- `talosctl health` — cluster-level check across CPs and workers.
- `talosctl -n <all-ips> get ethernetstatus flannel.1` — all five offload
  features off on every node.
- Cross-node TCP verification (both directions) per
  [OPERATIONS.md](OPERATIONS.md).
- `kubectl -n longhorn-system get volumes.longhorn.io` — all Healthy, no
  rebuilds outstanding.
- ArgoCD: all apps Healthy/Synced.
- `kubectl top nodes` — CPs under 70% memory.
- `talops status` — no config drift.
- Commit the pin bump if it was not already merged, so the tracked target and
  the live cluster agree again.

## Abort criteria

- Any CP comes back on a different IP → stop immediately; repair the peer URL
  before anything else.
- etcd healthy members < 2, or a rebooted CP absent from the mesh after
  10 minutes → stop; treat as an incident.
- A node fails to reach Ready within ~10 minutes of the upgrade returning →
  stop, roll that node back, investigate. Ten minutes is the per-node timeout
  the `talops upgrade` branch uses (`upgradeNodeTimeout`).
- Longhorn volumes still degraded after the heal wait → stop before the next
  worker.
- Cross-node pod TCP fails while ICMP passes → the VXLAN offload settings did
  not re-land; fix before continuing.
- Drained worker's pods stuck Pending → uncordon, investigate capacity before
  resuming (pve2/3/4 workers are the 4G ones).

## Out of scope

- **Kubernetes version upgrades.** `talosctl upgrade-k8s` is a separate
  operation with its own `--from`/`--to` and a `--dry-run` plan mode. Note
  that the talosctl client's default `--to` tracks the client's own bundled
  Kubernetes version and is **not** this cluster's target — always pass
  `--to` explicitly from `kubernetes_version` in tfvars. No runbook for this
  exists yet.
- **Schematic changes.** Adding or removing a system extension changes the
  Image Factory schematic ID, not just the tag. Different change, different
  review.
- **Rebuild-in-place.** Replacing a node rather than upgrading it is
  `talops reconcile` territory.

## Unverified — confirm before relying on these

Stated explicitly so nobody treats them as established fact:

- **`talops upgrade` is not available.** A `talops upgrade` command exists on
  the **unmerged** branch `feat/talops-upgrade` (commits `bf0d68a`,
  `e0e9b38`, `facb1d5`) implementing exactly the ordering and gates above,
  with `--plan`/`--dry-run`, `--nodes`, `--image`, `--auto-approve`, and
  `UPGRADE-START`/`UPGRADE-NODE`/`UPGRADE-END` audit entries. It is **not on
  `main`** — `main`'s `bootstrap/cmd/root.go` has no `upgrade` subcommand and
  `bootstrap/internal/talos/client.go` has neither `Upgrade` nor
  `NodeVersion`. This runbook therefore uses `talosctl` directly. If that
  branch merges, the manual sequence collapses to a single reviewed
  `talops upgrade --plan` followed by a human-approved run — re-verify this
  document at that point.
- **No previous Talos upgrade of this cluster is recorded in the repo.** The
  v1.12.13 → v1.13.4 bump (`576baff`) moved pins only; its message says live
  node configs "are untracked and updated operationally during preflight".
  Nothing in `logs/` records an upgrade run. The per-node sequence here is
  reconstructed from the `talops upgrade` branch, the surrounding runbooks,
  and `talosctl upgrade --help` — **it has not been executed end to end and
  reviewed against a real run.** Treat the first execution as a rehearsal:
  do cp-01 alone, then stop and re-read this document against what actually
  happened.
- **Version-skew rules.** Gate 1 says "confirm against upstream release
  notes" rather than naming a supported range, because this repo records only
  one data point (1.12 lost support at 1.13.0). Do not infer a general rule
  from it.
- **`talosctl upgrade` flag defaults** (`--wait` true, `--drain` true,
  `--drain-timeout 5m`) were read from client **v1.13.4** on the operator
  workstation. Re-run `talosctl upgrade --help` with the client you are
  actually holding.
- **Longhorn replica data and root wipes.** `clusters/core/patches/node-300.yaml`
  warns that "a Talos root wipe (upgrade --wipe, reinstall) destroys replica
  data" for workers whose Longhorn replicas still live on the EPHEMERAL
  partition. No `--wipe` flag appears in `talosctl upgrade --help` on client
  v1.13.4, so the exact mechanism named in that comment could not be
  confirmed — but the underlying risk is real and the dedicated-disk
  migration in
  [scenarios/longhorn-dedicated-disk.md](../scenarios/longhorn-dedicated-disk.md)
  is what removes it. Until that migration lands, assume an upgrade that
  wipes EPHEMERAL takes replica data with it, and rely on gate 6.
- **The live `installer_image` value** could not be read: `terraform.tfvars`
  is untracked and the vault was not hydrated while writing this. The
  schematic ID quoted above comes from the `scenarios/scaling_tests/`
  fixtures, which may lag the live value. Read the live one before upgrading.
- **Etcd leader placement.** Upgrading by ascending VMID takes no account of
  which member is leader; leadership simply re-elects when the leader
  reboots. Whether stepping down first is worth the extra step here is
  untested.
