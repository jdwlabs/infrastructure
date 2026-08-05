# Restricting Public Access to the Kubernetes and Talos APIs

Runbook for removing internet exposure of `6443` (Kubernetes) and `50000`
(Talos) without losing the ability to administer the cluster.

Nothing here is applied automatically. Every step is manual, and each one lists
its own rollback.

> **Read the lockout test first.** These two ports are the only paths by which
> this cluster is administered. If both are wrong at once, recovery is the
> Proxmox console on the hosts — there is no second network path in.

## Why this is safer than it looks

The admin path from the LAN **does not traverse the router.** `kubectl` and
`talosctl` reach HAProxy at `192.168.1.199` directly, so removing a WAN
port-forward cannot break on-LAN administration. The failure mode of the router
step is limited to off-site access.

That safety rests on one undocumented detail: the workstation's `hosts` file
overrides `cluster.jdwlabs.com` to `192.168.1.199`. Public DNS resolves the same
name to the WAN address, so **any machine without that override reaches the
cluster through the router** and will lose access when the forward is removed.
Treat a second LAN machine as the real test, not the workstation that already
has the override. What that override is, why it is the only one of its kind that
matters, and what it would take to retire it:
[lan-name-resolution.md](lan-name-resolution.md).

## Topology that matters here

| Layer | Where | Who can change it |
| --- | --- | --- |
| WAN port-forward (DNAT) | Router at `192.168.1.254` | **Human only** — no API, no Terraform resource |
| Source-IP allowlist on the API frontends | `haproxy.cfg` on the HAProxy VM, rendered by `talops` | `talops reconcile` |
| Listener itself | Talos control plane, `6443`/`50000` on every control-plane node | Talos machine config |

HAProxy at `192.168.1.199` fronts both APIs: `6443` and `50000` to the
control-plane nodes, `80`/`443` to the ingress NodePorts, and its own stats on
`9000`. It is a hand-built VM with no Terraform resource, so do not expect to
recreate it from this repo if it is lost.

Restricting at the **listener** (Talos) is the option to avoid. A bad machine
config on the control plane takes out `talosctl` and `kubectl` together, which
is exactly the state with no network recovery path.

## Order of operations

Router first, allowlist second. That ordering is deliberate: the router step is
the one that cannot lock out LAN administration, and it removes the exposure on
its own. The allowlist is defence in depth for the case where the forward is
ever re-added.

### Step 0 — record the current state (do this even if you are confident)

```bash
# From the LAN. Both must succeed before you change anything.
kubectl get --raw /version
talosctl -e 192.168.1.199 -n <control-plane-ip> version

# Which ports the router currently forwards, observed by hairpin NAT:
for p in 22 80 443 6443 9000 50000; do
  timeout 5 bash -c "</dev/tcp/<wan-ip>/${p}" 2>/dev/null \
    && echo "${p} forwarded" || echo "${p} not forwarded"
done
```

On the router, photograph or copy out each forwarding rule before deleting it —
WAN port, destination host, destination port. That copy **is** the rollback for
step 2; there is no export function.

### Step 1 — establish off-site access before removing it (optional, additive)

Skip only if off-site *command-line* administration is not wanted at all. Note
what this step does and does not restore: the Headlamp dashboard at
`dashboard.jdwlabs.com` is public HTTPS on `443` behind OIDC and is unaffected
by anything in this runbook, so browser-based cluster administration survives
the lockdown on its own. What the removed forwards cost is `kubectl` and
`talosctl` from off-LAN.

The existing Tailscale tailnet does not include any cluster node or the HAProxy
VM, so it is not yet an alternative path — it has to be extended first. Install
Tailscale **on the HAProxy VM** (not on the Talos nodes; Talos has no package
manager and this needs no machine-config change):

```bash
ssh <haproxy-vm>
curl -fsSL https://tailscale.com/install.sh | sh
sudo tailscale up --advertise-routes=192.168.1.0/24 --accept-dns=false
# then approve the subnet route in the Tailscale admin console
```

Verify from a tailnet device on a different network *before* step 2:

```bash
tailscale status                      # HAProxy VM present and online
kubectl get --raw /version            # with cluster.jdwlabs.com -> 192.168.1.199
```

**Rollback:** `sudo tailscale down` on the VM, and remove the machine from the
tailnet. Additive only — it does not touch the API path, so a failure here
leaves the cluster exactly as it was.

