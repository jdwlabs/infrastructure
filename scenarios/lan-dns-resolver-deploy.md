# Runbook: Deploy the LAN DNS resolver to the live HAProxy VM

Human-executed. No `terraform apply` and no `kubectl apply`/`delete` are
involved, but this SSHes into the VM that fronts the cluster's API and all
HTTP(S) ingress and installs a new service on it — that crosses the same
"changes production" line the agent contract reserves for a human.

For the *why* — the gateway can't hold a per-name override, the options that
were weighed, and what's still open — see
[lan-name-resolution.md](../docs/lan-name-resolution.md). This runbook is the
*how* for the option that doc lands on: a small resolver on the HAProxy VM.

## Why this runbook exists alongside the cloud-init change

`terraform/templates/haproxy-cloud-init.yaml.tftpl` now installs and
configures the same resolver on **day 0** — but cloud-init only runs at first
boot. The HAProxy VM already live at `192.168.1.199` (`haproxy-1`, cut over
from the hand-built `haproxy-0` on 2026-08-09 — see the correction at the top
of [haproxy-vm-provisioning.md](../docs/haproxy-vm-provisioning.md)) has
already had its one first boot; editing the template does not reach it. This
runbook installs the exact same config on that VM by hand, so the fix lands
now instead of waiting for the next rebuild.

Both paths read from one file, `terraform/files/dnsmasq-jdwlabs-lan.conf` —
copy it verbatim in Step 2 rather than retyping it, so the live VM and the
cloud-init day-0 path can never drift from each other.

## Before you start

- SSH access to the HAProxy VM (`192.168.1.199`) as the `haproxy-admin` user
  (this VM disables root login; see the cloud-init template)
- Confirm you're targeting the live VM, not some other host:
  ```bash
  ssh haproxy-admin@192.168.1.199 'hostname; systemctl is-active haproxy; cloud-init status'
  ```
  Expect `haproxy-1`, `active`, `status: done`. Anything else means either the
  hand-built `haproxy-0` is still live (this runbook's cloud-init assumptions
  don't apply — treat this as `haproxy-0` and adjust the login user
  accordingly) or you're pointed at the wrong address entirely.
- Confirm nothing is already bound to port 53 on the VM's LAN address before
  assuming a clean install:
  ```bash
  ssh haproxy-admin@192.168.1.199 'sudo ss -tulpn | grep ":53 "'
  ```
  Expect only `systemd-resolved`'s own stub, bound to `127.0.0.53`/`127.0.0.54`
  — loopback only. `dnsmasq`'s `listen-address=192.168.1.199` in the config
  below doesn't collide with that; if something else already holds
  `192.168.1.199:53`, stop and investigate before installing.
