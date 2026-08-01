# Proxmox Host Management Addressing

How the five Proxmox hypervisors get their management addresses, why the
current arrangement is fragile, and what has to happen at the gateway to fix
it.

This is about the **hypervisor hosts**, not the Talos VMs. Talos node addresses
are a separate concern handled by the reconciler.

## Why this matters

`talops` reaches every host over SSH to repopulate ARP before it discovers live
VM state. It gets each host's address from `proxmox_node_ips` in
`terraform.tfvars`. When one of those addresses is wrong, the SSH dial fails,
ARP repopulation is skipped for that host, and its VMs drop out of discovery.

The failure is **quiet**. The ARP failure is a `WARN`, discovery reports a
reduced `found=` count as `INFO`, and the reconciliation plan then renders at
full confidence over partial live state. A plan built that way can propose
rewriting every node config while a control plane is unmapped — with 2/3 etcd
quorum, that is the shape of an outage this cluster has already had.

`terraform.tfvars` also carries `proxmox_endpoint`, pointed at pve1's address.
If pve1's address moves, Terraform and `talops` lose the Proxmox API outright,
not just one host's ARP.

## Current state

Verified from each host's own `/etc/network/interfaces` and confirmed
independently against the certificate each Proxmox host serves on `:8006`.

| Host | Address | How it is held | vmbr0 MAC |
| --- | --- | --- | --- |
| pve1 | 192.168.1.200 | DHCP lease, no reservation | `84:47:09:35:75:1f` |
| pve2 | 192.168.1.201 | DHCP lease, no reservation | `84:47:09:63:06:4e` |
| pve3 | 192.168.1.202 | DHCP lease, no reservation | `84:47:09:63:61:31` |
| pve4 | 192.168.1.203 | DHCP lease, no reservation | `84:47:09:62:ff:cd` |
| pve5 | 192.168.1.169 | `inet static` on the host | `bc:fc:e7:ea:23:de` |

The gateway, DHCP server and DNS server are all `192.168.1.254` — an AT&T
BGW320-500, identified from the vendor option in pve1's lease. Lease time is
86400s, so a DHCP-held address is only guaranteed for a day at a time.

**Both shapes above are unpinned, for different reasons.**

pve1-pve4 hold their addresses purely at the gateway's discretion. Nothing
stops a renewal landing somewhere else.

pve5 is static on the host, which survives reboots — but `192.168.1.169` sits
**inside the DHCP pool**. Static-on-host tells the host what to use; it does not
tell the gateway to stop handing that address to anything else. The pool has
already issued `.169` once: reconciler logs from an earlier run show it leased
to a Talos worker VM. A static host inside the pool is a duplicate-address
collision waiting for the right lease.

The pool is observed to span roughly `.64`-`.253` — the BGW320-500 default, and
consistent with every address the reconciler has ever seen a VM receive. The
existing vLLM inference VM sits deliberately at `192.168.1.50`, below that
floor, and is the one host on this network whose address is genuinely
unreachable by DHCP.

## Decision

**Create DHCP reservations at the gateway for all five hosts, keyed by MAC,
pinning each to the address it holds today. Leave pve5's static configuration in
place, and reserve `.169` to match it.**

Reservations are what actually pins an address, because they act on the
component that hands addresses out. Host-static config is a statement of intent
that the gateway never sees.

Keeping pve5 static *and* reserved is deliberate rather than redundant: the
static config keeps pve5 addressable if the gateway is rebuilt or its
reservation table is lost, and the reservation stops the pool leasing `.169` out
from under it. They must agree — a reservation for a different address than the
host's static config produces a host the gateway believes is somewhere it is
not.

Reservations were chosen over converting pve1-pve4 to host-static because the
addresses in question are inside the pool. Making four more hosts static inside
the pool would spread pve5's collision exposure to the whole cluster while
looking like a fix.

### The alternative, and why it is not the first move

The cleaner end state is to move all five hosts to static addresses **below**
`.64`, outside the pool entirely, matching what the vLLM VM already does. That
removes the dependency on gateway configuration surviving a firmware update or
a factory reset.

It is not the first move because it is far more invasive: it edits the network
configuration of all five hypervisors, and a mistake takes a hypervisor off the
network with no remote path back — recovery is physical console access. It also
changes `proxmox_endpoint`, so Terraform and `talops` lose the API until the
tfvars change is sealed and merged. Reservations achieve the same pinning today,
need no host downtime, and are undone from a web UI.

Treat the move below `.64` as the eventual target, sequenced one host at a time
with console access available, not as a same-sitting change.

## The human step

**Creating the reservations is a manual change on the gateway at
`192.168.1.254`, and it is the only part of this an agent cannot perform.**
There is no API, no Terraform resource and no `talops` path to it.

On the BGW320-500 admin interface, under the LAN IP allocation settings, bind
each MAC in the table above to the address listed beside it. All five, including
pve5.

Verify afterwards, from any host that can reach the LAN:

```bash
for h in pve1:192.168.1.200 pve2:192.168.1.201 pve3:192.168.1.202 \
         pve4:192.168.1.203 pve5:192.168.1.169; do
  name=${h%%:*}; ip=${h##*:}
  echo | openssl s_client -connect "$ip":8006 2>/dev/null \
    | openssl x509 -noout -subject | grep -q "CN=$name" \
    && echo "$name $ip OK" || echo "$name $ip MISMATCH"
done
```

Each host answering on `:8006` with its own name in the certificate subject is
the check that matters — it proves the address maps to the host the repo thinks
it does, which a ping cannot.

Then confirm the reconciler sees the whole fleet:

```bash
talops reconcile --plan --cluster core   # expect: discovered live state found=8
```

A `found=` below the node count, or an `ARP repopulation failed` warning, means
an address in `proxmox_node_ips` no longer matches reality. Re-derive it from
the host rather than guessing — `pvesh get /cluster/status` from any other node
in the cluster reports every member's address.

## SSH host keys are part of this

`talops` verifies host keys against `~/.ssh/known_hosts` and does not constrain
which key algorithm it will negotiate. If a host's entry covers only some of the
key types it offers, the negotiated type may be one that is absent, and the
connection fails as `knownhosts: key mismatch` — which reads like a compromised
host but is a gap in the trust store.

This is not hypothetical: it is why pve5 stayed undiscovered even once its
address was correct. pve1-pve4 were scanned for all three key types when they
were added; pve5 only ever had an ed25519 entry.

When adding a host, capture every type it offers:

```bash
ssh-keyscan -t rsa,ecdsa,ed25519 <host-ip> >> ~/.ssh/known_hosts
```

Confirm what is trusted for a host with `ssh-keygen -F <host-ip>`; the entries
returned should cover the types `ssh-keyscan <host-ip>` reports.
