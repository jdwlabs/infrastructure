# Trusted TLS on the Proxmox Web UI — Execution Checklist

Sequential, copy-pasteable steps to replace the self-signed certificate on all
five hypervisors with a Tailscale-issued Let's Encrypt certificate, and to put
its renewal on a timer.

**This has been executed on all five hosts, 2026-09-01.** Each serves a
Let's Encrypt certificate under its tailnet name, and the renewal timer is
enabled. Kept as the runbook for a rebuild or a sixth host; see the applied
state and its evidence in
[docs/proxmox-tls-certificates.md](../docs/proxmox-tls-certificates.md).

One thing the first run found that the steps below did not anticipate: the
hosts were joined by browser login, which cannot apply a tag, so all five are
untagged and their node keys would have expired — and an expired node key stops
renewal, after which the certificate lapses quietly. Key expiry was disabled on
all five the same day, so this is closed, but prefer an auth key carrying
`tag:pve` when repeating these steps: a tagged node never expires in the
first place, and the toggle has to be remembered per host.

For the *why* — why not Proxmox's own ACME client with the Porkbun plugin, why
a certificate cannot be issued on `haproxy-1` and copied over, why the CA-import
workaround does not work on this cluster, and what renewal actually costs — read
[docs/proxmox-tls-certificates.md](../docs/proxmox-tls-certificates.md) first.
This checklist follows that decision exactly and does not restate it.

## Read this before starting

**The admin URL changes.** `pveproxy` serves one certificate for every name a
client might use. After this, `https://pve<n>.tail5bbd6f.ts.net:8006` is
trusted; `https://pve<n>:8006`, `https://pve<n>.attlocal.net:8006` and
`https://192.168.1.20<x>:8006` will still warn — as a name mismatch rather than
an unknown issuer. No publicly trusted CA can sign those names, so this is true
of every available option, not a defect in this one. Browsing by the ts.net name
requires the admin device to be on the tailnet.

**Nothing is applied to the hypervisors by this repository.** The hosts are what
Terraform talks to, not what it provisions. Every step below is run by a human
over SSH, and the three files in `scenarios/files/` are the only record of what
ends up on the hosts.

**Rollback is available at every step** and is collected at the end. The change
is additive: removing the custom certificate returns `pveproxy` to exactly
today's behaviour.

## Before you start

- Root SSH to all five hypervisors (`192.168.1.200`–`192.168.1.204`)
- Owner/admin access to the Tailscale admin console for tailnet
  `tail5bbd6f.ts.net`
- An admin device that is a tailnet member and has **not** had any Proxmox CA
  imported into its trust store — that device is what Step 6 verifies from
- The subnet router (`haproxy-1` / `100.103.1.41`) is live; see
  [docs/tailscale-subnet-router.md](../docs/tailscale-subnet-router.md).
  It is not a prerequisite for issuance, but it is how the tailnet is reached

## Step 1 — Enable MagicDNS and HTTPS certificates for the tailnet

Admin console only. There is no API credential for the tailnet in this
repository, and no Terraform resource for the policy file — the same situation
the subnet router's route approval is in.

1. Admin console → **DNS**.
2. Enable **MagicDNS** if it is not already on. HTTPS certificates require it.
3. Under **HTTPS Certificates**, select **Enable HTTPS**.

Record the tailnet name shown on that page. This checklist assumes
`tail5bbd6f.ts.net`; if it differs, `TAILNET_DOMAIN` in
[`files/tailscale-pveproxy-cert.sh`](files/tailscale-pveproxy-cert.sh) must be
changed to match before Step 4.

> If HTTPS certificates cannot be enabled, stop here. Every remaining step
> depends on it and `tailscale cert` will fail on the first host.

**Rollback:** disabling HTTPS Certificates again stops new issuance. It does not
revoke certificates already issued, and does not affect MagicDNS.

## Step 2 — Add a hypervisor tag and mint a tagged auth key

