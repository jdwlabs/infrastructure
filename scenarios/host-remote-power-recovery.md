# Remote Power Recovery for Proxmox/TrueNAS Hosts

How to recover a pve1-5 or TrueNAS host that has gone unreachable without
physical access, what this procedure does and does not cover, and how the
unreachable-host alert works.

## Why this exists

2026-08-11: TrueNAS (192.168.1.205) was found powered off with no warning and
no diagnostic trail. Cause unknown — no remote visibility into why it went
down, and recovery required physically finding and power-cycling the machine.
See JDWLABS-325 for the full incident writeup and blast-radius detail
(devbox stuck in D-state, Loki ingestion failing, democratic-csi
crash-looping, Terraform's MinIO state backend unreachable).

## Hardware reality: no host in this fleet has IPMI/BMC, TrueNAS included

Checked live against pve1-5, 2026-08-14 (`dmidecode -t 2`, `dmidecode -t 38`,
`/dev/ipmi*`). TrueNAS checked separately, 2026-08-21, once SSH access
existed (see "TrueNAS access" below) — `ipmitool mc info` (fails: "Could not
open device at /dev/ipmi0 or /dev/ipmi/0 or /dev/ipmidev/0: No such file or
directory"), `lspci | grep -iE 'ipmi|bmc|aspeed|management'` (no match),
`ls /dev/ipmi*` (no match). `dmidecode` itself could not be run on TrueNAS —
it needs root and the `truenas_admin` session has no non-interactive sudo
(see below) — but three independent no-BMC signals agree, which is the same
evidence class the pve1-5 row already relies on:

| Host | Address | Manufacturer / Product | IPMI/BMC |
| --- | --- | --- | --- |
| pve1 | 192.168.1.200 | GMKtec (mini PC) | none |
| pve2 | 192.168.1.201 | Bosgame ADB20 (mini PC) | none |
| pve3 | 192.168.1.202 | Bosgame ADB20 (mini PC) | none |
| pve4 | 192.168.1.203 | Bosgame ADB20 (mini PC) | none |
| pve5 | 192.168.1.204 | ASUS ROG STRIX X870E-E (consumer desktop board) | none |
| TrueNAS | 192.168.1.205 | AMD AM5 board (per JDWLABS-325 comment history: B850M EAGLE / Ryzen 7 7700X) | none (confirmed 2026-08-21, see above) |

All six hosts are consumer-grade with no enterprise out-of-band management,
and none can gain it without a hardware swap — this rules out the IPMI path
for the whole fleet, not just pve1-5.

## Mechanism: Wake-on-LAN, with a real scope limit

No networked PDU or smart-plug hardware exists on this rack today — checked
2026-08-14, the only PDU in the rack (StarTech 16NM8-RACK-MOUNT-PDU) is
metered-display-only with a single shared breaker, no network control, no
per-outlet switching even locally. The mechanism available without new
hardware is Wake-on-LAN.

**What WoL covers:** a host that is soft-off (ACPI shutdown, OS crash while
the PSU still has standby power) can be woken remotely by sending a magic
packet to its NIC.

**What WoL does not cover:** a host that has lost AC power entirely — breaker
trip, someone switching off the strip, a PSU that hard-cut — cannot be woken
by WoL, because the NIC itself has no standby power to listen for the packet.
The 2026-08-11 TrueNAS incident's root cause was never confirmed, so this gap
is real, not hypothetical. Closing it fully needs a networked smart
plug/PDU per host — tracked as follow-up, not done here.

## Current state (pve1-5)

WoL enabled and persisted via a systemd oneshot unit
(`/etc/systemd/system/wol-enable.service`, `ExecStart=/sbin/ethtool -s <nic>
wol g`, `WantedBy=multi-user.target`) on all five hosts, 2026-08-14. Chosen
over an `/etc/network/interfaces` `post-up` hook to avoid touching the live
network stanza on production hypervisors — this file only runs `ethtool`,
nothing network-topology-affecting.

| Host | Physical NIC | Wake-on |
| --- | --- | --- |
| pve1 | `nic1` | `g` (enabled) |
| pve2 | `nic0` | `g` (enabled) |
| pve3 | `nic0` | `g` (enabled) |
| pve4 | `nic0` | `g` (enabled) |
| pve5 | `nic0` | `g` (enabled) |
| TrueNAS | `enp7s0` (MAC `30:56:0f:24:01:61`) | not yet configured — see "TrueNAS access" below |

MACs are not repeated here — `docs/host-addressing.md` is the single source
of truth for them (it already documents this fleet's history of address/MAC
drift), and the wake commands below pull from there directly rather than
from a second, driftable copy.

Verify on any host:

```bash
ssh root@<host-ip> "ethtool <nic> | grep -i wake"
# expect: Wake-on: g
```

## Waking a host

From any machine on the same LAN segment (magic packets don't route past the
gateway without explicit relay config — this is a LAN-only procedure, same
scope as `host-addressing.md`'s management access):

Look up the target host's MAC in `docs/host-addressing.md`'s `vmbr0 MAC`
column (that table is the single source of truth — see the note above), then:

```bash
# needs the `wakeonlan` or `etherwake` package
wakeonlan <mac-from-host-addressing.md>
```

Confirm the host is back:

```bash
ssh root@<host-ip> uptime
```

An uptime near zero confirms this was a genuine cold boot, not a network
blip — useful when the cause of the original outage is still unclear.

## Detection: unreachable-host alert

A `prometheus-blackbox-exporter` TCP-connect probe (port 22) against all 6
management IPs fires `HardwareHostUnreachable` after 5 minutes of failed
connects — see `jdwlabs/platform` PR #247 for the alerting change. ICMP was
not used: it needs the `NET_RAW` capability, and TCP connect to SSH gives an
equally valid reachability signal with zero elevated capabilities — a
least-privilege choice, not one forced by namespace policy (the `monitoring`
namespace's Pod Security Admission is `privileged`, since it carries
`quotaTier: platform`, so `NET_RAW` would in fact have been permitted here).

This closes the original incident's actual failure mode: TrueNAS going dark
was discovered only through downstream Kubernetes/Terraform symptoms,
minutes to hours after the fact, instead of a direct alert within 5 minutes.

**Verified live in-cluster, 2026-08-21** (the PR's test plan explicitly left
this for post-deploy verification): `platform-blackbox-exporter-*` pod is
`Running`, all 6 `ServiceMonitor` objects
(`prometheus-blackbox-exporter-{pve1..pve5,truenas}-mgmt`) exist in the
`monitoring` namespace, and a direct Prometheus query
(`GET /api/v1/query?query=probe_success`) returns `probe_success == 1` for
all 6 targets (`pve1-mgmt` … `pve5-mgmt`, `truenas-mgmt`). The
`HardwareHostUnreachable` and `HardwareHostProbeMissing` rules are present
in the `platform-blackbox-exporter-prometheus-blackbox-exporter`
`PrometheusRule` object with the expected `expr`s. This is reachability
confirmation only — the alert firing on a real outage has not been
independently drilled (see "Testing" below, which covers WoL recovery, not
the alert path itself).

## Off-LAN access (JDWLABS-284 relationship)

This procedure is LAN-only end to end: waking a host needs a magic packet
sent from the same LAN segment (see "Waking a host" above), and the alert
in the previous section fires from Prometheus, which already runs
in-cluster/on-LAN — neither leg depends on off-LAN connectivity today.

`JDWLABS-284`'s Tailscale subnet router (see
`docs/tailscale-subnet-router.md`) is a different capability: off-LAN
`kubectl`/`talosctl` access, not a relay for this runbook's mechanisms. It
does not gate anything here. Two things worth flagging if the operator ever
wants to run this procedure from off-LAN:

- The subnet router's own off-LAN path is **not yet proven** — per
  `docs/tailscale-subnet-router.md`, the route is live and approved on the
  LAN side, but step 3 (verifying access from a genuinely different network)
  is still outstanding as of that doc's last update.
- Even once proven, it is **not established** that a WoL magic packet sent
  through the routed `192.168.1.0/24` subnet would actually reach a target
  host. `wakeonlan` typically sends to the LAN broadcast address
  (`192.168.1.255`); whether a *directed* broadcast like that traverses a
  Tailscale subnet route depends on the router's own directed-broadcast
  handling, which has not been tested and is often disabled by default as
  an anti-smurf-attack measure. Do not assume off-LAN WoL works without
  testing it directly — if it doesn't, unicast `wakeonlan -i <lan-ip>` to
  each host's real IP (bypassing broadcast) is the fallback to test instead.

Neither point blocks this ticket's Definition of Done, which only requires
a documented, tested (LAN) procedure — but both are relevant the day someone
tries this from outside the house.

## Known gap

WoL-based recovery does not satisfy full "unplanned power-off" recovery —
only the soft-off/crash subset. Closing the AC-loss gap needs a networked
smart plug or switched PDU per host, which is not present today. Tracked as
a residual risk on JDWLABS-325 rather than silently treated as solved by
this runbook.

**Operator decision, 2026-08-14 (recorded on JDWLABS-325):** no follow-up
purchase for smart-plug/PDU hardware. WoL-only coverage (soft-off/crash) is
accepted as the durable state; AC-loss recovery stays physical-access-only.
Re-raise as a new ticket if that trade-off changes.

## TrueNAS access

**Resolved 2026-08-21** — this was the last open item blocking the
Definition of Done. As of 2026-08-14 the operator workstation this runbook
was written from had no `~/.ssh/id_ed25519_pve`, the key
`scenarios/truenas-nfs-storage.md` and `scenarios/minio-tls-state-backend.md`
document for `truenas_admin@192.168.1.205`. Checked again 2026-08-21 from
this session's workstation:

```bash
ssh -o BatchMode=yes truenas_admin@192.168.1.205 "hostname; uptime"
# truenas
#  up 1 day, 23:39, load average: 0.17, 0.20, 0.14
```

This succeeds using the workstation's plain `~/.ssh/id_ed25519` — no
`id_ed25519_pve` needed. Either that key was since added to
`truenas_admin`'s authorized_keys (the fix this doc originally suggested),
or this session's workstation already carried it; either way the access
path this doc always claimed exists is now confirmed live, not just
documented. `root@192.168.1.205` still refuses (`Permission denied
(publickey)`) — use `truenas_admin`, matching the other TrueNAS runbooks in
this repo.

**Still open: WoL support/config on TrueNAS is unconfirmed**, and this
session deliberately did not change it. `truenas_admin` has no
non-interactive sudo (`sudo -n ethtool ...` → `sudo: a password is
required`), so `ethtool enp7s0` cannot report or set the `Wake-on` field —
the read attempt returned `netlink error: Operation not permitted` before
the unprivileged fields it can show. TrueNAS SCALE's WoL setting (if the
hardware supports it) lives in its own web UI under the network interface
configuration, not via raw `ethtool` the way the Proxmox hosts were
configured.

**TODO (needs a human with TrueNAS admin/root access):**
1. Confirm WoL hardware support and current state — either via the TrueNAS
   web UI (Network → Interfaces → `enp7s0`) or `sudo ethtool enp7s0 |
   grep -i wake` from an interactive root/sudo session.
2. If supported, enable it and persist it (TrueNAS SCALE config, not a
   systemd unit like pve1-5 — this is a different OS with its own
   persistence model).
3. Update the "Current state (pve1-5)" table above with TrueNAS's real
   `Wake-on` value once known.
4. Live-test wake-on-LAN against TrueNAS the same way pve2 was tested below,
   off-hours, with the same "send it twice, expect several minutes" caveat.

## Testing

Tested live on pve2, 2026-08-14:

1. Baseline: `ssh root@192.168.1.201 uptime` → `up 66 days`
2. `ssh root@192.168.1.201 "shutdown -h now"` — graceful VM shutdown + poweroff
   took ~90s (2 running VMs: `talos-cp-01`, `talos-worker-02`)
3. Magic packet sent to `84:47:09:63:06:4e` (UDP broadcast, port 9). No
   response after ~3 minutes — **sent a second time** (ports 9 and 7 both),
   host came back within the next poll window.
4. `ssh root@192.168.1.201 uptime` → `up 0 min` — confirmed genuine cold
   boot, not a stale connection
5. `qm list` confirmed both VMs autostarted and running post-boot

**Takeaway: send the magic packet twice** (a single UDP broadcast can be
dropped with nothing to retry it) and expect up to ~3-4 minutes total for a
host carrying VMs to gracefully power off and cold-boot back up — don't
declare failure before that window passes.

Only pve2 tested so far. pve1, pve3, pve4 configured identically and expected
to behave the same; pve5 deliberately not tested (hosts the operator's own
active session) — treat as configured-but-unverified until tested
separately, off-hours.
