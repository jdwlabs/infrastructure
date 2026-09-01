# Proxmox Web UI TLS Certificates

Why every Proxmox host still throws a browser warning, which of the available
fixes was chosen, and which ones were rejected and why.

This is about the certificate `pveproxy` serves on `:8006` — the hypervisor
admin UI. It is not about the Kubernetes or Talos API certificates, which are
a separate trust chain handled by `talops`, nor about the public
`*.jdwlabs.com` certificate cert-manager issues in the cluster.

Companion to [host-addressing.md](host-addressing.md) (which hypervisor answers
on which address) and [tailscale-subnet-router.md](tailscale-subnet-router.md)
(the tailnet these certificates depend on). The execution steps live in
[scenarios/proxmox-tailscale-tls.md](../scenarios/proxmox-tailscale-tls.md).

**Status: applied and verified on all five hosts, 2026-09-01.** Every
hypervisor now serves a publicly trusted certificate on `:8006` under its
tailnet name. The "Current state" section below is retained as the before
picture; see "Applied state" for what is live.

## Applied state

All five hosts run Tailscale 1.102.3, are tailnet members under their short
hostname, and serve a Let's Encrypt certificate that verifies with no CA
import and no `-k`:

| Host | Tailnet address | Certificate subject | Issuer | Not after |
| --- | --- | --- | --- | --- |
| pve1 | 100.93.95.22 | `CN=pve1.tail5bbd6f.ts.net` | `O=Let's Encrypt, CN=YE2` | 2026-11-30 |
| pve2 | 100.96.84.99 | `CN=pve2.tail5bbd6f.ts.net` | `O=Let's Encrypt, CN=YE1` | 2026-11-30 |
| pve3 | 100.122.82.10 | `CN=pve3.tail5bbd6f.ts.net` | `O=Let's Encrypt, CN=YE2` | 2026-11-30 |
| pve4 | 100.69.71.70 | `CN=pve4.tail5bbd6f.ts.net` | `O=Let's Encrypt, CN=YE2` | 2026-11-30 |
| pve5 | 100.109.203.17 | `CN=pve5.tail5bbd6f.ts.net` | `O=Let's Encrypt, CN=YE2` | 2026-11-30 |

Verified from a client holding no extra trust anchors — `ssl_verify_result=0`
is the load-bearing field, since a `200` alone would also be returned with
`-k`:

```
$ for i in 1 2 3 4 5; do
    curl -sS -o /dev/null -w "pve$i http=%{http_code} tls_verify=%{ssl_verify_result}\n" \
      https://pve$i.tail5bbd6f.ts.net:8006/
  done
pve1 http=200 tls_verify=0
pve2 http=200 tls_verify=0
pve3 http=200 tls_verify=0
pve4 http=200 tls_verify=0
pve5 http=200 tls_verify=0
```

The LAN address path is untouched and still answers on all five
(`https://192.168.1.20x:8006/` → `200`), still with the cluster CA's
certificate. That is deliberate: the tailnet name is an addition, not a
replacement, so losing the tailnet costs the warning-free UI and nothing else.

Each host joined with `--accept-dns=false --accept-routes=false`, so no
hypervisor's name resolution or routing table changed. The subnet router on the
HAProxy VM continues to advertise `192.168.1.0/24` and no hypervisor accepts it,
which is what keeps a route already reachable over the LAN from being pulled
through a peer.

### Nodes are untagged, and node-key expiry is the consequence

The design below calls for a `tag:pve-host` on each hypervisor. The hosts were
joined interactively through a browser login instead, which cannot apply a tag,
so all five are untagged and owned by the joining user account.

That matters for exactly one reason: tagged nodes never key-expire, untagged
ones do on the tailnet's expiry schedule. An expired node key drops the host off
the tailnet, and once that happens `tailscale cert` cannot renew — the
certificate then lapses on its own 90-day clock some time later, and the browser
warning returns with a *different* cause than the one this work removed.

