# Runbook: Kubernetes version upgrade (`talosctl upgrade-k8s`)

Status: PLANNED — `talosctl upgrade-k8s` (or `talops upgrade-k8s --apply`) is
executed by a human. The agent contract forbids autonomous cluster mutation;
agents may run the read-only inspection commands here (`kubectl get`,
`kubectl version`, `talosctl get`, `talops upgrade-k8s` without `--apply`)
but never the upgrade itself.

Scope: upgrading **Kubernetes** on an already-running Talos cluster — control
plane static pods, kube-proxy, and every kubelet. This is a separate
operation from a Talos OS upgrade (see [talos-upgrade.md](talos-upgrade.md))
with its own gates; the two are deliberately decoupled so only one variable
moves at a time. For system design see [ARCHITECTURE.md](ARCHITECTURE.md).

## Current state (verified 2026-08-05)

- **Live cluster**: `kubectl version` reports server `v1.35.1` on all nodes.
- **Live Talos OS**: `v1.13.4` (the tracked pin has moved to `v1.13.7` per
  [talos-upgrade.md](talos-upgrade.md), but that OS upgrade has not run yet —
  read the live value with `talosctl version`, not the tracked pin, before
  relying on either number).
- **Target**: `v1.36.3` — the latest published Kubernetes 1.36 patch
  (released 2026-07-23) and inside Talos 1.13's supported range.
  [Talos 1.13's support matrix](https://docs.siderolabs.com/talos/v1.13/getting-started/support-matrix)
  covers Kubernetes 1.31–1.36 for every point release in that line, so 1.36.3
  is valid against the live `v1.13.4` even though `v1.13.4` itself bundles
  `v1.36.1` by default — Talos does not require running its bundled default,
  only a version inside the supported minor range.

## Where the target version comes from

`kubernetes_version` in `terraform/terraform.tfvars` (untracked; the tracked
example is `terraform/terraform.tfvars.example`) — mirrors the
`talos_version`/`installer_image` split in
[talos-upgrade.md](talos-upgrade.md#where-the-target-version-comes-from).
Renovate owns the tracked pin via a dedicated custom manager in
`renovate.json` (`kubernetes_version pin in Terraform tfvars and the
scaling-test fixtures`), kept in its own group separate from the Talos group
by design:

> The Kubernetes pin is deliberately kept out of the Talos group. A
> Kubernetes bump is rolled out with `talosctl upgrade-k8s`, a separate human
> runbook from a Talos OS upgrade... Like the Talos PRs, a Kubernetes PR is a
> prompt, not an auto-rollout.

**The tracked pin has already moved.** `terraform/terraform.tfvars.example`
and all 14 `scenarios/scaling_tests/*.tfvars` fixtures pin `v1.36.3`
(commit `5adaf8c`, merged 2026-07-27) — that predates the Renovate manager
that now tracks it (`1bf03b3`, 2026-07-30), so it was a manual bump, but the
target it landed on is still correct as of this writing.

**The live `terraform/terraform.tfvars` and `clusters/core/nodes/*.yaml`
machine configs are still `v1.35.1` and deliberately untouched.** They are
untracked and move operationally, the same split
[talos-upgrade.md](talos-upgrade.md#where-the-target-version-comes-from)
uses for the Talos OS pin. Updating them before the upgrade actually runs
would let a `talops reconcile` regenerate and push new
apiserver/scheduler/controller-manager/kube-proxy/kubelet images through
`talosctl apply-config` — bypassing `upgrade-k8s`'s coordinated, one-stage-
at-a-time rollout entirely. Update them **after** a successful upgrade, as
the last step below, so the tracked pin and the live cluster agree again.

## Deprecation check: Kubernetes 1.35 → 1.36

Checked against every manifest actually deployed to this cluster —
`platform` and `deployments` repos, raw manifests and Helm chart templates —
plus the machine config in this repo. Nothing here blocks the upgrade.

| Change in 1.36 | Found in this cluster? | Detail |
|---|---|---|
| kube-proxy IPVS mode **removed** (deprecated since 1.35) | No | `clusters/core/nodes/*.yaml` sets no `mode` under `proxy:` — default (iptables). No override in `platform`/`deployments` either. |
| `gitRepo` volume plugin **removed permanently** (deprecated since 1.11) | No | No `volumes: - gitRepo:` anywhere in either repo. |
| Service `.spec.externalIPs` **formally deprecated** (not yet removed) | Found, inert | Only in the vendored, disabled Redis subchart under `platform/helm-charts/litellm-helm/charts/redis` (`redis.enabled: false` in the tenant values, default `externalIPs: []`, no override). Not live. Worth pruning as dead vendored code, but not an upgrade blocker. |
| Ingress-NGINX project retired (context, not a 1.36 API removal) | Found, inert | Same `litellm-helm` chart sets `ingress.className: "nginx"`, but `ingress.enabled` defaults `false` and is not overridden — the resource never renders. No other `ingress-nginx` reference anywhere; the cluster uses NGF Gateway API resources as established. |
| General deprecated-API sweep (`policy/v1beta1` PodSecurityPolicy, `extensions/v1beta1`, `batch/v1beta1`, `autoscaling/v2beta1/2`, `networking.k8s.io/v1beta1`, etc.) | Found, inert | Confined to the same vendored, disabled `litellm-helm/charts/{redis,postgresql}` bitnami-style subcharts (`psp.yaml`, a dead conditional branch in `ingress.yaml`). Nothing in `deployments`. |

Follow-up hygiene (not a blocker, not done as part of this ticket): the
vendored `charts/redis` and `charts/postgresql` subdirectories under
`platform/helm-charts/litellm-helm` are unused (`redis.enabled: false`,
`db.deployStandalone: false`) and carry all of the above dead references —
worth pruning in the `platform` repo on its own ticket.

## Preflight checklist — all must pass before `--apply`

Lighter than the Talos OS upgrade's gate list: `upgrade-k8s` restarts control
plane static pods and every kubelet in place, it does not reboot the node or
touch the VXLAN/flannel link, so the DHCP-lease-flip and offload-recreation
failure modes in [talos-upgrade.md](talos-upgrade.md) do not apply here.

1. **Re-verify the live version** — `kubectl version` still reports
   `v1.35.1` server-side, and `talosctl version` on each control plane still
   reports `v1.13.4` (or whatever the live OS version is at execution time —
   re-derive live, do not trust this document).
2. **Version range legal.** Confirm the live Talos version's support matrix
   covers the target Kubernetes minor (1.13 covers 1.31–1.36, confirmed
   above). If the live Talos version has since moved past 1.13, re-check the
   matrix for whatever is actually running.
3. **etcd healthy 3/3**:
   `talosctl -n 192.168.1.241,192.168.1.98,192.168.1.125 etcd status` —
   three members, single leader, no learners, no alarms.
4. **kube-proxy mode still default (iptables).** Re-run the grep above
   against live machine config if anything has changed it since this
   document was written — an IPVS override introduced between now and the
   upgrade would hit the 1.36 removal.
5. **Longhorn healthy**: `kubectl -n longhorn-system get volumes.longhorn.io`
   all Healthy, no degraded replicas, no rebuilds in flight.
6. **Cluster quiet**: ArgoCD apps Healthy/Synced; no in-flight infra changes;
   Vault unsealed with the auto-unseal CronJob green.
7. **Manifest inventory preview is clean.** Run the command below *without*
   `--apply` first (its default mode) and read the report: it reads
   `kube-system/talos-bootstrap-manifests-inventory` and shows what a prune
   would touch. The current template carries no `extraManifests` entries, so
   this should show nothing at risk — confirm that is still true rather than
   assuming it.
8. **Baseline captured** — `kubectl get nodes -o wide`, `kubectl version`,
   record before/after for the post-check comparison.

## The command

Run through `talops` (`bootstrap/cmd/upgrade.go`, already merged to `main`),
which wraps `talosctl upgrade-k8s` with a manifest-prune guard applied by
default — a prune is the last stage of the upgrade and can delete objects a
GitOps release has since taken over, with no clean abort point once it
starts. `--node` must be one control plane IP (`talosctl` targets every node
in the talosconfig if `-n` is omitted, which `talops` refuses to allow
silently).

```bash
# Preview (default): nothing is mutated. Read the manifest-inventory report.
talops upgrade-k8s --to v1.36.3 --node 192.168.1.241

# Perform the upgrade — prune guard still applied
talops upgrade-k8s --to v1.36.3 --node 192.168.1.241 --apply
```

If `talops` is unavailable for some reason, the equivalent raw command is:

```bash
talosctl upgrade-k8s --to v1.36.3 --manifests-no-prune
```

but prefer `talops upgrade-k8s` — it reads the inventory first and reports
which keys a prune would delete, which raw `talosctl` does not do.

## Post-checks

- `kubectl get nodes -o wide` — all 8 nodes Ready.
- `kubectl version` — server reports `v1.36.3`.
- `talosctl -n <cp-ips> etcd status` — still 3/3, no alarms (this upgrade
  does not touch etcd, but a static-pod restart is a reasonable place to
  re-confirm).
- `kubectl -n longhorn-system get volumes.longhorn.io` — all Healthy.
- ArgoCD: all apps Healthy/Synced.
- **Update the operational pins to match**: `terraform/terraform.tfvars`
  and every `clusters/core/nodes/*.yaml` image tag
  (`kubelet`, `kube-apiserver`, `kube-controller-manager`, `kube-proxy`,
  `kube-scheduler`) to `v1.36.3`, so the tracked example and the live
  cluster agree again — the same closing step
  [talos-upgrade.md](talos-upgrade.md#post-checks) uses for the Talos OS
  pin.

## Out of scope

- **Talos OS upgrades.** See [talos-upgrade.md](talos-upgrade.md).
- **Schematic changes.** Not applicable here — no installer image is
  involved in a Kubernetes-only upgrade.

## Unverified — confirm before relying on these

- **No previous Kubernetes upgrade of this cluster is recorded in the
  repo.** This is a first run. Treat it as a rehearsal the same way
  [talos-upgrade.md](talos-upgrade.md#unverified--confirm-before-relying-on-these)
  treats the first Talos OS upgrade: nothing here has been executed
  end-to-end and reviewed against a real run.
- **Static-pod restart disruption during the control-plane stage.** This
  runbook assumes `upgrade-k8s` restarting apiserver/controller-
  manager/scheduler in place is non-disruptive to a healthy 3-member etcd
  quorum and HAProxy-fronted API, consistent with upstream Talos's design,
  but that has not been observed on this specific cluster.
- **kubelet restart impact on running pods.** A kubelet restart does not
  evict pods, but any pod actively using an exec/log/port-forward stream
  through that kubelet at the moment of restart will see it drop.
