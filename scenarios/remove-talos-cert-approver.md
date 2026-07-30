# Runbook: Remove the Talos-bundled kubelet-serving-cert-approver

Status: the repo change is MERGED; the cluster work is PLANNED. The template
change is on `main` — `cluster.extraManifests` is already absent from
`bootstrap/internal/talos/patches/control-plane.yaml`. The live machine config
on the three control planes still carries the entry, so everything from the
apply onwards is outstanding. Every `talosctl apply-config` and `kubectl delete`
in this runbook is executed by a human. The agent contract forbids autonomous
cluster mutation.

## Why

`kubelet-serving-cert-approver` had two declared owners that disagreed about
which image to run:

- `platform-kubelet-serving-cert-approver` (ns `kubelet-serving-cert-approver`)
  — the platform GitOps release, Synced and Healthy, pinning `appVersion:
  0.11.0` through its own chart.
- The Talos machine config, via `cluster.extraManifests` in the embedded talops
  patch template (`bootstrap/internal/talos/patches/control-plane.yaml`), which
  pulled `standalone-install.yaml` from upstream's `main` branch. That manifest
  ships `image: ghcr.io/alex1989hu/kubelet-serving-cert-approver:main` — a
  rolling tag.

Live today is `0.11.0`, so the two agree — by timing, not by design. This is the
same mutable-reference failure class as the servicediscovery outage, sitting in
the path that approves kubelet **serving** certificates, so a surprise version
change degrades metrics and log endpoints cluster-wide rather than one app.

This is no longer latent. The entry actively blocks `talosctl upgrade-k8s`,
which re-applies `cluster.extraManifests` as its final stage — so every
Kubernetes upgrade fetches that rolling tag, not just a machine-config apply.
The apply cannot succeed, because the two owners disagree on an immutable field:

```
$ talosctl upgrade-k8s --to 1.36.3 --dry-run
error diffing manifests: apply dry run failed for
  Deployment/kubelet-serving-cert-approver:
  spec.selector: Invalid value:
  {"matchLabels":{"app.kubernetes.io/instance":"kubelet-serving-cert-approver",...}}:
  field is immutable
```

Upstream's static manifest sets `instance` to the bare name; the chart sets it
to the Helm release name (`platform-kubelet-serving-cert-approver`). Pinning the
URL to a release tag does **not** resolve this — no upstream tag will ever carry
the release-name prefix. Removing the entry is the only fix that lands.

### Removal is not automatically inert

Talos does not delete these objects when the entry disappears from config, so
the leftovers below still need deleting by hand. But "Talos never garbage-
collects" is too strong for 1.13: it records every applied bootstrap manifest in
`kube-system/talos-bootstrap-manifests-inventory` and prunes entries missing
from the desired set by default, and that sync runs on `upgrade-k8s`.

The inventory on this cluster is stale — 46 days old, still listing the
`kube-system` metrics-server objects removed in `ace8290`, because `upgrade-k8s`
has not run since. It also lists
`_kubelet-serving-cert-approver__Namespace`. Whether Talos's prune would in fact
delete a removed extraManifest's objects is **unverified here**; what is certain
is that pruning that Namespace would cascade to everything the GitOps release
owns inside it. Pass `--manifests-no-prune` on the first `upgrade-k8s` after
this removal, and confirm the inventory has dropped these keys before relying on
prune again.

### What each side actually owns

Field managers across the whole release, not one sampled object
(`kubectl get <obj> --show-managed-fields`). Nine objects, not eight: two
RoleBindings share the name `events:kubelet-serving-cert-approver` in different
namespaces, and only one of them is GitOps-owned — so the rows below are
namespace-qualified.

| Object | Managers |
| --- | --- |
| `Namespace/kubelet-serving-cert-approver` | `argocd-controller(Apply)`, `talos(Update)` |
| `ServiceAccount/kubelet-serving-cert-approver` | `argocd-controller(Apply)`, `talos(Update)` |
| `Deployment/kubelet-serving-cert-approver` | `argocd-controller(Apply)`, `kube-controller-manager(Update)` |
| `ClusterRole/certificates:kubelet-serving-cert-approver` | `argocd-controller(Apply)`, `talos(Update)`, `argocd-application-controller(Update)` |
| `ClusterRole/events:kubelet-serving-cert-approver` | `argocd-controller(Apply)`, `talos(Update)`, `argocd-application-controller(Update)` |
| `ClusterRoleBinding/kubelet-serving-cert-approver` | `argocd-controller(Apply)`, `talos(Update)`, `argocd-application-controller(Update)` |
| `RoleBinding/events:kubelet-serving-cert-approver` (ns `kubelet-serving-cert-approver`) | `argocd-controller(Apply)`, `argocd-application-controller(Update)` |
| `RoleBinding/events:kubelet-serving-cert-approver` (ns `default`) | `talos(Update)` — **only owner** |
| `Service/kubelet-serving-cert-approver` | `talos(Update)` — **only owner** |

