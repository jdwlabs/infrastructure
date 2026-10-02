# Runbook: hardware watchdog and crash evidence for the Proxmox hosts

Status: **assessed 2026-10-02, nothing installed.** Every measurement below was
taken read-only. No module was loaded, no watchdog device was opened and no
host was restarted, so three things the design rests on are still unproven on
this hardware: that `sp5100_tco` binds, that its expiry actually resets the
board, and that netconsole works on `vmbr0`. Each is a numbered check in the
install and test sections, and a human runs them.

## Why this exists

pve1 hard-froze on 2026-10-01 and stayed frozen for 2h08m until someone
power-cycled it by hand. The power LED was on, Wake-on-LAN was ignored, and the
journal simply stops between two entries of a once-a-minute cadence:

```
2026-10-01T23:43:53+00:00 pve1 sshd-session[2657411]: Connection closed by 192.168.1.87 port 7663
2026-10-01T23:44:53+00:00 pve1 sshd-session[2657615]: Connection closed by 192.168.1.87 port 34287
                                                    # next boot: 2026-10-02T01:53:07+00:00
```

pve1 carries the only load balancer, so the Kubernetes API and all ingress were
down for the whole window.

A watchdog was armed at the time and did nothing. Every host runs `softdog`
with a 10 second timeout, petted once a second by Proxmox's `watchdog-mux`.
`softdog` is a kernel timer: when the kernel stops, the timer that would have
reset the machine stops with it. It protects against a hung userspace, not a
hung kernel. Nothing else on these hosts can reset them either, since no host
in this fleet has IPMI or a BMC (`host-remote-power-recovery.md`).

The freeze also left no evidence. pstore was empty, kdump is not installed, and
`kernel.panic = 0` means even a clean panic would have sat there forever.

This runbook covers both halves: a timer that keeps counting when the kernel
does not, and a place for the kernel's last words to go.

## What was measured, 2026-10-02

All five hosts run kernel `7.0.6-2-pve`, `pve-manager 9.2.3`,
`pve-ha-manager 5.2.4` and systemd 257, and boot through GRUB with
`root=/dev/mapper/pve-root ro quiet`. Collected 07:13 to 07:24 UTC.

| | pve1 | pve2 / pve3 / pve4 | pve5 |
|---|---|---|---|
| Board, CPU | GMKtec NucBox K8, Ryzen 7 8845HS | Bosgame ADB20, Ryzen 5 3550H | ASUS ROG STRIX X870E-E, Ryzen 9 9950X |
| BIOS | NucBox K8 1.07, 2024-03-27 | ADB20D 1.04, 2024-08-28 | 1401, 2025-05-05 |
| FCH SMBus (`lspci -nn`) | `1022:790b` rev 71 | `1022:790b` rev 61 | `1022:790b` rev 71 |
| `sp5100_tco` module | present, not loaded | present, not loaded | present, not loaded |
| Blacklisted | yes | yes | yes |
| `/sys/class/watchdog/watchdog0` | Software Watchdog, active, timeout 10 | same | same |
| Holder of `/dev/watchdog` | `watchdog-mux` | `watchdog-mux` | `watchdog-mux` |
| `RuntimeWatchdogUSec` / `RebootWatchdogUSec` | 0 / 10min | 0 / 10min | 0 / 10min |
| pstore backend, contents | `efi_pstore`, empty | `efi_pstore`, empty | `efi_pstore`, empty |
| kdump, kexec, makedumpfile | not installed | not installed | not installed |
| `crashkernel=` on cmdline | no | no | no |
| RAM total / available | 28.2 / 12.6 GiB | 12.6 / 0.9, 0.8, 1.2 GiB | 123.5 / 26.3 GiB |
| Secure Boot | disabled | disabled | enabled |
| cpuidle driver, states | `acpi_idle`: C1, C2, C3 | `acpi_idle`: C1, C2 | `acpi_idle`: C1, C2, C3 |
| Uptime | 5h20m | 35 days | 9 days |
| NVMe unsafe shutdowns / power cycles | 29 / 49 | 38 / 76, 30 / 69, 29 / 68 | 33 / 38 |

