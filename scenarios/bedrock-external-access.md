# Exposing Bedrock servers to the internet

How a Minecraft Bedrock server running in the cluster becomes reachable from
outside the LAN, and how to add a second, third or fourth without redesigning
anything.

Written against the live setup: an AT&T BGW320-500 gateway at `192.168.1.254`,
a wildcard `*.jdwlabs.com` already resolving to the WAN address, and servers
exposed in-cluster as UDP NodePorts.

## Why a port forward is unavoidable

The instinct is to route Bedrock through the existing HTTPS gateway the way
every other service is reached — `something.jdwlabs.com` and done. That cannot
work, for three independent reasons, any one of which is fatal:

**Bedrock is UDP.** It speaks RakNet over UDP. The nginx Gateway listens on
HTTP/80 and HTTPS/443 only, and HAProxy is `mode tcp` throughout. Neither
carries UDP at all. The `UDPRoute` CRD exists in the cluster only because the
Gateway API bundle installs it; NGINX Gateway Fabric does not implement it.

**The protocol carries no hostname.** Several names share one address on 443
because TLS SNI announces the hostname inside the handshake, and the gateway
routes on it. RakNet has no equivalent field. A client resolves the name to an
address and the name is then gone — it never appears in the packets. So even a
UDP-capable proxy listening on one port would receive an anonymous stream with
nothing distinguishing one server's traffic from another's.

**Something has to open the door.** Even with perfect in-cluster routing,
packets from the internet must first enter the LAN, and the gateway holds the
public address.

The consequence shapes everything below: **servers are told apart by port, not
by name.** DNS gets a player to the house; the port number picks the room.

## The path a packet takes

Three hops, and only the middle one is described by this runbook:

```
player ──19132/udp──▶ BGW320-500        one rule, the range 19132-19141,
                      192.168.1.254     forwarded with no port rewrite
       ──19132/udp──▶ HAProxy VM        nftables table ip udpnat DNATs
                      192.168.1.199     each external port to a NodePort
       ──31132/udp──▶ cluster node      NodePort answers on every node
                      192.168.1.163
```

The HAProxy VM is the **single external UDP ingress hop**. It is statically
addressed and already under Terraform management, which is why the router rule
can be written once and left alone.

The nftables rules on that VM are generated from cluster state by
`bootstrap/internal/udpnat`: it reads every Service carrying
`platform.jdwlabs.io/udp-external-port` and renders the DNAT and masquerade
rules. **Publishing a server is an annotation on its Service, not a new router
rule** — see `scenarios/haproxy-vm-rebuild.md` for how that state lives on the
VM and how to restore it after a rebuild.

## The port convention

External UDP `191NN` maps to NodePort `311NN`. The last two digits match, which
makes the mapping memorable and keeps every NodePort inside Kubernetes' default
30000-32767 range.

| Server | Player enters | External UDP | NodePort | Namespace |
|---|---|---|---|---|
| FWB | `mc.jdwlabs.com` | 19132 | 31132 | `jdwillmsen-prd` |
| *(next)* | `mc.jdwlabs.com:19133` | 19133 | 31133 | |
| *(next)* | `mc.jdwlabs.com:19134` | 19134 | 31134 | |

The first server deliberately uses **19132**, Bedrock's default port. A client
fills that in by itself, so players type a bare hostname and nothing else.
Every subsequent server costs the player an explicit `:port`.

The gateway forwards the whole range **19132-19141** to one address and never
rewrites the port. Ten servers therefore cost one router rule, defined once in
Step 2 and never edited again. The 191NN-to-311NN translation happens further
in, on the HAProxy VM, from cluster state.

Java Edition, if one is ever added, is **TCP** rather than UDP and conventionally
25565 — a separate forward with a different protocol, not part of this range.

## Prerequisites

- The server is running and answers on its NodePort from inside the LAN.
- `allow-list=true` and `online-mode=true` in `server.properties`, and
  `allowlist.json` lists the intended players. Exposing a Bedrock server with
  an empty allowlist means anyone with an Xbox account can join.
- The gateway's **Device Access Code** — printed on a sticker on the side of
  the BGW320-500. It is not the Wi-Fi password.
