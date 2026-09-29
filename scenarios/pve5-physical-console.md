# Runbook: pve5 at the physical console

What to do, and what not to do, when you are standing at pve5 with a keyboard
and monitor plugged in: how to tell a healthy boot from a stuck one, how to
recognise and leave the GRUB recovery-mode trap, and why the power key is
never the answer once the node is back in the cluster. Print it and keep it
by the box; the person at the console during a power cycle usually has no
live view of cluster state.

This is the console half only. The reasons a cold power cycle is needed at
all (a wedged GPU) and the drain/shutdown/uncordon sequence around it live in
[gpu-passthrough-wedge-recovery.md](gpu-passthrough-wedge-recovery.md) and
[host-restart-coordination.md](host-restart-coordination.md) §pve5. Follow
those for the procedure; use this page for what you see on the screen.

## Why this exists

2026-09-23: a planned power-off of pve5 to cold-reset its GPU turned into an
extra unplanned hour, entirely through console actions on a node that was
fine. From the pve5 journal:

```
Sep 23 00:39:53 pve1 corosync: [QUORUM] Members[5]: 1 2 3 4 5
Sep 23 00:40:05 pve5 pve-guests[1813]: VM 500 started with PID 1860.
Sep 23 00:42:06 pve5 systemd-logind[1041]: Watching system buttons on /dev/input/event9 (Logitech G413 Carbon Mechanical Gaming Keyboard)
Sep 23 00:42:26 pve5 systemd-logind[1041]: Power key pressed short.
Sep 23 00:43:18 pve5 kernel: Command line: BOOT_IMAGE=/boot/vmlinuz-7.0.6-2-pve root=/dev/mapper/pve-root ro single dis_ucode_ldr
Sep 23 00:43:19 pve5 systemd[1]: Reached target rescue.target - Rescue Mode.
Sep 23 00:43:34 pve5 systemd[1]: Startup finished in 19.487s (firmware) + 11.680s (loader) + 6.270s (kernel) + 16.181s (userspace) = 53.620s.
```

Read in order: the node rejoined quorum at 00:39:53 and had its guests
running by 00:40:05. The console still looked idle, so at 00:42:26 the power
key was pressed on a healthy node. The next boot took the GRUB
"(recovery mode)" entry (`single dis_ucode_ldr`), landed in `rescue.target`,
and sat at a rescue shell for 22 minutes, answering ping and nothing else,
until a cold power cycle at 01:05 booted it normally.

Two properties of this box make that easy to repeat:

- **The console shows almost nothing on a good boot.** pve5 boots with
  `quiet` on the AMD iGPU output, so a healthy boot prints a few lines and
  then stops drawing. "Booting fine", "sitting at a rescue shell" and "hung"
  look the same on the monitor.
- **Ping succeeds in rescue mode.** On Proxmox, `networking.service`
  (ifupdown2) has no default dependencies and runs before `sysinit.target`,
  so a host in rescue mode still brings up 192.168.1.204. A ping answer does
  not mean the host is up.

## Symptom to fix