**Key expiry is disabled on all five, so nothing is on a clock.** Done the same
day via the Tailscale API, and confirmed from both the API
(`keyExpiryDisabled=true`) and the node side (`tailscale status --json` reports
`KeyExpiry` absent). The same was applied to `haproxy-1` and `devbox`: checking
the whole tailnet showed every node untagged and expiring, and `haproxy-1` is the
subnet router, so its key lapsing would have cost off-LAN `kubectl` and
`talosctl` — a worse outcome than a hypervisor browser warning. Personal devices
were left expiring deliberately; a portable device that never expires keeps
tailnet access after it is lost until someone revokes it by hand.

When auditing this later, read `keyExpiryDisabled`, not `expires`. The API keeps
returning a populated `expires` timestamp on nodes whose expiry is disabled —
it is retained metadata, not an enforced deadline, and reading it as one leads
to the wrong conclusion.

The tag itself is still unapplied, which is a weaker gap but a real one: a
rebuild or re-join brings a host back untagged *and* expiring, with nothing at
policy level to catch it. Closing that wants a `tagOwners` entry plus a tagged
auth key, and belongs to the next reprovision rather than a standalone re-auth
of hosts that are working.

## Current state before this work

Collected read-only from each host on 2026-08-18. All five run
`pve-manager/9.2.3`, none has a custom certificate installed, and none is a
tailnet member.

| Host | Certificate subject | Issuer | Not after | Custom cert | Tailscale |
| --- | --- | --- | --- | --- | --- |
| pve1 | `CN=pve1.attlocal.net` | `O=PVE Cluster Manager CA, OU=087f4dec-…` | 2028-01-03 | absent | not installed |
| pve2 | `CN=pve2.attlocal.net` | `O=PVE Cluster Manager CA, OU=087f4dec-…` | 2028-02-07 | absent | not installed |
| pve3 | `CN=pve3.attlocal.net` | `O=PVE Cluster Manager CA, OU=087f4dec-…` | 2028-02-07 | absent | not installed |
| pve4 | `CN=pve4.attlocal.net` | `O=PVE Cluster Manager CA, OU=087f4dec-…` | 2028-02-07 | absent | not installed |
| pve5 | `CN=pve5.attlocal.net` | `CN=pve1` | 2028-06-07 | absent | not installed |

Confirmed the same certificate is what actually reaches a client, not just
what sits on disk:

```
$ openssl s_client -connect 192.168.1.200:8006 </dev/null 2>/dev/null \
    | openssl x509 -noout -subject -issuer
subject=OU=PVE Cluster Node, O=Proxmox Virtual Environment, CN=pve1.attlocal.net
issuer=CN=Proxmox Virtual Environment, OU=087f4dec-0d3e-436c-9b50-4d83ded5bff4, O=PVE Cluster Manager CA
```

Neither issuer is in any operating system or browser trust store, which is the
whole of the reported problem. Nothing is expired and no name is mismatched.

### The documented workaround does not actually work

The standing advice for a Proxmox self-signed cluster CA is "import
`/etc/pve/pve-root-ca.pem` into the admin device's trust store once". On this
cluster that fixes exactly one host out of five:

```
$ openssl x509 -in /etc/pve/pve-root-ca.pem -noout -subject -enddate
subject=CN=pve1
notAfter=Feb 20 01:25:26 2036 GMT

$ for n in pve1 pve2 pve3 pve4 pve5; do
      printf '%s: ' "$n"
      openssl verify -CAfile /etc/pve/pve-root-ca.pem /etc/pve/nodes/$n/pve-ssl.pem
  done
pve1: error /etc/pve/nodes/pve1/pve-ssl.pem: verification failed
pve2: error /etc/pve/nodes/pve2/pve-ssl.pem: verification failed
pve3: error /etc/pve/nodes/pve3/pve-ssl.pem: verification failed
pve4: error /etc/pve/nodes/pve4/pve-ssl.pem: verification failed
pve5: /etc/pve/nodes/pve5/pve-ssl.pem: OK
```