Full procedure, including route approval, the auto-approver policy and the
off-LAN verification set: [tailscale-subnet-router.md](tailscale-subnet-router.md).

### Step 2 — remove the WAN forwards for 6443 and 50000 (human, at the router)

**This step has no CLI. It is a manual change on the router's admin interface at
`192.168.1.254`, and it is the only step an agent cannot perform.**

Delete the forwarding rules for `6443` and `50000`. Keep `80` and `443` — public
ingress depends on them. Also remove `22` and `9000` if they are still
forwarded: `22` reaches a general-purpose Linux host and `9000` is HAProxy's
stats page, whose credentials sit in `terraform.tfvars`.

Verify from a network that is not the LAN — a phone hotspot is enough. **Failure
is the success condition:**

```bash
curl -k --max-time 5 https://<wan-ip>:6443/version   # must fail to connect
timeout 5 bash -c '</dev/tcp/<wan-ip>/50000'         # must fail to connect
curl -sI --max-time 5 https://<a-service>.jdwlabs.com/  # must still return a response
```

Then re-run the step 0 commands on the LAN. They must still pass.

**Rollback:** re-create the forwarding rules recorded in step 0. Confirm with the
hairpin loop from step 0 — a forward that is back reports `forwarded` from
inside the LAN within seconds.

### Step 3 — add the source-IP allowlist (defence in depth)

`talops` already renders this; the knob is off, not missing. Setting
`admin_allowed_cidrs` makes it emit, on **both** the `6443` and `50000`
frontends:

```
tcp-request connection reject unless { src <cidr> <cidr> ... }
```

Set it in `terraform/terraform.tfvars` (the example file carries the empty
default):

```hcl
admin_allowed_cidrs = ["192.168.1.0/24", "100.64.0.0/10"]
```

The second entry is the Tailscale CGNAT range and is only needed if step 1 was
done. `ADMIN_ALLOWED_CIDRS` is an environment override of the same setting and
takes precedence, which makes it the safer way to rehearse.

Before applying, copy the live config so the rollback is a file restore:

```bash
ssh <haproxy-vm> 'sudo cp /etc/haproxy/haproxy.cfg /etc/haproxy/haproxy.cfg.bak'
talops reconcile   # renders and pushes haproxy.cfg, then reloads
```

Verify from the LAN — both step 0 commands — and from a **second LAN host** if
one exists.

**Rollback:** restore the file and reload, which does not depend on `talops`
working:

```bash
ssh <haproxy-vm> 'sudo cp /etc/haproxy/haproxy.cfg.bak /etc/haproxy/haproxy.cfg \
  && sudo systemctl reload haproxy'
```

Then remove `admin_allowed_cidrs` from `terraform.tfvars`, or the next
`talops reconcile` re-applies it.

## How you know you are locked out, and what to do

You are locked out when, from the LAN:

```bash
kubectl get --raw /version          # hangs or connection refused
talosctl -e 192.168.1.199 -n <cp> version   # hangs or connection refused
```

Work outwards in this order — each rung needs less to be working than the last:

1. **Is it DNS or the path?** `curl -k https://192.168.1.199:6443/version` by IP.
   A `401` is a healthy apiserver and the problem is name resolution, not access.
2. **Is it HAProxy or the cluster?** `curl -k https://<control-plane-ip>:6443/version`
   direct to a node, bypassing HAProxy. If that answers, the fault is in
   `haproxy.cfg` — go to the step 3 rollback.
3. **Is HAProxy reachable at all?** SSH to the VM on the LAN. If SSH works, the
   step 3 rollback works.
4. **If SSH does not work:** the Proxmox console for the HAProxy VM. Local login,
   restore `haproxy.cfg.bak`, reload.
5. **If the control plane itself is unreachable on `50000`:** the machine config
   was changed, which this runbook tells you not to do. Recovery is the Proxmox
   console on each control-plane node.

A restored file at rung 4 is enough to get `kubectl` back. Nothing in steps 1-3
can produce a state that requires re-bootstrapping.

## What does not break

Verified before writing this: no automation reaches the API from outside the LAN.
ArgoCD, External Secrets and the in-cluster agents all use the in-cluster
endpoint, and CI never authenticates to the cluster at all — the only `talosctl`
in any workflow is a client-side version check and offline config-generation
tests. Removing the public forwards therefore costs human off-site access and
nothing else.