- SSH to the HAProxy VM as `haproxy-admin`, to read and apply its nftables
  rules. That VM is the single external UDP ingress hop; without it the gateway
  half of this runbook forwards into a black hole.

## Step 1 — confirm the edge address

The forward points at exactly one device: the HAProxy VM. Everything downstream
of it is decided by nftables on that host, not by the gateway.

| Device | Address | MAC |
|---|---|---|
| HAProxy VM (`haproxy-1`) | `192.168.1.199` | `bc:24:11:1e:cd:65` |

No gateway reservation is needed. The address is set by cloud-init from the
`haproxy_vms` entry in the vaulted tfvars, so Terraform is its source of truth —
see `scenarios/haproxy-vm-rebuild.md`. Confirm it, and re-derive the MAC if the
VM has been rebuilt:

```bash
ssh haproxy-admin@192.168.1.199 'ip -4 addr show eth0; cat /sys/class/net/eth0/address'
```

Confirm the VM is also carrying the UDP rules before touching the gateway. A
correct forward into a host with no `udpnat` table is indistinguishable, from
outside, from no forward at all:

```bash
ssh haproxy-admin@192.168.1.199 'sysctl net.ipv4.ip_forward'   # expect: = 1
ssh haproxy-admin@192.168.1.199 'sudo nft list table ip udpnat'
```

Expect `net.ipv4.ip_forward = 1`, and one `udp dport <external> dnat to
<node-ip>:<nodeport>` line per published server. Forwarding off means the VM
accepts the packets and drops them — no refusal, no log — because the DNAT
destination is a *different* host.

`No such file or directory` from the second command has two causes, and they
need different responses:

- **A `minecraft` table is present instead.** That is the hand-applied
  predecessor of `udpnat`, and it is what `192.168.1.199` carries today. Do the
  migration below before anything else. Check with
  `sudo nft list tables`.
- **Neither table is present.** The VM was rebuilt, or the rules were never
  placed. Go to *Placing the nftables rules* below and come back.

## Step 2 — define the service on the gateway

The BGW320-500 splits this into defining a *service*, then attaching it to a
*device*. Both halves are required; doing only the first appears to work and
forwards nothing.

1. **Firewall → NAT/Gaming**.
2. Open **Custom Services** (a link on that page, not a top-level menu).
3. Create the service — **once, for the whole range**:

   | Field | Value |
   |---|---|
   | Service Name | `bedrock-udp` |
   | Global Port Range | `19132` to `19141` |
   | Base Host Port | `19132` |
   | Protocol | **UDP** |

   `Global Port Range` is what the internet connects to; `Base Host Port` is
   what the LAN device receives on. They are **deliberately equal** here: the
   gateway must deliver each port unchanged, because the HAProxy VM's nftables
   rules match on the external port. Setting `Base Host Port` to a NodePort
   (`31132`) makes the gateway rewrite the port before delivery, and the DNAT
   rule then matches on a port that never arrives — the forward looks correct
   in the UI and drops every packet.

   All port translation belongs to nftables, and only to nftables.
4. **Add**.

This is the only gateway rule this design ever needs. Servers two through ten
are already covered by the range.

### The gateway's device names are misleading

The BGW learns names from DHCP option 12, which a client sends when it takes a
lease. Statically addressed hosts never perform that exchange, so they never
send a name and the gateway falls back to the MAC vendor string — or to a stale
label left by whatever previously held the address.

The effect is visible in **Home Network → Device List**: every `dhcp` device
carries a correct name, and every `static` one is wrong or generic. Three
devices appear as `Proxmox Server Solutions GmbH`, and the HAProxy VM appears
as something it has never been.

Select devices by this table, not by the name in the dropdown:

| Device | Address | Shown in the gateway as | MAC |
|---|---|---|---|
| HAProxy VM (`haproxy-1`) | `192.168.1.199` | **`Windows 10`** | `bc:24:11:1e:cd:65` |
| devbox | `192.168.1.56` | `Proxmox Server Solutions GmbH` | `bc:24:11:11:dd:af` |
| GPU VM | `192.168.1.50` | `Proxmox Server Solutions GmbH` | `bc:24:11:ce:9c:f0` |

Confirm against the MAC in the Device List before trusting a name, and re-derive
it if a VM is rebuilt:

