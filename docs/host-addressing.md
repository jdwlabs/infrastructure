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
| pve5 | 192.168.1.204 | DHCP lease with a gateway-side Fixed Allocation | `bc:fc:e7:ea:23:de` |

The gateway, DHCP server and DNS server are all `192.168.1.254` — an AT&T
BGW320-500, identified from the vendor option in pve1's lease. Lease time is
86400s, so a DHCP-held address is only guaranteed for a day at a time.

**pve5's addressing changed since this document was first written.** It was
originally `inet static` on the host at `192.168.1.169`, inside the DHCP pool
— a duplicate-address collision waiting to happen, and it happened: a 2026-08-06
IP collision took down pmxcfs and cascaded into a full 5-node corosync quorum
outage. Recovery moved pve5 off host-static entirely and onto plain
`iface vmbr0 inet dhcp` (confirmed live via SSH — `/etc/network/interfaces` has
no static block), with a gateway-side Fixed Allocation now pinning it to
`.204`. `.169` is dead: it accepts no connection on `22` or `8006`.

**All five hosts are now DHCP-held, and only pve5 has a gateway reservation.**
pve1-pve4 hold their addresses purely at the gateway's discretion — nothing
stops a renewal landing somewhere else. pve5 has the fixed allocation this
document's own Decision section (below) recommends extending to the other
four; it just arrived at that state via an outage rather than the planned
route.

The pool is observed to span roughly `.64`-`.253` — the BGW320-500 default, and
consistent with every address the reconciler has ever seen a VM receive. The
existing vLLM inference VM sits deliberately at `192.168.1.50`, below that
floor, and is the one host on this network whose address is genuinely
unreachable by DHCP.

## Reaching a host by name

Every host already answers to a name on the LAN, and **the name is the better
address to use** — not merely a convenience. This is easy to miss because
nothing in this repository references it.

The gateway at `192.168.1.254` publishes forward and reverse records for its
DHCP clients under `attlocal.net`. Verified against that resolver directly:

| Name | Resolves to | Reverse |
| --- | --- | --- |
| `pve1.attlocal.net` | 192.168.1.200 | `pve1.attlocal.net` |
| `pve2.attlocal.net` | 192.168.1.201 | `pve2.attlocal.net` |
| `pve3.attlocal.net` | 192.168.1.202 | `pve3.attlocal.net` |
| `pve4.attlocal.net` | 192.168.1.203 | `pve4.attlocal.net` |
| `pve5.attlocal.net` | 192.168.1.204 **and 192.168.1.169** | `.204` → `pve5.attlocal.net`, `.169` → `pve5` |

The records only exist on that resolver. A client pointed at a public resolver
gets `NXDOMAIN`, so this is a LAN-only path — which is the correct scope for a
hypervisor management interface, and the reason no public record should be
created for one.

This is a different mechanism from how `*.jdwlabs.com` names resolve on the LAN,
which the same gateway cannot help with at all — see
[lan-name-resolution.md](lan-name-resolution.md).

### The name matches the certificate; the address does not

Each host serves a certificate on `:8006` whose subject is
`CN=pve<n>.attlocal.net`, with `DNS:pve<n>` and `DNS:pve<n>.attlocal.net` in the
subject alternative names. Browsing to `https://pve1.attlocal.net:8006` therefore
produces a hostname *match*; browsing to `https://192.168.1.200:8006` produces a
mismatch, because the only IP in that certificate is `192.168.1.233`.

Either way the issuer is the Proxmox cluster's own CA, so a browser still warns
until that CA is trusted once. The point is that the name removes a second,
permanent warning that the address cannot.