Talos writes with `operation: Update`, not `Apply`, so it never competed for
server-side-apply co-ownership of the Deployment's image field. Every object the
GitOps release renders is already owned by ArgoCD, which is why removing the
entry hands over cleanly. The two rows above with `talos` as the **only** owner
are the exceptions — the chart renders neither, so they are the objects this
runbook has to delete by hand.

The `Namespace` carries a `governance-platform` tracking id — it comes from the
tenant envelope, not from this chart, which is why the chart sets
`namespace.create: false`.

### The two orphans: a dead Service and a stale RBAC grant

Two objects have no GitOps owner and survive the removal. Neither is inert
enough to leave alone.

#### The Service is dead, not orphaned-but-working

`Service/kubelet-serving-cert-approver` has never routed traffic in this
cluster:

```
spec.selector:  app.kubernetes.io/instance: kubelet-serving-cert-approver
pod labels:     app.kubernetes.io/instance: platform-kubelet-serving-cert-approver
```

Upstream's static manifest hardcodes the bare name; the chart labels pods with
the Helm release name. The selector has therefore matched nothing since the
GitOps release took over the Deployment:

```
$ kubectl -n kubelet-serving-cert-approver get endpoints kubelet-serving-cert-approver
NAME                            ENDPOINTS   AGE
kubelet-serving-cert-approver   <none>      66d
```

No ServiceMonitor selects it either, so the `metrics` port it exposes (9090) is
neither reachable nor scraped. Deleting it removes a misleading object, not a
working one. Restoring cert-approver metrics is separate work and is ticketed —
it needs a Service whose selector matches the chart's labels plus a
ServiceMonitor, both rendered by the chart.

#### The `default` RoleBinding is a grant nothing will ever reconcile

Two RoleBindings carry the name `events:kubelet-serving-cert-approver`, and it
is easy to check the wrong one — the release-namespace copy is ArgoCD-owned and
fine, while upstream's static manifest also drops one in `default`:

```
$ kubectl get rolebinding -A | grep -i cert-approver
default                         events:kubelet-serving-cert-approver   ClusterRole/events:kubelet-serving-cert-approver   67d
kubelet-serving-cert-approver   events:kubelet-serving-cert-approver   ClusterRole/events:kubelet-serving-cert-approver   67d
```

The `default` copy has `talos(Update)` as its only manager, no
`argocd.argoproj.io/tracking-id` annotation, and it binds the release's
ServiceAccount to `ClusterRole/events:kubelet-serving-cert-approver`. ArgoCD
tracks by that annotation and never prunes untracked resources even with
`prune: true`, so once the extraManifests entry is gone nothing owns it, nothing
recreates it, and nothing removes it — it just sits there as a live RBAC grant
in a namespace the release has no business in. Delete it alongside the Service.

## Preconditions

1. The talops change removing the extraManifests entry is merged to `main`.
   **DONE** — the template on `main` carries no entries at all. What follows
   still has to be run by hand; the merge changed the template, not the
   running cluster.
2. Platform release healthy: `kubectl -n kubelet-serving-cert-approver get deploy
   platform-kubelet-serving-cert-approver` shows Available, and the ArgoCD app
   `platform-kubelet-serving-cert-approver` is Synced + Healthy.
3. The running image is the pinned tag, not the rolling one:
   `kubectl -n kubelet-serving-cert-approver get deploy kubelet-serving-cert-approver
   -o jsonpath='{.spec.template.spec.containers[*].image}'` returns
   `ghcr.io/alex1989hu/kubelet-serving-cert-approver:0.11.0`.
4. No kubelet serving CSRs are stuck Pending before starting:
   `kubectl get csr | grep -c Pending` — a backlog here means the approver is
   already unhealthy and this is the wrong time to touch it.
5. Cluster quiet: no in-flight node upgrades or CP maintenance.

## Sequence

1. Rebuild talops so the embedded template no longer carries the entry:
   `cd bootstrap && go build -o build/ ./...` (or `build.bat`).
2. Regenerate node configs without contacting any node:
   `talops reconcile --generate-only`. Inspect a regenerated CP config and
   confirm `cluster.extraManifests` is absent entirely. (A plain
   `talops reconcile` also detects the template change on its own via the
   recorded template hash in `bootstrap-state.json`, and lists the affected
   nodes under "Update N node config(s)" in the plan.)
3. **HUMAN**: review the machine-config diff before any apply. Expect exactly
   one removed key on the cluster section and nothing else.
4. **HUMAN**: apply the regenerated config to the three control planes, one at a
   time (workers carry the same cluster section but extraManifests only acts on
   control planes; applying to workers too keeps drift at zero):
   `talosctl -n <cp-ip> apply-config -f clusters/core/nodes/node-control-plane-<vmid>.yaml`
   — extraManifests is not a machine-section change; no reboot is expected.
   After each CP: `talosctl -n <cp-ip> etcd status` healthy before the next.
