# Runbook: RTX 5090 passthrough wedged — GPU dead on MMIO after a VM restart

Status: **occurred once, 2026-09-22; recovered 2026-09-23 by cold power cycle.**
The card was not faulty. Every reset the hypervisor can issue failed; only
removing rail power cleared it.

## Why this exists

The `vllm-inference` guest (VM 500 on pve5) holds the RTX 5090 by vfio-pci
passthrough. After a stop/start of that VM the GPU stopped answering on its
register aperture and never recovered on its own. The failure is worth its own
runbook for two reasons: the driver's own error message points at the wrong
cause, and the recovery is the single most disruptive action available on this
host, so it should not be reached for by guesswork.

## Symptom, outermost to innermost

Each layer reports something that looks like a different problem.

1. Consumers see a refused connection, because nothing is listening:
   ```
   dial tcp 192.168.1.50:8000: connect: connection refused
   ```
2. `vllm.service` crash-loops with an error that reads like a config fault:
   ```
   RuntimeError: Failed to infer device type
   ```
3. `nvidia-smi` cannot talk to a driver, and no NVIDIA kernel module is loaded
   (`lsmod | grep ^nvidia` is empty).
4. The kernel log blames the hardware for being too old:
   ```
   NVRM: The NVIDIA GPU 0000:01:00.0 (PCI ID: 10de:2b85)
   NVRM: installed in this system is not supported by open
   NVRM: nvidia.ko because it does not include the required GPU
   NVRM: System Processor (GSP).
   ```

## Do not believe the GSP message

A GB202 has a GSP. That message is what the open driver prints when it cannot
read the chip's architecture at all, and it appears identically on driver
580.173.02 and 580.178.04 — the same 580.173.02 that had been serving for days
beforehand. Chasing driver versions, firmware blobs or `nvidia-open` packaging
costs hours and finds nothing.

Everything shallow also looks healthy, which reinforces the wrong conclusion:

```
setpci -s 01:00.0 00.l          -> 2b8510de      # config space fine
lspci -vvv -s 01:00.0           -> Region 1: ... [size=32G], LnkSta: 32GT/s Width x16
cat .../0000:01:00.0/reset_method -> flr bus
dmesg | grep vfio               -> vfio-pci 0000:01:00.0: reset done
```

## The decisive test

Read `PMC_BOOT_0` — the chip ID register at offset 0 of BAR0. On a healthy card
it returns a non-zero identifier. Wedged, it returns zeros while PCI config
space still reads correctly:

```bash
# On the guest. Requires the device to be unbound or its driver probe to have failed.
sudo python3 -c '
import mmap, os
fd = os.open("/sys/bus/pci/devices/0000:01:00.0/resource0", os.O_RDONLY)
m = mmap.mmap(fd, 4096, prot=mmap.PROT_READ)
print("PMC_BOOT_0=0x%08x" % int.from_bytes(m[0:4], "little"))'
```

`PMC_BOOT_0=0x00000000` with a good config-space read is the signature. Start
here rather than at the driver.

## What does not work

All of these were tried on 2026-09-22/23 and left `PMC_BOOT_0` at zero:

| Attempt | Result |
|---|---|
| Function-level reset (`echo 1 > .../reset`) | `reset done` in dmesg, still zeros |
| Secondary bus reset | same |
| `echo 1 > .../remove` then `echo 1 > /sys/bus/pci/rescan` | re-enumerates, BARs reassign correctly, still zeros |
| `qm stop` / `qm start` of VM 500 | still zeros |
| Unbinding vfio-pci to force D0 | device does leave D3, still zeros |
| Rebooting the guest | still zeros |

Two things are worth knowing before improvising further.

**There is no per-slot power control for this card.** Only `0000:17:00` and
`0000:47:00` appear in `/sys/bus/pci/slots` (the NVMe slots). The GPU's slot
does not, so the GPU cannot be power-cycled independently of the host.

**Do not set `rombar=1` on `hostpci0`.** It looks like a fix — the guest sees no
expansion ROM, so hiding the VBIOS seems like a plausible cause. It is not: the
guest then hangs in OVMF before reaching disk, with one vCPU pegged at 100% and
only ~11 MB read from the boot disk. The working configuration is
`mapping=gpu-rtx5090,pcie=1,rombar=0,x-vga=0`.

## The fix: cold power cycle of pve5

A warm `reboot` does **not** clear the wedge — the card has to lose rail power.
No host in this fleet has IPMI or a BMC, and the standing decision recorded in
`host-remote-power-recovery.md` is that no smart plug or switched PDU is being
bought, so AC-loss recovery is physical-access-only. In practice that means
this fix cannot be performed remotely: someone has to be at the machine.

The blast radius is the whole host, so run the pve5 procedure in
`host-restart-coordination.md` for the drain and guest shutdown, then instead of
its reboot step:

1. `ssh root@pve5 'poweroff'` and wait for it to stop answering.
2. Cut power for ~30 seconds — PSU switch, wall plug, or a switched outlet.
3. Power on. All three guests carry `onboot: 1`, but only 304 and 500 actually
   come back: devbox (111) fails its autostart on every boot because its
   cloud-init drive is on the NFS datastore. Start it by hand —
   `ssh root@pve5 'qm start 111'` — see the pve5 section of
   `host-restart-coordination.md` for why.

Then verify, from devbox2 until devbox answers again:

```bash
ssh vllm@192.168.1.50 'nvidia-smi'        # the check that actually decides it
ssh vllm@192.168.1.50 'systemctl is-active vllm'
curl http://192.168.1.50:8000/v1/models
kubectl uncordon talos-lx0-6a4
```

`vllm.service` is enabled and its `ExecStartPre` reloads the NVIDIA modules, so
it comes up on its own once the GPU is real. Note that the preflight only
repairs a module/NVML mismatch — it cannot do anything about a wedged card, and
exits 0 regardless so as not to mask the service's own error.

If `nvidia-smi` still fails after a genuine cold cycle, the card itself is the
suspect and this runbook is exhausted.

## Evidence, 2026-09-22 to 2026-09-23

Trigger was a Terraform-driven `qmshutdown`/`qmstart` of VM 500 at 06:33:33 UTC
on 2026-09-22. The guest's own boot at 06:33:44 already failed the NVIDIA probe
— 25 seconds *before* an unattended `apt dist-upgrade` began at 06:34:09. That
upgrade aborted on a dpkg file conflict and left the NVIDIA stack unconfigured
with no DKMS module for the running kernel, which is a real second fault and had
to be repaired separately, but it did not cause this one. The boot before it
(2026-09-12) had loaded 580.173.02 successfully.

Recovery was a cold power cycle at approximately 06:05 UTC on 2026-09-23:

```
NVIDIA GeForce RTX 5090, 580.178.04, 32607 MiB, 36     # immediately after
NVIDIA GeForce RTX 5090, 30362 MiB, 40                 # 20h later, model resident
```

Whether the wedge recurs on every VM 500 restart is not yet known — there has
been no stop/start of that VM since the recovery. Treat a restart of VM 500 as
carrying this risk until that has been tested deliberately.

## Caution learned the hard way

The 2026-09-23 recovery cost an extra unplanned hour because pve5 was power-
buttoned while healthy and then booted into the GRUB *recovery mode* entry
(`ro single dis_ucode_ldr`). Recovery mode brings up networking but neither SSH
nor the Proxmox services, so the host pings while looking dead. If pve5 answers
ping but nothing else after a power event, check which GRUB entry it booted
before assuming the cycle failed.