```bash
ssh haproxy-admin@192.168.1.199 'ip -4 addr show eth0; cat /sys/class/net/eth0/address'
```

There is no API on the BGW320 to rename a device, so nothing in this repo can
correct these labels. Switching the VMs to DHCP with reservations would fix the
names permanently, at the cost of making the gateway the source of truth for
addressing — which is the opposite of how these hosts are provisioned today.

Do **not** use *Clear and Rescan for Devices* to tidy this up. It clears the
stale label, but the statically addressed hosts then all resolve to the same
vendor string, replacing one uniquely-wrong name with three identical ones.

## Step 3 — attach the service to the HAProxy VM

1. Back on **Firewall → NAT/Gaming**.
2. **Service** — select `bedrock-udp`.
3. **Needed by Device** — select the HAProxy VM at `192.168.1.199`, MAC
   `bc:24:11:1e:cd:65`. It is listed as `Windows 10`, not by its hostname; see
   the table above.
4. **Add**, then **Save**.

The device list is populated from DHCP leases, and the HAProxy VM is statically
addressed — it never takes one. It appears under a stale or vendor label, and on
a freshly rescanned gateway it may not appear at all. If it is missing, give it
a temporary DHCP lease long enough for the gateway to learn it, attach the
service, then restore the static address; the attachment is keyed on the MAC and
survives. Match on the MAC in every case, never on the displayed name.

## Step 4 — verify

Verify hop by hop, in order. Each check isolates one of the three legs, so a
failure names the leg that broke rather than just "it does not work".

**1. The NodePort answers** — the cluster leg, from inside the LAN:

```bash
kubectl -n jdwillmsen-prd run mcprobe --rm -i --restart=Never \
  --image=itzg/mc-monitor:latest -- \
  status-bedrock --host 192.168.1.163 --port 31132
```

Expect `version=… online=… max=…`.

**2. The HAProxy VM answers on the external port** — the nftables leg. This is
the check that catches a missing `udpnat` table, `ip_forward` left at `0`, and
a gateway configured to rewrite the port. It needs no off-network access:

```bash
kubectl -n jdwillmsen-prd run mcprobe --rm -i --restart=Never \
  --image=itzg/mc-monitor:latest -- \
  status-bedrock --host 192.168.1.199 --port 19132
```

Note the address and port: the **intermediate** hop, on the **external** port —
`192.168.1.199:19132`, not `:31132`. Check 1 succeeding while this fails means
the DNAT rule is absent or points at the wrong node. If this passes and the
external check below fails, the fault is on the gateway and nowhere else.

**3. From outside.** This is the step that actually tests the forward,
and it cannot be done from the LAN — most consumer gateways do not hairpin, so
connecting to the public address from inside fails even when the forward is
correct. Use a phone on mobile data, or any host off-network:

```bash
mc-monitor status-bedrock --host mc.jdwlabs.com --port 19132
```

In the game client: **Servers → Add Server**, address `mc.jdwlabs.com`, port
`19132`.

## Placing the nftables rules on the HAProxy VM

No `talops` command owns this step yet, so it is done by hand. This is the
procedure the rest of this document means by "apply the rules", and it is what
`scenarios/haproxy-vm-rebuild.md` points at after a rebuild.

The paths and unit name below are this runbook's convention. `192.168.1.199`
was set up by hand before they were written down, so confirm against the live
host rather than assuming — `systemctl cat udpnat-rules` and
`sudo nft list tables` are the two answers.

1. **Work out the ruleset.** The format is pinned by
   `bootstrap/internal/udpnat/testdata/single-forward.nft` in the
   `infrastructure` repo — one `dnat` line and one `masquerade` line per
   published server, sorted by external port. Copy that file and extend it;
   do not invent a layout.
2. **Write it to the VM:**
   ```bash
   sudo install -d /etc/nftables.d
   sudo tee /etc/nftables.d/udpnat.nft >/dev/null <<'EOF'
   # ... contents from step 1 ...
   EOF
   ```
