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

- The interim `vxlan-offload-fix` DaemonSet **no longer exists** — it was
  retired from the platform repo once this rollout completed, and it is not
  present on the cluster. The Talos `EthernetConfig` layer below is now the
  only thing disabling the offload, so there is **no second safety net**: a
  node whose `EthernetConfig` is removed or overridden is unprotected
  immediately. Re-read this precondition before re-running the rollout — the
  original text asserted the DaemonSet was still in place, which was true
  only during the migration.
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
   → `node-worker-304`
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

### DaemonSet removal (after full rollout) — already performed

This step has been completed: the DaemonSet is gone from the platform repo and
from the cluster. The steps are kept because they are the procedure to re-run
if the interim DaemonSet is ever reintroduced.

Only after all 8 nodes pass verification (3 control plane + 5 workers; the
count grew when the fifth worker was added, so a run that stops at 7 leaves a
node unverified):

1. Open a follow-up PR in the **platform** repo removing
   `tenants/platform/services/vxlan-offload-fix/` and its `tenant.yaml`
   service entry; let ArgoCD sync the deletion.
2. Re-run step 3 (both directions) against at least one worker and one
   control plane with the DaemonSet gone — this is the proof that the
   Talos layer alone holds.
3. Keep monitoring for the original symptom (cross-node TCP timeouts with
   healthy ICMP) for a full reboot cycle of the cluster.

### Rollback

The DaemonSet is no longer a safety net — it has been removed, so the Talos
layer is the only protection and a rollback of that layer leaves the node
exposed until something re-applies the offload settings.

To roll back the Talos layer itself, revert the patch-template commit, rebuild
`talops`, regenerate configs, and re-apply per node (a no-reboot change).

To restore protection quickly without the Talos layer, revert the platform
commit that retired `tenants/platform/services/vxlan-offload-fix/` and let
ArgoCD sync it back — roughly one sync interval. Verify the DaemonSet is
actually `READY` on the affected node before treating protection as restored;
its absence is what makes the Talos layer load-bearing today.

Fastest per-node restore, if a single node is left with the offload enabled:
re-apply that node's machine config, or set the offload back directly with
`talosctl -n <node-ip> patch machineconfig` re-adding the `EthernetConfig`
document. Confirm with `talosctl -n <node-ip> get ethernetstatus flannel.1`
before declaring the node healthy.

## iSCSI initiator timeouts — Talos-layer rollout

### Background: no iscsid.conf on these nodes

A transient stall on the NAS took an ext4 volume permanently read-only. The
chain: the initiator's NOP-Out ping went unanswered for five seconds, the
connection was torn down, session recovery did not complete inside the
120-second replacement window, the SCSI layer returned `EIO`, ext4 aborted the
journal and latched the mount read-only. The session recovered on its own
minutes later and the device was never faulty — but a read-only-latched ext4
mount does not heal when the device comes back. It stays read-only until the
volume is unmounted and the journal replayed, so a network-length event
becomes an outage that needs a human.

It happened twice in one day, which rules out a one-off.

Those two numbers — a 5-second NOP-Out timeout inside a 120-second replacement
window — are not configured anywhere. They are open-iscsi's compiled-in
defaults, and they apply because **there is no `iscsid.conf` on these nodes at
all**:

```bash
talosctl -n <worker-ip> ls -l /etc/iscsi
# initiatorname.iscsi is the only entry
```

The `iscsi-tools` extension builds open-iscsi with `-Dhomedir=/etc/iscsi`, so
`/etc/iscsi/iscsid.conf` is the file it would read, then deletes its own `/etc`
during install on the grounds that Talos generates the initiator name itself.
Nothing puts a config file back. Confirm the resulting values on any attached
volume by reading the node record the initiator wrote:

```bash
talosctl -n <worker-ip> ls -r /var/lib/iscsi/nodes
talosctl -n <worker-ip> read \
  '/var/lib/iscsi/nodes/<target-iqn>/<ip>,<port>,<tpgt>/default' \
  | grep -E 'noop_out|replacement_timeout'
```

A record is a file, not a directory: open-iscsi keys it
`nodes/<iqn>/<ip>,<port>,<tpgt>/<iface>`, and with no bound iface the leaf is
`default`. `ls -r` above prints the exact path — read that, not the portal
directory, or the read errors.

