# Runbook: Fix pve5's stale IP in the Proxmox cluster filesystem (pmxcfs)

Status: PLANNED — needs a maintenance window with console access to at least
one node available. Not yet executed; the safe diagnostics below already
ruled out five candidate fixes without touching cluster state destructively.

## Why

Since the 2026-08-06 pve5 IP-drift recovery (`docs/proxmox-host-ip-drift-and-dhcp.md`
— pve5 moved `.169` → `.204`), every other Proxmox node's cluster filesystem
still records pve5's address as the dead `192.168.1.169`. This breaks the
Proxmox API's inter-node proxy: any `/nodes/pve5/...` call routed through
pve1 or pve2 fails with `HTTP 595 No route to host`, because those nodes try
to reach pve5 at an address nothing answers on anymore. Confirmed blocking:
Terraform/`talops` operations targeting VMs on pve5 (the GPU inference VM,
`talos-worker-05`, and the planned dev VM — `infra#109`/`infra#110`).

Direct API calls to pve5's own endpoint (`192.168.1.204:8006`) work fine —
this is purely an inter-node proxy problem, not a pve5 reachability problem.

## What's actually stale, and what isn't

Checked live (2026-08-09/10) and confirmed **correct**, not the cause:

- `corosync.conf` `ring0_addr` for every node, including pve5 (`192.168.1.204`)
- `corosync-cfgtool -s` on pve1: pve5 shows `connected`
- `corosync-quorumtool -l` on pve1 and pve5: all 5 nodes present, quorum 5/5
- pve5's own live network config (`ip addr`/`ip route`): only `.204`, no
  leftover `.169` anywhere
- DNS at the gateway, `pve5.attlocal.net`: was round-robining `.169`/`.204`
  (a separate, now-fixed problem — see below), but this isn't what pveproxy
  consults for inter-node routing, so fixing it didn't fix the `595`s

