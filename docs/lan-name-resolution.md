# Name Resolution on the LAN

Which names resolve where, which of those answers are load-bearing, and what it
would take to stop depending on a single workstation's `hosts` file.

Companion to [host-addressing.md](host-addressing.md), which covers the Proxmox
hypervisors' own addresses and names.

## Resolution today

Verified 2026-08-04 against each resolver directly.

| Name | Public resolver | Gateway resolver (`192.168.1.254`) | Workstation `hosts` |
| --- | --- | --- | --- |
| `jdwlabs.com` | `104.53.12.62` | `104.53.12.62` | — |
| `cluster.jdwlabs.com` | CNAME → apex → `104.53.12.62` | same | `192.168.1.199` |
| `<anything>.jdwlabs.com` | `104.53.12.62` (wildcard) | same | ~22 names pinned to `192.168.1.199` |
| `pve<n>.attlocal.net` | `NXDOMAIN` | the host's LAN address | — |

Two facts fall out of that table and they set the whole scope of the work.

**The gateway is not a split-horizon resolver.** It forwards `jdwlabs.com`
upstream unchanged and returns the same public answer a public resolver does.
The only `jdwlabs.com` override anywhere on this network is the `hosts` file on
one workstation.

**Public DNS answers for names that do not exist.** A wildcard record covers
`*.jdwlabs.com`, so an arbitrary label resolves to the WAN address. Any test that
merely checks a name resolves proves nothing here.

## Only one `hosts` entry is actually load-bearing

This is the correction that shrinks the job, and it is easy to get backwards.

The `hosts` file pins about two dozen `*.jdwlabs.com` service names to
`192.168.1.199`. Almost all of them are redundant. Public ingress still forwards
`80` and `443`, so a LAN client with no entry at all reaches those services by
hairpinning out to the WAN address and back:

```
$ curl -s -o /dev/null -w "%{http_code} remote=%{remote_ip}" https://alertmanager.jdwlabs.com/
200 remote=104.53.12.62          # no hosts entry for this name — it works anyway
```

The entries are a latency and hairpin-dependency optimisation for those names,
not an access requirement. Removing one is not an outage.

`cluster.jdwlabs.com` is the exception, because the API it fronts is on `6443`
and that forward was deliberately removed:

```
$ curl -k -o /dev/null -w "%{http_code} remote=%{remote_ip}" https://cluster.jdwlabs.com:6443/version
401 remote=192.168.1.199         # hosts entry honoured — healthy apiserver

$ curl -k --resolve cluster.jdwlabs.com:6443:104.53.12.62 https://cluster.jdwlabs.com:6443/version
000                              # what a machine without the entry gets
```

A LAN machine without that one line has no path to the apiserver at all. That
line is the single point of failure worth removing; the rest are cleanup.

Note the method: `nslookup` and `dig` query a resolver directly and bypass the
`hosts` file entirely, so they answer a different question and will disagree
with `curl` and `kubectl` on exactly this name. Verify with the client that does
the work, and read `%{remote_ip}`.

### Four lines that do nothing

The same file carries four literal `*.jdwlabs.com` entries. The `hosts` file
format has no wildcard support — each is parsed as a hostname containing an
asterisk, which nothing ever queries. They are inert.

They are worse than merely useless: they look like split-horizon coverage, and
anyone reading the file could reasonably conclude every `jdwlabs.com` name is
already handled locally. Delete them.

## The gateway cannot serve a name override

The router, DHCP server and DNS server on this network are one device — an AT&T
BGW320-500 at `192.168.1.254`, running a `lighttpd` admin interface.

Its complete settings sitemap was enumerated (36 pages, `/cgi-bin/sitemap.ha`).
The DNS-adjacent pages are `dhcpserver.ha` ("Subnets & DHCP") and `ipalloc.ha`
("IP Allocation" — MAC-to-address reservations). There is no local-DNS,
host-mapping, or custom-record page anywhere in the set.

So the obvious approach — one override on the device that already answers every
LAN query — is not available. The settings pages themselves are access-code
gated, so a human logging in should confirm the absence before committing to a
resolver; but the page inventory is the device's own, and nothing in it can hold
a record.

Recording this as a finding matters more than it looks. "Add an override on the
router" is the first thing anyone will try, and it is a dead end on this
hardware.

## Options, and the recommendation

Ordered by how much new surface each one introduces.

**1. Do nothing beyond deleting the inert lines.** Keep the single
`cluster.jdwlabs.com` entry, and add it to any machine that needs cluster
access. Honest, zero new components, and the cost is one line per new admin
machine. Reasonable while there is exactly one admin.

**2. A small resolver on the HAProxy VM** (`dnsmasq` or `unbound`, LAN-only,
forwarding everything else to `192.168.1.254`). The VM is already the front door
for both admin APIs and already a single point of failure for cluster access, so
this adds no *new* failure domain — if it is down, `kubectl` was already down.
Port `53` is currently closed there, so nothing is listening yet.

The catch is distribution: clients have to be pointed at it, and the BGW320-500
must be able to hand out a custom DNS server in its DHCP options for that to
happen automatically. That capability is unconfirmed and is the thing to check
during the same gateway login as the reservation work. If it cannot, every
client needs manual resolver configuration — which is the `hosts` file problem
wearing a different hat, and option 1 is then strictly better.

**3. Tailscale MagicDNS.** Once a subnet router is on the tailnet (see
[tailscale-subnet-router.md](tailscale-subnet-router.md)), a split-DNS entry in
the tailnet policy can serve `cluster.jdwlabs.com` to tailnet members. This is
the only option that also fixes the name **off**-LAN, which neither of the
others touch. It covers only devices on the tailnet, so it complements option 1
or 2 rather than replacing them.

**Recommended: option 1 now, option 3 alongside the subnet router, and option 2
only if the gateway turns out to be able to advertise a custom DNS server.**
Standing up a resolver that clients cannot be pointed at automatically buys
nothing over the `hosts` line it replaces.

## Order of work

The ordering is the part that can go wrong.

1. Delete the four inert wildcard lines. No verification needed — they were
   never doing anything.
2. Leave `cluster.jdwlabs.com` alone until a resolver actually serves it.
   Removing it first costs cluster access on the machine doing the work.
3. Stand up whichever resolver option is chosen, and prove it from a **second**
   LAN machine that has never had the entry — the workstation with the override
   cannot distinguish a working resolver from its own `hosts` file.
4. Only then remove the entry from the workstation, and re-verify **after**
   removal, not before.
5. Confirm public resolution is unchanged and ingress still works:
   `curl --resolve <name>:443:104.53.12.62 https://<name>/`.

The service-name entries can be dropped at any point, independently — hairpin
covers them.

## Reaching the hypervisors by name

Separate mechanism, already working, and not affected by any of the above: the
gateway publishes forward and reverse records for its DHCP clients under
`attlocal.net`, so `https://pve<n>.attlocal.net:8006` reaches each Proxmox host
and matches the certificate it serves, which the raw address does not.

One defect remains there — `pve5.attlocal.net` still returns two addresses, and
`192.168.1.204` is dead. Re-confirmed 2026-08-04; the resolver alternates
between the two, so a single query can look healthy. Details and the gateway
step that clears it are in [host-addressing.md](host-addressing.md).

No public record should be created for a hypervisor management interface. The
`attlocal.net` names are LAN-only, which is the correct scope, and `8006` is not
forwarded on the WAN.