Once a record exists it *is* the live configuration for that target. It is
written from `iscsid.conf` when the record is first created and then persists
under `/var/lib/iscsi/nodes` on the `/var` partition, surviving logout,
re-stage and reboot, because `iscsiadm --login` reuses an existing record
rather than re-deriving it. Shipping an `iscsid.conf` therefore only changes
what *new* records get. Every target already attached — including the one on
the worker that broke, whose record pins `replacement_timeout = 120` — keeps
its old values until that record is deleted. **Applying this change to a node
does not protect the volume this work is about; deleting its stale record
does.** Step 4 of the rollout is where that happens.

### Mechanism: a Talos `ExtensionServiceConfig` document

The defaults suit a multipath SAN, where abandoning an unresponsive path in
five seconds costs nothing because a second path picks the I/O up. There is one
portal to the NAS and no second path, so abandoning it costs the filesystem.
The worker role patch therefore ships an `iscsid.conf` carrying a 30-second
NOP-Out timeout, a 10-second NOP-Out interval and a 300-second replacement
timeout; the reasoning for each number is in the comment above the document in
`bootstrap/internal/talos/patches/worker.yaml`.

Why `ExtensionServiceConfig` rather than the alternatives:

- **`machine.files`** writes into `/etc` through a bind-mount overlay. It is
  the obvious route and it is the wrong one: it is reported upstream to leave
  nodes completely unresponsive after the next reboot, which is a far worse
  failure than the one being fixed.
- **`ExtensionServiceConfig`** is the supported way to hand a config file to an
  extension service. Talos writes the content under its own state directory and
  bind-mounts it read-only at the requested path inside the service container.
  That is the mount namespace that matters here: the CSI driver does not carry
  its own initiator, it runs the host binary through
  `nsenter --mount=/proc/<iscsid-pid>/ns/mnt`, so the `iscsiadm` that creates
  node records reads the `iscsid.conf` visible to the `ext-iscsid` service.
- **Rebuilding the extension** with a baked-in config would change the Image
  Factory schematic ID, which is a schematic migration rather than a config
  change — much larger, and it puts the values somewhere no one thinks to look.

**Unverified before rollout, and the reason the first node is a canary.** The
extension bind-mounts host `/etc/iscsi` into its container read-only, and
`iscsid.conf` does not exist inside it. Whether the runtime can create that
mountpoint on a read-only parent to land the bind mount has not been proven
here — only that the document itself validates (`talosctl validate`). If it
cannot, `ext-iscsid` fails to start and **that node loses iSCSI entirely**,
which takes down every attached volume on it. Prove it on one drained worker
before going near a second.

### iSCSI rollout plan (node-by-node)

Preconditions:

- Rebuild `talops` so the embedded patch template carries the new document:
  `cd bootstrap && go build -buildvcs=false -o build/ ./...`.
- Pick a canary worker with **no** attached iSCSI volume, or drain the one it
  has first. `talosctl -n <worker-ip> ls /var/lib/iscsi/nodes` lists what is
  attached; an empty or missing directory means the node is free.
- Control-plane nodes are deliberately out of scope. They attach no iSCSI
  volumes — `/var/lib/iscsi/nodes` does not exist on any of them — so the
  control-plane role patch is untouched.

Order: canary worker first and fully verified, then the remaining workers one
at a time.

1. Regenerate and inspect the config without applying:

   ```bash
   talosctl machineconfig patch clusters/core/secrets/worker.yaml \
     --patch @<rendered-worker-patch> -o /tmp/worker-check.yaml
   talosctl validate --config /tmp/worker-check.yaml --mode metal
   ```

   The `ExtensionServiceConfig` document named `iscsid` must be present.
2. **HUMAN**: apply to the canary worker. Applying a changed
   `ExtensionServiceConfig` restarts `ext-iscsid`, which is why the node is
   drained first.
3. The service must come back — this is the step the canary exists for:

   ```bash
   talosctl -n <worker-ip> services | grep ext-iscsid
   talosctl -n <worker-ip> logs ext-iscsid | tail -20
   ```

   Two conditions, both required. `services` reports `Running`, **and** the
   lines emitted *after* the restart no longer contain

   ```text
   can't open iscsid.safe_logout configuration file /etc/iscsi/iscsid.conf
   ```

   Talos appends to a service's log across restarts, so grep the tail rather
   than the whole file — the pre-restart occurrence is still in there and will
   match. Every node emits that line today, and its absence from the lines that
   follow the restart is the only direct evidence that `iscsid` opened the file
   rather than the machine config merely carrying it. `talosctl get
   extensionserviceconfigs` is not evidence: that resource is rendered *from*
   the machine config, so it can only ever confirm what was applied, never what
   the service can see. Either condition failing means the config-file mount
   did not land; roll back that node immediately (see below) before the drain
   is lifted.