**Confirmed stale**: `/etc/pve/.members` (pmxcfs's own clustered node
registry, not a plain file — it's presented through the FUSE mount but backed
by pmxcfs's internal database, replicated via corosync). Every node's copy,
including pve5's own self-entry, reports `pve5` at `192.168.1.169`. This is
almost certainly what pveproxy's request router actually consults to decide
where to forward a `/nodes/pve5/...` call — everything that should touch it
left it unchanged (below), which means the value isn't derived live from
corosync.conf or the network stack at all; it's persisted state from
whenever pve5 first joined the cluster at `.169`, and nothing short of a
proper node re-sync appears to refresh it.

## Ruled out (2026-08-09/10, all verified safe — quorum never dropped)

1. `systemctl restart pve-cluster` on pve1 (the reader) — no change.
2. `systemctl restart pve-cluster` on pve5 (the source) — no change.
3. `systemctl restart pveproxy` on pve1 — no change.
4. `/etc/hosts` pin (`192.168.1.204 pve5.attlocal.net pve5`) on pve1-pve4 —
   fixed DNS resolution (`getent hosts` now deterministic), did **not** fix
   the API proxy. Left in place — harmless, and fixes the separate DNS
   round-robin problem outright.
5. `corosync.conf` `config_version` bump (12→13) + live propagated reload —
   the sanctioned Proxmox mechanism (edit `/etc/pve/corosync.conf`, pmxcfs
   propagates automatically, corosync reloads without a service stop).
   Verified quorum-safe: 5/5 connected before and after, pmxcfs's internal
   file version counter incremented (proof the reload was real). `.members`
   was unaffected — still `.169` for pve5, on every node, after this.
   **This change is permanent and correct; nothing to roll back.**

Community precedent for this exact symptom (`HTTP 595` after a node IP
change): [Proxmox forum — "Changed node IP and it is now giving a No route
to host (595) error"](https://forum.proxmox.com/threads/solved-changed-node-ip-and-it-is-now-giving-a-no-route-to-host-595-error.74899/),
[change host ip in pve cluster: No route to host(595)](https://forum.proxmox.com/threads/change-host-ip-in-pve-cluster-no-route-to-host-595.114290/).
Consensus: a live reload (what step 5 above did) is not always sufficient;
the escalation is a coordinated stop/edit/restart of cluster services.

## Preconditions — hard gates, all must pass

1. **Quorum 5/5 healthy**: `corosync-quorumtool -l` on at least two different
   nodes shows all 5 members present, `quorate: 1` via `pvecm status`.
2. **Console access available** for every node touched (Proxmox host
   console via BMC/physical, not SSH) — if a node's `pve-cluster`/`corosync`
   fails to come back, SSH may be unreachable and only console recovers it.
   This is the same posture the 2026-08-06 recovery required.
3. **Backup `/etc/pve/corosync.conf`** off-cluster before touching anything
   (`scp` to the workstation) — it's small, cheap insurance.
4. **No in-flight VM operations** on any node (migrations, backups,
   snapshots) — a `pve-cluster` restart briefly interrupts the `/etc/pve`
   FUSE mount; don't stack it on top of something else that reads/writes
   through it.
5. **Off-hours / low-traffic window** — Vault, ArgoCD, and every workload
   depending on the cluster staying quorate should not be mid-critical-path.

## Sequence — one node at a time, verify quorum between each

The forum precedent's procedure, adapted to this case (addresses are already
correct in `corosync.conf`; this is about forcing pmxcfs to actually re-sync
its persisted node registry, not changing any address):

1. Re-verify gate 1 (quorum 5/5) immediately before starting.
2. On **pve5 only** (the node whose record is wrong):
   - `systemctl stop pve-cluster`
   - `systemctl stop corosync`
   - Confirm both stopped: `systemctl is-active corosync pve-cluster` → both
     `inactive`.
3. Still on pve5: `pmxcfs -l` (starts pmxcfs in local, non-clustered mode —
   this is what actually lets you write to `/etc/pve` without corosync
   running, per the community precedent).
4. Verify `/etc/pve/corosync.conf` still shows `ring0_addr: 192.168.1.204`
   for pve5 (it should — nothing here changes the file, only the daemon
   mode). If pmxcfs's local-mode `.members`/self-registration writes a fresh
   entry for pve5 at this point, that's the actual fix; check
   `cat /etc/pve/.members` before moving on.
5. Stop the local-mode pmxcfs (`killall pmxcfs` or `fg`+Ctrl-C depending on
   how step 3 was run), then bring the normal cluster stack back in order:
   - `systemctl start corosync`
   - Verify `corosync-cfgtool -s` shows pve5 reconnecting to the other 4
     before proceeding — do not start `pve-cluster` on a corosync that
     isn't talking to its peers yet.
   - `systemctl start pve-cluster`
6. From pve1 (not pve5): `cat /etc/pve/.members` — check pve5's entry.
   `corosync-quorumtool -l` — confirm 5/5 still present.
7. If pve5's entry is now `.204`: re-test the actual failure — API proxy
   from pve1 and pve2 to `/nodes/pve5/status` (both should return `200`).
8. If it's still `.169` after this: the local-mode pmxcfs step didn't
   re-register the address either. Next escalation is Proxmox's fully
   invasive path — de-join and re-join pve5 to the cluster (`pvecm delnode`
   from a healthy node, then `pvecm add` from pve5) — which the same forum
   thread explicitly recommends avoiding unless nothing else works, because
   it briefly removes pve5 from cluster membership and re-adds it as a new
   join. That is a separate, larger decision — stop and re-plan rather than
   improvising it in this window.

## Abort criteria

- Quorum drops below 3/5 at any point → stop immediately, do not proceed to
  the next node/step. Recover quorum first (the surviving majority keeps the
  cluster alive; a `pve-cluster`/`corosync` restart on the affected node,
  once its peers are confirmed healthy, should rejoin it).
- pve5 fails to reconnect after `systemctl start corosync` in step 5 →
  do not start `pve-cluster` on it in this state; use console access to
  investigate before retrying.
- Any node other than pve5 shows corosync link trouble during this window →
  stop; this procedure should only ever touch pve5's own services.

## Post-checks

- `corosync-quorumtool -l` on all 5 nodes (or at least 3) — 5/5 present.
- `/etc/pve/.members` on pve1, pve2, and pve5 — pve5 shows `192.168.1.204`
  everywhere, not just on pve5 itself.
- API proxy re-test: `curl` `/nodes/pve5/status` through pve1's and pve2's
  endpoints — both `200`.
- `terraform plan` in `terraform/` — the GPU VM (`vmid 500`) and
  `talos-worker-05` (`vmid 304`) refresh cleanly instead of erroring.
- Only then: the dev VM's `terraform apply` (`infra#109`/`#110`) is safe to
  run against pve5.

## Related, separate, already fixed

The DNS round-robin at the gateway (`pve5.attlocal.net` resolving to both
`.169` and `.204`) is a distinct problem from the pmxcfs issue above — fixed
tonight via `/etc/hosts` pins on pve1-pve4, unrelated to whether this runbook
gets executed. See `docs/host-addressing.md` for the gateway-DNS context;
its IP Allocation table was re-verified live and pve1-pve5 already all carry
Fixed Allocation reservations (that document's premise that they were
missing was stale as of 2026-08-09).