Identical on all five: `kernel.panic = 0`, `panic_on_oops = 0`,
`softlockup_panic = 0`, `hardlockup_panic = 0`, `nmi_watchdog = 1`,
`kernel.printk = 3 4 1 7`, `/etc/default/pve-ha-manager` untouched (its only
content is the commented `#WATCHDOG_MODULE=ipmi_watchdog` example),
`ha-manager status` reporting `fencing standby (CRM watchdog standby)`, no
`/run/watchdog-mux.active`, and `systemd-pstore.service` enabled.

**A hardware watchdog is probably present on all five, and that is as far as a
read-only check can go.** The PCI device `sp5100_tco` binds to (`1022:790b`) is
there on every host, at a revision (`0x61`, `0x71`) the driver handles through
its newest register layout, which applies from revision `0x51` up
(`drivers/watchdog/sp5100_tco.c`, `tco_reg_layout()`). Whether the firmware
lets the driver enable the timer only shows when the module is loaded. The
driver has three ways to refuse, each with its own message: `Watchdog hardware
is disabled`, `Failed to enable the timer`, and `Failed to reserve MMIO or
alternate MMIO region`. pve5 is the one host with something already reserved at
the driver's preferred address (`feb00000-feb00fff : PNP0C02:03` in
`/proc/iomem`); the driver has an alternate address for that case, untested
here.

**The blacklist is real and does not matter.** The kernel package ships it:

```
$ grep -n -E 'softdog|sp5100' /lib/modprobe.d/blacklist_proxmox-kernel-7.0.6-2-pve.conf
56:blacklist softdog
57:blacklist sp5100_tco
$ dpkg -S /lib/modprobe.d/blacklist_proxmox-kernel-7.0.6-2-pve.conf
proxmox-kernel-7.0.6-2-pve-signed: /lib/modprobe.d/blacklist_proxmox-kernel-7.0.6-2-pve.conf
```

`softdog` is on the same list and is loaded on every host, because `blacklist`
only stops alias-driven autoloading and `watchdog-mux` asks for its module by
name. Nothing needs to be removed from the blacklist, and nothing should be: a
new file ships with every kernel.

**Do not use `wdctl` to inspect a watchdog nobody holds.** On these hosts it
tries to open the device rather than read sysfs:

```
$ wdctl /dev/watchdog
wdctl: cannot read information about /dev/watchdog: Device or resource busy
```

That failure is the safe outcome, and it only happens because `watchdog-mux`
already holds the device. Opening an unheld watchdog starts it. Read
`/sys/class/watchdog/watchdogN/` instead, which is what every check in this
runbook does.

### Unclean boot history

From `journalctl --list-boots` and `last -x`, converted to UTC. An unclean end
that several hosts share to within seconds is a power event, not a freeze.

