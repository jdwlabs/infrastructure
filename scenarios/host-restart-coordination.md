# Runbook: Coordinating a Proxmox host restart

Status: **devbox2 does not exist yet.** This runbook describes the
steady-state procedure for once it is provisioned
(`docs/devbox2-provisioning.md`, `terraform/devbox2-node.tf`, VMID 112,
192.168.1.57 on pve1) — that apply is a later, human-gated step, not
something this document performs or assumes has happened. The pve5 memory
reclaim it depends on for headroom (`dev_vm_memory` 32768 → 16384) is
committed in the same design but likewise not yet applied. **Until both
land, a pve5 restart has no lifeboat**: if devbox is down, there is nowhere
else in this fleet to work from, and this procedure's pve5 section cannot be
followed as written. Live guest and quorum state below was checked
2026-09-01/02; re-verify before acting if reading this more than a few weeks
later.

## Why this exists

Every one of the five Proxmox hosts carries something that either interrupts
cluster access or reduces its fault tolerance when it goes down. None of that
is new — what has been missing is a place to work from while pve5, the host
running the only interactive workstation, is rebooting. `devbox2` closes that
gap; this runbook is what actually using it looks like, plus the equivalent
procedure for the other four hosts, which don't have that problem but do have
their own.

## Cluster-wide constraints

Read these before touching any host — they're why the per-host sections below
say "never concurrent with" and "check before assuming," not just "reboot it."

**Corosync quorum: 5 votes, needs 3.** Confirmed live:

```
root@pve1:~# pvecm status
...
Votequorum information
----------------------
Expected votes:   5
Highest expected: 5
Total votes:      5
Quorum:           3
Flags:            Quorate
```

Never take more than two hosts down at once, and never start rebooting a
second host before the first has fully rejoined and `pvecm status` shows
`5/5` again. Two down still holds quorum (3/5); a third would not, and the
cluster filesystem (`pmxcfs`) goes read-only fleet-wide at that point —
everything from `qm` to Terraform to this runbook's own verification
commands stops working, not just the guests on the missing host.

**CNPG's 3-replica cluster needs 3 distinct schedulable workers.** There is
one 3-instance CloudNativePG cluster in this fleet,
`database/platform-postgresql-cluster-prd` — the other two CNPG clusters
(`platform-litellm-db-cluster`, `platform-postgresql-cluster-non`) run a
single instance each and aren't affected by worker count. Checked live:

```
$ kubectl get cluster.postgresql.cnpg.io -n database platform-postgresql-cluster-prd -o jsonpath='{.spec.affinity}'
{"podAntiAffinityType":"preferred","topologyKey":"kubernetes.io/hostname"}
```

This is `preferred`, not `required` — CNPG will not leave a replica
`Pending` if fewer than three distinct workers are schedulable. What it does
instead is quietly stack two replicas on the same node, which is worse than
it sounds: a single node failure at that point can take out a majority of
the cluster instead of one replica. Preferred anti-affinity fails soft, so
nothing will page about it — check where the replicas actually are
(`kubectl get pods -n database -l cnpg.io/cluster=platform-postgresql-cluster-prd -o wide`)
before draining a worker, rather than trusting the scheduler to keep the
spread you assumed.

**Longhorn replica data is node-anchored.** Draining a Talos worker
reschedules the *pods* that used its volumes, but a volume's replicas stay
where they were written — they don't follow the pod. The workload comes back
up reading through the network to a replica sitting on a node that's about
to reboot, or Longhorn starts rebuilding a fresh replica elsewhere, which
costs both time and disk I/O across the CSI path. Check volume health before
and after every host restart:

```
$ kubectl -n longhorn-system get volumes.longhorn.io
NAME                                       ...   STATE      ROBUSTNESS   SCHEDULED   SIZE          NODE            AGE
pvc-0e353eaf-...                           ...   attached   healthy                  26843545600   talos-lx0-6a4   103d
...
```

