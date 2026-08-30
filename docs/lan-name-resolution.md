# Name Resolution on the LAN

Which names resolve where, which of those answers are load-bearing, and what it
would take to stop depending on a single workstation's `hosts` file.

Companion to [host-addressing.md](host-addressing.md), which covers the Proxmox
hypervisors' own addresses and names.

**Status: resolver live and boot-proven; distribution still on the gateway,
and the gateway almost certainly cannot do it.** A LAN resolver (`dnsmasq`)
ships as part of the HAProxy VM (`terraform/files/dnsmasq-jdwlabs-lan.conf`,
[scenarios/lan-dns-resolver-deploy.md](../scenarios/lan-dns-resolver-deploy.md))
and has been answering on `192.168.1.199:53` since 2026-08-13. As of
2026-08-30 the gateway still hands every DHCP client `192.168.1.254`, so no
client is using the resolver yet. What remains is one gateway login
([scenarios/lan-dns-gateway-check.sh](../scenarios/lan-dns-gateway-check.sh))
to confirm the finding below, and — because the expected answer is "no field"
— the per-client fallback in "Pointing clients at the resolver".

## Re-verified 2026-08-30 (agent, from the devbox — a DHCP client with no `hosts` entry)

```
$ ssh haproxy-admin@192.168.1.199 'uptime -s; systemctl show dnsmasq -p ActiveEnterTimestamp'
2026-08-13 06:08:53
ActiveEnterTimestamp=Thu 2026-08-13 06:09:00 UTC   # up 7 s after boot, unattended, 17 days ago

$ dig +short chaos txt version.bind @192.168.1.199
"dnsmasq-2.90"
$ for n in cluster grafana argocd randomtestname123; do dig +short $n.jdwlabs.com @192.168.1.199; done
192.168.1.199                                       # x4 — real wildcard, per the config
$ dig +short google.com @192.168.1.199
108.177.122.100
108.177.122.139                                     # forward-to-gateway path works

$ dig +short cluster.jdwlabs.com @192.168.1.254
jdwlabs.com.
104.53.12.62                                        # gateway: still the public answer, unchanged

$ resolvectl status eth0 | grep -E 'DNS Servers|Current'
Current DNS Server: 2600:1700:3b40:cd0::1
       DNS Servers: 192.168.1.254 2600:1700:3b40:cd0::1   # DHCP + IPv6 RA both hand out the gateway
$ getent hosts cluster.jdwlabs.com
104.53.12.62    jdwlabs.com cluster.jdwlabs.com     # so this client gets the WAN answer

$ curl -sk -o /dev/null -w '%{http_code} %{remote_ip}\n' --resolve cluster.jdwlabs.com:6443:192.168.1.199 https://cluster.jdwlabs.com:6443/version
401 192.168.1.199                                   # apiserver behind HAProxy healthy
$ kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}'
https://192.168.1.199:6443                          # devbox kubectl works only because it bypasses the name
```

The reboot test the deploy runbook asks for before Step 4 is satisfied by the
first two lines: the VM's only boot since install brought `dnsmasq` up on its
own. The last two lines are the ticket's problem restated from a second
machine: a healthy apiserver, a working resolver, and a client that reaches
neither by name because nothing has told it where the resolver is.

The `Current DNS Server` line is a new finding and it changes the runbook —
see "IPv6 hands out the gateway too" below.

## Resolution today

Verified 2026-08-04 against each resolver directly, **re-verified live
2026-08-13** from a LAN device (`192.168.1.56`) with no `jdwlabs.com` entries
of its own anywhere in its resolution path — every row below reproduced
identically, including the exact CNAME chain and the WAN address:

