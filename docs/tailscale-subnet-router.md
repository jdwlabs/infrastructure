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

## State this procedure assumes

Verified 2026-08-04, and worth re-confirming before starting because the answer
is what makes the work necessary:

```
$ tailscale status
100.122.181.6  jakegpc                 windows  -
100.71.248.90  jake-inspiron-5406-2n1  linux    offline, last seen 19h ago
```

Two workstations, no infrastructure host, and no advertised subnet routes from
anyone. The tailnet exists but carries no path to the cluster.

The LAN is `192.168.1.0/24` with its gateway, DHCP server and DNS server all at
`192.168.1.254`.

Re-checked 2026-08-13 from a machine on the LAN: `192.168.1.199` answers ICMP,
so the host is reachable at the network layer, but SSH to it as either the
admin user or `root` returns `Permission denied (publickey)` — this
environment's key isn't authorized there. Still no credentialed path to the
VM; Steps 1 and 3 below remain human-executed.

## Prerequisites

- SSH access to the HAProxy VM from the LAN as the admin user
- Owner or admin access to the Tailscale admin console, to approve the route
- A second device already on the tailnet and on a **different** network — a
  phone hotspot is enough. Have it ready before starting, because it is the only
  thing that can prove the result

## Step 1 — install and advertise (human, on the HAProxy VM)

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

**Rollback:** `sudo tailscale down`.

## Step 2 — approve the route (human, in the admin console)

In the Tailscale admin console, open the machine's row and enable the pending
`192.168.1.0/24` subnet route.

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

## Step 3 — verify from off-LAN

Run these from the tailnet device on the other network, **not** from a LAN
machine, and not from the workstation whose `hosts` file already shortcuts the
name. All four must hold.

```bash
tailscale status                       # HAProxy VM present, online, route listed

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

The VM at `192.168.1.199` is hand-built; `haproxy_vms` is absent from the live
tfvars, so Terraform provisions nothing for it today and this runbook is the
only record of the install.

`terraform/haproxy-node.tf` and its cloud-init template do describe a
Terraform-built replacement. Tailscale is intentionally **not** in that template:
joining a tailnet needs an auth key, and putting one in day-0 user-data writes a
credential into the Proxmox VM config where the secret vault cannot reach it.
A rebuilt VM therefore repeats step 1 by hand, or joins with a short-lived
ephemeral key issued at rebuild time.

## What this does not do

- No WAN port is opened, and no router setting is touched
- No Talos machine config changes
- No `haproxy.cfg` change, unless step 4 is taken separately
- It does not make `cluster.jdwlabs.com` resolve correctly off-LAN — that is a
  resolver problem, tracked in [lan-name-resolution.md](lan-name-resolution.md)
