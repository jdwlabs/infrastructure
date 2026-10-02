# Runbook: A Proxmox host goes dark

One of pve1-5 stops answering without anyone having asked it to. This is the
unplanned counterpart to `scenarios/host-restart-coordination.md`: there was no
pre-flight, no drain, and the first job is working out what is actually down.

Written from the 2026-10-01 pve1 outage and re-checked against the live fleet
on 2026-10-02. What kind of failure it was is established — a hard hang with
the power still on. What caused the hang is **not** — see
[Evidence for root cause](#evidence-for-root-cause).

## What happened on 2026-10-01

pve1 (`192.168.1.200`) stopped at about 23:45 UTC. Its own journal ends at
23:44:53 with a routine line and no shutdown sequence after it. Two rounds of
Wake-on-LAN did nothing. At the rack its power LED was on. A manual power
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

## Symptom

Any of these, usually several at once:

- `HardwareHostUnreachable` fires — a TCP probe of port 22, five minutes of
  failures (`scenarios/host-remote-power-recovery.md`, "Detection"). Later
  than that if the dead host was carrying the prober; see above.
- `kubectl` hangs or times out. That is pve1 specifically: kubeconfig points
  at the load balancer, and the load balancer lives there.
- A Kubernetes node goes `NotReady` and stays that way.
- `ssh root@<host>` times out — a timeout, not `Permission denied`. A refusal
  with the host still answering on port 22 is a different failure with a
  remote fix: `scenarios/host-remote-power-recovery.md`, "SSH key auth
  failure".

## Before you start

- **Work from a machine that is not on the dead host.** devbox is on pve5 and
  devbox2 on pve1, so each is the lifeboat for the other's host.
- **The cluster API without the load balancer.** Needed whenever pve1 is the
  host that is down:
  ```bash
  kubectl --server=https://192.168.1.241:6443 get nodes   # or .98, .125
  ```
  Every `kubectl` in this runbook takes the same flag until `192.168.1.199`
  answers again.
- **The Proxmox API without pve1.** `pvesh` over SSH to any surviving node
  works as-is. Terraform does not: `proxmox_endpoint` is pve1. The override is
  in `docs/host-addressing.md`.
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

Only for pve1, and only if step 4 below is on the table. VM 110 has
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
reason. The power LED was on when the host was reached: it was the hung case.

### 3. Physical power cycle

There is no BMC on any host and no switched PDU
(`scenarios/host-remote-power-recovery.md`), so this is someone at the rack.
**Look before touching** — the next thirty seconds are the only chance to
collect this, and it is most of what separates one cause from another:

- **Power LED: on, off, or blinking.** On means the host had power and was
  hung, which is why WoL did nothing. Off means soft-off or no power; with WoL
  having failed, off points at the supply, the brick or the strip rather than
  the OS.
- Whether the power brick's own LED is lit, and whether the neighbours on the
  same strip are up.
- Fan noise, and whether the case is hot to the touch.
- Anything on an attached display, photographed.

Then hold the power button until it cuts, wait ten seconds, and press once.
For pve5, which has traps of its own at the console, read
`scenarios/pve5-physical-console.md` first.

Write down what you saw. On 2026-10-01 the one thing noted was the power LED,
on, and it is the observation that settled power loss against a hang. Nothing
else from the list was recorded.

### 4. Rebuild what the host carried — last resort

Only when the host is not coming back in a time you can accept. What that
means per guest:

- **The load balancer (pve1).** `scenarios/haproxy-vm-rebuild.md`, "Emergency
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

If the host was pve1, two more things rode on the load balancer and neither
shows in the checks above:

```bash
ssh haproxy-admin@192.168.1.199 'sudo nft list tables; sysctl net.ipv4.ip_forward'
ssh haproxy-admin@192.168.1.199 'sudo nft list table ip minecraft'   # `ip udpnat` after the migration
tailscale ping --c 1 haproxy-1
```

**External UDP.** The table must be there, hold one `dnat` line per published
server, and `ip_forward` must read `1`. It is reloaded at boot by its unit,
and was intact after the 2026-10-02 boot. Rules present is not packets
forwarded: check 2 of Step 4 in `scenarios/bedrock-external-access.md` probes
the load balancer on the external port from inside the LAN, and is the test
that counts. The rest is in `scenarios/haproxy-vm-rebuild.md`, "Verify the UDP
ingress rules".

**The subnet router.** A `pong` means it rejoined with the identity it already
had — no credential is minted on a plain reboot. Its name appearing in
`tailscale status` proves nothing; offline peers are listed too.

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
- **The power LED does.** It was on when the owner reached the host, which
  then needed a manual power cycle. A host with its LED lit, ignoring
  Wake-on-LAN, and leaving no log is a hard hang with power present — not a
  lost supply, and not a machine that shut itself off.
- The unsafe-shutdown count was read after the event with no earlier reading
  to compare, so it shows this host has a history of unclean stops and cannot
  show whether this one added to it. **Record it now on every host**, so the
  next outage has a before.
- 71 °C is one reading on a lightly loaded host half an hour after boot. There
  is no temperature history: none of the five hosts runs a metrics exporter
  (nothing listening on 9100 on any of them, 2026-10-02), so whether pve1 was
  hot at 23:44 is not recoverable.
- The journal's last line bounds the stop from below only. journald writes to
  disk at intervals, so an abrupt stop can lose the final lines along with
  whatever the kernel said in them.

So: a hard freeze, power on throughout. **What froze it is unknown.** Nothing
above points at a component — the logs are silent, the one temperature
reading postdates the event, and the drive reports healthy. Do not write a
cause down that these do not support.