```
$ nslookup cluster.jdwlabs.com 192.168.1.254
cluster.jdwlabs.com  canonical name = jdwlabs.com.
jdwlabs.com          Address: 104.53.12.62

$ nslookup randomtestname123.jdwlabs.com 192.168.1.254   # wildcard proof
randomtestname123.jdwlabs.com  canonical name = jdwlabs.com.
jdwlabs.com                    Address: 104.53.12.62

$ curl -sk -m5 -o /dev/null -w "%{http_code} exit=%{exitcode}" https://cluster.jdwlabs.com:6443/version
000 exit=7                              # WAN forward deliberately closed — no path without an override

$ curl -sk -o /dev/null -w "%{http_code} remote=%{remote_ip}" https://192.168.1.199:6443/version
401 remote=192.168.1.199                # apiserver itself is healthy

$ curl -s -o /dev/null -w "%{http_code} remote=%{remote_ip}" https://alertmanager.jdwlabs.com/
200 remote=104.53.12.62                 # hairpin still covers service names with no override
```

Nothing here contradicts the 2026-08-04 findings; this is corroboration from
an independent vantage point, not a revision.

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

This lines up with how the BGW320 family is reported to behave more broadly:
its DNS settings pick which upstream resolver the gateway itself forwards to
(or pass through, in IP-passthrough mode), not per-host records, and the
gateway is reported to insert itself as a DNS proxy in front of whatever
resolver a client is handed — even one assigned by a second DHCP server on the
same LAN. That proxying behaviour is the risk to flag against option 2 below.

### Can it at least advertise a different DNS server over DHCP? (researched 2026-08-30)

Live gateway: `BGW320-500`, software `6.35.8` (`/cgi-bin/sysinfo.ha`, which
is readable without the access code; `dhcpserver.ha` is not, so the field
itself is still only confirmable by a human login).

AT&T publishes no documentation for this page. Every independent report
found says the same thing, across firmware generations and both BGW320
variants:

- A BGW320-505 owner, Feb 2025, on setting up Pi-hole: the gateway "doesn't
  allow me to change its DHCP config to substitute my pi-hole as the default
  DNS server"; the fix they landed on was disabling the gateway's DHCP and
  running Pi-hole's — https://discourse.pi-hole.net/t/setting-up-pi-hole-dhcp-server-with-at-t-residential-gateway-with-guest-wifi-enabled/76779
- Earlier AT&T gateways (Pace, 2020): "With AT&T, you cannot change the DNS
  or disable the DHCP" — the workaround is shrinking the gateway's DHCP pool
  to a single address, running a second DHCP server, and disabling IPv6 on
  the gateway because clients were otherwise still handed AT&T's IPv6
  resolver — https://discourse.pi-hole.net/t/pi-hole-with-at-t-router/26989
- Owners pairing a BGW320 with a second router (Asus, Deco, Orbi) all reach
  the same conclusion: custom DHCP-advertised DNS is set on the second
  router, never on the BGW320 — e.g.
  https://www.snbforums.com/threads/dns-assignments-at-t-bgw320-gateway-and-tplink-deco.94624/
- Separately, AT&T's account-level "DNS Error Assist" intercepts NXDOMAIN
  answers and redirects them to `104.239.207.44`; it is toggled on att.com,
  not the gateway, and is reported to behave inverted —
  https://gist.github.com/CollinChaffin/24f6c9652efb3d6d5ef2f5502720ef00.
  Irrelevant to `*.jdwlabs.com` (the wildcard means nothing under it is ever
  NXDOMAIN) but worth knowing when a "why did this random name resolve"
  question comes up.

So the expected result of
[lan-dns-gateway-check.sh](../scenarios/lan-dns-gateway-check.sh) Stage 2 is
**no field**. Run it anyway — it costs one login, and a firmware surprise in
the other direction is the cheapest possible outcome — but plan for the
per-client path below rather than for the DHCP path.

