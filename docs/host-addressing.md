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
| pve1 | 192.168.1.200 | Fixed Allocation (confirmed live on the gateway UI 2026-08-09) | `84:47:09:35:75:1f` |
| pve2 | 192.168.1.201 | Fixed Allocation (confirmed live on the gateway UI 2026-08-09) | `84:47:09:63:06:4e` |
| pve3 | 192.168.1.202 | Fixed Allocation (confirmed live on the gateway UI 2026-08-09) | `84:47:09:63:61:31` |
| pve4 | 192.168.1.203 | Fixed Allocation (confirmed live on the gateway UI 2026-08-09) | `84:47:09:62:ff:cd` |
| pve5 | 192.168.1.204 | Fixed Allocation, set during 2026-08-06 recovery, confirmed live on the gateway UI 2026-08-09 | `bc:fc:e7:ea:23:de` |

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

**Correction (2026-08-09): all five hosts already have gateway Fixed
Allocation reservations, not just pve5.** This document originally claimed
pve1-pve4 held their addresses at the gateway's discretion with no
reservation — checked live against the gateway's IP Allocation page and that
was stale; all five show `Fixed Allocation`. The Decision section below (its
core recommendation — reservations over host-static config) was already
carried out at some point after this document was written; it just never got
updated here. Nothing left to do on this front.

**Nothing is still live here as of 2026-09-18.** Two `.169` leftovers used to
be: a gateway DNS answer that returned `.169` alongside `.204` for pve5's name,
and pve1/pve2's cluster filesystem caching `.169` as pve5's address, which broke
inter-node API proxying. The pmxcfs one was fixed at source on 2026-08-09 — the
cause was pve5's own `/etc/hosts` self-entry, not a cached value on the readers
(`scenarios/pve-stale-node-ip-corosync.md`, itself marked resolved). The DNS one
aged out of the gateway on its own. Both re-verified live; see "pve5's name was
broken; it is not any more" below for the evidence.

The pool is observed to span roughly `.64`-`.253` — the BGW320-500 default, and
consistent with every address the reconciler has ever seen a VM receive. The
existing vLLM inference VM sits deliberately at `192.168.1.50`, below that
floor, and is the one host on this network whose address is genuinely
unreachable by DHCP.

## Reaching a host by name

Every host already answers to a name on the LAN, and **a name is the better
address to use** — not merely a convenience. This is easy to miss because
nothing in this repository references it. Which name depends on what you need:
`pve<n>.tail5bbd6f.ts.net` is the only one that resolves from anywhere on the
tailnet *and* matches the certificate served on `:8006`; the `attlocal.net`
names below are LAN-only and mismatch it.

The gateway at `192.168.1.254` publishes forward and reverse records for its
DHCP clients under `attlocal.net`. Verified against that resolver directly:

| Name | Resolves to | Reverse |
| --- | --- | --- |
| `pve1.attlocal.net` | 192.168.1.200 | `pve1.attlocal.net` |
| `pve2.attlocal.net` | 192.168.1.201 | `pve2.attlocal.net` |
| `pve3.attlocal.net` | 192.168.1.202 | `pve3.attlocal.net` |
| `pve4.attlocal.net` | 192.168.1.203 | `pve4.attlocal.net` |
| `pve5.attlocal.net` | 192.168.1.204 | `pve5.attlocal.net` |