5. **HUMAN**: delete both orphans (Talos will not). These are the two objects
   with `talos` as their only field manager — the chart renders neither, and
   ArgoCD will not prune them because they carry no tracking-id annotation:

   ```bash
   kubectl -n kubelet-serving-cert-approver get svc kubelet-serving-cert-approver
   kubectl -n kubelet-serving-cert-approver delete svc kubelet-serving-cert-approver

   kubectl -n default get rolebinding events:kubelet-serving-cert-approver
   kubectl -n default delete rolebinding events:kubelet-serving-cert-approver
   ```

   Namespace matters on the second one: the identically named RoleBinding in
   `kubelet-serving-cert-approver` is the chart's, and deleting it only makes
   ArgoCD recreate it. Nothing else from the upstream manifest needs deleting —
   every remaining object is owned and continuously reconciled by the GitOps
   release. The Service's matching EndpointSlice is garbage-collected with it.

6. Confirm the manifest stage is unblocked before running a real Kubernetes
   upgrade — this is what the removal was for:

   ```bash
   talosctl -n <cp-ip> upgrade-k8s --to <version> --dry-run
   ```

   It must reach `updating manifests (dry run)` without the immutable-selector
   error.

7. **HUMAN**: the first real Kubernetes upgrade after this removal takes
   `--manifests-no-prune` — mandatory, not advisory:

   ```bash
   talosctl -n <cp-ip> upgrade-k8s --to <version> --manifests-no-prune
   ```

   Without it the manifest sync prunes inventory entries missing from the
   desired set, and `_kubelet-serving-cert-approver__Namespace` is one of them —
   pruning a Namespace cascades to every object the GitOps release owns inside
   it. The flag stays required until the inventory post-check below comes back
   empty. `upgrade-k8s` runs the manifest stage **last**, so a failure here
   arrives after the control plane and kubelets have already moved.

## Post-checks

- The approver still approves: pick a node, `kubectl get csr` shows kubelet
  serving CSRs reaching `Approved,Issued` rather than accumulating Pending.
  `kubectl top nodes` and `kubectl logs` against a pod both still work — each
  depends on a valid kubelet serving cert.
- `kubectl get deploy kubelet-serving-cert-approver -n kubelet-serving-cert-approver
  -o jsonpath='{.spec.template.spec.containers[*].image}'` still reports the
  pinned `0.11.0`.
- No `talos` field manager survives anywhere for this release after the apply —
  check all nine objects, not a sample, and do not scope the sweep to the
  release namespace. A namespace-scoped check passes while a `talos`-managed
  object sits in `default`, which is exactly how the second orphan was missed:

  ```bash
  kubectl get sa,deploy,svc,rolebinding -n kubelet-serving-cert-approver --show-managed-fields -o json
  kubectl get rolebinding events:kubelet-serving-cert-approver -n default --show-managed-fields -o json
  kubectl get ns/kubelet-serving-cert-approver \
    clusterrole/certificates:kubelet-serving-cert-approver \
    clusterrole/events:kubelet-serving-cert-approver \
    clusterrolebinding/kubelet-serving-cert-approver --show-managed-fields -o json
  ```

  Both deletions from step 5 should read back NotFound; every object that
  remains should list `argocd-controller` and no `talos`.
- ArgoCD app `platform-kubelet-serving-cert-approver` stays Synced + Healthy.
- Next `talops status` run shows no config drift.
- After the first real `upgrade-k8s`, the inventory has dropped these keys —
  until then prune is still working from a stale desired set:

  ```bash
  kubectl -n kube-system get cm talos-bootstrap-manifests-inventory \
    -o jsonpath='{.data}' | tr ',' '\n' | grep -i cert-approver
  ```

  Expect eight keys before and none after: four cluster-scoped
  (`_kubelet-serving-cert-approver__Namespace`, the two `ClusterRole`s, the
  `ClusterRoleBinding`), three in the release namespace (`Service`,
  `ServiceAccount`, `Deployment`), and one keyed to `default` —
  `default_events__kubelet-serving-cert-approver_rbac.authorization.k8s.io_RoleBinding`.
  The inventory carries only the `default` RoleBinding, not the chart's copy in
  the release namespace, which is a second way to tell the two apart. The
  `__Namespace` key is the one that matters: it is the only entry whose prune
  would cascade to objects the GitOps release owns.

## Abort criteria

- Kubelet serving CSRs start piling up Pending after the apply → the approver
  Deployment is not running or lost its RBAC. Confirm the ArgoCD app is Synced
  and its pod is Ready; if RBAC objects were removed by mistake, sync the app to
  restore them. Nothing needs to be restored from Talos.
- A `talos` field manager appears on an object after the apply → the entry was
  not actually removed from the config that got applied. Re-check the applied
  node YAML before deleting anything else.
- Step 5 deleted the RoleBinding in the release namespace instead of the one in
  `default` → recoverable, and not a reason to stop: hard-refresh and sync
  `platform-kubelet-serving-cert-approver` to render it again, then delete the
  `default` copy. The mistake is only silent in the other direction — leaving
  the `default` copy behind, which nothing will ever report.
