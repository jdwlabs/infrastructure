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

**`kubectl uncordon` does not rebalance the fleet.** Pods evicted by a drain
land wherever the scheduler puts them and stay there after `uncordon` —
Kubernetes never moves a running pod back to rebalance. A host restart
followed by uncordon leaves the fleet in whatever shape the drain left it,
not restored to its pre-restart spread. If that reshuffle stacked replicas
that used to be spread across hosts, or ate into the slack a *later* restart
was counting on (see the drain-capacity check above), nothing in this
runbook's post-flight fixes it automatically — a restart makes the next
restart's capacity check more important, not less.

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
| pve5 | devbox (111), talos-worker-05 (304), vllm-inference (500) | The heavy one. vllm-inference cannot migrate — it holds an RTX 5090 through PCI passthrough (`hostpci0: mapping=gpu-rtx5090`) and its disk is on pve5's local `local-lvm`, not shared storage — so it always goes down and comes back only when pve5 does. talos-worker-05 carries most of the `monitoring` namespace — Prometheus, Grafana, Loki, Alertmanager, Tempo — but not all of it: `kube-state-metrics`, the `kube-prometheus-stack` operator, `github-repo-health-exporter`, and the `am-check` job pods run on `talos-4h8-zy6` (pve1) instead (confirmed live via `kubectl get pods -n monitoring -o wide`). Expect the dashboards and alerting pipeline to blink out for the duration; expect metrics scraping/exporters on pve1 to keep running. Work from devbox2. |

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

**Drain-capacity check, before draining the worker on this host:** confirm
some *other* schedulable worker has room for what's about to be evicted —
`kubectl drain` does not check this for you, and a fleet this tight can fail
it. Get the non-DaemonSet memory requests on the draining worker (what has to
land somewhere else — DaemonSet pods aren't evicted) and compare against
other workers' allocatable minus their current requests:

```
$ kubectl get pods --all-namespaces --field-selector spec.nodeName=<worker-about-to-drain> -o json | python3 -c "
import json, sys; data = json.load(sys.stdin)
def parse_mem(s):
    if not s: return 0
    s = str(s).strip()
    units = {'Ki': 1024, 'K': 1000, 'Mi': 1024**2, 'M': 1000**2, 'Gi': 1024**3, 'G': 1000**3, 'Ti': 1024**4, 'T': 1000**4}
    for u in sorted(units.keys(), key=len, reverse=True):
        if s.endswith(u): return int(float(s[:-len(u)]) * units[u])
    return int(float(s))
total = 0
for pod in data.get('items', []):
    is_ds = any(o.get('kind') == 'DaemonSet' for o in pod['metadata'].get('ownerReferences', []))
    if not is_ds:
        total += sum(parse_mem(c.get('resources', {}).get('requests', {}).get('memory', '0')) for c in pod['spec'].get('containers', []) + pod['spec'].get('initContainers', []))
print(f'Non-DaemonSet memory: {total/(1024**3):.2f} GiB')
"
$ kubectl describe node <every other worker> | grep -A3 'Allocated resources'
```

Measured live 2026-09-02: `talos-lx0-6a4` (pve5) carries 11.42 GiB of
reschedulable non-DaemonSet memory requests. Of the other four workers, only
`talos-4h8-zy6` (pve1) has room — ~11.8 GiB allocatable, others sit at ~98%
requests with only ~20-30 MiB free each — no room to absorb anything. If the
worker you're about to drain, plus every worker with slack, doesn't clear
this check, expect Pending pods, not a silent reschedule — see the pve5
section below for what this looks like when it's genuinely marginal.

## pve1

**Guests:** haproxy-1 (110), talos-worker-01 (300), devbox2 (112, once
provisioned).

**Work from:** devbox, not devbox2. devbox2 lives on pve1 — it goes down for
exactly the restart it would otherwise let you drive, so it's useless as a
lifeboat for its own host. This is the one restart in this table that still
depends on devbox being up, same as before devbox2 existed.

1. Pre-flight (above), including the drain-capacity check against
   `talos-4h8-zy6`.
2. Drain the worker:
   ```
   kubectl drain talos-4h8-zy6 --ignore-daemonsets --delete-emptydir-data
   ```
3. Shut down the guests. haproxy-1 first — nothing routes to the cluster API
   through it once it's down anyway, so there's no ordering cost — then
   devbox2, then the now-drained worker:
   ```
   ssh root@pve1 'qm shutdown 110'
   ssh root@pve1 'qm shutdown 112'
   ssh root@pve1 'qm shutdown 300'
   ```
4. Reboot pve1.
5. Post-flight:
   ```
   ssh root@pve1 'qm list'                 # 110, 112, 300 running again
   kubectl uncordon talos-4h8-zy6
   kubectl get nodes                       # 8 Ready
   kubectl -n longhorn-system get volumes.longhorn.io   # back to healthy
   kubectl get pods -A --field-selector status.phase=Pending
                                            # empty output = everything rescheduled successfully
   kubectl get applications.argoproj.io -A --no-headers | awk '$3!="Synced" || $4!="Healthy"'
                                            # empty output = everything synced and healthy
   ```
   Confirm the cluster API itself is reachable again (HAProxy back up) before
   trusting any of the `kubectl` output above — a `kubectl` command that
   hangs or times out here is HAProxy, not the cluster. Remember uncordon
   only re-enables scheduling on `talos-4h8-zy6`; it does not move anything
   back that the drain relocated elsewhere.

## pve2

**Guests:** talos-cp-01 (200), talos-worker-02 (301).

**Work from:** either devbox or devbox2 — neither guest on this host is a
workstation.

1. Pre-flight, including the drain-capacity check against `talos-k3y-y3e` —
   measured live 2026-09-02, this worker sits at ~98% memory requests with
   essentially no free room of its own; confirm some other worker can
   absorb what it's carrying before draining.
2. Drain the worker:
   ```
   kubectl drain talos-k3y-y3e --ignore-daemonsets --delete-emptydir-data
   ```
3. Shut down the worker, then the control-plane member:
   ```
   ssh root@pve2 'qm shutdown 301'
   ssh root@pve2 'qm shutdown 200'
   ```
4. Reboot pve2.
5. Post-flight, same shape as pve1's step 5 (`ssh root@pve2 'qm list'`,
   `kubectl uncordon talos-k3y-y3e`, 8 Ready, Longhorn healthy, no Pending
   pods, ArgoCD synced) minus the HAProxy caveat — the API endpoint doesn't
   live on this host. Uncordon doesn't rebalance here either.

