# Operations

Operational runbooks for the jdwlabs core cluster. For system design, see
[ARCHITECTURE.md](ARCHITECTURE.md).

## VXLAN tx-checksum offload fix — Talos-layer rollout

### Background

On virtio_net VMs, `flannel.1` keeps the `tx-checksum-ip-generic` offload
enabled, which leaves inner TCP checksums unfilled after VXLAN encapsulation.
The receiving node's conntrack marks those packets `INVALID` and kube-proxy's
nftables backend drops them, silently blackholing cross-node pod TCP while
ICMP still passes. `rx-checksumming` is fixed-on under virtio_net, so the fix
must be sender-side `tx` offload disablement on **every** node.

The interim mitigation is a privileged `hostNetwork` DaemonSet
(`vxlan-offload-fix` in the platform repo) that re-runs `ethtool -K` in a
loop. The permanent fix moves this to the Talos machine-config layer.

### Mechanism: Talos `EthernetConfig` documents

Both role patch templates (`bootstrap/internal/talos/patches/{control-plane,worker}.yaml`)
append two `EthernetConfig` documents — one for `flannel.1`, one for the
physical uplink — disabling `tx-checksum-ip-generic`,
`tx-generic-segmentation`, `tx-tcp-segmentation`, `tx-tcp6-segmentation`, and
`rx-gro` (the same set the DaemonSet disables via
`ethtool -K tx-checksum-ip-generic/tx/gso/gro/tso off`).

Why `EthernetConfig` over the alternatives:

- **`EthernetConfig`** is the native, declarative Talos mechanism for ethtool
  link settings and is what upstream Talos documents for exactly this
  flannel-on-virtualized-NIC checksum conflict. It lives in the machine
  config, so it survives reboot and upgrade by construction. The
  `network.EthernetSpecController` retries with backoff while a named link
  does not yet exist, so the settings land on `flannel.1` shortly after
  flannel creates it at each boot. No privileged workload, no custom image.
- **Oneshot ethtool ExtensionService / ExtensionServiceConfig**: requires
  building and hosting a custom system extension image (Talos ships no
  ethtool extension) and a oneshot would race flannel.1 creation at boot —
  strictly worse than the built-in controller.
- **udev rules**: Talos's udevd has no ethtool binary to `RUN+=`, so a rule
  cannot apply the setting.
- **Kernel module parameters**: virtio_net exposes no parameter to disable
  tx checksum offload.

Known residual gap: the controller re-applies on spec changes and on its own
restart/boot, but does not watch link recreation. Flannel reuses an existing
`flannel.1` unless its VXLAN attributes change, so mid-life recreation is
rare (effectively only flannel/CNI upgrades). After any flannel or Talos
upgrade, re-check `talosctl get ethernetstatus flannel.1` per node. The
DaemonSet stays in place until the full rollout is verified, so there is no
unprotected window during migration.

### Rollout plan (node-by-node)

Preconditions:

- Interim `vxlan-offload-fix` DaemonSet is still deployed and healthy — it
  applies the identical settings, so each node is protected before, during,
  and after its config update.
- Rebuild `talops` so the embedded patch templates include the
  `EthernetConfig` documents: `cd bootstrap && go build -o build/ ./...`
  (or `build.bat`).
- Regenerate and validate node configs without applying:
  render the role patch, apply with
  `talosctl machineconfig patch clusters/core/secrets/<role>.yaml --patch @<patch> -o <out>`
  and inspect that the two `EthernetConfig` documents are present.
  `talops reconcile` resolves the secrets dir from `--cluster`,
  `CLUSTER_NAME`, or the `cluster_name` in `terraform.tfvars`; no
  secrets-dir override is needed.

Order: workers first (smaller blast radius), then control planes one at a
time with etcd health checks between each.

1. `node-worker-300` → `node-worker-301` → `node-worker-302` → `node-worker-303`
2. `node-control-plane-200` → `node-control-plane-201` → `node-control-plane-202`
   (after each: `talosctl -n <ip> etcd status` healthy before proceeding)

Per node:

1. Apply the regenerated config:
   `talosctl -n <node-ip> apply-config -f clusters/core/nodes/<node-file>.yaml`
   (EthernetConfig is dynamic network config — applies without reboot).
2. Confirm the settings took effect:

   ```bash
   talosctl -n <node-ip> get ethernetstatus flannel.1 -o yaml | grep -E 'tx-checksum-ip-generic|tx-generic-segmentation|tx-tcp-segmentation|tx-tcp6-segmentation|rx-gro'
   talosctl -n <node-ip> get ethernetstatus eth0 -o yaml | grep -E 'tx-checksum-ip-generic|tx-generic-segmentation|tx-tcp-segmentation|tx-tcp6-segmentation|rx-gro'
   ```

   All five features must report `false`/off.
3. Cross-node TCP verification — **both directions** (the fault is
   sender-side, so test the node as sender *and* as receiver):

   ```bash
   # Sender side: pod pinned to the just-updated node, TCP to other nodes
   kubectl run nettest-tx --rm -i --restart=Never \
     --image=nicolaka/netshoot \
     --overrides='{"spec":{"nodeName":"<node-name>"}}' -- sh -c '
       curl -skm5 https://10.96.0.1:443/version >/dev/null && echo apiserver-ok;
       dig +tcp +time=3 kubernetes.default.svc.cluster.local @10.96.0.10 >/dev/null && echo dns-tcp-ok'

   # Receiver side: pod on a DIFFERENT node, TCP to a pod hosted on this node
   kubectl get pods -A -o wide --field-selector spec.nodeName=<node-name>   # pick a target pod IP + port
   kubectl run nettest-rx --rm -i --restart=Never \
     --image=nicolaka/netshoot \
     --overrides='{"spec":{"nodeName":"<other-node-name>"}}' -- \
     nc -zvw3 <target-pod-ip> <target-port>
   ```
4. Reboot-persistence check (first worker and first control plane at
   minimum): `talosctl -n <node-ip> reboot`, wait for `Ready`, then repeat
   steps 2–3. This proves the settings re-land on the freshly recreated
   `flannel.1`.

### DaemonSet removal (after full rollout)

Only after all 7 nodes pass verification:

1. Open a follow-up PR in the **platform** repo removing
   `tenants/platform/services/vxlan-offload-fix/` and its `tenant.yaml`
   service entry; let ArgoCD sync the deletion.
2. Re-run step 3 (both directions) against at least one worker and one
   control plane with the DaemonSet gone — this is the proof that the
   Talos layer alone holds.
3. Keep monitoring for the original symptom (cross-node TCP timeouts with
   healthy ICMP) for a full reboot cycle of the cluster.

### Rollback

The DaemonSet is independent of the Talos config and remains the safety net
until the removal PR merges. To roll back the Talos layer itself, revert the
patch-template commit, rebuild `talops`, regenerate configs, and re-apply
per node (also a no-reboot change). If the DaemonSet was already removed,
re-deploying it via ArgoCD revert restores protection within one sync.
