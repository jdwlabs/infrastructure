# Runbook: Live-migrate the dev VM (devbox) between Proxmox hosts

Status: **DOCUMENTED, UNTESTED — blocked on cluster capacity.** No node
besides pve5 currently has enough free RAM to host devbox. This runbook
records the procedure so it's ready to execute (and prove) the moment a
second qualifying host exists; see `docs/dev-vm-provisioning.md` §8/§9 for
the capacity gap driving the block.

## Why this should be cheap

devbox's disk lives on `truenas-vmdisks`, NFS storage shared across every
Proxmox node (`docs/dev-vm-provisioning.md` §4). Because the disk doesn't
need to move, `qm migrate --online` only has to transfer VM RAM state over
the network — seconds of blip, not a restore-from-backup exercise.

## Why it's blocked today

Migration needs a *second* node that can actually hold the VM — migrating
pve5 → pve5 proves nothing. devbox was sized 8 cores / 32GB RAM when this
runbook was first written; `dev_vm_memory` has since been resized to 16GB
against measured use (`docs/devbox2-provisioning.md` §4), but that resize is
committed, not yet applied — devbox is still running at 32GB today. The table
below checks the more useful, forward-looking question anyway: does the
fleet clear even the smaller, post-resize footprint?

Figures captured live 2026-09-01 (`docs/devbox2-provisioning.md` §2). The
pve1 and pve5 free-RAM columns are **projected, not measured** — devbox2
doesn't exist yet and the pve5 reclaim hasn't been applied, so those two
numbers assume both land as designed, not what `pvesh` returns right now:

| Node | Total RAM | Free RAM | Fits 16 GB? | Excluded? |
|---|---|---|---|---|
| pve1 | 28.2 GiB | ~3.2 GiB *(projected, after devbox2)* | No | — |
| pve2/pve3/pve4 | 12.6 GiB each | ~2.6 GiB each | No | Control-plane hosts, excluded by design requirement #4 |
| pve5 | 123.5 GiB | ~11.5 GiB *(projected, after the reclaim)* | n/a — devbox's own host | Not a valid migration target for its own guest |

The "Fits 16 GB?" column shows free RAM with each host's existing guests
still resident — all nodes are being asked to fit devbox *in addition to*
what they already run. pve5 is marked `n/a` not because of insufficient
headroom, but because a guest cannot migrate to the host it already runs on
— the question of where devbox would go doesn't apply when it's already
there. The free-RAM figure for pve5 (~11.5 GiB) includes devbox's 16 GiB
allocation already counted in pve5's committed memory; if devbox were moved
away, pve5 would have ~27.5 GiB free instead. Halving devbox's allocation
did not unblock migration: pve1 still can't fit an 8-core/16GB VM even on
the more favorable projected figure, and no other node clears it either. No
node in the cluster, today or under this design's projected end state, can
be the "other side" of a migrate/migrate-back cycle. This is a hardware
ceiling, not a process gap.

## Preconditions — re-check live, do not trust this table

1. Confirm the target's free memory covers the VM plus headroom for its own
   other workloads:
   ```bash
   pvesh get /nodes/<target>/status --output-format json | jq '.memory'
   ```
2. Confirm `truenas-vmdisks` is active on the target (it should be
   cluster-wide already — §4 correction in the provisioning doc — but verify
   rather than assume):
   ```bash
   pvesh get /nodes/<target>/storage --output-format json \
     | jq -r '.[] | select(.storage=="truenas-vmdisks")'
   ```
3. Confirm the target isn't a control-plane host (pve2/pve3/pve4) — req #4
   in the provisioning doc applies to any node devbox actually runs on, not
   just its original placement.
4. devbox is reachable and healthy before touching it:
   ```bash
   ssh dev-admin@192.168.1.56 'uptime'
   ```
5. Don't mistake a clean precondition check for proof the cloud-init snippet
   moves with the guest — it doesn't:
   ```bash
   pvesh get /nodes/pve5/qemu/111/migrate --target pve1 --output-format json
   # local_disks: [], local_resources: [], allowed_nodes: all four other hosts
   ```
   `local` is declared on every node, so devbox's
   `cicustom: user=local:snippets/devbox-cloud-init.yaml` resolves
   storage-wise everywhere and the check reports nothing local to block the
   migrate. But the snippet *file* itself lives in exactly one place —
   `devbox-cloud-init.yaml` on pve5, and nowhere else
   (`docs/devbox2-provisioning.md` §7). Migration succeeds and the guest
   keeps running on its existing cloud-init state either way; the exposure is
   that regenerating the drive from the new host afterward would find no
   user-data to read. That's latent configuration drift, not a migration
   blocker — worth knowing before acting on this runbook, not a reason to
   abort.

## Migrate out

```bash
qm migrate 111 <target> --online
```

1. Watch the task log in the Proxmox UI (Datacenter → Tasks) or:
   ```bash
   pvesh get /nodes/pve5/tasks --output-format json | jq -r '.[0]'
   ```
2. Once complete, confirm the guest agent answers on the new node:
   ```bash
   qm agent 111 ping
   ```
3. Confirm SSH still resolves at the same address (cloud-init's static IP
   config travels with the VM, not the host):
   ```bash
   ssh dev-admin@192.168.1.56 'hostname && uptime'
   ```
   A fresh, near-zero `uptime` would mean the VM rebooted instead of live
   migrating — that's a failure of the *online* part of this runbook, not a
   successful migration.

## Measure downtime

Proxmox's migration task log reports a final "switched" timestamp; compare
against the last successful ping/SSH before migration started. Record the
actual gap here once measured — the design doc's "seconds" estimate is
theoretical until this runs once.

## Migrate back

Repeat the same command in reverse once the outbound leg is verified:

```bash
qm migrate 111 pve5 --online
```

Re-run the same post-checks. Only after both legs succeed is the "movable
between Proxmox hosts" requirement (provisioning doc §3.3) actually proven,
not just designed for.

## Abort criteria

- Target's free memory doesn't clear the VM's *actual applied* allocation
  (32GB until the pending `dev_vm_memory` resize lands, 16GB after) with
  headroom for the target's existing workloads — this is the current,
  standing blocker either way.
- `truenas-vmdisks` isn't active on the target.
- `qm agent 111 ping` fails to answer within a minute of the task log
  reporting completion.

## Rollback

`qm migrate` is transactional on failure — Proxmox aborts and leaves the VM
running on the source node if the target can't accept it. No manual
rollback should be needed; if the VM ends up unreachable on the source
*after* a reported failure, check `qm status 111` on pve5 first before
assuming it needs to be started elsewhere.

## Once this becomes unblocked

Re-run the capacity table above against live state — don't assume the gap
recorded 2026-09-01, or its projected devbox2/reclaim figures, still holds.
When a qualifying second node exists, execute both legs, fill in the
measured downtime, and flip this doc's status to TESTED.
