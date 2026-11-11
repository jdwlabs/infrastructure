# Runbook: pve1 CPU power policy for heat

**Applied live on pve1, 2026-11-11 08:02 UTC.** `cpu-thermal.service` is
installed, enabled and active. Nothing in this repository applies it: the unit
in `scenarios/files/cpu-thermal.service` is a verbatim copy of the one on the
host (sha256 `b0b4f5f6…042ed0`), kept so a rebuild of pve1 gets it back.

## Why

pve1 (GMKtec NucBox K8, Ryzen 7 8845HS) hard powered off on 2026-11-05 and
2026-11-06 with nothing in its logs, at a CPU temperature of 85-88C. Its
cooling is degraded: over the 14 days before the change its 6h CPU average was
84C, against 50-74C on pve2-pve5, and it sat at a firmware ceiling of 90-91C
for most of every day at only 25-35 W package power.

Part of that heat was policy, not load. The CPU driver is `amd-pstate-epp` in
`active` mode, and Proxmox leaves the governor at `performance`, which pins the
energy-performance preference (EPP) to `performance`: at about 10% busy the
cores averaged 3.6 GHz and the package drew 24 W. The unit switches to the
`powersave` governor with EPP `balance_performance`. Under `amd-pstate-epp`,
`powersave` does not cap the clock — the firmware still boosts to 5.1 GHz on
demand — it only stops the cores idling at high clocks.

This treats a symptom. The physical checks (dust, fan, paste, airflow, power
brick, BIOS) are the fix and are tracked on JDWLABS-826.

| | before (PVE default) | after |
|---|---|---|
| `amd_pstate/status` | `active` | `active` |
| `scaling_governor` (all 16) | `performance` | `powersave` |
| `energy_performance_preference` | `performance` | `balance_performance` |
| `cpufreq/boost` | `1` | `1` |

## Install (rebuild or reinstall of pve1)

From the repository root:

```sh
scp scenarios/files/cpu-thermal.service root@pve1:/etc/systemd/system/
ssh root@pve1 'systemctl daemon-reload && systemctl enable --now cpu-thermal'
```

It takes effect at once, with no restart and no VM impact. The unit names
pve1, but nothing in it is host-specific; do not install it on another host
without a reason of its own.

## Verify

On the host:

```sh
systemctl is-enabled cpu-thermal; systemctl is-active cpu-thermal  # enabled, active
cat /sys/devices/system/cpu/amd_pstate/status                      # active
sort /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor | uniq -c                # 16 powersave
sort /sys/devices/system/cpu/cpu*/cpufreq/energy_performance_preference | uniq -c   # 16 balance_performance
```

`amd_pstate/status` must read `active`. In `passive` or `guided` mode, or on
`acpi-cpufreq`, there is no `energy_performance_preference` file and the unit
fails, which `systemctl is-active` shows.

In Prometheus, after at least 6 hours:

```
avg_over_time(host:hardware_cpu_temperature_celsius:max{host="pve1"}[6h])
avg_over_time((host:hardware_cpu_temperature_celsius:max{host="pve1"} >bool 88)[24h:1m])
```

The goal is a 6h average under 75C and no time at or above 88C.
`HardwareHostTemperatureElevated` (jdwlabs/platform) fires while the 6h
average is at or above 80C. Watch the FWB tick-rate alert for a regression
after any step here.

## Revert

```sh
ssh root@pve1 'systemctl disable --now cpu-thermal'
```

`ExecStop` puts every governor back to `performance`, which also returns EPP
to `performance`. Delete `/etc/systemd/system/cpu-thermal.service` and run
`systemctl daemon-reload` to remove it completely.

## Next step if it is not enough

If the 6h average is still above 75C after a day on this policy, turn boost
off. It caps the cores at the 3.8 GHz base clock and costs FWB about a quarter
of its single-thread peak (5.1 to 3.8 GHz), so watch tick rate:

```sh
echo 0 > /sys/devices/system/cpu/cpufreq/boost   # revert: echo 1
```

To keep it across a reboot, add these two lines to the unit's `[Service]`
section, each **below** the existing line of the same kind, then update the
copy in `scenarios/files/` in the same pull request:

```ini
ExecStart=/bin/sh -c 'echo 0 > /sys/devices/system/cpu/cpufreq/boost'
ExecStop=-/bin/sh -c 'echo 1 > /sys/devices/system/cpu/cpufreq/boost'
```

A oneshot unit runs its `ExecStart` lines in order and stops at the first that
fails, so the governor loop stays first and a failed boost write marks the unit
failed rather than half-applied (`/sys/devices/system/cpu/cpufreq/boost`
exists only while the driver is `amd-pstate-epp` or another driver with
boost control). The `-` on `ExecStop` lets the boost restore fail without
skipping anything after it. EPP `balance_power` is the gentler step between
the two:
the same loop as the unit with `balance_power` in place of
`balance_performance`.