The cluster CA was regenerated on 2026-02-21 (the mtime on both
`pve-root-ca.pem` and `pve-root-ca.key`). pve1–pve4's certificates predate
that and were signed by the CA it replaced; only pve5, whose certificate was
reissued during the 2026-08-06 recovery, chains to the CA that is present
today. The old CA is not on the cluster filesystem any more, so there is no
file an admin could import that would validate pve1–pve4.

`pveproxy.service` runs `ExecStartPre=-/usr/bin/pvecm updatecerts --silent` on
every start, which creates a missing node certificate but does not reissue one
that merely fails to chain — so this state persists across restarts rather
than self-healing.

This does not block the work below (a custom certificate takes precedence over
`pve-ssl.pem` for the web UI), and it is left untouched here because
regenerating node certificates is a live change to a five-node cluster. It is
recorded because it removes the "just import the CA" fallback from the table
of options, and because the same certificates are used for inter-node proxying
— which is worth its own investigation, separately from this change.

## The constraint everything else follows from

`pveproxy` serves **one** certificate for **all** names a client might use, and
it takes the certificate path once at startup —
`/usr/share/perl5/PVE/Service/pveproxy.pm` selects
`/etc/pve/local/pveproxy-ssl.pem` if it and its key exist, and there is no
watcher that would notice the file changing afterwards.

No publicly trusted certificate authority will ever sign `pve1`,
`pve1.attlocal.net`, or `192.168.1.200`: the first two are not names in a
domain anyone can demonstrate control of, and the third is a private address.
So **whichever mechanism is chosen, the admin URL has to change** to some new
fully-qualified name, and reaching a host by its short name, its
`attlocal.net` name, or its IP will still produce a browser warning — a name
mismatch instead of an unknown issuer.

That is not a shortcoming of the option chosen below. It is true of every
option, and it should be stated on the ticket rather than discovered during
verification.

## Decision

**Join each hypervisor to the tailnet and serve a Tailscale-issued Let's
Encrypt certificate for `pve<n>.tail5bbd6f.ts.net`, refreshed by a systemd
timer that reloads `pveproxy` when the material changes.**

The reasons it wins are narrower than "it was the ticket's idea":

- **It works from wherever an admin already is.** Every admin device is
  expected to be a tailnet member since the subnet router landed, and MagicDNS
  resolves `pve<n>.tail5bbd6f.ts.net` on-LAN and off-LAN identically with no
  DNS work on this network at all.
- **It does not put a `*.jdwlabs.com` key on a hypervisor.** See the rejected
  options below — this turns out to be the deciding security argument.
- **It publishes only non-routable names.** Tailscale certificates are real
  Let's Encrypt certificates and their names do land in Certificate
  Transparency logs, so this is not free; but `pve1.tail5bbd6f.ts.net` resolves
  to nothing outside the tailnet, whereas a `jdwlabs.com` name resolves to the
  WAN address today via the public wildcard.

What it costs, stated plainly:

- Five new tailnet nodes whose only purpose is certificate issuance. Reaching
  the hypervisors was already solved by the subnet router; this adds nodes for
  trust, not for connectivity.
- Renewal is ours to run. It is not automatic — see below.
- A hypervisor's web UI now depends on the tailnet being reachable for the
  certificate to keep renewing. The UI itself keeps working on the LAN either
  way, and an expired certificate degrades to today's warning rather than to
  an outage.

### Why a certificate issued elsewhere cannot be copied onto these hosts

The obvious shortcut — issue on `haproxy-1`, which is already a tailnet node,
and distribute — does not work, for a reason that has nothing to do with file
copying.