Re-verified 2026-09-18: one address per name, including pve5 (see "pve5's name
was broken" below for what changed).

The records only exist on that resolver. A client pointed at a public resolver
gets `NXDOMAIN`, so this is a LAN-only path — which is the correct scope for a
hypervisor management interface, and the reason no public record should be
created for one.

### The resolver caveat: `dig @192.168.1.254` is not the same as your system resolver

"Verified against that resolver directly" is load-bearing. A client only gets
these records if it actually queries `192.168.1.254` for them, and not every LAN
client does.

The devbox is the worked example: `dig pve1.attlocal.net @192.168.1.254` answers,
while `curl https://pve1.attlocal.net:8006/` fails outright with
`curl: (6) Could not resolve host` — its configured resolvers do not include the
gateway for this zone. So on the devbox the `attlocal.net` names are not usable
at all, and the choices are the tailnet name (which MagicDNS resolves, and which
is also the only one that matches the served certificate) or the raw address.

Check before relying on a name from any given machine:

```bash
getent hosts pve1.attlocal.net || echo "this machine cannot resolve it"
```

This is a different mechanism from how `*.jdwlabs.com` names resolve on the LAN,
which the same gateway cannot help with at all — see
[lan-name-resolution.md](lan-name-resolution.md).

### Only the tailnet name matches the certificate

Since 2026-09-01 every host serves one publicly trusted Let's Encrypt
certificate on `:8006`, issued for its tailnet name. `pveproxy` serves a single
certificate for every name a client might reach it by, so this is what the LAN
address and the `attlocal.net` name get too — there is no per-name selection.
Verified live 2026-09-18 on all five: subject `CN=pve<n>.tail5bbd6f.ts.net`,
sole SAN `DNS:pve<n>.tail5bbd6f.ts.net`, issuer `O=Let's Encrypt`, expiring
2026-11-30.

What that means per URL:

| URL | Browser result |
| --- | --- |
| `https://pve<n>.tail5bbd6f.ts.net:8006` | clean — publicly trusted, name matches |
| `https://pve<n>.attlocal.net:8006` | hostname mismatch |
| `https://192.168.1.20x:8006` | hostname mismatch |

The mismatch is not fixable by importing anything: the issuer is already
publicly trusted, so the Proxmox cluster CA is no longer part of the browser
path at all. That removes the "import the cluster CA once" advice this section
used to give — and that fallback never worked for pve1-pve4 anyway, see
[proxmox-tls-certificates.md](proxmox-tls-certificates.md) (also the source for
what is live and why, with [scenarios/proxmox-tailscale-tls.md](../scenarios/proxmox-tailscale-tls.md)
for steps and rollback).

**Superseded:** this section previously stated that the `attlocal.net` name
produced a hostname *match* against a `CN=pve<n>.attlocal.net` cluster-CA
certificate served on `:8006`. True until the tailnet certificate landed; false
since.

The cluster-CA node certificates still exist at `/etc/pve/nodes/<n>/pve-ssl.pem`
and are still what inter-node proxying uses. They carry stale embedded addresses
— `.233`, `.222`, `.221`, `.223` for pve1-pve4 and `.169` for pve5 — which is
evidence in its own right that **all five hosts have moved at least once under
DHCP**; the risk described above is not hypothetical for any of them. They are
simply no longer what a browser sees.

### pve5's name was broken; it is not any more

**Resolved as of 2026-09-18.** `pve5.attlocal.net` now returns exactly one
address, `192.168.1.204`, and so does every other host's name. The dead
`192.168.1.169` answer the gateway used to serve alongside it is gone — nobody
released it by hand (it was never visible in the gateway UI to release), so it
aged out of the gateway's DNS layer on its own.

The deeper pmxcfs-level staleness this was tangled up with is also gone, fixed
at its real source on 2026-08-09 (`scenarios/pve-stale-node-ip-corosync.md` —
pve5's own `/etc/hosts` self-entry). Confirmed live 2026-09-18 from pve1:
`/etc/pve/.members` and `corosync.conf` both record pve5 at `.204`, the cluster
is quorate 5/5, and `pvesh get /nodes/pve5/status` proxies through successfully.

So the advice is now uniform: prefer the name over the address for all five
hosts, subject to the resolver caveat above.

The `/etc/hosts` pins on pve1-pve4 (`192.168.1.204 pve5.attlocal.net pve5`) are
redundant now that gateway DNS answers correctly, and still accurate. Left in
place deliberately — removing them is a change on four hypervisors that buys
nothing, and they are a cheap hedge if the stale record ever returns. Revisit
only if pve5's address changes, when they become the wrong answer instead of a
duplicate of the right one.

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

**(Done — see "The human step" below.) Create DHCP reservations at the
gateway for pve1-pve4, keyed by MAC, pinning each to the address it holds
today. pve5 already has one, at `.204`, put in
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

**Done as of 2026-08-09** — all five hosts confirmed with Fixed Allocation on
the gateway's IP Allocation page (`192.168.1.254`). No further reservation
work needed here.

The stale `192.168.1.169` DNS answer that used to accompany pve5's name is
**gone as of 2026-09-18** — nothing was done at the gateway to remove it (it
appeared on neither the IP Allocation page nor the Device List / LAN Host
Discovery page, so there was never a UI-exposed entry to release); it aged out
of the gateway's DNS layer on its own. The `/etc/hosts` pins on pve1-pve4 that
worked around it are still in place and now redundant. The more serious
pmxcfs-level staleness it was confused with had a different cause entirely and
was fixed at source on 2026-08-09 — `scenarios/pve-stale-node-ip-corosync.md`.

Verify afterwards, from any host that can reach the LAN:

```bash
for h in pve1:192.168.1.200 pve2:192.168.1.201 pve3:192.168.1.202 \
         pve4:192.168.1.203 pve5:192.168.1.204; do
  name=${h%%:*}; ip=${h##*:}
  echo | openssl s_client -connect "$ip":8006 2>/dev/null \
    | openssl x509 -noout -subject | grep -qE "CN ?= ?$name" \
    && echo "$name $ip OK" || echo "$name $ip MISMATCH"
done
```

Each host answering on `:8006` with its own name in the certificate subject is
the check that matters — it proves the address maps to the host the repo thinks
it does, which a ping cannot. The subject is now
`CN=pve<n>.tail5bbd6f.ts.net` rather than `CN=pve<n>.attlocal.net`, so the grep
still matches on the host-number prefix; it no longer says anything about which
*domain* the certificate covers.

The grep is `CN ?= ?$name` rather than `CN=$name` because OpenSSL ≥ 1.1.0 prints
the subject with spaces around the `=` (`subject=CN = pve1.tail5bbd6f.ts.net`),
so the tighter pattern matches nothing and reports `MISMATCH` for every host —
the exact false alarm this check exists to rule out. Reproduced on OpenSSL
3.0.13; the hypervisors run Debian 13's OpenSSL 3.5, so it fails there too.
`-nameopt compat` restores the old spacing if a stricter pattern is wanted.

Then confirm each name resolves to exactly one address, which is what proves the
stale `.169` record is gone:

```bash
for n in pve1 pve2 pve3 pve4 pve5; do
  printf '%-20s %s\n' "$n.attlocal.net" \
    "$(dig +short "$n.attlocal.net" @192.168.1.254 | tr '\n' ' ')"
done   # expect exactly one address per host
```

As of 2026-09-18 that loop returns exactly one address for every host.

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
