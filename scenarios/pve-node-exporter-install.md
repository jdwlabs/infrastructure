# Runbook: Install node-exporter on the Proxmox hosts

Status: **NOT YET EXECUTED.** Written 2026-10-02 from read-only inspection of
all five hosts; nothing has been installed. Human-executed: it installs a
package and starts a network listener on every hypervisor the cluster runs
on, which is the "changes production" line the agent contract reserves for a
human. No `terraform apply`, no `kubectl apply`.

## Why

On 2026-10-01 pve1 stopped abruptly and stayed down for 2h08m. The cause
cannot be proven, because there was nothing to prove it with: the only
Prometheus series for any Proxmox host was a TCP probe of port 22, which says
*that* a host went away and nothing about *why*. A thermal power-off in
particular writes no log — the host is simply gone — so the temperature
history leading up to it is the only evidence there can be, and none was
being recorded. After the reboot pve1's thermal zone read 67C, then 79C
within minutes at a load of about 2, against trip points of 105C and 110C.

This runbook puts `prometheus-node-exporter` on pve1–pve5 so that history
exists next time. The scrape config and the thermal alerts that consume it
live in `jdwlabs/platform`
(`tenants/platform/services/kube-prometheus-stack/postInstall/scrapeconfig-pve-hosts.yaml`
and `rules-hardware-hosts.yaml`); see "Order of work" at the end for how the
two halves sequence.

## What was found on the hosts (2026-10-02, read-only)

Checked on **all five**, not sampled:

| | pve1 | pve2 | pve3 | pve4 | pve5 |
|---|---|---|---|---|---|
| LAN address (`vmbr0`, DHCP reservation) | 192.168.1.200 | .201 | .202 | .203 | .204 |
| CPU | Ryzen 7 8845HS | Ryzen 5 3550H | Ryzen 5 3550H | Ryzen 5 3550H | Ryzen 9 9950X |
| `k10temp` hwmon (`Tctl`) | yes | yes | yes | yes | yes (+ `Tccd1`, `Tccd2`) |
| ACPI thermal zone | `acpitz`, trips 105C hot / 110C critical | none | none | none | none |
| `prometheus-node-exporter` installed | no | no | no | no | no |
| apt candidate | 1.9.0-1+b4 | same | same | same | same |
| Anything listening on `:9100` | no | no | no | no | no |
| `pve-firewall status` | `disabled/running` | same | same | same | same |
| `tailscale0` present | yes | yes | yes | yes | yes |

Three things follow from that table and shape the steps below:

- **Every host has a CPU temperature under hwmon; only pve1 has a thermal
  zone.** The verification in Step 3 therefore expects `node_hwmon_temp_celsius`
  everywhere and `node_thermal_zone_temp` on pve1 alone. An empty
  `node_thermal_zone_temp` on pve2–pve5 is correct, not a fault.
- **The Proxmox firewall is off on every host**, so nothing needs opening for
  the cluster to reach `:9100` — and, equally, nothing is restricting who else
  can. The listen address is the only access control there is (Step 1).
- **Every host has a tailnet interface.** The package's default is to listen
  on every address, which would put unauthenticated host metrics on
  `tailscale0` as well. Step 1 binds the LAN address only, and does it before
  the package is installed so there is no window on the default.

`apt-get install -s --no-install-recommends prometheus-node-exporter` was
simulated on all five: one new package, nothing upgraded, nothing removed.

## Before you start

- Root SSH to each host. Do one host at a time and finish it — configure,
  install, verify — before starting the next. There is no reason to have five
  half-configured listeners at once.
- Re-run the simulation on the host you are about to change. A Proxmox host
  is the one place an apt transaction that removes something is not a detail:

  ```bash
  ssh root@192.168.1.200 'apt-get update && apt-get install -s --no-install-recommends prometheus-node-exporter | tail -4'
  ```

  Expect `0 upgraded, 1 newly installed, 0 to remove`. Anything in the
  "to remove" column, or `proxmox-ve` / `pve-manager` appearing anywhere in
  the output: stop.
- Confirm the port is still free, in case something has claimed it since the
  table above was written:

  ```bash
  ssh root@192.168.1.200 'ss -ltnp | grep ":9100 " || echo free'
  ```

The commands below use pve1 (`192.168.1.200`). Substitute the address for
each host; the address appears inside the config written in Step 1, so
re-read the command before running it on the next host rather than
re-running the previous one from shell history.