`ROBUSTNESS` is what matters — `healthy` or `unknown` (the latter is normal
for a currently-detached volume; it doesn't mean broken) are fine to proceed
past. `degraded` means a replica is missing and the volume is running on
fewer copies than it should; don't start a host restart with any volume in
that state, and don't consider a restart's post-flight clean until every
volume that was healthy beforehand is healthy again.

**No zone spread is enforced.** `topology.kubernetes.io/zone` is unset on
all 8 Kubernetes nodes (confirmed: `kubectl get nodes -o jsonpath='{.items[*].metadata.labels.topology\.kubernetes\.io/zone}'` returns nothing), so nothing
in the scheduler's topology constraints knows that these are 8 VMs on only 5
physical machines. A "3-replica spread" can land all 3 copies on 3 VMs that
happen to share one Proxmox host, and the scheduler has no way to tell you
that's what it did. The CNPG pod-location check above is the general pattern:
before rebooting a host, check where things actually are running, not where
the replica count implies they must be.

## Not in scope

**Proxmox HA groups and fencing** are not part of this procedure — checked
live, `pvesh get /cluster/ha/resources` returns `[]`. No HA resources are
defined anywhere in this cluster; every guest here is manually managed, and
this runbook is the manual process that stands in for automatic failover
until (if ever) that changes.

**Automating any of this in `talops`** is out of scope too. Nothing below is
written as a `talops` subcommand or proposed as one — it's a sequence a human
runs by hand, on purpose, given how much of it is "check, then decide"
rather than "run this."

## Per-host reference

| Host | Guests | What happens on reboot |
|---|---|---|
| pve1 | haproxy-1 (110), talos-worker-01 (300), devbox2 (112, once provisioned) | HAProxy is the control-plane API endpoint and the only instance — cluster API access drops until it's back, there's no second instance to fail over to. devbox2 goes down with it, so pve1 restarts are worked from devbox, not devbox2. |
| pve2 | talos-cp-01 (200), talos-worker-02 (301) | One etcd member down; quorum holds at 2/3. Draining the worker removes one of the three CNPG-eligible workers for the duration. |
| pve3 | talos-cp-02 (201), talos-worker-03 (302) | Same shape as pve2. Never concurrent with pve2 or pve4 — see the corosync constraint above. |
| pve4 | talos-cp-03 (202), talos-worker-04 (303) | Same shape as pve2. |
| pve5 | devbox (111), talos-worker-05 (304), vllm-inference (500) | The heavy one. vllm-inference cannot migrate — it holds an RTX 5090 through PCI passthrough (`hostpci0: mapping=gpu-rtx5090`) and its disk is on pve5's local `local-lvm`, not shared storage — so it always goes down and comes back only when pve5 does. talos-worker-05 carries the entire `monitoring` namespace (Prometheus, Grafana, Loki, Alertmanager, Tempo — confirmed live via `kubectl get pods -A --field-selector spec.nodeName=talos-lx0-6a4`), so observability blinks out for the duration too. Work from devbox2. |

VM name ≠ Kubernetes node name — Proxmox's guest name (e.g.
`talos-worker-01`) is not what `kubectl drain` wants. The mapping, confirmed
live against `kubectl get nodes -o wide` and cross-checked against
`docs/talos-upgrade.md`:

| Proxmox host | Proxmox guest (VMID) | Kubernetes node name |
|---|---|---|
| pve1 | talos-worker-01 (300) | `talos-4h8-zy6` |
| pve2 | talos-cp-01 (200) | `talos-oam-s4g` |
| pve2 | talos-worker-02 (301) | `talos-k3y-y3e` |
| pve3 | talos-cp-02 (201) | `talos-6iz-oey` |
| pve3 | talos-worker-03 (302) | `talos-2qd-v0u` |
| pve4 | talos-cp-03 (202) | `talos-fow-vbk` |
| pve4 | talos-worker-04 (303) | `talos-g1i-e3h` |
| pve5 | talos-worker-05 (304) | `talos-lx0-6a4` |

## Pre-flight (every host, before touching anything)

```
$ ssh root@pve1 'pvecm status'          # Quorate, 5/5 votes
$ kubectl get nodes                     # 8 nodes, all Ready
$ kubectl -n longhorn-system get volumes.longhorn.io   # no ROBUSTNESS=degraded
```

If any of these three don't come back clean, stop — fix that first. A
restart is not the moment to also be diagnosing an unrelated quorum or
volume problem.

## pve1

**Guests:** haproxy-1 (110), talos-worker-01 (300), devbox2 (112, once
provisioned).

**Work from:** devbox, not devbox2. devbox2 lives on pve1 — it goes down for
exactly the restart it would otherwise let you drive, so it's useless as a
lifeboat for its own host. This is the one restart in this table that still
depends on devbox being up, same as before devbox2 existed.

1. Pre-flight (above).
2. Drain the worker:
   ```
   kubectl drain talos-4h8-zy6 --ignore-daemonsets --delete-emptydir-data
   ```
3. Shut down the guests. haproxy-1 first — nothing routes to the cluster API
   through it once it's down anyway, so there's no ordering cost — then
   devbox2, then the now-drained worker:
   ```
   qm shutdown 110
   qm shutdown 112
   qm shutdown 300
   ```