- Keep the `cluster.jdwlabs.com` `hosts` entry on your own workstation in
  place through this whole runbook. Removing it is the **last** step, on a
  human's say-so, after independent re-verification — see "Order of work" in
  [lan-name-resolution.md](../docs/lan-name-resolution.md#order-of-work).

## Step 1 — Install dnsmasq

```bash
ssh haproxy-admin@192.168.1.199 'sudo apt-get update && sudo apt-get install -y dnsmasq'
```

The package ships with its own default config and starts on install — that's
expected and gets fully replaced in the next step, not merged with.

**Rollback:** `sudo apt-get remove --purge dnsmasq` on the VM. Nothing else on
the host depends on the package.

## Step 2 — Install the override config

Copy `terraform/files/dnsmasq-jdwlabs-lan.conf` from this repo to the VM,
verbatim — don't retype it, and don't skip re-reading it first in case it's
changed since this runbook was written:

```bash
scp terraform/files/dnsmasq-jdwlabs-lan.conf haproxy-admin@192.168.1.199:/tmp/dnsmasq.conf
ssh haproxy-admin@192.168.1.199 'sudo install -o root -g root -m 644 /tmp/dnsmasq.conf /etc/dnsmasq.conf && rm /tmp/dnsmasq.conf'
```

Validate before restarting anything:

```bash
ssh haproxy-admin@192.168.1.199 'sudo dnsmasq --test'
```

Expect `dnsmasq: syntax check OK`. Stop here if it doesn't — a config that
fails to parse is safer left un-applied than force-started.

```bash
ssh haproxy-admin@192.168.1.199 'sudo systemctl enable --now dnsmasq && sudo systemctl restart dnsmasq'
```

**Rollback:** `sudo systemctl disable --now dnsmasq` on the VM. Nothing on the
LAN is pointed at this resolver yet at this stage of the runbook (that's Step
4/the gateway wizard), so stopping it here is a no-op for every client except
whoever is mid-verification against it directly.

## Step 3 — Verify from the VM itself

```bash
ssh haproxy-admin@192.168.1.199 '
  dig @192.168.1.199 cluster.jdwlabs.com +short
  dig @192.168.1.199 jdwlabs.com +short
  dig @192.168.1.199 grafana.jdwlabs.com +short
  dig @192.168.1.199 pve1.attlocal.net +short
'
```

Expect the first three to return `192.168.1.199` (apex and every subdomain,
per the wildcard override) and the fourth to return `192.168.1.200` (proof
the forward-to-gateway path for everything else still works). A `SERVFAIL` or
empty answer on the fourth line means the `server=192.168.1.254` forwarder
isn't reachable from the VM — check the VM's own network path to the gateway,
not the resolver config.

This step proves the resolver itself is correct. It does **not** yet prove
any LAN client is using it — that's Step 5/6 below, and it's the part that
actually matters (a resolver that answers when asked directly but isn't in
any client's path proves nothing about the ticket this closes).

## Step 4 — Point clients at the resolver

This is the part the gateway may or may not let happen automatically. Run
[lan-dns-gateway-check.sh](lan-dns-gateway-check.sh) — it walks the BGW320-500
admin UI to check whether the DHCP subnet options can hand out a custom DNS
server, and if so, sets it to `192.168.1.199`.

If the wizard confirms the gateway **can** advertise a custom DNS server:
continue to Step 5 once a client has actually picked up the new lease (a
DHCP renewal, not just a config change — `ipconfig /renew` / `dhclient -r &&
dhclient` / a link down-up, depending on the client OS).

If the wizard finds **no** such field, or finds the gateway proxying DNS
itself regardless of what the DHCP option says (the wizard's own Stage 3
tests for this): stop here and record the finding in
[lan-name-resolution.md](../docs/lan-name-resolution.md) — the resolver this
runbook built is still useful (point individual admin machines at it by
static per-client resolver config), but it is not the zero-touch,
travels-with-the-client fix the ticket asked for. That's a materially
different outcome than "done" and the doc's recommendation section needs to
say so plainly, not bury it.

## Step 5 — Verify from a LAN client (Definition of Done)

Run every check from a machine that has never held the
`cluster.jdwlabs.com` `hosts` override — the workstation that holds it cannot
tell a working resolver apart from its own `hosts` file. See
[lan-name-resolution.md](../docs/lan-name-resolution.md#verification-checklist)
for the full checklist and why each command is shaped the way it is; the
short form:

```bash
nslookup cluster.jdwlabs.com                       # expect 192.168.1.199
kubectl get nodes                                   # expect all 8 nodes, from a 2nd LAN machine with no hosts entry
nslookup cluster.jdwlabs.com 8.8.8.8                # expect 104.53.12.62 — public resolution unaffected
curl -o /dev/null -w "%{http_code} remote=%{remote_ip}" \
  --resolve alertmanager.jdwlabs.com:443:104.53.12.62 \
  https://alertmanager.jdwlabs.com/                 # expect 200 remote=104.53.12.62 — WAN ingress unaffected
```

## Step 6 — Remove the workstation `hosts` entries (human, after re-verification)

Only after Step 5 passes from a second machine. On the original workstation:

1. Delete the `cluster.jdwlabs.com 192.168.1.199` line.
2. Delete the four inert `*.jdwlabs.com` lines (they never did anything —
   hosts files don't support wildcards — see
   [lan-name-resolution.md](../docs/lan-name-resolution.md#four-lines-that-do-nothing)).
3. Delete whichever of the ~22 remaining per-service `.199` lines you want;
   they're redundant with the wildcard override now serving the same names,
   and were already redundant with WAN hairpin before this runbook (removing
   any of them was never an outage — see the same doc).
4. Re-run `nslookup cluster.jdwlabs.com` and `kubectl get nodes` on **this**
   workstation, now that its own override is gone. This is the check that
   actually distinguishes "the resolver serves this machine" from "this
   machine's `hosts` file was doing the work all along."

## Known follow-ups

- This deploy is a manual SSH step outside `talops`' reconcile loop —
  `talops haproxy status` doesn't know dnsmasq exists and won't report drift
  on it. If `/etc/dnsmasq.conf` is hand-edited on the VM later, this runbook
  and the cloud-init template are the only two places that would need to be
  told, and neither would notice on its own. That's an acceptable gap for a
  five-line static config; it would not be for anything that changes as often
  as `haproxy.cfg` does.
- If the HAProxy VM is ever rebuilt (`scenarios/haproxy-vm-rebuild.md`), the
  replacement gets this resolver automatically from cloud-init — this runbook
  does not need to be re-run, only re-verified.
