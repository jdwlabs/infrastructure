# Runbook: Pin static IPs on control-plane nodes

Status: PLANNED — every `talosctl apply-config` in this runbook is executed by
a human. The agent contract forbids autonomous applies.

## Why

All three control-plane VMs get their address from a DHCP lease. Twice now a
CP node has rebooted onto a *different* lease and broken the etcd peer mesh —
etcd peer URLs pin the old IP, the member never rejoins, and the API goes down
(2026-06-28: a CP moved .240 → .98; later recurrence: cp-03/fow-vbk moved
.244 → .125). A reboot does **not** heal a stale peer URL; only a surgical
`etcdctl member update` through the surviving quorum does (see Repair below).

Static addressing on the CPs kills this failure mode. Each node keeps the
exact IP it holds today, so there is **zero peer-URL churn** on adoption —
the change is a no-op from etcd's point of view.

## Mechanism

Per-node patches at `clusters/core/patches/node-{200,201,202}.yaml`. `talops`
applies these after the role template when generating node configs
(`resolveNodePatch` in `bootstrap/internal/talos/config.go`); `TemplateHash`
includes them, so `talops status` / `talops reconcile --dry-run` will report
the three CP node configs as drifted until regenerated. The patch keys on
`interface: eth0` — the same name-based selector the role template uses — so
it strategically merges over the template's `dhcp: true` entry.

## Live mapping (verified 2026-07-23 — always re-derive before applying)

| VM (vmid) | K8s node | IP (keep!) | eth0 MAC | etcd member |
|-----------|----------|-----------|----------|-------------|
| cp-01 (200) | talos-oam-s4g | 192.168.1.241/24 | bc:24:11:70:10:ed | 327e88294e54db55 |
| cp-02 (201) | talos-6iz-oey | 192.168.1.98/24 | bc:24:11:f2:94:84 | b2860e26db455b63 |
| cp-03 (202) | talos-fow-vbk | 192.168.1.125/24 | bc:24:11:9c:e1:86 | 7a07833c5e979e83 |

Gateway **and** DNS: `192.168.1.254` (the LAN router — NOT `.1`; a `.1`
gateway takes the node off the network). Re-derive the table live before
touching anything: `kubectl get nodes -o wide`, `talosctl -n <ip> get links`.
If a live IP differs from a patch file, STOP and fix the patch first — the
patch must always encode the address the node currently holds.

## Preconditions — hard gates, all must pass

1. **DHCP reservations/exclusions.** On the DHCP server (router at .254),
   reserve .241/.98/.125 for the MACs above — or exclude them from the pool —
   so DHCP can never hand these addresses to another client once the nodes
   stop renewing their leases. Static config on the node does not protect the
   address from the pool's perspective; this step does.
2. **etcd healthy 3/3**: `talosctl -n 192.168.1.241,192.168.1.98,192.168.1.125 etcd status`
   — three members, single leader, no learners, no errors.
3. **etcd snapshot taken**:
   `talosctl -n 192.168.1.241 etcd snapshot ./etcd-$(date +%F).db`.
4. **Configs regenerated and diffed.** Hydrate secrets (`talops secrets
   hydrate`), regenerate node configs (`talops reconcile --dry-run` shows the
   three CP configs drifted; regenerate via talops or
   `talosctl machineconfig patch clusters/core/secrets/control-plane.yaml ...`),
   then diff old vs new node YAML: the only delta must be the
   `machine.network` stanza (dhcp false, address, route, nameservers). Any
   other delta → stop and investigate before applying.

## Sequence — one node at a time: cp-01 → cp-02 → cp-03

Never touch two CPs in the same pass; quorum must stay ≥2/3 throughout.

Per node N (IP X):

1. Re-verify gate 2 (etcd 3/3).
2. **HUMAN**: `talosctl -n X apply-config --file clusters/core/nodes/node-control-plane-<vmid>.yaml`
   — the address does not change, so this is non-disruptive; Talos applies the
   network change in place (no reboot needed).
3. Verify the node kept its IP and switched off DHCP:
   `talosctl -n X get addresses` (same eth0 address, /24),
   `talosctl -n X get resolvers` (still 192.168.1.254).
4. Verify etcd unchanged: `talosctl -n X etcd status` — same member ID, 3/3
   healthy, one leader.
5. `kubectl get nodes` all Ready; API answering via cluster endpoint
   (haproxy .199). Then next node.
6. After all three: reboot ONE CP (change window) to prove the address
   survives a boot with DHCP off:
   `talosctl -n X reboot` → node returns on the same IP, etcd 3/3.

## Abort criteria

- Any node loses its address or drops off the network after apply → it kept
  the wrong gateway or address; power-cycle recovers DHCP only if the config
  is rolled back — use the Proxmox console + `talosctl -n X apply-config`
  with the previous (DHCP) config from the last commit.
- etcd healthy members < 2 → stop; repair below before anything else.

## Repair — stale etcd peer URL (the original failure mode)

If a CP ever comes up on a different IP (this change exists to prevent that,
but for the record): rebooting the node does NOT refresh its peer URL. The
fix is a surgical member update through the surviving quorum:

1. From a healthy CP, list members: `talosctl -n <healthy-ip> etcd members`.
2. Update the stale member's peer URL to the node's current IP with `etcdctl`
   against a **healthy** member endpoint (talosctl has no `member update`;
   use the etcd client certs from the hydrated secrets bundle):
   `etcdctl --endpoints=https://<healthy-ip>:2379 member update <member-id> --peer-urls=https://<current-ip>:2380`.
3. The member rejoins without data loss; verify `etcd status` 3/3, then fix
   the address problem (lease/reservation/static config) so it cannot recur.

This is the zero-downtime procedure that resolved both prior outages.

## Post-checks

- `talosctl -n 192.168.1.241,192.168.1.98,192.168.1.125 get addresses` — all
  static, correct /24, no DHCP addresses on eth0.
- `talops status` — no config drift on the three CP nodes.
- Router lease table shows no active DHCP leases for the three MACs (the
  reservations from gate 1 remain as a safety net).
- Workers stay on DHCP — only CPs host etcd, so only CPs need this. Extend to
  workers later only if something else pins worker IPs.