Use a **tagged** auth key, not a personal login. Tailscale disables key expiry
for tagged devices; a device joined under a personal login has its node key
expire (180 days by default), at which point the host drops off the tailnet and
every subsequent renewal fails silently until someone notices an expired
certificate.

1. Admin console → **Access controls**, add the tag owner:

   ```jsonc
   "tagOwners": {
     "tag:pve": ["autogroup:admin"],
   },
   ```

2. Admin console → **Settings → Keys → Generate auth key**. Set **Reusable**
   (five hosts use it), a short expiry, and **Tags: `tag:pve`**.
3. Confirm the tailnet ACLs permit reaching `tcp:8006` on `tag:pve` from your
   admin devices. On a default allow-all policy this is already true; on a
   tightened one it is not.

The auth key is a credential. Do not paste it into this repository, a ticket, or
a commit — use it from the shell in Step 3 and let it expire.

**Rollback:** revoke the key in the console; delete the `tagOwners` entry.

## Step 3 — Install Tailscale on each hypervisor

Run on **each** of `192.168.1.200`–`192.168.1.204`. The hosts run Debian 13
(trixie) under `pve-manager/9.2.3`.

```bash
ssh root@192.168.1.200

curl -fsSL https://pkgs.tailscale.com/stable/debian/trixie.noarmor.gpg \
    -o /usr/share/keyrings/tailscale-archive-keyring.gpg
curl -fsSL https://pkgs.tailscale.com/stable/debian/trixie.tailscale-keyring.list \
    -o /etc/apt/sources.list.d/tailscale.list
apt-get update && apt-get install -y tailscale
```

Join. **Do not advertise routes** — `haproxy-1` already advertises
`192.168.1.0/24`, and five more advertisers of the same subnet changes which
node carries LAN traffic for the whole tailnet, which is not this change's
business. `--accept-dns=false` matches the subnet router and keeps Tailscale
from rewriting `/etc/resolv.conf` on a hypervisor:

```bash
tailscale up --authkey=tskey-auth-... --accept-dns=false --hostname="$(hostname -s)"
```

Confirm the node's own certificate domain — the script derives it as
`<short-hostname>.<tailnet>`, and a name collision at join time would have made
Tailscale append a numeric suffix:

```bash
tailscale status --json | tr -d ' ' | grep -A3 '"CertDomains"'
```

Expect `pve1.tail5bbd6f.ts.net` on pve1, and so on. If a name differs, pass the
real FQDN as the unit's argument rather than editing the script.

**Rollback:** `tailscale logout && tailscale down`, then
`apt-get remove --purge tailscale`. Nothing on the host has changed yet.

## Step 4 — Install the renewal script and timer on each hypervisor

Copy all three files **verbatim** from `scenarios/files/` — they are the source
of truth for what is on the hosts, and editing a copy in place is how the two
drift. From a checkout of this repository, for each host:

```bash
scp scenarios/files/tailscale-pveproxy-cert.sh \
    root@192.168.1.200:/usr/local/sbin/tailscale-pveproxy-cert
scp scenarios/files/tailscale-pveproxy-cert.service \
    scenarios/files/tailscale-pveproxy-cert.timer \
    root@192.168.1.200:/etc/systemd/system/

ssh root@192.168.1.200 'chmod 0755 /usr/local/sbin/tailscale-pveproxy-cert && systemctl daemon-reload'
```

Note the target filename has no `.sh` — the unit's `ExecStart` expects
`/usr/local/sbin/tailscale-pveproxy-cert`.

**Rollback:** delete the three files and `systemctl daemon-reload`.

## Step 5 — Issue and install the certificate, per host

Run the unit once by hand so any failure is visible immediately rather than
buried in a timer's journal:

```bash
ssh root@192.168.1.200
systemctl start tailscale-pveproxy-cert.service
systemctl status tailscale-pveproxy-cert.service --no-pager
journalctl -u tailscale-pveproxy-cert.service --no-pager -n 20
```

