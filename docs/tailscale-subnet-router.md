# Off-LAN Cluster Administration via a Tailscale Subnet Router

How to restore `kubectl` and `talosctl` access from outside the LAN without
re-opening a single WAN port.

Nothing here is applied automatically. Every step is manual and lists its own
rollback. The whole procedure is additive — it does not touch the API path, the
router, or any Talos machine config, so no step in it can produce a lockout.

For the sequential checklist a human runs to execute this — exact commands
in order, the four Definition-of-Done verification checks, and where to
paste each one's output — see
[tailscale-subnet-router.md](../scenarios/tailscale-subnet-router.md) (in
`scenarios/`, alongside this repo's other execution runbooks).
This doc stays the reference for rationale and rollback detail; that one is
the execution copy.

## Why a subnet router, and why on the HAProxy VM

The public forwards for `6443` and `50000` were removed deliberately. Re-adding
one to regain off-site access would undo that decision; a subnet router restores
the capability without advertising anything to the internet.

The HAProxy VM at `192.168.1.199` is the right host for it because it is already
the single front door for both administrative APIs — `6443` and `50000` both
terminate there — so one subnet router covers both, and reaching the VM is
sufficient to reach everything an admin needs.

It is deliberately **not** installed on the Talos nodes. Talos is an immutable,
API-driven OS with no package manager, so putting it there would mean a machine
config change on the control plane, which is the one change class that can take
`talosctl` and `kubectl` out together and leave no network path back in.

## Current state

**The subnet router is live as of 2026-08-13.** Steps 1 and 2 below have been
executed; they are kept as the reference for a rebuild, not as pending work.

Confirmed on the VM itself with `tailscale status --json` on 2026-08-13:

- Tailscale is installed on `192.168.1.199` and joined the tailnet as
  `haproxy-1` / `100.103.1.41`
- `192.168.1.0/24` is advertised **and approved** — the subnet appears in both
  `Self.AllowedIPs` and `Self.PrimaryRoutes`, which is what distinguishes an
  approved route from a merely advertised one
- `CorpDNS: false`, i.e. the `--accept-dns=false` this runbook requires held

The lockdown still holds alongside it. Re-swept from the LAN on 2026-08-17:
`6443`, `50000`, `22` and `9000` on the WAN IP all refuse, `443` answers. No WAN
port was opened to build this path.

Re-confirmed 2026-08-30, this time from the devbox as a tailnet peer rather
than from the VM alone:

- `tailscale status --json` on the devbox lists `haproxy-1` online with
  `PrimaryRoutes: ["192.168.1.0/24"]`, and `tailscale ping haproxy-1` answers
  direct (`via 192.168.1.199:41641`, no DERP relay)
- on the VM, `Self.AllowedIPs` still carries `192.168.1.0/24`, `Health` is
  empty, `net.ipv4.ip_forward=1` and `net.ipv6.conf.all.forwarding=1` are both
  persisted in `/etc/sysctl.d/99-tailscale.conf`
- the VM runs the `tailscale` package from the official apt repo
  (`https://pkgs.tailscale.com/stable/ubuntu noble main`, installed `1.102.2`,
  `1.102.3` available). There is no apt pin: the repo tracks the stable
  channel and Tailscale's own auto-update is on (`AutoUpdate.Apply: true`, the
  tailnet default), so the VM upgrades itself and a rebuild should expect
  whatever stable currently is, not `1.102.x`
- hairpin sweep of the WAN IP: `6443`, `50000`, `22` and `9000` all
  `Connection refused`; `80` answers. `443` timed out from both the devbox and
  the VM even though `https://192.168.1.199` answers LAN-direct — that is an
  inbound-NAT symptom on the router, unrelated to this path, and is tracked
  separately

**Still unproven: off-LAN access has not been demonstrated.** Step 3 needs a
tailnet device on a genuinely different network. The devbox is on the tailnet
but also on the LAN, so it reaches `192.168.1.0/24` directly and never sends
a packet through the router — its checks above confirm route state, not
forwarding. Until an off-LAN peer captures that evidence the honest statement
is *route live and approved; off-LAN access not yet demonstrated from a peer* —
not that the capability is proven end-to-end.

The LAN is `192.168.1.0/24` with its gateway, DHCP server and DNS server all at
`192.168.1.254`.

Note for anyone re-checking this: the devbox's key is authorized on the VM as
`haproxy-admin` (`ssh haproxy-admin@192.168.1.199`, passwordless `sudo`), and
the devbox is a tailnet member (`devbox.tail5bbd6f.ts.net`), so both vantage
points are available from an agent session. Earlier notes on this runbook
said neither was, which was true when written and is not any more.

## Prerequisites

- SSH access to the HAProxy VM from the LAN as the admin user
- Owner or admin access to the Tailscale admin console, to approve the route
- A second device already on the tailnet and on a **different** network — a
  phone hotspot is enough. Have it ready before starting, because it is the only
  thing that can prove the result

## Step 1 — install and advertise (done 2026-08-13, on the HAProxy VM)

```bash
ssh <admin-user>@192.168.1.199
curl -fsSL https://tailscale.com/install.sh | sh
sudo tailscale up --advertise-routes=192.168.1.0/24 --accept-dns=false
```

`--accept-dns=false` is load-bearing. The VM resolves the LAN through the
gateway at `192.168.1.254`; letting Tailscale take over `/etc/resolv.conf` would
change name resolution on the host that fronts both admin APIs, which is a
larger blast radius than this change is meant to have.

Advertising the route does not activate it. Until the next step it is a pending
request and nothing routes.