## Step 1 — Write the listen config, before the package exists

```bash
ssh root@192.168.1.200 '
  set -e
  printf "%s\n" "ARGS=\"--web.listen-address=192.168.1.200:9100\"" > /etc/default/prometheus-node-exporter
  mkdir -p /etc/systemd/system/prometheus-node-exporter.service.d
  cat > /etc/systemd/system/prometheus-node-exporter.service.d/lan-bind.conf <<EOF
[Unit]
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
RestartSec=10
EOF
  cat /etc/default/prometheus-node-exporter
'
```

Expect the one `ARGS="--web.listen-address=192.168.1.200:9100"` line back.

The order is the point. The package's post-install starts the service the
moment it is configured, and its default is to listen on every address —
with the firewall off, that includes `tailscale0`. Configuring after
installing leaves a window, however short, in which unauthenticated host
metrics are served on the tailnet. Writing the config first means the very
first start already binds the LAN address only. Nothing reads either file
until the package arrives, so this step on its own changes nothing.

Why the drop-in exists: the packaged unit has no network ordering at all and
restarts on failure with systemd's defaults. `vmbr0` on these hosts is
`inet dhcp`, so at boot the address this unit now insists on binding may not
exist yet when it first starts. A wildcard listener does not care; a
listener pinned to one address fails to bind, and with the default restart
interval it burns through systemd's start limit in about a second and stays
dead until someone notices. `After=network-online.target` (which
`networking.service` is ordered before on these hosts) removes the race in
the normal case; `RestartSec=10` with the start limit disabled makes it
retry indefinitely in the abnormal one, such as a slow DHCP answer.

That reasoning is from reading the unit and the host's network config, not
from a reboot test — rebooting a hypervisor to test an exporter is not worth
it. It gets tested for free the next time each host restarts for another
reason: see "After the next reboot" below.

**Rollback:**

```bash
ssh root@192.168.1.200 'rm -rf /etc/default/prometheus-node-exporter /etc/systemd/system/prometheus-node-exporter.service.d'
```

## Step 2 — Install the package

```bash
ssh root@192.168.1.200 '
  set -e
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    -o Dpkg::Options::=--force-confold prometheus-node-exporter
  systemctl is-active prometheus-node-exporter
  ss -ltnp | grep ":9100 "
'
```

Expect dpkg to report `Using current old file as you requested` for
`/etc/default/prometheus-node-exporter`, then `active`, and exactly one
listener, on `192.168.1.200:9100` — not `*:9100` and not `0.0.0.0:9100`.

`--force-confold` is what makes Step 1 stick: that file is a dpkg conffile,
and without the option dpkg stops to ask which version to keep (or, with no
terminal, picks for you). With it, the file from Step 1 is kept and the
packaged default is left beside it as `.dpkg-dist`. Checked in a throwaway
Debian 13 container against this exact package version; the service start
itself could not be exercised there.

`--no-install-recommends` is deliberate too. The package recommends
`prometheus-node-exporter-collectors`, which in turn recommends `ipmitool`,
`jq` and `nvme-cli`; without the flag the same command was simulated at 21
new packages and 14 upgrades, including a partial `util-linux` upgrade
(`mount`, `login`, `libblkid1`, …) pulled in through `nvme-cli`. None of that
belongs in a change whose purpose is one exporter. See "Follow-ups" for the
NVMe counters that package would have brought.

If the listener is on the wildcard address after all, the config was not
kept. Fix it in place rather than leaving it — re-run Step 1's command, then
`systemctl daemon-reload && systemctl restart prometheus-node-exporter` on
the host — and check `systemctl cat prometheus-node-exporter` shows
`EnvironmentFile=/etc/default/prometheus-node-exporter`.

**Rollback:** `apt-get purge -y prometheus-node-exporter`, which also removes
the default file from Step 1; the drop-in directory is not the package's and
has to be removed by hand (see "Rollback (everything)"). The package adds a
`prometheus` system user and `/var/lib/prometheus`; purge removes the
exporter's files and leaves the user, which is harmless.

## Step 3 — Confirm the temperatures are actually exported

The hwmon and thermal_zone collectors are on by default in this build, so no
flag enables them. This step proves they found something:

```bash
curl -s http://192.168.1.200:9100/metrics | grep -E 'node_hwmon_temp_celsius|node_thermal_zone_temp'
curl -s http://192.168.1.200:9100/metrics | grep -E '^node_hwmon_chip_names'
```