One etcd member is down for the duration; quorum holds at 2/3 as long as
pve3 and pve4's control-plane members stay up, which is exactly why this
can't run concurrently with a pve3 or pve4 restart.

## pve3

**Guests:** talos-cp-02 (201), talos-worker-03 (302).

Same shape as pve2, substituting node name and VMIDs. `talos-2qd-v0u` is
another of the ~98%-full workers — run the drain-capacity check before
draining it, same as pve2:

```
kubectl drain talos-2qd-v0u --ignore-daemonsets --delete-emptydir-data
ssh root@pve3 'qm shutdown 302'
ssh root@pve3 'qm shutdown 201'
# reboot pve3
kubectl uncordon talos-2qd-v0u
kubectl get pods -A --field-selector status.phase=Pending
```

Never concurrent with pve2 or pve4 — same corosync and etcd-quorum reasoning.

## pve4

**Guests:** talos-cp-03 (202), talos-worker-04 (303).

Same shape again. `talos-g1i-e3h` is the third ~98%-full worker — same
drain-capacity check applies:

```
kubectl drain talos-g1i-e3h --ignore-daemonsets --delete-emptydir-data
ssh root@pve4 'qm shutdown 303'
ssh root@pve4 'qm shutdown 202'
# reboot pve4
kubectl uncordon talos-g1i-e3h
kubectl get pods -A --field-selector status.phase=Pending
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
3. Drain-capacity check (per Pre-flight above) — this drain is genuinely
   marginal, not a formality. Measured live 2026-09-02: `talos-lx0-6a4`
   carries ~10.7 GiB of reschedulable (non-DaemonSet) memory requests. Of
   the other four workers, only `talos-4h8-zy6` (pve1) has room — ~11.8 GiB
   free — and the other three sit at ~98% requests with only ~20-30 MiB
   free each, i.e. no room at all. That leaves roughly 1 GiB of fleet-wide
   slack once the drain lands entirely on `talos-4h8-zy6`. If anything has
   grown since this was measured, re-run the check
   (`kubectl describe node talos-lx0-6a4 talos-4h8-zy6 | grep -A3
   'Allocated resources'`) before draining — a drain that doesn't fit
   leaves pods `Pending`, not silently spread across the other three, which
   have nothing to give.
4. Drain the worker. This host runs most of the monitoring stack — Grafana,
   Prometheus, Loki, Alertmanager, Tempo — so expect those to be unreachable
   from drain until the post-flight uncordon; `kube-state-metrics`, the
   `kube-prometheus-stack` operator, `github-repo-health-exporter`, and the
   `am-check` pods live on pve1's worker instead and are unaffected. There's
   no ordering trick that avoids the dashboards/alerting gap, it's what "the
   heavy one" means for this host:
   ```
   kubectl drain talos-lx0-6a4 --ignore-daemonsets --delete-emptydir-data
   kubectl get pods -A --field-selector status.phase=Pending
                                            # empty output — confirm before continuing
   ```
5. Shut down guests, GPU-passthrough first since it can't migrate and has
   nothing left to lose by going first, then devbox, then the drained
   worker:
   ```
   ssh root@pve5 'qm shutdown 500'
   ssh root@pve5 'qm shutdown 111'
   ssh root@pve5 'qm shutdown 304'
   ```
6. Reboot pve5.
7. Post-flight, from devbox2 until devbox answers again:
   ```
   ssh root@pve5 'qm list'                 # 111, 304, 500 running again
   kubectl uncordon talos-lx0-6a4
   kubectl get nodes                       # 8 Ready
   kubectl -n longhorn-system get volumes.longhorn.io   # healthy
   kubectl get pods -A --field-selector status.phase=Pending
                                            # empty output = everything rescheduled successfully
   kubectl get applications.argoproj.io -A --no-headers | awk '$3!="Synced" || $4!="Healthy"'
   ssh dev-admin@192.168.1.56 'uptime'     # devbox reachable again
   ```
   Uncordon re-enables scheduling on `talos-lx0-6a4`; it does not move the
   pods the drain relocated to `talos-4h8-zy6` back. That worker stays more
   loaded than before until something else redistributes it — which makes a
   later pve1 restart's drain-capacity check more likely to come up short,
   not less. Re-run that check rather than assuming pve1's headroom is what
   it was before this restart.

vllm-inference does not come back on its own guarantee — its GPU passthrough
means the guest firmware has to re-claim the RTX 5090 on boot. If `qm list`
shows it running but inference requests fail, that's a separate problem from
this runbook (start with `ssh root@pve5 'qm config 500 | grep hostpci'` and
the guest's own boot log, not with anything here).