Expect `installed certificate for pve1.tail5bbd6f.ts.net; reloading pveproxy`
and a clean exit. Then arm the timer:

```bash
systemctl enable --now tailscale-pveproxy-cert.timer
systemctl list-timers tailscale-pveproxy-cert.timer --no-pager
```

Confirm the host is now serving the new certificate:

```bash
openssl s_client -connect 192.168.1.200:8006 \
    -servername pve1.tail5bbd6f.ts.net </dev/null 2>/dev/null \
  | openssl x509 -noout -subject -issuer -dates
```

The issuer must be Let's Encrypt, not `CN=pve1` and not
`O=PVE Cluster Manager CA`. Paste this block for all five hosts onto the ticket
— it is the Definition of Done's per-host evidence.

Repeat Steps 3–5 for `.201`, `.202`, `.203`, `.204`.

**Rollback (per host), returns to exactly today's behaviour:**

```bash
systemctl disable --now tailscale-pveproxy-cert.timer
rm -f /etc/pve/local/pveproxy-ssl.pem /etc/pve/local/pveproxy-ssl.key
systemctl reload pveproxy
```

`pveproxy` falls back to `pve-ssl.pem` when the custom pair is absent, so this
is a complete revert and needs no reissue.

## Step 6 — Verify from a device with a clean trust store

From the tailnet device identified in "Before you start", with no Proxmox CA
imported, browse each of:

```
https://pve1.tail5bbd6f.ts.net:8006
https://pve2.tail5bbd6f.ts.net:8006
https://pve3.tail5bbd6f.ts.net:8006
https://pve4.tail5bbd6f.ts.net:8006
https://pve5.tail5bbd6f.ts.net:8006
```

Each must load with no interstitial and a valid padlock. Attach a screenshot or
the browser's certificate detail for one host to the ticket.

If a name does not resolve, MagicDNS is not in use on that device — that is a
device setting, not a certificate problem.

## Step 7 — Prove the renewal path, on one host only

The timer will not renew anything for about 60 days, so waiting is not a
verification. Force one rotation instead by discarding tailscaled's cached
certificate and re-running the unit:

```bash
ssh root@192.168.1.202
sha256sum /etc/pve/local/pveproxy-ssl.pem
rm -rf /var/lib/tailscale/certs
systemctl start tailscale-pveproxy-cert.service
sha256sum /etc/pve/local/pveproxy-ssl.pem
```

The hash must change and the journal must show `reloading pveproxy`. Run the
unit once more immediately: it must log
`certificate for … unchanged; pveproxy left alone` and leave the hash alone,
which is what stops the daily timer from restarting `pveproxy` every day.

Do this on **one** host. Each forced rotation is a real Let's Encrypt issuance
against Tailscale's per-node request limits; there is no reason to spend five.

## Step 8 — Record the result

- Paste the five `openssl` blocks from Step 5 and the Step 7 rotation evidence
  onto the ticket.
- Update the **Status** line at the top of
  [docs/proxmox-tls-certificates.md](../docs/proxmox-tls-certificates.md) and
  its "Current state" table, and the "Nothing here has been executed" note at
  the top of this file.
- If the tailnet name or any node's assigned name differed from the assumptions
  here, correct them in both documents and in
  [`files/tailscale-pveproxy-cert.sh`](files/tailscale-pveproxy-cert.sh).

## Known gaps this checklist does not close

- Reaching a host by `pve<n>`, `pve<n>.attlocal.net` or its IP still warns. See
  the constraint section of the rationale document; no option available avoids
  this.
- pve1–pve4's `pve-ssl.pem` certificates do not chain to the cluster CA that is
  present today, so no importable CA file validates them. Installing a custom
  certificate makes that irrelevant for the web UI but not for inter-node
  proxying. Untouched here deliberately — reissuing node certificates is a live
  change to a five-node cluster and belongs in its own ticket.
- A rebuilt hypervisor serves the self-signed certificate again until Steps 3–5
  are re-run for it. Steps 1–2 are tailnet-wide and survive.
