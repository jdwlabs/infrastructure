# Runbook: Remove the Talos-bundled metrics-server

Status: PLANNED — every `talosctl apply-config` and `kubectl delete` in this
runbook is executed by a human. The agent contract forbids autonomous cluster
mutation.

## Why

The cluster runs two metrics-servers:

- `platform-metrics-server` (ns `metrics-server`) — the platform GitOps
  release, managed by ArgoCD. This is the one that BACKS the
  `v1beta1.metrics.k8s.io` APIService (Available=True).
- `metrics-server` in `kube-system` — no Helm/ArgoCD provenance. It was
  installed by the Talos machine config: `cluster.extraManifests` in the
  embedded talops patch template
  (`bootstrap/internal/talos/patches/control-plane.yaml`) pulled
  `https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml`.
  It consumes ~200Mi of requests and is referenced by nothing.

The extraManifests entry has been removed from the template. Talos does NOT
garbage-collect resources created by removed extraManifests — the kube-system
copy stays until deleted by hand.

## Preconditions

1. The talops change removing the extraManifests entry is merged to `main`.
2. Platform release healthy: `kubectl -n metrics-server get deploy
   platform-metrics-server` shows Available, and
   `kubectl get apiservice v1beta1.metrics.k8s.io -o wide` shows
   `Available=True` with service `metrics-server/platform-metrics-server`.
3. Cluster quiet: no in-flight node upgrades or CP maintenance.

## Sequence

1. Rebuild talops so the embedded template no longer carries the entry:
   `cd bootstrap && go build -o build/ ./...` (or `build.bat`).
2. Regenerate node configs without contacting any node (talops auto-hydrates
   the vault): `talops reconcile --generate-only` resolves the secrets dir
   from `--cluster`, `CLUSTER_NAME`, or `cluster_name` in `terraform.tfvars`
   and rewrites every YAML under `clusters/<name>/nodes/` from the current
   templates. Inspect a regenerated CP config and confirm
   `cluster.extraManifests` lists only `kubelet-serving-cert-approver`.
   (A plain `talops reconcile` also detects the template change on its own —
   it compares the recorded template hash in `bootstrap-state.json` against
   the current template inputs, regenerates stale YAMLs, and lists the
   affected nodes under "Update N node config(s)" in the plan. State files
   written before template-hash tracking regenerate configs on the first run
   and backfill the hash without applying anything if the YAML is unchanged.)
3. **HUMAN**: apply the regenerated config to the three control planes, one
   at a time (workers carry the same cluster section but extraManifests only
   acts on control planes; applying to workers too keeps drift at zero):
   `talosctl -n <cp-ip> apply-config -f clusters/core/nodes/node-control-plane-<vmid>.yaml`
   — extraManifests is not a machine-section change; no reboot is expected.
   After each CP: `talosctl -n <cp-ip> etcd status` healthy before the next.
4. **HUMAN**: delete the orphaned kube-system copy (Talos will not):

   ```bash
   kubectl -n kube-system delete deployment metrics-server
   kubectl -n kube-system delete service metrics-server
   # RBAC from the same upstream manifest (names are distinct from the
   # platform release, which prefixes everything with platform-metrics-server;
   # verify with the get before each delete):
   kubectl -n kube-system delete serviceaccount metrics-server
   kubectl -n kube-system delete rolebinding metrics-server-auth-reader
   kubectl delete clusterrole system:metrics-server system:aggregated-metrics-reader
   kubectl delete clusterrolebinding system:metrics-server metrics-server:system:auth-delegator
   ```

   Do NOT delete the `v1beta1.metrics.k8s.io` APIService — it is owned by the
   platform release and points at `metrics-server/platform-metrics-server`.

## Post-checks

- `kubectl get apiservice v1beta1.metrics.k8s.io` stays `Available=True`.
- `kubectl top nodes` and `kubectl top pods -A` return data (served by the
  platform release).
- `kubectl -n kube-system get deploy,svc -l k8s-app=metrics-server` returns
  nothing.
- Next `talops status` run shows no config drift.

## Abort criteria

- APIService flips to `Available=False` after the kube-system deletion →
  the platform release was not actually backing it; re-check
  `kubectl get apiservice v1beta1.metrics.k8s.io -o yaml` service ref and
  restore by letting ArgoCD sync the platform release before retrying.
