# Runbook: pve5's stale IP in the Proxmox cluster filesystem (pmxcfs)

Status: **RESOLVED 2026-08-09.** Root cause was simpler than the diagnostics
below first suggested — kept here because the ruled-out list and the
recovery mechanics (especially the SSH lockout) are worth knowing if this
class of problem recurs on another node.

## Why this mattered

Since the 2026-08-06 pve5 IP-drift recovery (`docs/proxmox-host-ip-drift-and-dhcp.md`
— pve5 moved `.169` → `.204`), every other Proxmox node's cluster filesystem
still recorded pve5's address as the dead `192.168.1.169`. This broke the
Proxmox API's inter-node proxy: any `/nodes/pve5/...` call routed through
pve1 or pve2 failed with `HTTP 595 No route to host`, which blocked
Terraform/`talops` for anything on pve5 — the GPU inference VM,
`talos-worker-05`, and the planned dev VM (`infra#109`/`#110`).

Direct API calls to pve5's own endpoint (`192.168.1.204:8006`) worked fine
throughout — this was purely an inter-node proxy problem.

## Root cause

**pve5's own `/etc/hosts` had a hardcoded self-entry left over from before
its 2026-08-06 migration off static addressing:**

```
192.168.1.169 pve5.attlocal.net pve5
```

`pmxcfs` resolves its own hostname at startup to determine "the default node
IP address" to register itself with in the cluster — confirmed directly in
its startup log:

```
[main] notice: resolved node name 'pve5' to '192.168.1.169' for default node IP address
```

That resolution reads `/etc/hosts` first (standard NSS order), so it kept
finding the dead address no matter what corosync.conf said, no matter how
many times any cluster service was restarted, and no matter what DNS at the
gateway returned. Every node's `.members` (including pve5's own self-view)
inherited this because pve5 announces its own address to its peers via
corosync — it wasn't independently wrong on 4 different nodes, it was wrong
once, at the source, and correctly propagated everywhere.

## What ruled this out took five wrong turns first

All safe, none of them fixed it (kept for the next time this class of
problem shows up and looks like a corosync/pmxcfs bug rather than a stale
`/etc/hosts` line):

1. `systemctl restart pve-cluster` on pve1 (the reader) — no change.
2. `systemctl restart pve-cluster` on pve5 (the source) — no change. In
   hindsight this *should* have been the first clue it wasn't a live-derived
   value, but pmxcfs's own startup log wasn't checked until much later.
3. `systemctl restart pveproxy` on pve1 — no change.
4. `/etc/hosts` pins added on **pve1-pve4** (`192.168.1.204 pve5.attlocal.net
   pve5`) — fixed DNS resolution *from those nodes*, but the actual problem
   was on pve5 itself, resolving its own name wrong. Left in place; harmless
   and still correct.
5. `corosync.conf` `config_version` bump (12→13) + live propagated reload —
   verified quorum-safe (5/5 before and after), pmxcfs's internal version
   counter incremented proving the reload was real — but `.members` was
   untouched, because corosync.conf was never the source of the bad value.

The lesson: when a Proxmox node's `.members` entry disagrees with its actual
address and nothing corosync-side fixes it, check the node's own
`/etc/hosts` for a self-referential entry before assuming pmxcfs's internal
database needs surgery.

## The fix, as executed

On **pve5 only**, in order:

```
systemctl stop pve-cluster
systemctl stop corosync
pmxcfs -l                              # local mode; confirms/replaces the bad self-resolution
grep pve5 /etc/hosts                   # found: 192.168.1.169 pve5.attlocal.net pve5
sed -i 's/192.168.1.169 pve5.attlocal.net pve5/192.168.1.204 pve5.attlocal.net pve5/' /etc/hosts
killall pmxcfs                         # exit local mode
systemctl start corosync
# verify from another node that pve5 reconnects (corosync-cfgtool -s) before continuing
systemctl start pve-cluster
cat /etc/pve/.members                  # now correctly shows 192.168.1.204
```

Verified afterward: `.members` correct on pve1 and pve5, quorum 5/5 on both,
`terraform plan` refreshes the GPU VM and `talos-worker-05` cleanly (no more
`595`), and a direct API proxy test from both pve1 and pve2 to
`/nodes/pve5/status` returned `200`.

## Important side effect: this breaks SSH key auth on the target node

**`systemctl stop pve-cluster` immediately locks out root's SSH key-based
login on that node.** Proxmox propagates root's `authorized_keys`
cluster-wide by symlinking it through `/etc/pve/priv/` — killing pmxcfs
unmounts that, and the symlink target vanishes. Hit this live: the SSH
session doing the stop got `Permission denied (publickey,password)` on the
very next connection attempt.

**Password authentication (PAM) survives fine** — it doesn't touch
`/etc/pve` at all. The actual workable precondition for this class of fix
is "keep a password-authenticated SSH session open to the target node
throughout," not full physical/BMC console access (though that remains the
ultimate fallback if password auth is disabled or the network path itself
is the problem). Confirmed: reconnecting with `ssh <host>` and the root
password worked immediately, even with `pve-cluster` down, and stayed
connected through the entire stop → edit → restart sequence.

**Where that root password comes from (JDWLABS-445):** at the time this
runbook was first written there was nowhere retrievable to get it from —
exactly the gap that caused the 2026-08-27 pve3 outage (JDWLABS-437) to be
console-only. As of JDWLABS-445, each host's root PAM password is stored in
this cluster's Vault at `kv/pve-hosts/<host>/root` (field `password`) —
retrieve it with `kubectl exec -n vault platform-vault-0 -- vault kv get
-field=password kv/pve-hosts/<host>/root` and use it at `sshd`'s password
prompt (never as a CLI arg). See
`scenarios/host-remote-power-recovery.md`'s "SSH key auth failure" section
for the full retrieval procedure and `scenarios/pve-root-vault-wizard.sh`
for how the password gets set/stored in the first place. As of 2026-08-29
this is the documented mechanism, not a guarantee — confirm the password
actually exists for the target node (`vault kv list kv/pve-hosts/`) before
relying on it during a live incident.

## If this recurs on a different node

1. Confirm quorum is healthy first (`corosync-quorumtool -l` from an
   uninvolved node) and stays ≥3/5 throughout — abort and investigate if it
   drops below that.
2. Have a password-authenticated SSH session to the target node ready
   *before* stopping `pve-cluster` there — key auth will break the instant
   it stops. Get the password from Vault (`kv/pve-hosts/<host>/root`, see
   above) rather than assuming you already have it memorized or saved
   elsewhere.
3. Check `grep <hostname> /etc/hosts` on the target node itself first. If a
   stale self-entry exists, this is almost certainly the whole problem —
   fix that before considering anything more invasive (corosync.conf edits,
   de-join/re-join).
4. Never touch more than one node's cluster services at a time.