A Tailscale certificate is issued for a MagicDNS name, and a MagicDNS name
exists only because a node owns it. If pve1 is not a tailnet node then
`pve1.tail5bbd6f.ts.net` is not a name in the tailnet, and no node can obtain a
certificate for it — `haproxy-1` included. And if such a certificate somehow
existed, MagicDNS would still resolve the name to whichever node owns it, so a
browser would be sent to `haproxy-1`, not to pve1. `haproxy-1` can only obtain
a certificate for `haproxy-1.tail5bbd6f.ts.net`, which is the wrong name to
serve from pve1.

Reverse-proxying all five hypervisor UIs behind `haproxy-1`'s own name was
considered as the way around that and rejected: it makes the VM that already
fronts both administrative APIs a single point of failure for hypervisor
access too, which is precisely the access path needed when that VM is the
thing that is broken.

### Rejected: Proxmox's own ACME client with the Porkbun DNS-01 plugin

This deserves more than a line, because on renewal alone it is the better
mechanism and it was close.

PVE 9.2.3 ships a complete ACME client and — checked on the host —
`/usr/share/proxmox-acme/dnsapi/dns_porkbun.sh` is present, which is the exact
DNS provider cert-manager already uses for `*.jdwlabs.com`. Both endpoints are
reachable from the hypervisors (`acme-v02.api.letsencrypt.org` and
`api.porkbun.com` each returned `200`). The plugin credential would live in
`/etc/pve/priv/acme/`, which is on the cluster filesystem and therefore
configured once for all five nodes. Renewal would be entirely native:
`pve-daily-update.timer` runs `pveupdate`, which calls
`PVE::API2::ACME->renew_certificate` inside 30 days of expiry and restarts
`pveproxy` itself. Zero custom code, nothing to rot.

It was rejected on three grounds, in order of weight:

1. **Reachability is worse, not better.** `pve<n>.jdwlabs.com` is currently
   answered by the public wildcard (`104.53.12.62`) for every client on this
   LAN, and by the dnsmasq resolver on `192.168.1.199` — which points the
   *whole* `jdwlabs.com` domain at itself — for any client configured to use
   it. Making the name reach a hypervisor needs explicit per-host exception
   lines in `terraform/files/dnsmasq-jdwlabs-lan.conf` **and** that resolver
   actually being used, which is still open (`lan-name-resolution.md` records
   the distribution question as unresolved; this devbox and the hypervisors
   themselves both resolve via the gateway). Off-LAN it would not work at all
   until tailnet split-DNS exists. Tailscale's MagicDNS needs none of that.
2. **A wildcard certificate on a hypervisor is a real security regression.**
   Requesting per-host `pve<n>.jdwlabs.com` names publishes them to
   Certificate Transparency attached to a domain whose wildcard resolves to
   the live WAN address — the exposure class the ticket set out to avoid. The
   obvious dodge is to request `*.jdwlabs.com` instead, which adds nothing to
   CT. But that puts a key valid for `argocd.jdwlabs.com`,
   `dashboard.jdwlabs.com` and every other service name onto all five
   hypervisors. A hypervisor compromise would then also be an impersonation
   capability for the entire public estate. That is a strictly worse trade
   than the problem being solved.
3. **Five identical wildcard orders sit exactly on a rate limit.** Let's
   Encrypt allows five certificates per week for an identical set of names.
   Five nodes issued on the same day renew in the same week forever, with no
   headroom for a single retry. Designing onto the exact ceiling is a fault
   waiting for a bad week.

If the LAN resolver question is ever settled and split-DNS lands on the
tailnet, point 1 weakens and this is worth revisiting for per-host names —
but point 2 rules out the wildcard variant permanently.

### Rejected: fronting the hypervisor UIs through nginx-gateway-fabric

Already rejected on the ticket, and the reasoning holds: it makes a privileged
hypervisor admin surface publicly discoverable, which is the exposure the API
lockdown closed. Recorded here so the option is not re-proposed.

## Renewal

The ticket assumed Tailscale certificates auto-renew. **They do not**, in the
form used here. Tailscale's documentation is explicit: when `tailscale cert`
writes files with `--cert-file`/`--key-file`, "the `tailscaled` daemon doesn't
know where to place a renewed certificate", so renewal is the operator's
responsibility. The certificates are 90-day Let's Encrypt certificates.