The "second DHCP server" workaround those reports use is deliberately **not**
adopted here. It would mean either racing the gateway for leases (the pool
can't be disabled, only shrunk), or making the HAProxy VM the LAN's DHCP
server as well as its DNS, API front door and ingress — every LAN device's
addressing would then depend on one VM. `dnsmasq-jdwlabs-lan.conf` says as
much in its last paragraph. Two admin machines needing a static resolver
setting is a far smaller cost than that.

### IPv6 hands out the gateway too

The devbox re-verification above shows the client's *active* resolver is
`2600:1700:3b40:cd0::1` — the gateway's IPv6 address, learned from router
advertisements (RDNSS), not from DHCPv4. `systemd-resolved` picks one server
per link and sticks with it until it fails; on this box it picked the IPv6
one. The same applies to Windows and macOS, which both honour RDNSS.

Consequence: even if the gateway *did* let DHCPv4 option 6 be changed, a
dual-stack client could keep asking the gateway over IPv6 and keep getting
the WAN answer. The wizard's Stage 3 check ("what DNS server did the client
report") must look at the whole server list, not just the first IPv4
address. And the per-client static configuration below has to replace the
link's *entire* DNS server list, not add `192.168.1.199` alongside the
advertised ones.

## Which resolver: the decision record

Three ways to host the override were weighed. Two have their own sections
above; this table is the side-by-side.

| | (a) `dnsmasq` on the HAProxy VM — **chosen, deployed** | (b) In-cluster CoreDNS via a LAN LoadBalancer | (c) Pi-hole / AdGuard Home VM |
| --- | --- | --- | --- |
| Dependency loop | None. The VM is *upstream* of the cluster; the name that reaches the cluster never depends on the cluster. | Yes — resolving `cluster.jdwlabs.com` would need the cluster, and the cluster's DNS VIP already has an observed cross-node timeout failure mode. | None. |
| Blast radius if it dies | Same as today: the VM is already the accepted SPOF for API and ingress. Only grows if DHCP hands it to every client, which the gateway can't do. | Every `jdwlabs.com` name on any CoreDNS/MetalLB incident. | A new VM to patch, back up and keep on; a new SPOF that didn't exist before. |
| GitOps-ability | Config is one file in this repo, baked into cloud-init, hand-deployed to the live VM by runbook. Drift undetected by `talops` (accepted for a 9-line file). | Best on paper (`hosts` plugin block in the `platform` repo), but needs MetalLB or a `hostNetwork` DaemonSet that doesn't exist yet. | Web-UI-driven state; would need Terraform + Ansible to be reproducible. Ad-blocking is scope creep for a one-domain override. |
| Needs the gateway's DHCP option 6 changed | Yes for zero-touch — and that's unavailable; falls back to per-client config. | Same. | Same — Pi-hole's usual answer is "become the DHCP server", rejected above. |
| New components | One package on an existing VM. | LoadBalancer allocator + Service + CoreDNS config in another repo. | A whole VM plus its application. |

(a) wins on every row that matters for a name whose purpose is reaching the
cluster. (b) is the one to revisit only if a LAN-facing load balancer lands
for other reasons *and* the override list grows beyond "everything under one
domain points at one host" — today it is one `address=` line. (c) buys
nothing here that (a) doesn't.

## Pointing clients at the resolver

The gateway path is [lan-dns-gateway-check.sh](../scenarios/lan-dns-gateway-check.sh):
log in at `http://192.168.1.254/cgi-bin/dhcpserver.ha`, look for a DNS field
scoped to the LAN subnet's DHCP options, set `192.168.1.199` primary and
`192.168.1.254` secondary, save, force a lease renewal, verify. If the field
isn't there — expected — do this instead on each machine that needs cluster
access (today: the original workstation and `jake-Inspiron-5406-2n1`).

Replace the link's DNS list, don't append to it (see the IPv6 note above).
Keep `192.168.1.254` as a second entry so a resolver outage degrades to
"cluster names give the WAN answer", not "no DNS at all".

**Windows (PowerShell, as Administrator)** — the adapter name comes from
`Get-NetAdapter`:

```powershell
Set-DnsClientServerAddress -InterfaceAlias "Ethernet" -ServerAddresses 192.168.1.199,192.168.1.254
Get-DnsClientServerAddress -InterfaceAlias "Ethernet"      # should list only those two
ipconfig /flushdns
```

Also set the IPv6 servers on the same adapter to the same two addresses'
IPv6 equivalents or, simpler, to none — `Set-DnsClientServerAddress` with
`-AddressFamily IPv6 -ResetServerAddresses` still leaves RDNSS in play, so
if `nslookup` below keeps returning `104.53.12.62`, disable IPv6 on the
adapter (`Disable-NetAdapterBinding -InterfaceAlias "Ethernet" -ComponentID ms_tcpip6`)
and re-test. Undo: `Set-DnsClientServerAddress -InterfaceAlias "Ethernet" -ResetServerAddresses`.

**Linux, `systemd-resolved` via netplan (Ubuntu server / the devbox)** —
persistent; a bare `resolvectl dns` call is lost on the next lease renewal:

```yaml
# /etc/netplan/99-lan-resolver.yaml
network:
  version: 2
  ethernets:
    eth0:
      dhcp4: true
      dhcp4-overrides: { use-dns: false }
      dhcp6: true
      dhcp6-overrides: { use-dns: false }
      accept-ra: true
      ra-overrides: { use-dns: false }
      nameservers:
        addresses: [192.168.1.199, 192.168.1.254]
```

```bash
sudo netplan apply
resolvectl status eth0 | grep -A1 'DNS Servers'    # expect only 192.168.1.199 192.168.1.254
resolvectl flush-caches
```

`ra-overrides` needs netplan 1.0+; on older releases drop that stanza and
verify the `DNS Servers` line has no IPv6 entry — if it still does, add
`ipv6-address-generation`/`accept-ra: false` for the link. Undo: delete the
file, `sudo netplan apply`.

**Linux, NetworkManager (desktop Ubuntu, Fedora)**:

```bash
nmcli con mod "<connection>" ipv4.dns "192.168.1.199 192.168.1.254" ipv4.ignore-auto-dns yes ipv6.ignore-auto-dns yes
nmcli con up "<connection>"
```

**macOS**: System Settings → Network → the active interface → Details… →
DNS → replace the list with `192.168.1.199` and `192.168.1.254`. Or
`sudo networksetup -setdnsservers Wi-Fi 192.168.1.199 192.168.1.254`
(`-setdnsservers Wi-Fi empty` to undo).

**Then, on that machine**, run the Definition-of-Done checks:

```bash
nslookup cluster.jdwlabs.com          # expect 192.168.1.199, server 192.168.1.199
kubectl get nodes                     # all 8 nodes — kubeconfig server must be https://cluster.jdwlabs.com:6443, not the IP
nslookup cluster.jdwlabs.com 8.8.8.8  # expect 104.53.12.62 — public unchanged
curl -o /dev/null -w "%{http_code} remote=%{remote_ip}\n" \
  --resolve alertmanager.jdwlabs.com:443:104.53.12.62 https://alertmanager.jdwlabs.com/   # 200 remote=104.53.12.62
```

Note the `kubectl` caveat: a kubeconfig whose `server:` is
`https://192.168.1.199:6443` (the devbox's is) passes `kubectl get nodes`
with no resolver at all and proves nothing. Point it at the name for the
check, or run the check on the machine whose kubeconfig already uses it.

Only after that passes on a machine that never had the `hosts` override:
clean up the original workstation per "Order of work" — delete the four
inert `*.jdwlabs.com` lines, the `cluster.jdwlabs.com` line, and the ~22
per-service `.199` lines (the wildcard override now covers every one of
them, and WAN hairpin already did) — then re-run the four checks there.

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

There is a second, sharper catch even if the DHCP option can be set: gateways
in this family are reported to proxy DNS themselves rather than pass queries
through to whatever resolver a client was handed, which would make the
LAN resolver unreachable from ordinary DHCP clients regardless of what the
DHCP option says. That is only checkable by testing end-to-end after standing
the resolver up — a client accepting the DHCP option is not proof it is
actually being queried. Confirm with `nslookup cluster.jdwlabs.com
<lan-resolver-ip>` on the LAN resolver directly first, then repeat with no
resolver argument on a client that only has the DHCP-assigned server, and
compare the two answers.

Distributed via DHCP, option 2 also becomes the resolver for every name for
every LAN client, not just `cluster.jdwlabs.com` — a materially bigger new
failure domain than "if it's down, `kubectl` was already down." If it goes
down, all LAN clients lose all DNS. Mitigate by handing out `192.168.1.254`
as the secondary DNS server in the same DHCP option (client failover is slow
and inconsistent, so this is a mitigation, not a fix), or by scoping the
resolver to admin machines only via static config instead of DHCP.

**3. Tailscale MagicDNS.** Once a subnet router is on the tailnet (see
[tailscale-subnet-router.md](tailscale-subnet-router.md)), a split-DNS entry in
the tailnet policy can serve `cluster.jdwlabs.com` to tailnet members. This is
the only option that also fixes the name **off**-LAN, which neither of the
others touch. It covers only devices on the tailnet, so it complements option 1
or 2 rather than replacing them.

A tailnet member sitting on the LAN then has two authorities for
`cluster.jdwlabs.com` (MagicDNS and whichever of option 1/2 is live) that can
disagree, and a device that roams off-LAN with a cached LAN answer can fail
silently until the cache clears. Separately, any client with DNS-over-HTTPS
enabled bypasses the LAN resolver entirely under option 2 — the same
gateway-proxying failure mode above, but client-side and just as invisible to
a single-machine checklist run.

**Decision (2026-08-13): option 2, triggered.** The condition
this document originally set for moving off option 1 — "reasonable while
there is exactly one admin" — is no longer true. The `6443` WAN forward
was deliberately closed, and a second admin machine (`jake-Inspiron-5406-2n1`) has
no `hosts` override and therefore no path to the cluster at all. Option 1's
"add a line per new admin machine" is no longer a maintenance cost worth
preferring over standing up the resolver — it's now the thing actively
blocking access.

The resolver itself is built: `dnsmasq` on the HAProxy VM, config in
`terraform/files/dnsmasq-jdwlabs-lan.conf`, wired into the VM's cloud-init for
any future rebuild and deployed to the live VM by
[scenarios/lan-dns-resolver-deploy.md](../scenarios/lan-dns-resolver-deploy.md).
It answers `jdwlabs.com` and every subdomain — a real wildcard, unlike the
inert `hosts`-file lines below — and forwards everything else to the gateway
unchanged.

What is **not** yet resolved is this document's own second catch: whether the
BGW320-500 can hand that resolver's address to clients via DHCP, and whether
it actually passes queries through rather than proxying them itself.
[scenarios/lan-dns-gateway-check.sh](../scenarios/lan-dns-gateway-check.sh) is
the human wizard that checks both, because both require clicking through the
gateway's access-code-gated admin UI — nothing an agent can do from here. Two
outcomes follow from that check, and the doc will be updated with whichever
one actually happened once it's run:

- **Confirmed working** — DHCP hands out `192.168.1.199`, the gateway passes
  queries through untouched. This is the zero-touch, travels-with-the-client
  outcome the ticket asked for, and option 2 is fully realized.
- **Field missing, or present but not honoured** (either of this document's
  two flagged catches) — the resolver still exists and is still useful, but
  only for clients pointed at it by static per-client configuration. That is
  a real improvement over the `hosts` file (one resolver setting instead of
  an unbounded, wildcard-incapable list of names, and it survives adding new
  service names with zero client-side changes) but it is not automatic
  distribution, and this document should not claim it is one if the wizard
  finds otherwise.

Option 3 (Tailscale MagicDNS) remains a good idea alongside the subnet
router, independent of which branch above applies — it's the only option
that also fixes the name **off**-LAN. It is unchanged by this decision and
not re-evaluated here.

## Order of work

The ordering is the part that can go wrong. Steps 3-4 below are now a runbook
rather than a description:
[scenarios/lan-dns-resolver-deploy.md](../scenarios/lan-dns-resolver-deploy.md).

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
covers them, and the wildcard override in
`terraform/files/dnsmasq-jdwlabs-lan.conf` now covers them too.

## Why not an in-cluster CoreDNS override

Kubernetes clusters run CoreDNS by default, and this one is no exception —
but it answers on the cluster's internal `ClusterIP` (`10.96.0.10`), reachable
only from inside the pod network. There is no MetalLB (or any other
LAN-facing load-balancer) in the `platform` repo to expose it to LAN clients;
the only search hit for "coredns" there is an unrelated troubleshooting
runbook describing that same internal VIP.

Exposing it would mean new infrastructure (a `LoadBalancer` service via a
LAN-facing allocator, or a `hostNetwork` DaemonSet) to reach parity with what
the HAProxy VM already does for free, and it would trade one SPOF for a worse
one: the LAN resolver would then depend on cluster health, which is exactly
backwards for a name whose entire purpose is reaching the cluster in the
first place, and would take every other `jdwlabs.com` name down with it on
any cluster-DNS incident (the `platform` repo's own runbooks already
catalogue cross-node CoreDNS-VIP timeouts as a real, observed failure mode on
this cluster). The HAProxy VM is already an accepted SPOF for the API and all
ingress; adding a five-line static config to it introduces no *new* failure
domain, which an in-cluster service would. No `platform` repo change is part
of this decision.

## Verification checklist

Run once whichever option is implemented and serving `cluster.jdwlabs.com` on
the LAN. Each check either runs against a real client or, for the two public/
WAN checks, queries an external resolver or the WAN address directly — a
resolver that answers when asked directly but
isn't actually in the client's path proves nothing here.

- [ ] `nslookup cluster.jdwlabs.com` **from a LAN client** (not the workstation
      that held the `hosts` override) returns `192.168.1.199`. This only
      passes once an actual resolver is serving the override — it will not
      pass under option 1, where the name is still only in one machine's
      `hosts` file. That is expected, not a failure of option 1; it is the
      line between "the override travels with the client" and "the override
      travels with the file."
- [ ] `kubectl get nodes` returns all 8 nodes, run from a **second** LAN
      machine that has never had a `cluster.jdwlabs.com` hosts entry. This is
      the check that actually matters: it proves a machine with no manual
      override reaches the apiserver, which is the whole point of the ticket.
- [ ] `nslookup cluster.jdwlabs.com 8.8.8.8` (or `dig @1.1.1.1
      cluster.jdwlabs.com`) returns `104.53.12.62`, proving public resolution
      is unchanged. `--resolve` below pins the address in curl's own cache and
      consults no resolver at all, so it cannot stand in for this check.
- [ ] `curl -o /dev/null -w "%{http_code} remote=%{remote_ip}" --resolve
      alertmanager.jdwlabs.com:443:104.53.12.62
      https://alertmanager.jdwlabs.com/` returns `200 remote=104.53.12.62`,
      proving the WAN path and ingress still serve the site. Use port `443`,
      not `6443` — the Kubernetes apiserver's WAN forward was deliberately
      removed (see above), so a `6443` check against the WAN address always
      returns `000` and can never pass.
- [ ] Re-run the `nslookup`/`kubectl get nodes` checks above from the original
      workstation **after** removing its `cluster.jdwlabs.com` hosts entry
      (Order of work step 4) — the pre-removal state on that machine cannot
      distinguish a working resolver from its own `hosts` file.

Do these from a machine that never held the override, in the order above, and
only after the resolver is live — re-running them from the original
workstation cannot distinguish a working resolver from that machine's own
`hosts` file, per the method note above.

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
