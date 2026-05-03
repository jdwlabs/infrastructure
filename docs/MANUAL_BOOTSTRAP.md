# Manual Bootstrap Requirements & Persistent Configuration Overrides

This document tracks components that are not currently managed by ArgoCD and require manual intervention or patching if the cluster is re-initialized (e.g., via `infrastructure/bootstrap`).

## kubelet-serving-cert-approver

The `kubelet-serving-cert-approver` is currently deployed as part of the initial bootstrap process and is not monitored by ArgoCD.

### Required Manual Patches (Post-Bootstrap)
If the cluster is re-bootstrapped, apply the following patches to ensure stability:

1. **Permissions Patch:** Set container security context to run as non-root to satisfy `restricted` Pod Security Standards.
   ```bash
   # Patch to set runAsUser/runAsGroup
   kubectl patch deployment -n kubelet-serving-cert-approver kubelet-serving-cert-approver -p '{"spec":{"template":{"spec":{"containers":[{"name":"cert-approver","securityContext":{"runAsUser":65534,"runAsGroup":65534}}]}}}}'
   ```

2. **Probe Delay Patch:** Increase initialization delays for liveness/readiness probes to prevent crash-looping during startup.
   ```bash
   # Patch to increase delay
   kubectl patch deployment -n kubelet-serving-cert-approver kubelet-serving-cert-approver -p '{"spec":{"template":{"spec":{"containers":[{"name":"cert-approver","livenessProbe":{"initialDelaySeconds":30},"readinessProbe":{"initialDelaySeconds":30}}]}}}}'
   ```