| What you see | What it is | Do this |
| --- | --- | --- |
| A few boot lines, then the screen stops changing | Normal boot with `quiet` | Nothing at the console. Check from another machine (below) after 90 s |
| Screen idle, pve5 in `pvecm status` with 5 members | Healthy, rejoined node | Walk away. **Do not press the power key** |
| Pings, but SSH (22) and the web UI (8006) refuse, and `pvecm status` shows 4 members, 90 s+ after power-on | Booted into rescue mode, or a real boot failure | Press Enter at the console and read the prompt, then [Leaving rescue mode](#leaving-rescue-mode) |
| GRUB menu on screen | Boot menu before the kernel loads | Touch nothing. It times out to the default (top) entry |
| No ping at all 3 min+ after power-on, and no disk or fan activity change | Did not POST or did not reach the kernel | This is the one case where another power cycle is reasonable |
| Healthy node, VM 111 (devbox) not running | Known autostart fault while its cloud-init drive sits on the NFS pool (the start collides with a half-mounted share), not a boot failure | `ssh root@pve5 'qm start 111'` |

## What a normal boot looks like

Timings from the two good boots on 2026-09-23 (journal times, which start
after firmware):

- Firmware and GRUB take roughly 30 s before the kernel's first line
  (19.5 s firmware + 11.7 s loader were measured on the rescue boot; the
  normal entry uses the same firmware and loader).
- The node rejoins quorum about 17 s after the boot's first journal
  timestamp (00:39:36 → 00:39:53).
- Guests with `onboot` are started about 12 s after that (00:40:05).

On the monitor this is a few kernel and systemd lines followed by a static
screen. There is no login prompt to wait for.

Check from another machine (devbox2, or any host with SSH to pve1) rather
than from the console:

```
ssh root@pve1 'pvecm status | grep -E "Total votes|Quorate"'
                                   # Total votes: 5, Quorate: Yes -> pve5 is back
ssh root@pve5 'qm list'            # 304 and 500 running; 111 see the table above
nc -zv 192.168.1.204 22            # open
nc -zv 192.168.1.204 8006          # open
```

If `pvecm status` shows 5 votes, the node is up. Whatever the monitor shows,
leave it alone.

## Never press the power key on a node that has rejoined quorum

A short press is handled by `systemd-logind` as an orderly power-off. On a
node that has already started its guests, that power-off races the guest
shutdowns — on 2026-09-23 `stopall` reported "unexpected status" for exactly
this reason — and the Talos worker, the GPU VM and devbox all go down again.

If you think the node is hung, prove it from another machine first
(`pvecm status` from pve1, `nc -zv 192.168.1.204 22`). Only a node that
fails those checks after 90 s is a candidate for any power action, and even
then the first step is reading the console prompt, not pressing a button.

## Never pick the GRUB "recovery mode" entry

pve5 boots through plain GRUB (`proxmox-boot-tool` reports no boot UUIDs on
this host), and the menu lists an "(recovery mode)" entry for each installed
kernel directly under its normal entry — one arrow key away from the default.
Recovery mode boots with `single dis_ucode_ldr`: single-user, no CPU
microcode update, and it stops at `rescue.target`. That target brings up
networking but not SSH, corosync, `pveproxy` or any guest, so the node is
reachable only at this keyboard.

It is never the right choice for "the host seemed hung". If the GRUB menu is
on screen, let it time out.

### Recognising rescue mode

From another machine:

- ping to 192.168.1.204 answers;
- `nc -zv 192.168.1.204 22` and `nc -zv 192.168.1.204 8006` are refused;
- `ssh root@pve1 'pvecm status'` shows 4 votes, not 5.

At the console, press Enter. A rescue shell reprints systemd's maintenance
prompt, asking for the root password or Control-D. On 2026-09-23 that prompt
had scrolled off behind kernel and systemd lines, which is why the screen read
as a hang.

After the fact, the kernel command line is the proof. On the host, once it is
back up normally:

```
journalctl -b -1 -k | grep 'Command line'   # previous boot; "single dis_ucode_ldr" = recovery entry
cat /proc/cmdline                            # current boot; must not contain "single"
```

### Leaving rescue mode

The clean exit is a reboot into the default entry. Do not continue into the
normal target from rescue mode: the boot skipped the microcode update
(`dis_ucode_ldr`), and the host would run without it until its next boot.

1. At the maintenance prompt, log in as root. The password is the per-host
   root PAM password in Vault; see
   [host-remote-power-recovery.md](host-remote-power-recovery.md) §"SSH key
   auth failure". Retrieve it in a terminal of your own, not through an agent
   session.
2. Run `systemctl reboot`.
3. Do not touch the keyboard while the GRUB menu is up. It defaults to the
   normal entry.
4. Check from another machine as in
   [What a normal boot looks like](#what-a-normal-boot-looks-like).

If the root password is not to hand (Vault runs in the cluster, and may be
degraded while pve5's worker is down), a power cycle is acceptable **here
only**, because a rescue-mode host runs no guests and no corosync, so there is
nothing to race. Hold power until off, wait a few seconds, power on, and leave
the GRUB menu alone.

## Options to remove the trap (not adopted, pending decision)

None of these is applied on pve5 or any other host. They are recorded so the
decision has somewhere to land; until one is made, the procedure above is the
only mitigation.

- **`GRUB_DISABLE_RECOVERY=true`** in `/etc/default/grub`, then
  `update-grub`. Removes the recovery entries from the menu entirely. Cost:
  single-user mode is then reachable only by editing the kernel line at the
  GRUB prompt (`e`), which is also the only way to get it on purpose.
- **Drop `quiet`** from `GRUB_CMDLINE_LINUX_DEFAULT`, then `update-grub`. A
  healthy boot then visibly runs to a login prompt, so an idle screen stops
  being ambiguous.
- **A printed copy of this page** next to pve5.
- **Scope:** whether pve1–pve4 get the same change. They share the plain-GRUB
  layout question but not pve5's GPU-driven cold cycles, so they are at the
  console far less often.

Whichever is adopted, it lives only on the host: there is no config
management for Proxmox host boot configuration in this repo, so record the
change and its date here when it is made.