| When (UTC) | Hosts | Reading |
|---|---|---|
| 2026-05-09 22:03 | pve1, pve2, pve3, pve4 | fleet-wide, all back within two minutes |
| 2026-05-24 02:19 | pve1, pve2, pve3, pve4 | fleet-wide, 1h52m dark |
| 2026-05-24 05:37 | pve2 | alone, back in 67 seconds, same night as the two around it |
| 2026-05-24 05:40 to 05:43 | pve1, pve2, pve3, pve4 | fleet-wide |
| 2026-08-27 18:04 | pve3 | journal stops; next boot 10h17m later. This is the outage `host-remote-power-recovery.md` records as an SSH-key failure with `sshd` still answering, so it is not counted as a freeze. Why the journal stopped is not established. |
| 2026-08-28 04:19 to 04:20 | pve2, pve4 (and pve3's reboot) | all three back at 04:21 |
| 2026-10-01 23:44 | pve1 | alone, 2h08m, the freeze this runbook is about |

So in the journal history available (pve1's goes back to 2026-01-04), the
2026-10-01 event is the only one that looks like a single-host hard freeze.
pve5's `last -x` shows no unclean boot at all. The NVMe unsafe-shutdown counters
are lifetime totals that include whatever happened before these hosts were
installed, and do not separate freezes from pulled plugs.

pve2, pve3 and pve4 keep local time in `America/Adak` (UTC-9) while pve1 and
pve5 use `America/Chicago`. Pass `--utc` to `journalctl` when lining hosts up
against each other.

## Does watchdog-mux arm the device with no HA resources? Yes.

This decides the design, so it was read from source rather than assumed. If
`watchdog-mux` only petted the device on behalf of an HA client, switching its
module to a hardware one would arm nothing on a cluster with no HA resources.

It does not work that way. In `src/watchdog-mux.c` (pve-ha-manager, read at
HEAD on 2026-10-02; the strings in the installed 5.2.4 binary match):

- At startup, if `/dev/watchdog` does not exist, it runs `modprobe -q` on
  `$WATCHDOG_MODULE`, or on `softdog` when that is unset. The unit reads the
  variable from `/etc/default/pve-ha-manager`.
- It then opens `/dev/watchdog`, which starts the timer, and sets a timeout of
  10 seconds. The value is a constant in the source, not a setting.
- Its main loop wakes every second and issues `WDIOC_KEEPALIVE` whenever no
  client has expired. With no clients at all, that condition is always true.
- On `SIGTERM` with no active client it writes the magic close character and
  closes the device, which disarms it. With an active client it exits without
  closing, and the host resets ten seconds later. That is HA fencing working as
  designed.

The live hosts agree: `state: active`, `timeout: 10`, `timeleft: 9` on all
five, with `pvesh get /cluster/ha/resources` returning `[]`. The device is
armed and petted today. It is simply the wrong kind of device.

The Proxmox VE administration guide, HA chapter, "Configure Hardware Watchdog",
gives `WATCHDOG_MODULE` in `/etc/default/pve-ha-manager` as the supported way
to change it.

## Design

**`watchdog-mux` owns the hardware watchdog, selected with
`WATCHDOG_MODULE=sp5100_tco`.** One line in one file per host.

Why this owner:

- It is the mechanism Proxmox documents, and `watchdog-mux` opens
  `/dev/watchdog` on every Proxmox node whether or not anyone wants it to. A
  watchdog device has one opener. Anything else that wants the hardware timer
  has to arrange for `watchdog-mux` to end up on a different device.
- It changes one variable. The timeout (10 seconds), the petting cadence (1
  second) and the process doing the petting stay exactly as they have been on
  all five hosts, including 35 days on pve2 to pve4 under constant memory
  pressure, without a false reset. The only thing that changes is that the
  countdown no longer depends on the kernel being alive.
- If HA resources are ever defined, fencing is hardware-backed from that day
  with no further change.

**Timeout: 10 seconds, not configurable.** `watchdog-mux` overrides the driver's
`heartbeat` parameter with its own constant. A frozen pve1 would be reset within
ten seconds and, going by its last boot (54.9 s from firmware to userspace),
serving again in a few minutes rather than two hours.

The cost of that timeout is a new class of false positive. A kernel stall
longer than ten seconds that would have cleared by itself, a long firmware
interrupt for instance, now resets the host. `softdog` could not fire during
such a stall because its own timer was stalled too. Nothing of the kind is
visible in these hosts' logs, but it would not be: this is accepted, not
disproven.

**No HA behaviour is switched on by this.** `watchdog-mux` stops petting only
for a client that connected and then went quiet for 60 seconds. With no HA
resources the LRM and CRM never connect (`/run/watchdog-mux.active` does not
exist on any host), so losing quorum still does not reset a host.

**Normal reboots and kernel upgrades do not trip it.**

- On shutdown systemd stops `watchdog-mux`, which disarms the timer through the
  magic close. `sp5100_tco` supports that (`WDIOF_MAGICCLOSE`), is loaded with
  `nowayout=0`, and additionally registers a stop-on-reboot hook. Leave
  `nowayout` alone: setting it would make every reboot a race against ten
  seconds.
- The timer is not running during firmware, GRUB or early boot. The driver
  stops it when it loads, and it starts only when `watchdog-mux` opens the
  device. The other side of that: a host that freezes before `watchdog-mux`
  starts is not covered.
- A new kernel brings a new blacklist file, which does not affect a module
  loaded by name. `/etc/default/pve-ha-manager` is a dpkg conffile, so an
  upgrade of `pve-ha-manager` that changes the shipped file will ask which
  version to keep. Keep the local one.

**Rejected: systemd's `RuntimeWatchdogSec`.** It would give a configurable
timeout and have PID 1 do the petting, both attractive. But it opens
`/dev/watchdog0` by default (`systemd-system.conf(5)`), the device
`watchdog-mux` already holds. Making the two coexist means loading `sp5100_tco`
as a second watchdog device and pointing `WatchdogDevice=` at it, and which
driver becomes `watchdog0` depends on module load order at boot. Get the order
wrong and `watchdog-mux` takes the hardware device, systemd gets `EBUSY`, and
the configuration is silently not what was written down. It would also leave
any future HA fencing on `softdog`.

## Crash evidence

A watchdog reset destroys the state that would explain the freeze. Three
mechanisms, in the order they are worth having:

**1. Sysctls, so a detectable failure panics and a panic reboots.**

```
# /etc/sysctl.d/90-crash-capture.conf
kernel.panic = 5
kernel.panic_on_oops = 1
kernel.hardlockup_panic = 1
kernel.softlockup_all_cpu_backtrace = 1
kernel.printk = 5 4 1 7
```

- `panic = 5` is deliberately shorter than the watchdog's ten seconds. A
  panicked kernel stops petting, so both timers start together; the panic's own
  reboot should win, which keeps "the watchdog fired" meaning "the kernel never
  got as far as a panic".
- `hardlockup_panic = 1` turns a CPU stuck with interrupts off, which the NMI
  detector already notices after ten seconds, into a panic with a trace instead
  of a log line nobody reads.
- `panic_on_oops = 1` is a judgement call. A kernel that has oopsed is not
  trustworthy under running guests, and a clean reboot beats a half-alive
  hypervisor the watchdog keeps being told is fine. The cost: an oops that
  would have killed one task now restarts every guest on the host.
- `softlockup_panic` stays 0. pve2, pve3 and pve4 run with under 1.2 GiB
  available and swap in use, and pve5 has more memory allocated to guests than
  it has. A twenty-second scheduling stall under that pressure is plausible and
  survivable. `softlockup_all_cpu_backtrace` gets the evidence without the
  reboot.
- `printk = 5 4 1 7` raises the console level from 3 so that warnings and
  errors reach netconsole. At 3, an RCU stall report (error level) would never
  leave the host. 5 stops short of the notice-level audit lines these hosts
  emit steadily.

**2. netconsole on every host, to a receiver on a different host.** It costs no
memory, needs no reboot, and sends each kernel message as it is printed rather
than when journald next syncs to disk. `CONFIG_NETCONSOLE=m` is in the running
kernel on all five.

| Sender | Receiver | UDP port |
|---|---|---|
| pve1 | pve5 (192.168.1.204) | 6661 |
| pve2 | pve5 | 6662 |
| pve3 | pve5 | 6663 |
| pve4 | pve5 | 6664 |
| pve5 | pve1 (192.168.1.200) | 6665 |

One port per sender lets the receiver keep the streams apart without parsing.
The receiver is `socat`, already installed on all five, writing to the journal
so the lines get the receiver's timestamps and normal rotation. `pve-firewall`
is disabled on every host, so nothing needs opening, and equally nothing is
filtering: each listener accepts only its own sender's address. That is a
filter, not authentication. UDP source addresses can be forged by anything on
the LAN, so treat these lines as diagnostics from a trusted network and not as
a tamper-proof record.

Read it on the receiver:

```bash
ssh root@192.168.1.204 'journalctl --utc -t netconsole-pve1 --since "-1h"'
```

**3. pstore, already there.** `efi_pstore` is the active backend on all five and
`systemd-pstore.service` is enabled, so a panic should leave the tail of the
kernel log in EFI variables and the next boot should move it to
`/var/lib/systemd/pstore/`. It has never been exercised on these boards; the
panic test below is what proves it.

**kdump: not recommended now, and not possible on three hosts.**

| Host | Verdict |
|---|---|
| pve1 | Affordable (12.6 GiB available). Deferred: install it if a panic recurs and the netconsole trace is not enough to act on. |
| pve2, pve3, pve4 | No. A crash kernel needs a few hundred MiB reserved at boot, and these hosts have 0.8 to 1.2 GiB available with swap already in use. The reservation would come straight out of the Talos guests. |
| pve5 | Not now. Guests are allocated 128 GiB against 123.5 GiB of RAM, and Secure Boot is enabled, which constrains how a crash kernel may be loaded. Neither was investigated further. |

kdump would also not have helped on 2026-10-01. It runs on panic, and that
freeze produced none.

**What none of this captures.** If every CPU stops at once, or the machine
hangs in firmware, nothing is printed and nothing panics. The watchdog resets
the host and the only evidence is the reset itself: a gap in the journal and a
non-zero `bootstatus` on the watchdog device. That is still more than there is
today, when the same event is indistinguishable from a power cut.

## Install, one host

Written for pve1. Substitute the address, receiver and port from the table
above for the others. Everything up to step 4 is live and needs no reboot.

### 0. Receivers, once

On pve5, and the same on pve1 with `pve5` as the only instance:

```bash
cat > /etc/systemd/system/netconsole-rx@.service <<'EOF'
[Unit]
Description=netconsole receiver for %i
After=network-online.target
Wants=network-online.target

[Service]
# Port is 6660 plus the sender's host number, and its address 192.168.1.199
# plus the same, so one template serves all. range= drops datagrams from any
# other source, keeping stray traffic out of what will be read as evidence.
ExecStart=/bin/sh -c 'n=$${1#pve}; exec /usr/bin/socat -u UDP-RECV:$$((6660 + n)),reuseaddr,range=192.168.1.$$((199 + n))/32 STDOUT' sh %i
SyslogIdentifier=netconsole-%i
DynamicUser=yes
Restart=always

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now netconsole-rx@pve1 netconsole-rx@pve2 netconsole-rx@pve3 netconsole-rx@pve4
ss -ulnp | grep -E ':666[1-4]'          # four socat listeners
```

### 1. Prove the driver binds, without arming it

While `softdog` holds `watchdog0`, a second driver registers as `watchdog1`
and nobody opens it. Loading does write to the chipset (the driver enables the
timer's register decoding and then stops the timer), so this is a change to a
live host, but not one that can reset it.

```bash
cat /sys/class/watchdog/watchdog0/identity     # Software Watchdog
modprobe sp5100_tco
dmesg | grep -i -E 'sp5100|tco' | tail
cat /sys/class/watchdog/watchdog1/identity /sys/class/watchdog/watchdog1/state
rmmod sp5100_tco
```

Expected: `SP5100/SB800 TCO WatchDog Timer Driver`, `Using 0x... for watchdog
MMIO address`, `initialized. heartbeat=60 sec (nowayout=0)`, then `SP5100 TCO
timer` and `inactive`.

**Stop here if** there is no `watchdog1`, or dmesg shows `Watchdog hardware is
disabled`, `Failed to enable the timer` or `Failed to reserve MMIO`. The board
does not expose the timer to Linux and step 4 would leave the host with no
watchdog at all. Record the message under Known gaps and do the remaining steps
except 4.

Do not run `wdctl` and do not open `/dev/watchdog1` while it is loaded.

### 2. Sysctls

Write `/etc/sysctl.d/90-crash-capture.conf` as above, then:

```bash
sysctl --system
sysctl kernel.panic kernel.panic_on_oops kernel.hardlockup_panic kernel.printk
```

### 3. netconsole sender

The target MAC is the receiver's `vmbr0` MAC from `docs/host-addressing.md`.
Without one netconsole broadcasts every line to the LAN.

```bash
cat > /etc/modprobe.d/netconsole.conf <<'EOF'
options netconsole netconsole=6665@192.168.1.200/vmbr0,6661@192.168.1.204/<pve5 vmbr0 MAC>
EOF
cat > /etc/systemd/system/netconsole.service <<'EOF'
[Unit]
Description=Send kernel messages to the netconsole receiver
# Not modules-load.d: the module resolves its interface when it loads, and
# vmbr0 has no address that early in boot.
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/sbin/modprobe netconsole
ExecStop=/sbin/modprobe -r netconsole

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now netconsole
dmesg | grep -i netconsole | tail -3          # "network logging started"
echo '<2>netconsole test from pve1' > /dev/kmsg
ssh root@192.168.1.204 'journalctl -t netconsole-pve1 -n 3 --no-pager'
```

The interface has to be `vmbr0`. The physical NIC is a bridge port, and the
kernel refuses to attach netconsole to one.

Then confirm the bridge still accepts a new port, because a bridge with
netconsole attached requires every port to support it. Restart a guest that can
take it (devbox2 on pve1) and check its tap rejoins:

```bash
qm shutdown 112 && qm start 112
ls /sys/class/net/vmbr0/brif          # tap112i0 present
```

If the guest fails to start, or the test line never arrives, run
`systemctl disable --now netconsole` and record it. The watchdog does not depend
on this step.

### 4. Switch the watchdog module

```bash
cp -a /etc/default/pve-ha-manager /root/pve-ha-manager.before-watchdog
# The shipped file has no trailing newline, so a bare append would land on the
# end of its comment line and silently do nothing.
printf '\nWATCHDOG_MODULE=sp5100_tco\n' >> /etc/default/pve-ha-manager
grep -n '^WATCHDOG_MODULE' /etc/default/pve-ha-manager     # exactly one line
```

Nothing changes yet. `watchdog-mux` loads a module only when `/dev/watchdog`
does not exist, and `softdog` is providing it. The switch happens at the next
boot, so restart the host with its section of `host-restart-coordination.md`.

An in-place switch without a reboot is possible in principle (stop
`watchdog-mux`, `rmmod softdog`, start it again). It has not been tried, and the
reboot is preferred anyway because it proves the configuration that will be in
force after the next unplanned restart.

### 5. Verify after the reboot

```bash
journalctl -u watchdog-mux -b --no-pager
#   Loading watchdog module 'sp5100_tco'
#   Watchdog driver 'SP5100 TCO timer', version 0
cat /sys/class/watchdog/watchdog0/identity    # SP5100 TCO timer
cat /sys/class/watchdog/watchdog0/state       # active
cat /sys/class/watchdog/watchdog0/timeout     # 10
cat /sys/class/watchdog/watchdog0/nowayout    # 0
lsmod | grep -E 'sp5100|softdog'              # sp5100_tco only
systemctl is-active watchdog-mux netconsole
```

If `watchdog-mux` is `failed` with `watchdog open: No such file or directory`,
the module did not produce a device and the host has **no watchdog of any
kind**. Roll back at once.

### Rollback

```bash
cp -a /root/pve-ha-manager.before-watchdog /etc/default/pve-ha-manager
# then reboot per host-restart-coordination.md; softdog returns as the default.
# If watchdog-mux is already failed, no reboot is needed:
systemctl start watchdog-mux && cat /sys/class/watchdog/watchdog0/identity

systemctl disable --now netconsole
rm /etc/modprobe.d/netconsole.conf /etc/systemd/system/netconsole.service

rm /etc/sysctl.d/90-crash-capture.conf
sysctl -w kernel.panic=0 kernel.panic_on_oops=0 kernel.hardlockup_panic=0 \
  kernel.softlockup_all_cpu_backtrace=0 kernel.printk='3 4 1 7'
```

Removing the sysctl file does not restore the old values until a reboot, which
is why they are also set by hand.

The receivers serve every sender, so they come out only when netconsole is
being withdrawn from the whole fleet:

```bash
# on pve5; on pve1 the only instance is netconsole-rx@pve5
systemctl disable --now netconsole-rx@pve1 netconsole-rx@pve2 netconsole-rx@pve3 netconsole-rx@pve4
rm /etc/systemd/system/netconsole-rx@.service
systemctl daemon-reload
```

## Controlled test

Run it off-hours, in the same window as the step 4 reboot, with the host's
guests drained and shut down exactly as its section of
`host-restart-coordination.md` describes. Both tests end in a reset with no
shutdown sequence, and a guest that is still running takes it as a power cut.
One host at a time, and `pvecm status` back at 5/5 before the next.

Keep a ping running from another machine and note wall-clock times.

### A. The watchdog path

```bash
cat /sys/class/watchdog/watchdog0/identity    # must say SP5100 TCO timer
kill -STOP "$(pidof watchdog-mux)"
```

`SIGSTOP` freezes the petting process with the device still open, which is what
the watchdog sees when the whole machine freezes. Check the identity first:
`softdog` passes this same test, so a pass on the wrong driver proves nothing.

`systemctl stop watchdog-mux` is not a test. It disarms the timer cleanly and
the host carries on.

Expected:

| Time | Event |
|---|---|
| T+0 | `SIGSTOP` sent |
| within T+10 s | pings stop; the SSH session hangs with no shutdown messages |
| about T+1 min | host answers SSH (last measured boot: 49 to 60 s across the fleet) |
| a few minutes | guests with `onboot: 1` running again |

To back out inside the ten seconds: `kill -CONT "$(pidof watchdog-mux)"`.

After it is back:

```bash
uptime
cat /sys/class/watchdog/watchdog0/bootstatus   # 32 if the chipset reports a watchdog reset
last -x | head -3                              # previous boot ends in "crash"
journalctl --utc -b -1 -n 5 --no-pager         # ends abruptly, no "Reached target Shutdown"
journalctl -u watchdog-mux -b --no-pager
```

**If the host does not reset within about 30 seconds, the hardware timer does
not work on this board.** Send `SIGCONT`, and treat the host as unprotected: the
identity string was right and the reset still did not happen, which is worse
than `softdog` because it looks covered. Roll step 4 back and record it.

This test does not freeze the kernel, so it does not by itself show the timer
surviving a dead one. That property comes from the timer living in the chipset
instead of in a kernel timer. `bootstatus` is the supporting evidence: the
driver sets it only when the chipset's own fired flag is found set at the next
boot.

### B. The panic path

```bash
echo c > /proc/sysrq-trigger
```

This exercises the sysctls, netconsole and pstore. It is not a freeze test: the
kernel is alive and deliberately panics. Writing to `/proc/sysrq-trigger` works
regardless of the `kernel.sysrq` mask.

Expected: the receiver's journal shows `sysrq: Trigger a crash`, a trace,
`Kernel panic - not syncing` and `Rebooting in 5 seconds..`; the host reboots
without help; and afterwards:

```bash
ls -R /var/lib/systemd/pstore/                 # a dmesg file from the panic
cat /sys/class/watchdog/watchdog0/bootstatus   # 0: the panic rebooted it, not the watchdog
```

If `bootstatus` is non-zero here, the watchdog beat the panic's own reboot.
Lower `kernel.panic`, or accept that the two cannot be told apart.

Watch the first seconds of the next boot on both tests. A second reset during
firmware or GRUB would mean the chipset timer was still counting through the
restart, and this design is not safe on that board until the driver's
load-time stop can be relied on to come first.

### Evidence to paste

For each host: the step 1 dmesg lines, the step 5 output, and for each test the
T+0 time, the first and last lost ping, the first successful SSH, the
`bootstatus` value, the `last -x` line, and for test B the netconsole lines from
the receiver plus the pstore listing.

There is no clean way to produce a true hard freeze on demand. The kernel's
lockup test module is not built for this kernel (`modinfo test_lockup` finds
nothing), and anything cruder risks the disk.

## Rollout order

One host per window. Each window is steps 1 to 5 plus both tests.

1. **pve1.** It is the host that froze and the one whose outage costs the most.
   Its restart takes the cluster API down for the duration, as it always does.
2. **pve2, pve3, pve4**, in any order, never two together. Each carries an etcd
   member, and corosync needs three of five votes. Run test A on all three:
   they are the same model but each has its own firmware settings. Test B on
   one of them is enough.
3. **pve5 last, scheduled, with someone able to reach the machine.** Three
   things make it different:
   - It hosts devbox. Work from devbox2 throughout.
   - Any reset of pve5 restarts `vllm-inference`, which is the trigger for the
     wedge in `gpu-passthrough-wedge-recovery.md`. A watchdog reset is a warm
     reset and will not clear a wedged card; that needs a cold power cycle by
     hand.
   - devbox (111) does not autostart on any pve5 boot. After a watchdog reset
     it stays down until someone runs `qm start 111`.

   On pve5 the watchdog trades a frozen host for a host that is back with two
   of its three guests possibly needing attention. That is still the better
   state, but it is not a full recovery.

The netconsole receivers (step 0) and the sender and sysctl steps (2 and 3)
need no reboot and can go on all five hosts ahead of the windows.

## Idle-freeze mitigations

The freeze has one occurrence and no evidence, so its cause is unknown. Idle
power states are a common suspect for silent freezes on Ryzen systems, and the
options are listed here so they are ready, not because any of them is known to
apply.

What was measured: pve1 exposes three ACPI idle states and uses the deepest one
heavily. Over its first 5h20m of uptime, cpu0 spent 68% of its idle time in C3
(`ACPI IOPORT 0x415`, 350 µs exit latency). pve2 to pve4 expose only C1 and C2.
`processor.max_cstate` is at its default of 8 everywhere and no host has any
idle-related parameter on its command line.

| Option | Cost | Status |
|---|---|---|
| BIOS update for the NucBox K8 beyond 1.07 | Console access, and a firmware flash on the host with the only load balancer | **Unverified that one exists.** The vendor's support downloads page listed no BIOS for this model when checked on 2026-10-02. Ask the vendor directly. |
| BIOS "Power Supply Idle Control" set to "Typical Current Idle" | Console access and a reboot; slightly higher idle power | **Unverified on two counts.** The setting is the widely reported fix for idle lockups on first-generation Ryzen desktop parts. Whether the K8's BIOS exposes it, and whether that fault exists on this CPU generation, were not established from a primary source. |
| Disable C3 at runtime: write 1 to `/sys/devices/system/cpu/cpu*/cpuidle/state3/disable` | Higher idle power and temperature, not measured. No reboot, and writing 0 reverses it. Needs a unit to persist. | Mechanism documented in the kernel's CPU idle admin guide, including that it must be set on every CPU. Effect on this freeze unknown. |
| `processor.max_cstate=2` (or 1) on the kernel command line | Same power cost, plus a GRUB edit and a reboot | Mechanism documented in the same guide. Effect on this freeze unknown. |

Recommendation: none of them yet. Install the watchdog and the evidence path
first. If pve1 freezes again and netconsole shows nothing, the runtime C3
disable is the first experiment, because it is the only one that can be applied
and withdrawn without a restart. Reports of this fix working on similar mini
PCs exist but are anecdotal and were not used as evidence here.

## Known gaps

- **Nothing in this runbook has been executed.** In particular: `sp5100_tco`
  binding on each of the three board types, the reset actually happening,
  `bootstatus` surviving it, netconsole on `vmbr0` (including with tap ports
  joining afterwards), the receiver unit, and EFI pstore accepting a write.
  Each has a check above.
- **A freeze before `watchdog-mux` starts is not covered**, and neither is a
  host that fails to boot. A watchdog resets; it does not repair.
- **A host that freezes soon after every boot will reset in a loop**, restarting
  its guests each time. The fix is at the console.
- **Shutdown is not covered once `watchdog-mux` has exited.** systemd's
  `RebootWatchdogSec` (10min by default) is meant for the final phase of a
  reboot and may pick the device up once it is free. Whether it does on these
  hosts was not checked.
- **A reset is not a power cycle.** Anything that needs rail power removed, the
  pve5 GPU wedge being the known case, still needs a person.
- **TrueNAS is out of scope.** It is not a Proxmox node and was not assessed.
- **`watchdog-mux` was read at HEAD, not at the 5.2.4 tag.** The installed
  binary's strings and the live device state both agree with the reading, but
  the tagged source was not diffed.
- **The cause of the 2026-10-01 freeze is still unknown**, and this runbook does
  not find it. It makes the next one short and, with luck, legible.