4. **Delete the stale node record for the target before reading anything
   back.** Records persist across logout, re-stage and reboot (see Background),
   so a target that has been attached on this node before reads back its *old*
   values however correctly the mount landed — which would abort a correct
   rollout at step 5. With the volume detached, delete the record:

   ```bash
   kubectl -n democratic-csi exec <iscsi-node-pod-on-the-canary> \
     -c csi-driver -- iscsiadm -m node -T <target-iqn> -p <portal> -o delete
   ```

   Go through the CSI node pod, not the Talos host. There is no shell on a
   Talos node to enter the `ext-iscsid` mount namespace from, and `talosctl`
   has no verb that deletes a file, so the record cannot be removed from the
   host side. The driver's `iscsiadm` is a wrapper that already does the
   `nsenter` into `iscsid`'s mount namespace, which is both the namespace the
   record lives in and the one this change's `iscsid.conf` is mounted into.

   A canary with no prior attachment has nothing to delete; check with
   `talosctl -n <worker-ip> ls -r /var/lib/iscsi/nodes` first.

   This is a rollout step, not just a verification aid: until a target's record
   is deleted, that target keeps the timeouts this change exists to replace.
5. Prove the values reach a freshly created node record. They are read at
   record-creation time, so schedule a workload onto the node and read the new
   record back:

   ```bash
   talosctl -n <worker-ip> read \
     '/var/lib/iscsi/nodes/<target-iqn>/<ip>,<port>,<tpgt>/default' \
     | grep -E 'noop_out|replacement_timeout'
   ```

   Expect `noop_out_interval = 10`, `noop_out_timeout = 30`,
   `replacement_timeout = 300`. Old values here — *after* the record was
   deleted in step 4 — mean the mount landed but the initiator is not reading
   it; stop, do not roll further.
6. **Check the blast radius before the second node.** Longhorn attaches its own
   volumes over iSCSI on the same workers and inherits the same defaults today
   (`replacement_timeout = 120`, both NOP-Out values `5`). Whether its
   initiator calls run in the `ext-iscsid` mount namespace or the host's has
   not been established, so whether Longhorn picks these values up is an open
   question rather than a known outcome. Read back a freshly created Longhorn
   node record (`iqn.2019-10.io.longhorn:*`) on the canary and record which way
   it went. If Longhorn does inherit them, the trade is different for it: its
   target is a pod on the same node, and when that pod is recreated at a new
   address reconnection is impossible, so a longer replacement timeout only
   lengthens the hang before the same failure.

   **If it did not inherit them, that is an accepted end state, not a
   blocker.** It is the likelier result: Longhorn's manager nsenters into PID
   1's mount namespace, where no `iscsid.conf` exists. The node then carries two
   initiator configurations at once — records created through the CSI driver's
   `ext-iscsid` namespace get the new values, records created by Longhorn keep
   open-iscsi's compiled defaults. Write down which way it went and roll on.
   Longhorn is not the workload this change exists to protect, and by the trade
   above it is arguably better served by the short defaults. Do not chase parity
   by moving the file into PID 1's namespace with `machine.files` — that is the
   route ruled out at the top of this section, and the reason it was ruled out
   has not changed.
7. Repeat 2–5 for each remaining worker.
8. Reboot the canary once and re-run **step 3**. Step 5 is the wrong gate here
   and would pass regardless: the node record persists on `/var` across the
   reboot and the post-reboot re-stage reuses it rather than re-deriving it, so
   it would still read `300` even if the config-file mount failed to re-land at
   boot — which is precisely the unproven risk this rollout is canarying. Only
   `ext-iscsid` reporting `Running` with no `can't open …/iscsid.conf` line
   after the reboot proves the mount came back. Re-running step 5 on top of
   that is still worth doing, but as a record-persistence check only; for
   end-to-end proof after a reboot, delete and re-create the record (steps 4
   and 5) rather than reading the surviving one.

### iSCSI rollback

Per node, and immediately if `ext-iscsid` does not return to `Running`: revert
that node's machine config to the previous revision and re-apply. The node
returns to open-iscsi's compiled defaults — the exposure this change exists to
remove, but a working initiator.

Fleet-wide: revert the patch-template commit, rebuild `talops`, regenerate and
re-apply per node. No reboot is required, but `ext-iscsid` restarts on each
node, so drain each one first.

There is no second safety net. Until every worker is rolled, nodes are in two
different states, and a volume's exposure depends on which worker it lands on.