Run it from the devbox or any LAN machine, not from the host's own loopback
— the exporter no longer listens there, and a `curl localhost:9100` failing
is the bind working, not the exporter being down.

What to expect, and what each line is for:

- `node_hwmon_chip_names{chip="pci0000:00_0000:00:18_3",chip_name="k10temp"} 1`
  on **every** host. The `chip` label is a device path, not a driver name;
  `chip_name` is the only place `k10temp` appears, and it is what the
  recording rule in `jdwlabs/platform` joins on.
- `node_hwmon_temp_celsius{chip="pci0000:00_0000:00:18_3",sensor="temp1"}`
  on every host, with a plausible CPU temperature (50–80C at ordinary load).
  This is `Tctl`. pve5 also has `temp3`/`temp4` (`Tccd1`/`Tccd2`).
- `node_thermal_zone_temp{type="acpitz",zone="0"}` on **pve1 only**.
- Other `node_hwmon_temp_celsius` series — NVMe, the Realtek NICs, the
  DIMMs and motherboard sensors on pve5 — are expected and ignored by the
  CPU alerts.

**The exact label values above are the one part of this change that could
not be checked in advance**, because no exporter was running to ask. They are
derived from how node-exporter names hwmon devices and from each host's
`/sys/class/hwmon`. If a host shows `k10temp` under a different `chip` value,
that is fine — the rule joins on `chip_name`, not on `chip`. If a host shows
no `chip_name="k10temp"` line at all, stop and record what it does show:
the platform-side recording rule selects `chip_name=~"k10temp|coretemp"` and
`type=~"acpitz|x86_pkg_temp"`, and a host matching neither has no thermal
alerting until that rule is widened. The platform alert
`HardwareHostTemperatureMissing` exists to catch exactly this, but it only
fires once the scrape config is live.

## Step 4 — Confirm the cluster can reach it

The scrape comes from the Prometheus pod, not from the devbox, so a `curl`
from the devbox succeeding is not quite the proof that matters. The
blackbox-exporter already running in the cluster will make the connection on
request, read-only, without any config change:

```bash
kubectl get --raw "/api/v1/namespaces/monitoring/services/platform-blackbox-exporter-prometheus-blackbox-exporter:9115/proxy/probe?target=192.168.1.200:9100&module=tcp_connect" | grep '^probe_success'
```

Expect `probe_success 1`. Before the install the same call returns `0` (checked
against pve1), so a `1` here is the install, not a false positive.

If it reads `0` after Step 2 while the `curl` in Step 3 works from the
devbox, something between the pods and the host is filtering. Pod traffic to
the LAN leaves the cluster source-NATed to a node address, so that is what a
host firewall would see. With `pve-firewall` disabled, as it is today,
there is nothing on the host to do this — check that first:

```bash
ssh root@192.168.1.200 'pve-firewall status'
```

**If the Proxmox firewall is ever enabled**, on these hosts or datacenter-wide,
the scrape stops unless a rule admits it. Its default inbound policy drops
everything not explicitly allowed; `:9100` is not in the built-in management
set. The rule belongs in `/etc/pve/firewall/cluster.fw` (one file, replicated
to every host by pmxcfs):

```
[RULES]
IN ACCEPT -source 192.168.1.0/24 -p tcp -dport 9100 -log nolog
```

The source is the whole LAN subnet rather than a list of node addresses
because worker nodes are DHCP-assigned and their addresses are not pinned
anywhere a firewall rule could follow.

## Step 5 — Repeat for the remaining hosts

| Host | Address in Step 1's `ARGS=` |
|------|-----------------------------|
| pve1 | `192.168.1.200:9100` |
| pve2 | `192.168.1.201:9100` |
| pve3 | `192.168.1.202:9100` |
| pve4 | `192.168.1.203:9100` |
| pve5 | `192.168.1.204:9100` |

Then, once all five are done, one pass to confirm none was skipped:

```bash
for ip in 192.168.1.200 192.168.1.201 192.168.1.202 192.168.1.203 192.168.1.204; do
  printf '%s  ' "$ip"
  curl -s --max-time 5 "http://$ip:9100/metrics" | grep -c 'node_hwmon_temp_celsius' || echo unreachable
done
```

Every line should end in a non-zero count.

