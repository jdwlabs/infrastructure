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

## Hardware reality: no Proxmox host has IPMI/BMC; TrueNAS unconfirmed

Checked live against pve1-5, 2026-08-14 (`dmidecode -t 2`, `dmidecode -t 38`,
`/dev/ipmi*`):

| Host | Address | Manufacturer / Product | IPMI/BMC |
| --- | --- | --- | --- |
| pve1 | 192.168.1.200 | GMKtec (mini PC) | none |
| pve2 | 192.168.1.201 | Bosgame ADB20 (mini PC) | none |
| pve3 | 192.168.1.202 | Bosgame ADB20 (mini PC) | none |
| pve4 | 192.168.1.203 | Bosgame ADB20 (mini PC) | none |
| pve5 | 192.168.1.204 | ASUS ROG STRIX X870E-E (consumer desktop board) | none |
| TrueNAS | 192.168.1.205 | not checked — see "TrueNAS — pending" below | unknown |

All five Proxmox hosts are consumer-grade with no enterprise out-of-band
management, and none can gain it without a hardware swap — this rules out
the IPMI path for pve1-5 specifically. TrueNAS has not actually been
inspected yet (see below), so it isn't included in that conclusion.

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
| TrueNAS | unknown | not yet configured — pending SSH access |

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

## Known gap

WoL-based recovery does not satisfy full "unplanned power-off" recovery —
only the soft-off/crash subset. Closing the AC-loss gap needs a networked
smart plug or switched PDU per host, which is not present today. Tracked as
a residual risk on JDWLABS-325 rather than silently treated as solved by
this runbook.

## TrueNAS — pending

A documented access path exists — `scenarios/truenas-nfs-storage.md` and
`scenarios/minio-tls-state-backend.md` both use
`ssh -i ~/.ssh/id_ed25519_pve truenas_admin@192.168.1.205` — but that key is
not present on the operator workstation this runbook was written from
(checked 2026-08-14: no `~/.ssh/id_ed25519_pve` on that machine). This is a
missing-credential gap on one workstation, not a missing access path. Either
locate/restore `id_ed25519_pve`, or add the workstation's current key
(`~/.ssh/id_ed25519`, already authorized as `root` on pve1-5) to
`truenas_admin`'s authorized keys on TrueNAS.

TrueNAS SCALE's WoL setting (if the hardware supports it) lives in its own
web UI under the network interface configuration, not via raw `ethtool` the
way the Proxmox hosts were configured — confirm hardware support and enable
it there once access exists, then update the table above.

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
