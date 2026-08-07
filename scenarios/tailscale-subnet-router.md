# Tailscale Subnet Router — Execution Checklist (JDWLABS-284)

Sequential, copy-pasteable steps for standing up the Tailscale subnet router
on the HAProxy VM and closing out JDWLABS-284. This is a human-executed
checklist — no agent has SSH, console, or network access to run any of it.

For the *why* behind each step (why the HAProxy VM, why `--accept-dns=false`,
why route approval is manual, the tailnet-policy auto-approver, and rebuild
parity once `haproxy-node.tf` provisions this VM) see
[tailscale-subnet-router.md](../docs/tailscale-subnet-router.md), which this
checklist follows exactly. Read that doc first if anything here is unclear;
don't duplicate its rationale into ticket comments — link back to it instead.

## Before you start

- SSH access to the HAProxy VM (`192.168.1.199`) from the LAN as the admin user
- Owner/admin access to the Tailscale admin console
- A second device already on the tailnet, on a **different** network than the
  LAN (a phone hotspot is enough), with **no** local `hosts` entry or
  `--server` override shortcutting `cluster.jdwlabs.com` — that's the device
  every verification command below runs from
- The WAN IP to sweep in the final check (`104.53.12.62`, same one used in
  [tailscale-subnet-router.md](../docs/tailscale-subnet-router.md))
- If the off-LAN device is Linux, subnet routes aren't accepted by default —
  run `sudo tailscale set --accept-routes` on it first (Windows/macOS/Android/
  iOS accept them automatically)

## Step 1 — Install Tailscale and advertise the route

On the LAN, SSH to the VM and install:

```bash
ssh <admin-user>@192.168.1.199
curl -fsSL https://tailscale.com/install.sh | sh
sudo tailscale up --advertise-routes=192.168.1.0/24 --accept-dns=false
```

The command prints a login URL — open it and approve the machine into the
tailnet. `--accept-dns=false` keeps the VM's own name resolution on the LAN
gateway; see the linked doc for why that matters on this particular host.

Advertising the route does not activate it — it's a pending request in the
admin console until Step 2.

**Rollback:** `sudo tailscale down` on the VM. Removes the machine from the
tailnet's active routes immediately; nothing else on the LAN, HAProxy, or
Talos changes as a result, so this is always safe to run.

## Step 2 — Approve the route in the admin console

1. Open the Tailscale admin console → **Machines**
2. Find the HAProxy VM's row (hostname from Step 1's login)
3. Click the row → under **Subnet routes**, approve the pending
   `192.168.1.0/24` route

**Rollback:** same page, disable the route. Takes effect within seconds and
leaves the machine on the tailnet otherwise untouched.

(Optional, for surviving reinstalls without repeating this click — the
tailnet policy `autoApprovers` change described in
[tailscale-subnet-router.md, Step 2](../docs/tailscale-subnet-router.md#step-2--approve-the-route-human-in-the-admin-console).)

## Step 3 — Verify off-LAN and capture evidence

Run every command in this step from the **off-LAN tailnet device**, not from
a LAN machine and not from a workstation with an existing `hosts` shortcut
for the cluster. Paste each command's output as a comment on JDWLABS-284.

### 3a — Route is advertised and active

Plain `tailscale status` doesn't print routes — approved routes only show up
under `--json`:

```bash
tailscale status --json | jq '.Peer[] | select(.PrimaryRoutes) | {name: .HostName, online: .Online, routes: .PrimaryRoutes}'
```

Expect one entry: the HAProxy VM, `online: true`, `routes` containing
`192.168.1.0/24`. An empty result means the route isn't approved yet (back to
Step 2) or isn't accepted on this device (see the Linux prerequisite above).

```
paste the jq output here as a comment on JDWLABS-284
```

### 3b — kubectl reaches the cluster through the tailnet, not a hosts shortcut

Confirm the device's hosts file has **no** entry for `cluster.jdwlabs.com` or
`192.168.1.199` before running this — the point of the test is that the
tailnet route alone resolves reachability, not a local override:

```bash
kubectl get nodes
```

Expect all 8 nodes listed, `Ready`. If `kubectl` can't resolve
`cluster.jdwlabs.com` at all, that's DNS, not the router — see
[lan-name-resolution.md](../docs/lan-name-resolution.md); a temporary
`--server https://192.168.1.199:6443` is an acceptable substitute for this
check and doesn't need a hosts entry.

```
paste `kubectl get nodes` output here as a comment on JDWLABS-284
(note whether a hosts entry or --server override was needed, and why)
```

### 3c — talosctl reaches the Talos API through the tailnet

```bash
talosctl -e 192.168.1.199 version
```

Expect a successful version response (client and server). If the CLI
prompts for a node and the bare `-e` form doesn't resolve one, use
`talosctl -e 192.168.1.199 -n <cp-ip> version` instead — either form proves
the API path; note in the ticket comment which one was used.

```
paste `talosctl ... version` output here as a comment on JDWLABS-284
```

### 3d — No new WAN port opened

Still from the off-LAN device, sweep the WAN IP directly (not through the
tailnet):

```bash
for p in 6443 50000 22 9000; do
  timeout 5 bash -c "</dev/tcp/<wan-ip>/${p}" 2>/dev/null \
    && echo "${p} OPEN - investigate" || echo "${p} refused"
done
```

Expect `refused` on all four. Failure to connect *is* the passing result —
if any port prints `OPEN`, stop and treat it as a live exposure, not a
runbook error. (`80`/`443` are expected to stay open for public ingress and
aren't part of this sweep.)

```
paste the sweep output here as a comment on JDWLABS-284
```

## Step 4 (optional, separate decision) — arm the source-CIDR allowlist

Do not take this step in the same sitting as Steps 1–3, and only after all
four checks above pass. This is defence in depth on top of a working subnet
router, not a requirement to close this ticket — it's a distinct,
independently reversible change to the only administrative path into the
cluster, with its own rollback discipline.

Full steps: [tailscale-subnet-router.md, Step 4](../docs/tailscale-subnet-router.md#step-4-separate-decision--arm-the-source-cidr-allowlist).
In short: `admin_allowed_cidrs = ["192.168.1.0/24", "100.64.0.0/10"]` in
`terraform/terraform.tfvars`, then `talops reconcile`, rehearsed against
`ADMIN_ALLOWED_CIDRS` and with `haproxy.cfg` copied aside first per
[api-exposure-lockdown.md](../docs/api-exposure-lockdown.md).

## Closing the ticket

Once Steps 1–3 are all pasted into JDWLABS-284 with passing results, the
ticket's Definition of Done is met. Step 4 stays open as a separate,
optional follow-up — don't block this ticket on it.