3. **Apply it:**
   ```bash
   sudo nft -f /etc/nftables.d/udpnat.nft
   ```
   The file opens with `table ip udpnat` / `delete table ip udpnat` / `table ip
   udpnat {`, which replaces the table atomically. It is safe to re-run, and it
   never flushes.

   Never run `nft flush ruleset` on this host. Tailscale's rules live in
   `table ip filter` and are installed at runtime — flushing takes the VM off
   the tailnet, and `nftables.service` is disabled here for exactly that reason
   (its `/etc/nftables.conf` starts with `flush ruleset`).
4. **Make it survive a reboot:**
   ```bash
   sudo tee /etc/systemd/system/udpnat-rules.service >/dev/null <<'EOF'
   [Unit]
   Description=UDP ingress DNAT rules
   After=network-online.target
   Wants=network-online.target

   [Service]
   Type=oneshot
   RemainAfterExit=yes
   ExecStart=/usr/sbin/nft -f /etc/nftables.d/udpnat.nft

   [Install]
   WantedBy=multi-user.target
   EOF
   sudo systemctl daemon-reload
   sudo systemctl enable --now udpnat-rules.service
   ```
5. **Verify**, with check 2 of Step 4.

### Migrating off the `minecraft` table

`192.168.1.199` still carries `table ip minecraft`, the hand-applied
predecessor. It holds the same rules under a table named after the first
workload; `udpnat` replaces it because the generator is generic over UDP
services.

**Delete the old table first, then apply the new one.** The order matters more
than the brief gap it opens:

```bash
sudo nft delete table ip minecraft
sudo nft -f /etc/nftables.d/udpnat.nft
```

Both tables register a `prerouting` base chain at priority `dstnat`, and both
match `udp dport 19132`. With both loaded, which one wins is chain registration
order — undefined between tables. NAT binds on the first packet of a flow and
the decision is held for the life of the conntrack entry, so a client that
lands on the losing chain stays wrong until its entry expires, long after the
tables look correct. Deleting first costs a few seconds of dropped packets,
which Bedrock clients retry through.

## Adding another server

**Nothing on the gateway changes.** The range rule from Step 2 already carries
`19132-19141`; adding a server is a cluster-side change only.

1. Give it the next NodePort in the chart values (`31133`, then `31134`, …),
   with `serverPort` equal to `nodePort` — the chart requires them to match for
   clients to display a ping time.
2. Annotate its Service with the external port it claims:
   ```yaml
   metadata:
     annotations:
       platform.jdwlabs.io/udp-external-port: "19133"
   ```
3. Re-render and apply the nftables rules on the HAProxy VM — the procedure in
   *Placing the nftables rules* above, from step 1. The generator reads the
   annotation and emits the DNAT and masquerade rules; it refuses a port
   outside `19132-19141`, because the gateway forwards nothing else.
4. Add a row to the table at the top of this document.
5. Verify with check 2 of Step 4 against the new port, then from outside.

An eleventh server is the first one that needs the gateway touched again. Widen
the range on the gateway first, then the generator's bounds — three places, all
required: `ExternalPortMin`/`ExternalPortMax` in
`bootstrap/internal/udpnat/config.go`, and the literals in
`TestExternalPortRangeMatchesTheRouterRule`, which hardcodes `19132`/`19141` and
fails until it is updated. That test failing is the intended reminder that the
router rule and the generator must agree.

## Known gaps

**The WAN address is not guaranteed stable.** `*.jdwlabs.com` currently resolves
to a residential AT&T address. Nothing in this repo updates DNS when that
address changes, so every published server becomes unreachable until someone
notices and edits the record by hand. The credentials to fix this proactively
already exist — cert-manager holds Porkbun API credentials for DNS-01 — so a
small CronJob comparing the current WAN address against the record would close
it. Not built yet.

**The HAProxy VM is a single point of failure for external UDP.** Every
published server enters through `192.168.1.199`. That VM already carries the
Kubernetes API and all HTTP(S) ingress, so this adds no new failure domain — but
it does mean external play stops whenever it does. The keepalived VIP pair noted
in `scenarios/haproxy-vm-rebuild.md` is the fix, and is a separate change.