Those embedded addresses are also evidence in their own right. pve1-pve4 carry
`.233`, `.222`, `.221` and `.223` — addresses none of them holds now. The
certificates were minted when the hosts were there. **All five hosts have now
moved at least once under DHCP**; the risk described above is not hypothetical
for any of them. pve5's certificate still carries `192.168.1.169` (verified
live, `openssl s_client` against `.204:8006`) — the address it held under its
old host-static config, from before the 2026-08-06 outage moved it onto plain
DHCP. Unlike pve1-pve4, the cert has not been reissued since, so it now
mismatches pve5's live address the same way theirs do.

### pve5's name is currently broken

`pve5.attlocal.net` resolves to two addresses. `192.168.1.169` accepts no
connection on `22` or `8006` — it is a leftover record from before the outage
that the gateway never retired. Clients that fall through to the second
address recover after a connect timeout; clients that try only the first fail
outright. Either way the name is unreliable in a way the address is not, which
is the one place where preferring the name is currently the wrong advice.

Clearing it is part of the same gateway visit as the reservations below: the
stale lease has to be released there. Until then, prefer `192.168.1.204` for
pve5 specifically, and the name for the other four.

### Why the repository still uses addresses

`talops` cannot accept a name today. `ProxmoxNodeIPs` is typed
`map[string]net.IP`, and `ProxmoxSSHHost` is passed through `net.ParseIP`, which
yields `nil` for a hostname rather than an error — so a name substituted into
`proxmox_node_ips` or `proxmox_endpoint` would fail quietly rather than loudly.
Changing that is a code change, not a configuration change.

It would also buy less than it appears to. A DHCP-published name is derived from
the lease, so it inherits exactly the instability the lease has. Pinning the
addresses is what makes either form dependable; the name is what makes TLS
correct once they are.

## Decision

**Create DHCP reservations at the gateway for pve1-pve4, keyed by MAC, pinning
each to the address it holds today. pve5 already has one, at `.204`, put in
place during the 2026-08-06 outage recovery — extend the same treatment to the
remaining four rather than inventing a different mechanism for them.**

Reservations are what actually pins an address, because they act on the
component that hands addresses out. This is also, incidentally, what pve5's
outage recovery already proved: the previous approach — host-static config
inside the DHCP pool — is what caused that outage in the first place. A
reservation avoids repeating it, because it acts on the gateway rather than
asserting an intent the gateway never sees.

Reservations were chosen over host-static configuration for pve1-pve4 for the
same reason pve5 was moved off it: the addresses in question are inside the
pool, and host-static config there doesn't stop the pool leasing the same
address to something else — it just makes the eventual collision worse,
because the host insists on an address the gateway may have already handed
elsewhere.

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
each MAC in the table above to the address listed beside it, for pve1-pve4.
pve5 already has its reservation from the 2026-08-06 recovery — confirm it is
still present rather than recreating it.

While there, release the stale `192.168.1.169` lease so the gateway stops
publishing it as a second address for `pve5.attlocal.net`. A reservation alone
does not retract a record the gateway already holds.

Verify afterwards, from any host that can reach the LAN:

```bash
for h in pve1:192.168.1.200 pve2:192.168.1.201 pve3:192.168.1.202 \
         pve4:192.168.1.203 pve5:192.168.1.204; do
  name=${h%%:*}; ip=${h##*:}
  echo | openssl s_client -connect "$ip":8006 2>/dev/null \
    | openssl x509 -noout -subject | grep -q "CN=$name" \
    && echo "$name $ip OK" || echo "$name $ip MISMATCH"
done
```

Each host answering on `:8006` with its own name in the certificate subject is
the check that matters — it proves the address maps to the host the repo thinks
it does, which a ping cannot.

Then confirm each name resolves to exactly one address, which is what proves the
stale `.169` record is gone:

```bash
for n in pve1 pve2 pve3 pve4 pve5; do
  printf '%-20s %s\n' "$n.attlocal.net" \
    "$(dig +short "$n.attlocal.net" @192.168.1.254 | tr '\n' ' ')"
done   # expect exactly one address per host
```

Today that loop returns two addresses for `pve5` and one for each of the others.

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