4. Reboot pve1.
5. Post-flight:
   ```
   qm list                                 # 110, 112, 300 running again
   kubectl uncordon talos-4h8-zy6
   kubectl get nodes                       # 8 Ready
   kubectl -n longhorn-system get volumes.longhorn.io   # back to healthy
   kubectl get applications.argoproj.io -A --no-headers | awk '$3!="Synced" || $4!="Healthy"'
                                            # empty output = everything synced and healthy
   ```
   Confirm the cluster API itself is reachable again (HAProxy back up) before
   trusting any of the `kubectl` output above — a `kubectl` command that
   hangs or times out here is HAProxy, not the cluster.

## pve2

**Guests:** talos-cp-01 (200), talos-worker-02 (301).

**Work from:** either devbox or devbox2 — neither guest on this host is a
workstation.

1. Pre-flight.
2. Drain the worker:
   ```
   kubectl drain talos-k3y-y3e --ignore-daemonsets --delete-emptydir-data
   ```
3. Shut down the worker, then the control-plane member:
   ```
   qm shutdown 301
   qm shutdown 200
   ```
4. Reboot pve2.
5. Post-flight, same shape as pve1's step 5 (`qm list`, `kubectl uncordon
   talos-k3y-y3e`, 8 Ready, Longhorn healthy, ArgoCD synced) minus the
   HAProxy caveat — the API endpoint doesn't live on this host.

One etcd member is down for the duration; quorum holds at 2/3 as long as
pve3 and pve4's control-plane members stay up, which is exactly why this
can't run concurrently with a pve3 or pve4 restart.

## pve3

**Guests:** talos-cp-02 (201), talos-worker-03 (302).

Same shape as pve2, substituting node name and VMIDs:

```
kubectl drain talos-2qd-v0u --ignore-daemonsets --delete-emptydir-data
qm shutdown 302
qm shutdown 201
# reboot pve3
kubectl uncordon talos-2qd-v0u
```

Never concurrent with pve2 or pve4 — same corosync and etcd-quorum reasoning.

## pve4

**Guests:** talos-cp-03 (202), talos-worker-04 (303).

Same shape again:

```
kubectl drain talos-g1i-e3h --ignore-daemonsets --delete-emptydir-data
qm shutdown 303
qm shutdown 202
# reboot pve4
kubectl uncordon talos-g1i-e3h
```

Never concurrent with pve2 or pve3.

## pve5

**Guests:** devbox (111), talos-worker-05 (304), vllm-inference (500).

**Work from:** devbox2. This is the entire reason devbox2 exists — devbox
itself is going down as part of this procedure.

1. Pre-flight, run from devbox2:
   ```
   ssh root@pve1 'pvecm status'
   kubectl get nodes
   kubectl -n longhorn-system get volumes.longhorn.io
   ```
2. Check where the CNPG `-prd` cluster's replicas actually are before
   draining — with `preferred` anti-affinity (see the constraints section
   above), draining `talos-lx0-6a4` while a replica is already stacked
   elsewhere is a real risk, not a hypothetical one:
   ```
   kubectl get pods -n database -l cnpg.io/cluster=platform-postgresql-cluster-prd -o wide
   ```
3. Drain the worker. This host also runs the entire monitoring stack, so
   expect Grafana/Prometheus/Loki/Alertmanager/Tempo to be unreachable from
   drain until the post-flight uncordon — there's no ordering trick that
   avoids that, it's what "the heavy one" means for this host:
   ```
   kubectl drain talos-lx0-6a4 --ignore-daemonsets --delete-emptydir-data
   ```
4. Shut down guests, GPU-passthrough first since it can't migrate and has
   nothing left to lose by going first, then devbox, then the drained
   worker:
   ```
   qm shutdown 500
   qm shutdown 111
   qm shutdown 304
   ```
5. Reboot pve5.
6. Post-flight, from devbox2 until devbox answers again:
   ```
   qm list                                 # 111, 304, 500 running again
   kubectl uncordon talos-lx0-6a4
   kubectl get nodes                       # 8 Ready
   kubectl -n longhorn-system get volumes.longhorn.io   # healthy
   kubectl get applications.argoproj.io -A --no-headers | awk '$3!="Synced" || $4!="Healthy"'
   ssh dev-admin@192.168.1.56 'uptime'     # devbox reachable again
   ```

vllm-inference does not come back on its own guarantee — its GPU passthrough
means the guest firmware has to re-claim the RTX 5090 on boot. If `qm list`
shows it running but inference requests fail, that's a separate problem from
this runbook (start with `qm config 500 | grep hostpci` and the guest's own
boot log, not with anything here).
