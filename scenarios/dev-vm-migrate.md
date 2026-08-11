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
pve5 → pve5 proves nothing. devbox is sized 8 cores / 32GB RAM
(`terraform/dev-vm-node.tf`, `dev_vm_cores` / `dev_vm_memory`). Checked
2026-08-10:

| Node | Total RAM | Free RAM | Fits 32GB? | Excluded? |
|---|---|---|---|---|
| pve1 | 28.2GiB | ~5.8GiB | No — total is below the VM's allocation | — |
| pve2/pve3/pve4 | ~6GiB each (JDWLABS-78) | — | No | Control-plane hosts, excluded by design requirement #4 |
| pve5 | 123.5GiB | ~42GiB | Yes | This is devbox's current host — not a valid target |

No node in the cluster today can be the "other side" of a round-trip. This
is a hardware ceiling, not a process gap.

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

- Target's free memory doesn't clear the VM's allocation with headroom for
  the target's existing workloads (this is the current, standing blocker).
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
recorded 2026-08-10 still holds. When a qualifying second node exists,
execute both legs, fill in the measured downtime, and flip this doc's status
to TESTED.