**IP forwarding is a second prerequisite this step does not install.** On the
run that stood this up, `tailscale status` reported "Subnet routing is enabled,
but IP forwarding is disabled" — the VM shipped with `net.ipv4.ip_forward=0` and
`net.ipv6.conf.all.forwarding=0`. A subnet router in that state accepts route
approval and still forwards nothing, so the failure looks like a routing or
approval problem rather than a host sysctl. It was fixed persistently on
`192.168.1.199` via `/etc/sysctl.d/99-tailscale.conf`; a rebuild has to repeat
that, because the cloud-init template does not set it either.

**Rollback:** `sudo tailscale down`.

## Step 2 — approve the route (done 2026-08-13, in the admin console)

In the Tailscale admin console, open the machine's row and enable the pending
`192.168.1.0/24` subnet route.

This was approved for `haproxy-1` on 2026-08-13. Whether the `autoApprovers`
block below was also added is not recorded anywhere outside the tailnet policy
file — check the console before assuming a rebuild will re-approve itself.

Approval is a per-machine, per-route consent step and is deliberately manual —
a node advertising a route it should not carry is a routing hijack, so the
console does not auto-approve.

To make this survive a reinstall or a key rotation, add an auto-approver to the
tailnet policy file instead of clicking through each time:

```jsonc
"autoApprovers": {
  "routes": {
    "192.168.1.0/24": ["tag:lan-router"],
  },
},
```

and bring the node up as `sudo tailscale up --advertise-routes=192.168.1.0/24 --advertise-tags=tag:lan-router`.
The policy file lives in the Tailscale control plane, not in this repository —
it has no Terraform resource here, so it is edited in the console and this note
is the only record that it exists.

**Rollback:** disable the route in the console. Takes effect within seconds and
leaves the machine on the tailnet.

## Step 3 — verify from off-LAN (outstanding)

**This is the step that is still open.** The route is approved, but nothing has
yet sent traffic over it from another network, so "off-LAN admin access works"
remains an inference from the route state rather than an observation. Both
tailnet peers have been offline, and neither the devbox nor any agent session
has a device on a different network.

Run these from the tailnet device on the other network, **not** from a LAN
machine, and not from the workstation whose `hosts` file already shortcuts the
name. All four must hold.

```bash
# Plain `tailscale status` does not print routes — approved routes need --json
tailscale status --json | jq '.Peer[] | select(.PrimaryRoutes) | {name: .HostName, online: .Online, routes: .PrimaryRoutes}'

kubectl get nodes                      # expect 8 nodes
talosctl -e 192.168.1.199 -n <cp-ip> version
```

For `kubectl` to work off-LAN the client has to resolve `cluster.jdwlabs.com`
to `192.168.1.199`, which public DNS does not do — see
[lan-name-resolution.md](lan-name-resolution.md). Until a resolver serves that
name over the tailnet, an off-LAN client needs the same `hosts` entry a LAN
client needs, or `--server https://192.168.1.199:6443`.

Then confirm nothing was exposed in the process. Failure is the success
condition on all four ports:

```bash
for p in 6443 50000 22 9000; do
  timeout 5 bash -c "</dev/tcp/104.53.12.62/${p}" 2>/dev/null \
    && echo "${p} OPEN - investigate" || echo "${p} refused"
done
```

`80` and `443` stay open — public ingress depends on them.

## Step 4 (separate decision) — arm the source-CIDR allowlist

Only after step 3 passes, and deliberately not in the same sitting.

`talops` already renders a source-CIDR reject on both the `6443` and `50000`
frontends; the knob is off, not missing. Setting it in
`terraform/terraform.tfvars`:

```hcl
admin_allowed_cidrs = ["192.168.1.0/24", "100.64.0.0/10"]
```

`100.64.0.0/10` is the CGNAT range Tailscale addresses come from, and it is only
correct once the subnet router is working — arming this list before step 3
passes locks out the very path being built.

This is a live change to the only administrative path into the cluster, so it
carries the full rollback discipline in
[api-exposure-lockdown.md](api-exposure-lockdown.md): copy `haproxy.cfg` aside
first, and rehearse through the `ADMIN_ALLOWED_CIDRS` environment override,
which takes precedence over the tfvars value.

## Rebuild parity

**Updated 2026-08-09:** the VM at `192.168.1.199` is no longer hand-built —
`terraform/haproxy-node.tf` provisions it (`haproxy_vms` in the live tfvars
now holds one entry) and `scenarios/haproxy-vm-rebuild.md` records the cutover
that made it so. Steps 1 and 2 were then run against that Terraform-managed VM
on 2026-08-13 — so the live router sits on a VM whose shell Terraform owns but
whose tailnet membership it does not. This section is about what stays a manual
step even now.

Tailscale is intentionally **not** in the cloud-init template: joining a
tailnet needs an auth key, and putting one in day-0 user-data writes a
credential into the Proxmox VM config where the secret vault cannot reach it.
That's unchanged by the VM becoming Terraform-managed. A future rebuild
(`scenarios/haproxy-vm-rebuild.md`) therefore still repeats step 1 of this
runbook by hand, or joins with a short-lived ephemeral key issued at rebuild
time — Terraform owning the VM shell doesn't own tailnet membership.

A rebuild also drops the IP-forwarding sysctl from step 1 and the route
approval from step 2. Neither is in Terraform, so both have to be redone before
the rebuilt VM routes anything.

## What this does not do

- No WAN port is opened, and no router setting is touched
- No Talos machine config changes
- No `haproxy.cfg` change, unless step 4 is taken separately
- It does not make `cluster.jdwlabs.com` resolve correctly off-LAN — that is a
  resolver problem, tracked in [lan-name-resolution.md](lan-name-resolution.md)