## After the next reboot

The drop-in in Step 1 is untested against a real boot. The next time any of
these hosts restarts for its own reasons — a kernel upgrade, a power event —
check that host once:

```bash
ssh root@192.168.1.200 'systemctl is-active prometheus-node-exporter; systemctl show prometheus-node-exporter -p NRestarts; ss -ltnp | grep ":9100 "'
```

`active`, bound to the LAN address, is a pass whatever `NRestarts` says. A
non-zero `NRestarts` means the ordering alone was not enough and the retry
did the work; that is the design working, and worth a note here so the next
reader knows the retry is load-bearing. Not `active` means the reasoning in
Step 1 was wrong — the platform alert `HardwareHostMetricsDown` will be
firing for that host — and this section needs rewriting with what the
journal says (`journalctl -u prometheus-node-exporter -b`).

## Order of work

This is one half of a two-repo change. The other half is the scrape config
and alerts in `jdwlabs/platform`. ArgoCD deploys that half on merge, so the
order matters:

1. Merge this runbook.
2. Run it on all five hosts, through Step 5.
3. Merge the `jdwlabs/platform` change. Within a few minutes
   `up{job="pve-node"}` should read `1` for five targets and
   `host:hardware_cpu_temperature_celsius:max` should have one series per
   host.

Getting 2 and 3 the wrong way round is survivable but noisy:
Prometheus scrapes five targets that are not there, and
`HardwareHostMetricsDown` (plus the stack's own `TargetDown`) fires for each
after 15 minutes. Both are warnings, routed to the agent and not to the
human channel, and both clear on their own as each host is completed.

## Rollback (everything)

Per host:

```bash
ssh root@192.168.1.200 '
  set -e
  apt-get purge -y prometheus-node-exporter
  rm -rf /etc/systemd/system/prometheus-node-exporter.service.d
  systemctl daemon-reload
'
```

If the platform change has already merged, removing the exporters brings
`HardwareHostMetricsDown` back for every host removed. Revert the platform
change first, or accept the warnings.

## Follow-ups (not done here)

- **NVMe unsafe-shutdown counts as a metric.** Every abrupt stop increments
  the drive's unsafe-shutdown counter, so a time series of it would timestamp
  each one independently of anything the host logs. The packaged route is
  `prometheus-node-exporter-collectors` plus `nvme-cli` — its
  `prometheus-node-exporter-nvme.timer` writes `nvme_unsafe_shutdowns_total`
  to the textfile directory the exporter already reads
  (`/var/lib/prometheus/node-exporter`). It is left out because on these
  hosts today that install is not small: simulated on pve1, even with
  `--no-install-recommends` it is 11 new packages and 14 upgrades, the
  upgrades being a partial `util-linux` bump forced by `nvme-cli`'s
  dependencies. The clean time to add it is straight after a routine
  `apt full-upgrade` of the host, when those upgrades have already happened
  and the same command adds only the new packages. The packaged
  `smartmon` timer is not an alternative — its unit only runs when
  `/dev/sd*` or `/dev/hd*` exists, and these hosts are NVMe-only.

  Until then the counter is a manual read, and `smartmontools` is already
  installed everywhere:

  ```bash
  ssh root@192.168.1.200 'smartctl -a /dev/nvme0 | grep -E "Unsafe Shutdowns|Power Cycles"'
  ```

  Baseline on 2026-10-02, so the next read has something to diff against:

  | Host | Drive | Unsafe shutdowns | Power cycles | Power-on hours |
  |------|-------|------------------|--------------|----------------|
  | pve1 | Lexar SSD NM7A1 1TB | 29 | 49 | 16699 |
  | pve2 | AirDisk 512GB SSD | 38 | 76 | 5096 |
  | pve3 | AirDisk 512GB SSD | 30 | 69 | 4971 |
  | pve4 | AirDisk 512GB SSD | 29 | 68 | 4933 |
  | pve5 | Samsung SSD 9100 PRO 2TB | 33 | 38 | 601 |

  Note what the table says about pve1's "29 of 49": the ratio is in the same
  range on every host, including the ones with no history of unexplained
  stops. On its own it does not single pve1 out.

- **TrueNAS (192.168.1.205) is out of scope.** It is an appliance, not a
  Debian host to `apt-get install` on, and its CPU temperature already
  reaches Prometheus through its own exporter path in `jdwlabs/platform`.
