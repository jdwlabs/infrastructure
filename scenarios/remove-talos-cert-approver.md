# Runbook: Remove the Talos-bundled kubelet-serving-cert-approver

Status: PLANNED — every `talosctl apply-config` and `kubectl delete` in this
runbook is executed by a human. The agent contract forbids autonomous cluster
mutation.

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

It is latent rather than active. What makes it worth closing now is that the
trigger is scheduled: the control-plane metrics work applies machine config to
these same nodes, and that apply is the event that would fetch whatever `main`
points at by then.

The extraManifests entry has been removed from the template. Talos does NOT
garbage-collect resources created by removed extraManifests — the objects it
created stay until deleted by hand.

### What each side actually owns

Field managers across the whole release, not one sampled object
(`kubectl get <obj> --show-managed-fields`):

| Object | Managers |
| --- | --- |
| `Namespace/kubelet-serving-cert-approver` | `argocd-controller(Apply)`, `talos(Update)` |
| `ServiceAccount/kubelet-serving-cert-approver` | `argocd-controller(Apply)`, `talos(Update)` |
| `Deployment/kubelet-serving-cert-approver` | `argocd-controller(Apply)`, `kube-controller-manager(Update)` |
| `ClusterRole/certificates:kubelet-serving-cert-approver` | `argocd-controller(Apply)`, `talos(Update)`, `argocd-application-controller(Update)` |
| `ClusterRole/events:kubelet-serving-cert-approver` | `argocd-controller(Apply)`, `talos(Update)`, `argocd-application-controller(Update)` |
| `ClusterRoleBinding/kubelet-serving-cert-approver` | `argocd-controller(Apply)`, `talos(Update)`, `argocd-application-controller(Update)` |
| `RoleBinding/events:kubelet-serving-cert-approver` | `argocd-controller(Apply)`, `argocd-application-controller(Update)` |
| `Service/kubelet-serving-cert-approver` | `talos(Update)` — **only owner** |

Talos writes with `operation: Update`, not `Apply`, so it never competed for
server-side-apply co-ownership of the Deployment's image field. Every object the
GitOps release renders is already owned by ArgoCD, which is why removing the
entry hands over cleanly.

The `Namespace` carries a `governance-platform` tracking id — it comes from the
tenant envelope, not from this chart, which is why the chart sets
`namespace.create: false`.

### The Service is dead, not orphaned-but-working

`Service/kubelet-serving-cert-approver` is the one object with no GitOps owner,
and it has never routed traffic in this cluster:

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

## Preconditions

1. The talops change removing the extraManifests entry is merged to `main`.
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
5. **HUMAN**: delete the orphaned Service (Talos will not). Nothing else from
   the upstream manifest needs deleting — every other object is owned and
   continuously reconciled by the GitOps release, so deleting them would only
   make ArgoCD recreate them:

   ```bash
   kubectl -n kubelet-serving-cert-approver get svc kubelet-serving-cert-approver
   kubectl -n kubelet-serving-cert-approver delete svc kubelet-serving-cert-approver
   ```

   The matching EndpointSlice is garbage-collected with its Service.

## Post-checks

- The approver still approves: pick a node, `kubectl get csr` shows kubelet
  serving CSRs reaching `Approved,Issued` rather than accumulating Pending.
  `kubectl top nodes` and `kubectl logs` against a pod both still work — each
  depends on a valid kubelet serving cert.
- `kubectl get deploy kubelet-serving-cert-approver -n kubelet-serving-cert-approver
  -o jsonpath='{.spec.template.spec.containers[*].image}'` still reports the
  pinned `0.11.0`.
- No `talos` field manager reappears on any object in the release after the
  apply — check all of them, not a sample:
  `kubectl get ns,sa,deploy,svc -n kubelet-serving-cert-approver --show-managed-fields -o json`
  plus the two ClusterRoles, the ClusterRoleBinding, and the RoleBinding.
- ArgoCD app `platform-kubelet-serving-cert-approver` stays Synced + Healthy.
- Next `talops status` run shows no config drift.

## Abort criteria

- Kubelet serving CSRs start piling up Pending after the apply → the approver
  Deployment is not running or lost its RBAC. Confirm the ArgoCD app is Synced
  and its pod is Ready; if RBAC objects were removed by mistake, sync the app to
  restore them. Nothing needs to be restored from Talos.
- A `talos` field manager appears on an object after the apply → the entry was
  not actually removed from the config that got applied. Re-check the applied
  node YAML before deleting anything else.