Left alone, the web UI would therefore be trusted for one quarter and then
show an expired-certificate warning — a worse outcome than today, because it
would arrive silently months after anyone remembered doing this work.

So renewal is a first-class part of the change, not a footnote:

- `scenarios/files/tailscale-pveproxy-cert.sh` re-requests the certificate
  with `--min-validity=720h`. Tailscale reuses its cached certificate while
  more than 30 days remain, so the call is cheap on most days and renews on
  its own schedule inside the last 30.
- It writes into `/etc/pve/local/` only when the material actually changed,
  then `systemctl reload pveproxy` — required, because `pveproxy` reads the
  certificate path once at startup and has no file watcher. The ticket's claim
  that Proxmox auto-reloads on cert file change is not borne out by the
  service code.
- `tailscale-pveproxy-cert.timer` runs it daily with `Persistent=true` (a host
  powered off through its window catches up) and four hours of jitter (five
  hosts should not arrive as a burst).
- Roughly 30 daily attempts fall inside the renewal window before expiry, so
  a multi-day tailnet or Let's Encrypt outage cannot lapse the certificate.

The failure mode to watch is a **node key expiry**, not a certificate expiry:
if a hypervisor's tailnet key expires the node drops off the tailnet and every
renewal fails from then on. Joining the hosts with an ACL-tagged auth key
avoids it — tagged devices have key expiry disabled — which is why the runbook
uses a tag rather than a personal login.

## What a rebuild has to repeat

A reinstalled or replaced hypervisor serves the built-in self-signed
certificate again. None of this survives, and none of it is reachable from
Terraform: the hypervisors are what Terraform talks *to*, not what it
provisions, so there is no resource that could carry this.

The rebuild sequence is the runbook's steps 2 onwards for that host alone:

1. Install Tailscale and join with the tagged auth key.
2. Install the three files from `scenarios/files/` and enable the timer.
3. Run the unit once to issue and install the certificate.

Steps 1 (enabling HTTPS certificates for the tailnet) and the `tagOwners`
policy entry are tailnet-wide and survive a host rebuild.

Two tailnet-side settings live in Tailscale's control plane, which this
repository has no resource for and no API credential to reach — the same
situation the subnet router's route approval is in. They are recorded in the
runbook because the runbook is their only record:

- HTTPS Certificates enabled for the tailnet (requires MagicDNS enabled).
- A `tagOwners` entry for the hypervisor tag in the tailnet policy file.

## Open questions the applied run answered

Each of these was recorded as unknown before the rollout, and is settled now:

- **MagicDNS and HTTPS Certificates are enabled** for the tailnet. Confirmed
  per host rather than assumed — `tailscale status --json` reports a populated
  `CertDomains` (`["pve1.tail5bbd6f.ts.net"]`), which is only present once the
  feature is on.
- **The tailnet ACLs permit `:8006`** from an admin device to every hypervisor;
  all five answered over the tailnet name.
- **The renewal script and units have run for real.** Each host issued its
  certificate, installed it, and reloaded `pveproxy` on the first invocation. A
  second manual invocation on pve1 and pve5 printed `certificate for
  <fqdn> unchanged; pveproxy left alone`, confirming the no-op path taken on
  the ~89 days out of 90 when nothing needs renewing.
- **The timer is enabled and scheduled with jitter as designed.** pve1 next
  fires 2026-09-02 00:17 CDT, pve5 2026-09-02 02:54 CDT — the
  `RandomizedDelaySec=4h` spread is doing its job, and `systemctl is-enabled`
  returns `enabled` on both, so the schedule survives a reboot.

## Verification still outstanding

- The definition of done asks for a browser check from a device holding no
  manual CA import. `curl` proving `ssl_verify_result=0` against the system
  trust store is strong evidence and is what the table above records, but it is
  not literally a browser, and no check has been run from a device off the LAN.
