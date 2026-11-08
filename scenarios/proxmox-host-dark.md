# Runbook: A Proxmox host goes dark

One of pve1-5 stops answering without anyone having asked it to. This is the
unplanned counterpart to `scenarios/host-restart-coordination.md`: there was no
pre-flight, no drain, and the first job is working out what is actually down.

Written from the 2026-10-01 pve1 outage, re-checked against the live fleet on
2026-10-02, and extended with the 2026-11-05 recurrence. The best-supported
reading is a hard hang with power present; what caused it is **not**
established — see [Evidence for root cause](#evidence-for-root-cause).

## What happened on 2026-10-01

pve1 (`192.168.1.200`) stopped at about 23:45 UTC. Its own journal ends at
23:44:53 with a routine line and no shutdown sequence after it. Two rounds of
Wake-on-LAN did nothing. When the owner reached the rack, about 01:50 UTC,
its power LED was lit. A manual power
cycle brought it back; the kernel started at 01:53:07 UTC on 2026-10-02 —
2h08m down.

`HardwareHostUnreachable` did fire, at 23:59 — fourteen minutes after the
stop, not five. The blackbox exporter pod doing the probing was running on
`talos-4h8-zy6`, pve1's own worker, and died with the host. Nothing probed
anything until its replacement started on pve5 at 23:53, and the alert's
five-minute clock began there. Where that pod lands is the scheduler's choice,
so the same delay applies to whichever host happens to carry it.

It hosts the only load balancer, so for those two hours the cluster had no API
endpoint at `192.168.1.199`, no HTTP(S) ingress, no external UDP and no
Tailscale subnet route. The cluster itself kept running, and the API stayed
reachable on the control planes' own addresses throughout.

Hosts log in `America/Chicago`. Every timestamp read off a host below is CDT
(UTC-5); the ones in this section are converted.

## What happened on 2026-11-05

The same host hung again, 34 days into the boot that followed the first hang.
pve1's journal ends at `2026-11-04 20:17:17 CST` (02:17:17 UTC on 2026-11-05)
with a routine `sshd` connection close — no shutdown sequence, no kernel
message, empty `pstore`. Wake-on-LAN was ignored. A manual power cycle brought
it back; the next boot began at `02:14:20 CST` (08:14:20 UTC), about six hours
down.

What differed from 2026-10-01:

- **The host was not idle.** Host metrics now exist (node-exporter on
  `192.168.1.200:9100`). Between 01:00 and 02:10 UTC the CPU die sensor
  (`k10temp`) read 78–87.5 °C, load ran 0.7–2.2 and 3.1–3.2 GB of 28 was
  available. That is the same hot-under-ordinary-load picture as 2026-10-04,
  now on record for the hour before a hang. No thermal trip was reached.
- **One more unclean stop on the drive.** `Unsafe Shutdowns` went from 29 to
  30 and `Power Cycles` from 49 to 50 — exactly the one stop, consistent with
  a single hang and one manual power cycle.
- **The outage was six hours, not two**, because nobody was at the rack. That,
  not the hang, is what made it long: no remote way to cut power, no armed
  hardware watchdog, and no second load balancer.
- **The cluster stayed degraded even though the API answered on the control
  planes' own addresses.** Measured on 2026-11-06, the workers on pve2–4
  reserve 98–99% of their allocatable memory, and the pve5 worker is at its
  110-pod limit with memory to spare.
  Evicted pods with no room anywhere — Argo CD, cert-manager and the Vault
  operator — sat `Pending` for the whole outage. See
  [Pods stuck Pending after a host loss](#pods-stuck-pending-after-a-host-loss).

Host-side state on 2026-11-05 is unchanged from the first hang: `softdog` is
the only watchdog loaded, `sp5100_tco` is present but not loaded, and no crash
capture (`pstore`, `kdump`, `netconsole`) is configured.

## Symptom

Any of these, usually several at once:

- `HardwareHostUnreachable` fires — a TCP probe of port 22, five minutes of
  failures (`scenarios/host-remote-power-recovery.md`, "Detection"). Later
  than that if the dead host was carrying the prober; see above.
- `kubectl` hangs or times out. That is pve5 specifically: kubeconfig points
  at the load balancer, and the load balancer lives there — it moved off pve1
  on 2026-11-08.
- A Kubernetes node goes `NotReady` and stays that way.
- `ssh root@<host>` times out — a timeout, not `Permission denied`. A refusal
  with the host still answering on port 22 is a different failure with a
  remote fix: `scenarios/host-remote-power-recovery.md`, "SSH key auth
  failure".

## Before you start

- **Work from a machine that is not on the dead host.** devbox is on pve5 and
  devbox2 on pve1, so each is the lifeboat for the other's host. A pve5 loss
  now also takes the load balancer, so devbox2 on pve1 is the lifeboat for an
  LB outage as well.
- **The cluster API without the load balancer.** Needed whenever the host
  carrying VM 110 is down — pve5 since 2026-11-08:
  ```bash
  kubectl --server=https://192.168.1.241:6443 get nodes   # or .98, .125
  ```
  Every `kubectl` in this runbook takes the same flag until `192.168.1.199`
  answers again.
- **The Proxmox API without pve1.** `pvesh` over SSH to any surviving node
  works as-is. Terraform does not while pve1 is down: `proxmox_endpoint` is
  pve1. The vaulted tfvars also still place VM 110 on pve1, so no
  `terraform apply` may run until a human corrects `node_name`. The override
  is in `docs/host-addressing.md`.
- **Terraform's version**, if this ends in a rebuild: the repo requires
  `>= 1.16.3` and devbox's system binary was `1.16.0` on 2026-10-02. Find out
  now, not at step 5 of a rebuild.

## Checks: confirm the scope

Establish that it is one host, and that it is the host rather than the network
or your own machine, before acting on anything.

```bash
tailscale status                                  # which nodes the tailnet has lost
for n in 200 201 202 203 204; do
  nc -zw3 192.168.1.$n 22 && echo "192.168.1.$n up" || echo "192.168.1.$n DOWN"
done
ssh root@<surviving-host> 'pvecm status'          # Quorate, 4 of 5 votes
ssh root@<surviving-host> 'pvecm nodes'           # the missing name is the dead host
```

What the answers mean:

| You see | It is |
|---|---|
| One host missing everywhere, `pvecm` quorate at 4/5 | One dead host. Continue |
| Host answers ping and port 22 but is missing from `pvecm nodes` | Not dark — corosync or `pmxcfs` on a live host. `scenarios/pve-stale-node-ip-corosync.md` |
| Two hosts missing, 3/5 | Still quorate, with no margin. Do not reboot anything else |
| Three or more missing, or `pvecm` not quorate | `/etc/pve` is read-only fleet-wide and almost nothing below works. Suspect the switch, the gateway or the power strip before five separate failures |
| Everything missing, including the gateway | Your machine or the LAN, not the hosts |

Then find out what the host was carrying. The per-host table in
`scenarios/host-restart-coordination.md` ("Per-host reference") lists each
host's guests and what their absence does; it is not repeated here. Confirm it
against the cluster rather than the table — the dead host's guests still show,
with the node marked offline:

```bash
ssh root@<surviving-host> 'pvesh get /cluster/resources --type vm --output-format json' \
  | jq -r '.[] | [.vmid, .name, .node, .status] | @tsv' | sort -n
```

`jq` is not installed on the hypervisors; pipe to it locally.

## Leave the cluster alone while you work on the host

Nothing was drained, and that is fine. A dead worker's node goes `NotReady`;
Deployment pods are rescheduled once their toleration runs out, and
StatefulSet pods are not — they sit `Terminating` on the dead node and their
replacements do not start, because Kubernetes cannot tell a dead node from a
partitioned one and will not run two copies of a pod with a stable identity.

Do not force-delete them in the first minutes. If the host comes back, they
recover without help. Force deletion is for a host that is not coming back,
and it is a decision, not a cleanup: it tells Kubernetes the old pod is gone
when nothing has confirmed that. It is also a `kubectl delete`, so an agent
asks first.

Likewise do not `kubectl drain` or `kubectl delete node` a dead node on
reflex. Neither brings anything back sooner.

## Recovery order

Cheapest and most reversible first. Each step is tried only when the one
before it has failed.

### 1. If a replacement load balancer will be built, disarm the old one first

Only for the host carrying VM 110 — pve5 since 2026-11-08 — and only if step 4
below is on the table. VM 110 has
`onboot: 1` on the production address and starts within seconds of the host
booting. Set it to `0` from a surviving node **before** anyone powers the host
on — `scenarios/haproxy-vm-rebuild.md`, emergency rebuild, step 2. If no
replacement exists, leave it: you want it to come back by itself.

### 2. Wake-on-LAN

Per `scenarios/host-remote-power-recovery.md`, "Waking a host": MAC from
`docs/host-addressing.md`, sent twice, from the LAN, bound to the real LAN
adapter if the sender runs Tailscale. Allow three to four minutes before
calling it failed.

WoL only wakes a host that is soft-off with standby power. It does nothing for
a host that has lost AC, and nothing for one that is powered but hung — the
NIC is not listening for a magic packet while the machine believes it is
running. On 2026-10-01 both rounds failed against pve1, whose NIC reads
`Wake-on: g` (re-checked after it came back), so the setting was not the
reason. The LED was lit when the host was reached, which fits the hung case.

### 3. Physical power cycle

There is no BMC on any host and no switched PDU
(`scenarios/host-remote-power-recovery.md`), so this is someone at the rack.
**Look before touching** — the next thirty seconds are the only chance to
collect this, and it is most of what separates one cause from another:

- **Power LED: on, off, or blinking.** On, with WoL ignored, points at a hung
  host with power present. Off means soft-off or no power; with WoL having
  failed, off points at the supply, the brick or the strip rather than the OS.
  Either way it is a reading at that moment, not proof of the minutes before.
- Whether the power brick's own LED is lit, and whether the neighbours on the
  same strip are up.
- Fan noise, and whether the case is hot to the touch.
- Anything on an attached display, photographed.

Then hold the power button until it cuts, wait ten seconds, and press once.
For pve5, which has traps of its own at the console, read
`scenarios/pve5-physical-console.md` first.

Write down what you saw. On 2026-10-01 the one thing noted was the power LED,
lit, and it is the observation that favours a hang over a lost supply. Nothing
else from the list was recorded.

### 4. Rebuild what the host carried — last resort

Only when the host is not coming back in a time you can accept. What that
means per guest:

- **The load balancer (pve5).** `scenarios/haproxy-vm-rebuild.md`, "Emergency
  rebuild: the original host is offline". Unexecuted, with a state-surgery
  step nobody has planned; read its status paragraph before starting.
- **A Talos worker.** Nothing to rebuild in a hurry. The cluster runs on the
  remaining four, short of capacity: three of them sit near 98% of memory
  requests (`scenarios/host-restart-coordination.md`, "Pre-flight"), so expect
  `Pending` pods, not a clean reschedule.
- **A control plane (pve2-4).** etcd holds quorum at 2/3. Do not touch a
  second control-plane host until the first is back.
- **devbox / devbox2.** Each is the other's lifeboat. devbox has nightly
  backups on `truenas-backup`; devbox2 does not and is rebuilt from
  `docs/devbox2-provisioning.md`.
- **vllm-inference (pve5).** Pinned to that host's GPU. It returns when pve5
  does.

## After the host is back

Run all of it. A host that booted is not the same as a fleet that recovered,
and the 2026-10-02 return of pve1 left one guest down that `ping` would never
have shown.

```bash
ssh root@<host> 'uptime'                      # near zero: a real cold boot
ssh root@<host> 'pvecm status'                # Quorate, 5 of 5
ssh root@<host> 'qm list'                     # every onboot guest running, bar the one below
```

**Guests that do not autostart.** A guest whose cloud-init drive is on the NFS
datastore fails `pve-guests` with `disk image
'/mnt/pve/truenas-vmdisks/images/<vmid>/vm-<vmid>-cloudinit.qcow2' already
exists` and stays stopped. On pve1 that is devbox2 (112) — seen on the
2026-10-02 boot — and on pve5 it is devbox (111). Starting it by hand works,
because the mount has settled by then:

```bash
ssh root@pve1 'qm start 112'                  # human, or ask first
```

The Terraform fix (cloud-init drives on local storage) is merged and not yet
applied; `112.conf` still had `ide2` on `truenas-vmdisks` on 2026-10-02. Until
it is, expect this on every boot of either host and do not read it as a failed
recovery.

Then the cluster, through the load balancer again — no `--server`, which is
itself the test that `192.168.1.199` is serving:

```bash
talops haproxy status                         # source: terraform, backendsUp: N/N
kubectl get nodes                             # 8 Ready
kubectl get pods -A | grep -vE 'Running|Completed'
kubectl get statefulsets -A --no-headers | awk '{split($3,r,"/"); if (r[1]!=r[2]) print}'
kubectl get pods -A --field-selector status.phase=Pending
kubectl -n longhorn-system get volumes.longhorn.io    # no ROBUSTNESS=degraded
kubectl get applications.argoproj.io -A --no-headers | awk '$3!="Synced" || $4!="Healthy"'
```

Reading them:

- **HAProxy backends.** Anything short of `N/N` names a node that is `Ready`
  to Kubernetes and still unreachable from the load balancer. Without
  `talops`, HAProxy's own view is
  `ssh haproxy-admin@192.168.1.199 'echo "show stat" | sudo socat stdio /run/haproxy/admin.sock' | cut -d, -f1,2,18`.
- **Stuck StatefulSet pods.** The `awk` line prints any StatefulSet whose
  ready count is below its desired count; empty is the pass. A pod still
  `Terminating` or `Unknown` minutes after its node returned is the case from
  [Leave the cluster alone](#leave-the-cluster-alone-while-you-work-on-the-host)
  that did not resolve by itself.
- **`Error` pods from CronJobs** that fired during the outage are history, not
  a live fault. After 2026-10-02 two `version-check` job pods were left in
  `Error`, both timestamped inside the outage.
- **Longhorn.** `healthy`, or `unknown` on a detached volume, is clean.
  `degraded` after an abrupt stop means a replica on the returned host is
  rebuilding; wait for it before touching another host. On 2026-10-02 all
  twelve attached volumes were `healthy` 28 minutes after boot.
- **Nothing rebalances.** Pods evicted while the host was down stay where they
  landed. The returned worker comes back emptier than it left, and the others
  fuller — re-run the drain-capacity check in
  `scenarios/host-restart-coordination.md` before the next planned restart.

### Pods stuck Pending after a host loss

A host loss can leave pods `Pending` for as long as it lasts, even though the
API answers, because the survivors have no room. Two limits matter and
neither shows as a node condition:

```bash
kubectl describe nodes | grep -A6 'Allocated resources'     # memory Requests near 100%
for n in $(kubectl get nodes -o name | cut -d/ -f2); do
  echo "$n $(kubectl get pods -A --field-selector spec.nodeName=$n,status.phase!=Succeeded,status.phase!=Failed --no-headers | wc -l)/$(kubectl get node $n -o jsonpath='{.status.allocatable.pods}')"
done                                                          # pod count against maxPods
kubectl get events -A --field-selector reason=FailedScheduling | tail
```

- A worker at `N/N` pods cannot take evictions however much memory it has.
- A worker whose memory *requests* are near 100% cannot either; usage is
  irrelevant to the scheduler.
- Pods on the default `tenant-default` class never preempt anything. Only
  workloads on `platform-critical` can displace others, and as of 2026-11-06
  Argo CD, cert-manager, the Vault operator and the ai-sre relay were not on
  it.
- A single-replica StatefulSet pod stuck `Terminating` on a dead node does
  not move on its own. Once the node is confirmed off (not merely
  unreachable), a human can force it with
  `kubectl delete pod <pod> --force --grace-period=0` after checking its
  volume is not attached anywhere else.

If the host carried the load balancer (pve5 today), two more things rode on it
and neither shows in the checks above:

```bash
ssh haproxy-admin@192.168.1.199 'sudo nft list tables; sysctl net.ipv4.ip_forward'
ssh haproxy-admin@192.168.1.199 'sudo nft list table ip minecraft'   # `ip udpnat` after the migration
tailscale ping --c 1 haproxy-1
tailscale status --json | jq -c '.Peer[] | select(.HostName=="haproxy-1") | {Online, PrimaryRoutes}'
```

**External UDP.** The table must be there, hold one `dnat` line per published
server, and `ip_forward` must read `1`. It is reloaded at boot by its unit,
and was intact after the 2026-10-02 boot. Rules present is not packets
forwarded: check 2 of Step 4 in `scenarios/bedrock-external-access.md` probes
the load balancer on the external port from inside the LAN, and is the test
that counts. The rest is in `scenarios/haproxy-vm-rebuild.md`, "Verify the UDP
ingress rules".

**The subnet router.** A `pong` means the node rejoined with the identity it
already had — no credential is minted on a plain reboot. It does not show the
route works. The JSON line above must print `Online: true` and
`PrimaryRoutes: ["192.168.1.0/24"]` (the route advertised, approved and
primary); `192.168.1.0/24` absent means the approval was lost. Then prove
traffic crosses it, from an off-LAN tailnet client that accepts routes
(`tailscale set --accept-routes=true`; a client on the LAN itself proves
nothing, it reaches the address directly):

```bash
nc -zw3 192.168.1.241 6443 && echo "route carries traffic"    # a control plane, via the subnet route
```

Its name appearing in `tailscale status` proves nothing; offline peers are
listed too.

## Evidence for root cause

Collect this as soon as the host answers. `-b -1` is the outage boot only
until the next reboot renumbers it, and one item is purely a baseline for next
time.

```bash
ssh root@<host> 'journalctl --list-boots | tail -4'
ssh root@<host> 'journalctl -b -1 -n 50 --no-pager -o short-iso'
ssh root@<host> 'journalctl -b -1 -k -p warning --no-pager -o short-iso | tail -20'
ssh root@<host> 'ls -la /sys/fs/pstore/ /var/lib/systemd/pstore/ 2>&1'
ssh root@<host> 'for z in /sys/class/thermal/thermal_zone*; do echo "$(cat $z/type) $(cat $z/temp)"; done'
ssh root@<host> 'smartctl -a /dev/nvme0n1 | grep -iE "unsafe|power cycles|power on hours|critical warning|integrity errors|^temperature"'
```

What each one is for, and what pve1 showed on 2026-10-02:

| Check | What it tells you | pve1, 2026-10-02 |
|---|---|---|
| `--list-boots` | When the previous boot's journal stopped and the next began — the outage window from the host's side | Boot `-1` ran 2026-06-08 to 2026-10-01 18:44:53 CDT, 115 days. Boot `0` began 20:53:07 CDT |
| `-b -1`, last lines | A clean shutdown logs one. Its absence is an abrupt stop: power, hang or reset | No shutdown sequence. The last line is a routine once-a-minute `sshd` connection close |
| `-b -1 -k -p warning` | Kernel complaints before the stop: MCE, thermal throttling, NVMe timeouts, OOM | Nothing at warning level after 2026-08-02 |
| `pstore` | A panic dump, if the kernel died talking | Empty in both locations — no panic was recorded |
| Thermal zone | Current temperature only; millidegrees | `acpitz` 71000 (71 °C), 28 minutes after boot |
| `smartctl` unsafe shutdowns | Increments on every power loss the drive was not warned of | `Unsafe Shutdowns: 29` of `Power Cycles: 49`; critical warning `0x00`, 0 integrity errors, 52 °C |

What that does and does not establish:

- The stop was abrupt and the kernel logged nothing on the way down. That
  rules out an orderly shutdown and a panic that reached `pstore`. By itself
  it does not separate a hard hang from a power loss.
- **The power LED leans one way.** It was lit when the owner reached the host
  (about 01:50 UTC), Wake-on-LAN had been ignored before that, and the host
  then needed a manual power cycle. A hard hang with power present is the
  best-supported reading. One LED observation, made over an hour after the
  stop, does not show power was continuous and does not rule out an
  intermittent supply fault.
- The unsafe-shutdown count was read after the event with no earlier reading
  to compare, so it shows this host has a history of unclean stops and cannot
  show whether this one added to it. **Record it now on every host**, so the
  next outage has a before.
- 71 °C is one reading on a lightly loaded host half an hour after boot. On
  2026-10-02 there was no temperature history: none of the five hosts ran a
  metrics exporter (nothing listening on 9100 on any of them), so whether pve1
  was hot at 23:44 was not recoverable. That has since been fixed for the next
  occurrence — see the 2026-11-05 section above. A later read-only look, 2026-10-04 20:36
  CDT, found `k10temp` at 89.5 °C and `acpitz` at 84 °C with the worker VM at
  about 245% CPU and load near 2 — pve1 runs hot under ordinary load. That is
  an observation about the host, not a cause of the hang.
- The journal's last line bounds the stop from below only. journald writes to
  disk at intervals, so an abrupt stop can lose the final lines along with
  whatever the kernel said in them.

**2026-11-05, same host.** The drive's unsafe-shutdown counter read 30 of 50
power cycles, one more of each than on 2026-10-02. The journal again ended on
a routine `sshd` line with nothing at warning level after it, and `pstore` was
empty. The metrics from the hour before show a hot but not throttling CPU
(78–87.5 °C) at ordinary load, which fits a thermal or firmware-power-state
cause without proving either. Two occurrences on this host and none on pve2–5
(different boards, BIOS 1.04 and 1401) point at pve1's platform rather than
the fleet.

So: most likely a hard hang with power present. **What caused it is unknown.**
Nothing above names a component — the logs are silent, the temperatures were
read after the event, and the drive reports healthy. Do not write a cause down
that these do not support.
