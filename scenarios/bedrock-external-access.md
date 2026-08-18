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

Java Edition, if one is ever added, is **TCP** rather than UDP and conventionally
25565 — a separate forward with a different protocol, not part of this range.

## Prerequisites

- The server is running and answers on its NodePort from inside the LAN.
- `allow-list=true` and `online-mode=true` in `server.properties`, and
  `allowlist.json` lists the intended players. Exposing a Bedrock server with
  an empty allowlist means anyone with an Xbox account can join.
- The gateway's **Device Access Code** — printed on a sticker on the side of
  the BGW320-500. It is not the Wi-Fi password.

## Step 1 — pin the target node's address

The forward points at one node. NodePort answers on *every* node, so any of
them works; what matters is that the address does not move.

Worker nodes take their address by DHCP. Only the control planes are pinned by
static patches (`clusters/core/patches/node-20*.yaml` → `.241`, `.98`, `.125`).
Forwarding to a DHCP address works until the lease changes, and then breaks
silently, months later, with no obvious cause.

So reserve it on the gateway:

1. Browse to `http://192.168.1.254` and sign in with the Device Access Code.
2. **Home Network → IP Allocation**.
3. Find the node by MAC and set its allocation to fixed/reserved.

Current target:

| Node | Address | MAC |
|---|---|---|
| `talos-lx0-6a4` | `192.168.1.163` | `bc:24:11:16:c2:e1` |

Confirm the MAC before relying on this table — it changes if the VM is rebuilt:

```bash
export TALOSCONFIG=clusters/core/secrets/talosconfig
talosctl -e 192.168.1.163 -n 192.168.1.163 get links eth0 -o yaml | grep hardwareAddr
```

## Step 2 — define the service on the gateway

The BGW320-500 splits this into defining a *service*, then attaching it to a
*device*. Both halves are required; doing only the first appears to work and
forwards nothing.

1. **Firewall → NAT/Gaming**.
2. Open **Custom Services** (a link on that page, not a top-level menu).
3. Create the service:

   | Field | Value |
   |---|---|
   | Service Name | `minecraft-fwb` |
   | Global Port Range | `19132` to `19132` |
   | Base Host Port | `31132` |
   | Protocol | **UDP** |

   `Global Port Range` is what the internet connects to; `Base Host Port` is
   what the LAN device receives on. They differ here on purpose — players get
   Bedrock's default port while the cluster keeps a NodePort-range port.
4. **Add**.

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

## Step 3 — attach the service to the node

1. Back on **Firewall → NAT/Gaming**.
2. **Service** — select `minecraft-fwb`.
3. **Needed by Device** — select the HAProxy VM. It is listed as `Windows 10`,
   not by its hostname; see the table above.
4. **Add**, then **Save**.

The device list is populated from DHCP leases. A node that has never taken a
lease may not appear; Step 1 resolves that as a side effect.

## Step 4 — verify

From inside the LAN, confirm the NodePort itself still answers:

```bash
kubectl -n jdwillmsen-prd run mcprobe --rm -i --restart=Never \
  --image=itzg/mc-monitor:latest -- \
  status-bedrock --host 192.168.1.163 --port 31132
```

Expect `version=… online=… max=…`.

Then verify from **outside**. This is the step that actually tests the forward,
and it cannot be done from the LAN — most consumer gateways do not hairpin, so
connecting to the public address from inside fails even when the forward is
correct. Use a phone on mobile data, or any host off-network:

```bash
mc-monitor status-bedrock --host mc.jdwlabs.com --port 19132
```

In the game client: **Servers → Add Server**, address `mc.jdwlabs.com`, port
`19132`.

## Adding another server

1. Give it the next NodePort in the chart values (`31133`, then `31134`, …),
   with `serverPort` equal to `nodePort` — the chart requires them to match for
   clients to display a ping time.
2. Add a matching Custom Service on the gateway (`19133` → `31133`, UDP).
3. Attach it to the same node.
4. Add a row to the table at the top of this document.

Nothing else changes: one wildcard DNS record covers every name, and every
server rides the same node.

## Known gaps

**The WAN address is not guaranteed stable.** `*.jdwlabs.com` currently resolves
to a residential AT&T address. Nothing in this repo updates DNS when that
address changes, so every published server becomes unreachable until someone
notices and edits the record by hand. The credentials to fix this proactively
already exist — cert-manager holds Porkbun API credentials for DNS-01 — so a
small CronJob comparing the current WAN address against the record would close
it. Not built yet.

**The forward targets a single node.** NodePort answers on every node, but the
gateway forwards to exactly one. If that node is down, external play stops even
though the pod is healthy elsewhere in the cluster. A LoadBalancer address from
MetalLB would remove the dependency; MetalLB is not installed, and adding it is
a platform change rather than part of this runbook.

Neither gap affects LAN play, which is why both can go unnoticed.

## Troubleshooting

**Works on the LAN, not outside.** Test from genuinely off-network — phone on
mobile data, not Wi-Fi. Consumer gateways generally do not hairpin, so a LAN
test of the public address fails regardless of whether the forward is right.

**Nothing at all responds externally.** Confirm both halves exist on the
gateway: the Custom Service *and* its attachment to a device. The first alone
looks complete and forwards nothing.

**Worked before, silently stopped.** Check the WAN address against DNS, and
check the node still holds the reserved address — the two gaps above are the
usual causes and both present identically.

**Client shows the server but cannot join.** That is the allowlist, not the
network — the ping succeeds because it is unauthenticated while joining is not.
Check `allowlist.json` in the world volume.