**The DNAT destination is a DHCP address.** NodePort answers on every node, but
the rendered rule names exactly one — currently `192.168.1.163`. Worker nodes
take their address by DHCP, so a lease change breaks external play silently,
months later, with no obvious cause. The repo's precedent runs the other way:
`clusters/core/patches/node-200.yaml` pins each control plane statically,
written after DHCP lease flips twice moved a control plane and broke the etcd
peer mesh. Workers are left on DHCP because losing one is a reschedule rather
than an outage — which is true of every worker workload *except* this one. Two
ways to close it, neither done: add a static patch for the target worker
mirroring `node-200.yaml`'s shape, or install MetalLB and point the DNAT at a
LoadBalancer address, which removes the single-node dependency as well.

**The nftables rules are not deployed by `talops`.** The generator exists and
nothing calls it, so the file is still placed on the VM by hand. Until a command
owns that step, a rebuilt VM comes back with HAProxy healthy and every published
server dark.

None of these gaps affect LAN play, which is why all of them can go unnoticed.

## Removing a server, and rolling back the forward

Undo in the reverse of the order it was built, so nothing is ever exposed
without a server behind it.

**Remove one server, keep the ingress:**

1. Drop the `platform.jdwlabs.io/udp-external-port` annotation from its Service.
2. Re-render and apply the rules on the HAProxy VM — the procedure in *Placing
   the nftables rules* above, from step 1. The DNAT and masquerade lines for
   that port disappear; the table and every other server stay, because
   `nft -f` replaces the whole table atomically.
3. Confirm the port is gone, and that its neighbours survived:
   ```bash
   ssh haproxy-admin@192.168.1.199 'sudo nft list table ip udpnat'
   ```
4. Delete the row from the table at the top of this document.

The gateway is not touched. The range rule keeps forwarding `19133` to a host
with no rule for it, which drops the packets — the correct outcome.

**Remove external access entirely**, reversing Steps 2 and 3:

1. **Firewall → NAT/Gaming**. Find the `bedrock-udp` entry in the list of
   attached services and delete the **attachment** first, then **Save**. The
   service definition cannot be deleted while a device still references it.
2. Open **Custom Services**, select `bedrock-udp`, and delete the definition.
3. Optionally remove the nftables rules from the VM. Leaving them costs nothing
   once the forward is gone, but removing them keeps the host honest. Disable
   the unit as well as deleting the table — deleting only the table leaves it
   re-applied on the next reboot:
   ```bash
   ssh haproxy-admin@192.168.1.199 \
     'sudo systemctl disable --now udpnat-rules.service && sudo nft delete table ip udpnat'
   ```
   Delete only that table. The host also runs Tailscale, whose rules live in
   `table ip filter` — `nft flush ruleset` takes the VM off the tailnet.

   Leave `net.ipv4.ip_forward` alone. The Tailscale subnet router on this host
   needs it too, so reverting it would silently break LAN routing over the
   tailnet — a failure with no connection to Bedrock at all.
4. Confirm from off-network that the port no longer answers:
   ```bash
   mc-monitor status-bedrock --host mc.jdwlabs.com --port 19132
   ```
   Expect a timeout. LAN play is unaffected throughout.

## Troubleshooting

**Works on the LAN, not outside.** Test from genuinely off-network — phone on
mobile data, not Wi-Fi. Consumer gateways generally do not hairpin, so a LAN
test of the public address fails regardless of whether the forward is right.

**Nothing at all responds externally.** Confirm both halves exist on the
gateway: the Custom Service *and* its attachment to a device. The first alone
looks complete and forwards nothing.

**LAN NodePort works, external does not, and check 2 of Step 4 also fails.**
The fault is on the HAProxy VM, not the gateway. Either the `udpnat` table is
missing — the usual cause after a VM rebuild — or its DNAT rule points at a node
address that has since moved.

**Checks 1 and 2 pass, outside still fails.** Now it is the gateway. The most
likely cause is `Base Host Port` set to a NodePort instead of `19132`: the
gateway rewrites the port, and the nftables rule never matches. Both halves of
the config look right in the UI.

**Worked before, silently stopped.** Check the WAN address against DNS, and
check the target node still holds the address the DNAT rule names — the gaps
above are the usual causes and all present identically.

**Client shows the server but cannot join.** That is the allowlist, not the
network — the ping succeeds because it is unauthenticated while joining is not.
Check `allowlist.json` in the world volume.
