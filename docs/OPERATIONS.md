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

### Mechanism: a forked iscsi-tools extension with the config baked in

Two runtime mechanisms for delivering this file were tried first, and both are
proven broken on these nodes — on a drained canary, not in theory:

- **`ExtensionServiceConfig`** — what this runbook previously described — is
  structurally the wrong mechanism for this extension. The extension's own
  service spec already bind-mounts the host's `/etc/iscsi` into the container
  read-only, and `iscsid.conf` does not exist inside it, so by the time Talos
  injects the config-file mount there is no writable mountpoint left to create
  ("read-only file system"). `ext-iscsid` enters a restart-forever loop with
  **no working iSCSI initiator on the node**. The document validates and
  applies without complaint; the failure only appears when the service
  restarts. Recovery was minutes over the Talos API (remove the document,
  re-apply, restart the service), but the mechanism is dead.
- **`machine.files`** hits a hard Talos constraint: a `create` outside `/var`
  is rejected — `create operation not allowed outside of /var` — and the
  rejection happens in the boot-time `writeUserFiles` task, which blocks the
  `cri` service from ever registering. The node never joins Kubernetes and
  Talos self-schedules a retry reboot 35 minutes out. Recovery required
  regenerating the node's clean config from the unmodified template and
  re-applying with `--mode=reboot`. `op: overwrite` was not attempted and
  cannot rescue the approach: there is no `/etc/iscsi/iscsid.conf` on the host
  to overwrite, and an overwrite that fails fails in the same boot-blocking
  task with the same blast radius.

What remains is the option originally set aside as the bigger lift: fork the
extension so the file is part of the image and **no runtime write or runtime
mount injection is needed at all**. `extensions/iscsi-tools/` holds the fork:

- The Dockerfile layers three files onto the upstream image, pinned by digest
  to the exact build the current Image Factory schematic ships (the floating
  `v0.2.0` tag has been re-pushed upstream since and no longer matches).
- `iscsid.conf` carries the tuned values and their full rationale:
  `noop_out_interval` 10s, `noop_out_timeout` 30s, `replacement_timeout` 300s.
- The service spec inverts the `/etc/iscsi` mount: the extension's own
  `/usr/local/etc/iscsi` (containing `iscsid.conf` and a placeholder
  `initiatorname.iscsi`) becomes the container's `/etc/iscsi`, and the host's
  Talos-generated `initiatorname.iscsi` is file-bind-mounted over the
  placeholder. Both mount-ordering constraints are documented in the spec
  itself; the arrangement never asks the runtime to create a mountpoint on a
  read-only filesystem, which is the exact operation that killed the
  `ExtensionServiceConfig` route.
- The `Extension Image` workflow builds the extension, asserts the baked
  content, and on merge to main publishes `ghcr.io/jdwlabs/iscsi-tools` plus a
  custom nocloud installer `ghcr.io/jdwlabs/talos-installer` assembled by the
  matching `imager` from the fork and the digest-pinned `qemu-guest-agent` the
  current schematic also carries.

Delivery is `talosctl upgrade --image` per node, and that is where the risk
story improves over both dead mechanisms: the failure surface moves from
apply/boot time on a live node to build time in CI, and a Talos upgrade keeps
the previous boot image — a new image that fails to boot is automatically
reverted, where a failed `machine.files` write left the node out of the
cluster until a human re-applied a clean config.

Namespace scope is unchanged from the previous design: the file lands in the
`ext-iscsid` mount namespace, which is the namespace the CSI driver's
`iscsiadm` wrapper nsenters into. The host's `/etc/iscsi` (PID 1's namespace)
is untouched, so anything reading it — possibly Longhorn — keeps compiled
defaults; see the Longhorn gate below.

The worker role patch no longer carries the `iscsid` `ExtensionServiceConfig`
document. **Do not apply a machine config rendered before that removal**: a
stale rendered config still carrying the document restarts `ext-iscsid` into
the proven restart loop on the current extension. Rebuild `talops` and
regenerate before any future `apply-config` to a worker.

### iSCSI rollout plan (node-by-node)

Preconditions:

- The `Extension Image` workflow has published both images (it runs on the
  merge to main). Confirm:

  ```bash
  docker manifest inspect ghcr.io/jdwlabs/iscsi-tools:v0.2.0-jdwlabs.1
  docker manifest inspect ghcr.io/jdwlabs/talos-installer:v1.13.4-iscsi.1
  ```

- **HUMAN, one-time**: set both GHCR packages to public visibility (org
  package settings). Nodes pull the installer with no registry credentials
  configured; a private package fails the upgrade at pull time — before
  anything on the node changes, but it stops the rollout.
- Rebuild `talops` and regenerate node configs so the rendered worker configs
  no longer carry the `iscsid` `ExtensionServiceConfig` document:
  `cd bootstrap && go build -buildvcs=false -o build/ ./...`.
- Pick a canary worker with **no** attached iSCSI volume, or drain the one it
  has first. `talosctl -n <worker-ip> ls /var/lib/iscsi/nodes` lists what is
  attached; an empty or missing directory means the node is free. Draining a
  worker that carries Longhorn replicas needs Longhorn's own eviction first —
  set `evictionRequested: true` and `allowScheduling: false` on the
  `nodes.longhorn.io` object, wait for replicas to relocate, then cordon and
  drain; a plain drain is blocked by the instance-manager PDB. This procedure
  is proven on this cluster.
- Control-plane nodes are deliberately out of scope. They attach no iSCSI
  volumes — `/var/lib/iscsi/nodes` does not exist on any of them — and they
  stay on the Image Factory installer. Running workers on the forked installer
  and control planes on the factory one is fine: same Talos version, same
  extension set, one baked config file of difference.

Order: canary worker first and fully verified, then the remaining workers one
at a time.

1. **HUMAN**: upgrade the drained canary to the forked installer:

   ```bash
   talosctl -n <worker-ip> upgrade \
     --image ghcr.io/jdwlabs/talos-installer:v1.13.4-iscsi.1
   ```

   This reboots the node. Talos keeps the previous boot image and reverts to
   it automatically if the new one fails to boot, so the unrecoverable-node
   failure mode of the `machine.files` attempt does not exist here.
2. Confirm the fork is what booted:

   ```bash
   talosctl -n <worker-ip> get extensions
   ```

   `iscsi-tools` must report version `v0.2.0-jdwlabs.1`. The factory version
   string means the node booted the old image — stop and investigate before
   touching the service.
3. The service gate — this is the step the canary exists for:

   ```bash
   talosctl -n <worker-ip> services | grep ext-iscsid
   talosctl -n <worker-ip> logs ext-iscsid | tail -20
   ```

   Two conditions, both required. `services` reports `Running`, **and** the
   log lines from the current boot no longer contain

   ```text
   can't open iscsid.safe_logout configuration file /etc/iscsi/iscsid.conf
   ```

   Every node on the stock extension emits that line at service start, and its
   absence after the upgrade is the direct evidence that `iscsid` opened the
   baked file. Either condition failing means the mount arrangement did not
   land; roll back that node immediately (see below) before the drain is
   lifted.
4. **Delete the stale node record for the target before reading anything
   back.** Records persist across logout, re-stage and reboot (see Background),
   so a target that has been attached on this node before reads back its *old*
   values however correctly the config landed — which would abort a correct
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
   record lives in and the one the baked `iscsid.conf` is visible in.

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
   deleted in step 4 — mean the config is visible but the initiator is not
   reading it; stop, do not roll further.
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
   1's mount namespace, where no `iscsid.conf` exists — the fork deliberately
   leaves the host's `/etc/iscsi` alone. The node then carries two initiator
   configurations at once — records created through the CSI driver's
   `ext-iscsid` namespace get the new values, records created by Longhorn keep
   open-iscsi's compiled defaults. Write down which way it went and roll on.
   Longhorn is not the workload this change exists to protect, and by the trade
   above it is arguably better served by the short defaults. Do not chase
   parity by writing the file into PID 1's namespace with `machine.files` —
   that mechanism is proven to fail the node's boot (see Mechanism above).
7. Repeat 1–5 for each remaining worker, one at a time, drained first.
8. **Adopt the installer reference permanently.** Until `installer_image` in
   the tfvars points at the forked installer, the machine configs still name
   the Image Factory image, and the next `talos-upgrade.md` cycle or node
   reinstall would silently revert to the stock extension. After the fleet is
   rolled, bump `installer_image` (and keep `talos_version` in lockstep) in a
   follow-up change. A future Talos version bump rebuilds the fork first: bump
   `TALOS_VERSION`, the base digests, and both tags in
   `.github/workflows/extension-image.yml`, merge, then upgrade nodes to the
   new installer tag.

### iSCSI rollback

Per node, and immediately if step 2 or 3 fails: upgrade back to the Image
Factory installer the fleet runs today —

```bash
talosctl -n <worker-ip> upgrade \
  --image factory.talos.dev/nocloud-installer/b553b4a25d76e938fd7a9aaa7f887c06ea4ef75275e64f4630e6f8f739cf07df:v1.13.4
```

— which restores the stock extension and open-iscsi's compiled defaults: the
exposure this change exists to remove, but a working initiator. A forked image
that fails to boot at all never needs this; Talos reverts to the previous boot
image on its own.

Rolling back the image does not roll back node records. Any record created
while the fork was live keeps `30/10/300` until it is deleted (step 4's
procedure) and recreated under the stock defaults — the mirror image of the
staleness called out in Background.

There is no second safety net. Until every worker is rolled, nodes are in two
different states, and a volume's exposure depends on which worker it lands on.

## Kernel log shipping to the node-local collector — Talos-layer rollout

### Background: kmsg has no other route off the node

A fault that only the kernel sees currently reaches nobody. Since Linux 6.12
ext4's error handler latches a mount read-only through an internal emergency
bit rather than `SB_RDONLY`, so `/proc/mounts` keeps reporting `rw` throughout
and every layer above the kernel goes on reporting healthy. That is the same
failure mode the iSCSI timeouts above exist to prevent, and when it happens the
ring buffer is the only signal that exists at the moment it happens.

Talos exposes the ring buffer two ways, and only one is consumable by an
unattended collector:

- **The Talos API** (`talosctl dmesg`) needs a talosconfig. A collector pod has
  none, so this cannot be a data source.
- **A push to a destination named in the machine config.** The node dials out
  and streams the ring buffer as newline-delimited JSON. This inverts the usual
  direction — nothing scrapes the node, the node connects to the collector.

`machine.logging.destinations` is **not** this mechanism and does not cover
kmsg. It carries Talos *service* logs only; the ring buffer is a separate
surface with its own config document. Pointing `machine.logging.destinations`
at the collector would ship service logs and still leave kernel faults
invisible.

The collector's own `nodeLogs` feature stays off deliberately and is not an
alternative: it reads systemd journal files, and Talos runs no journald.
Enabling it would mount a path that never has entries and report nothing wrong
while doing it.

### Mechanism: a Talos `KmsgLogConfig` document

Both role patch templates
(`bootstrap/internal/talos/patches/{control-plane,worker}.yaml`) append a
`KmsgLogConfig` document pointing at `tcp://127.0.0.1:6050/`.

The target is loopback because the receiver is the collector pod on that same
node, reached through a hostPort. Talos kernel lines carry **no hostname of
their own**, so only a per-node listener can say truthfully which node produced
a message; the collector stamps the `node` label from its own `spec.nodeName`.
A shared Service in front of all the collectors would erase exactly the
identity the signal is for.

Why `KmsgLogConfig` rather than the `talos.logging.kernel` kernel argument:

- The kernel argument is the other documented route, and it is set through
  `machine.install.extraKernelArgs`. **These nodes cannot use that field.**
  They boot UEFI (`bios = "ovmf"` in `terraform/{control,worker}-nodes.tf`),
  and since Talos 1.10 a UEFI install is systemd-boot with a UKI whose kernel
  cmdline is baked into the image — the field is inert there. From 1.12 it is
  additionally mutually exclusive with `install.grubUseUKICmdline`, which
  defaults on, so `talosctl validate` rejects the pair outright:

  ```text
  * install.extraKernelArgs and install.grubUseUKICmdline can't be used together
  ```

- `KmsgLogConfig` is a normal config document applied at runtime. **No
  reinstall, no `talosctl upgrade`, no reboot** — which is also why this
  section's rollout is much lighter than the two above it.
- Delivery begins when `machined` starts rather than at kernel init, but that
  is **not** a loss of early boot lines: the reader opens `/dev/kmsg` at offset
  0 and follows rather than tailing, so the ring buffer is replayed from the
  start. The real limit is wrap — on a long-uptime node the ring may already
  have overwritten itself, and the reader skips forward silently when it has,
  so contents predating the apply can simply be gone. On a node rebooted or
  freshly applied, the full boot is delivered.
- Should `talos.logging.kernel` ever be added alongside this document, match the
  URL string byte for byte. Cmdline and document destinations are deduplicated
  by exact string compare, so `tcp://host:6050` and `tcp://host:6050/` are two
  destinations and every kernel line would be delivered twice.

Both patch templates used to carry an `extraKernelArgs` block of their own, and
it was a latent failure. It bit only once the base config carried
`grubUseUKICmdline`, which talosctl started emitting at 1.12: the getter
safe-dereferences a missing key to `false`, so a base generated before then
validated clean with `extraKernelArgs` present. The moment the base was
regenerated on v1.13 — which `baseConfigStale()` does on an installer-image
bump — `talosctl validate` would have started failing for a reason unrelated to
kernel logging.

That block has since been deleted, alongside a CI gate that renders both role
configs the way `talops` renders them for a real node and runs
`talosctl validate --mode metal` over the result — so this class of breakage is
now caught in a pull request rather than at step 1 of a runbook. Its `console=`
entries were inert for the same reason the kernel argument is, and redundant
besides: the nocloud image these nodes boot already sets
`console=tty1 console=ttyS0` in the UKI cmdline, and the VMs declare no serial
device for `ttyS0` to reach. Removing them was behaviour-free, and that is no
longer an untested claim.

### Ordering: the platform-side listener lands first

The receiving end is a per-node TCP listener on the `alloy-logs` DaemonSet in
the platform repo. Until it is live, this configuration names a port nothing is
bound to. **Nothing breaks in that window** — the connection is refused, and a
refused log destination costs a retry and nothing else — but the rollout proves
nothing until the listener exists, so land the platform change first.

### Kernel log rollout plan (canary first)

Preconditions:

- The platform-side listener is merged and the `alloy-logs` DaemonSet is
  `READY` on the canary node. Confirm before starting, not after.
- Rebuild `talops` so the embedded templates carry the document:
  `cd bootstrap && go build -buildvcs=false -o build/ ./...`.

Order: one canary worker, fully verified, then the remaining workers, then the
control planes. Nothing here restarts a service or reboots a node, so no drain
is required.

1. Regenerate and inspect the config without applying:

   ```bash
   talosctl machineconfig patch clusters/core/secrets/worker.yaml \
     --patch @<rendered-worker-patch> -o /tmp/worker-check.yaml
   talosctl validate --config /tmp/worker-check.yaml --mode metal
   ```

   A `KmsgLogConfig` document named `node-local-collector` with
   `url: tcp://127.0.0.1:6050/` must be present.

   **`validate` must pass cleanly.** Any error at all is yours — stop and fix it
   before applying anything.

   An earlier revision of this runbook told you to expect a single failure
   naming `install.extraKernelArgs`. That block has been removed from the patch
   templates and CI now validates the rendered configs on every change, so a
   clean result is the only acceptable one.
2. **HUMAN**: apply the config to the canary. It takes effect immediately —
   there is no reboot and no upgrade in this rollout.
3. Confirm Talos accepted the document:

   ```bash
   talosctl -n <node-ip> get kmsglogconfig -o yaml
   ```

   `-o yaml` is required: the default table form does not render the
   destination list, so it cannot show whether the URL is the intended one.
   Confirm `spec.destinations` carries `tcp://127.0.0.1:6050/`.

   This confirms only what was applied, never that anything is arriving. Step 4
   is the gate.
4. Confirm lines are actually arriving (next section). **This is what the
   canary exists for.**
5. Repeat 2–4 for each remaining worker, then the control planes.

### Confirming kernel lines actually arrive

A refused or blackholed destination is retried silently: the node logs no loud
error and `talosctl dmesg` looks entirely normal either way. **Absence of data
at the collector is the primary signal, so it has to be checked positively
rather than inferred from the node looking healthy.**

Three checks, in increasing order of strength. The third gates the rollout.

1. The listener is bound on the node, in the host namespace Talos dials into:

   ```bash
   talosctl -n <node-ip> netstat -l -t | grep 6050
   ```

   Nothing here means the hostPort did not land, and no node-side configuration
   will help.
2. The receiver has accepted records. Read it from the `alloy-logs` pod on the
   canary itself — a different node's pod proves nothing:

   ```bash
   kubectl -n monitoring get pods -o wide \
     --field-selector spec.nodeName=<node-name> | grep alloy-logs
   kubectl -n monitoring port-forward pod/<alloy-logs-pod-on-canary> 12345:12345 &
   curl -s localhost:12345/metrics \
     | grep 'otelcol_receiver_accepted_log_records.*talos_kernel'
   ```

   Two details, both required for this check to mean anything:

   - **Port-forward rather than `kubectl exec … curl`.** The Alloy image
     commonly ships no `curl`, so the exec form fails on a working node and
     reads as a rollout failure. Forwarding from the canary's own pod keeps the
     "on the canary itself" property that makes this check meaningful — a
     different node's pod proves nothing.
   - **Filter to the kernel receiver.** The metric is a series *per receiver*,
     and the pod-log receivers on that pod are nonzero regardless of whether a
     single kernel line ever arrived. An unfiltered `grep` will therefore show
     a healthy-looking count while the thing being tested is dead.

   A counter above zero for the kernel receiver is direct listener-side proof
   that the loopback path works, independent of anything downstream. Zero, with
   check 1 passing, points at CNI portmap not resolving loopback — see the
   fallback below.
3. The lines are stored, parsed, and attributed to the right node. In Grafana
   Explore against the platform Loki tenant:

   ```logql
   {job="integrations/talos/kernel", node="<canary-node-name>"}
   ```

   All three properties have to hold, and each catches a different failure:
   entries exist; the stored line is the kernel message rather than the raw
   JSON envelope; and `node` equals the canary rather than some other node. A
   populated stream labelled with the wrong node means loopback resolved to a
   shared endpoint and node identity is being erased — stop.

   Do not wait on organic traffic to decide this: a quiet kernel can emit
   nothing for minutes, and reading that as failure would be wrong. Generate a
   line instead — mounting or detaching a volume on the canary produces kernel
   output — then compare the window against
   `talosctl -n <node-ip> dmesg` for the same period. The two should describe
   the same events.

**If loopback does not work**, the fallback is the node's **own** address,
never a shared Service. Note that the patch templates render from a fixed set
of fields (`DefaultDisk`, `DefaultNetworkInterface`, `HAProxyIP`,
`ControlPlaneEndpoint`) and carry no per-node address, so that fallback is not
a one-line template edit — it needs either a new template field or per-node
patches under `clusters/core/patches/`.

Whether loopback resolves through the CNI portmap plugin on these nodes has
**not** been established. It is the single assumption this design rests on, and
check 2 on the canary is what settles it.

### Kernel log rollback

Nothing here is load-bearing for the node. The delivery controller declares no
outputs, so no other Talos controller consumes it and nothing reconciles on it;
an unreachable destination costs an infinite one-second retry logged at `Debug`,
below machined's default level. **A node left pointing at a listener that does
not exist is not degraded**, so there is no emergency rollback and no reason to
rush one. A malformed document is rejected server-side on apply with nothing
changed, so the apply in step 2 cannot leave a node part-configured.

Per node: revert that node's machine config to the previous revision and
re-apply. It takes effect immediately — unlike the kernel-argument route, there
is no boot entry to rewrite and nothing persists past the apply.

Fleet-wide: revert the patch-template commit, rebuild `talops`, regenerate and
re-apply per node. No reboot, no drain, no service restart.

To stop the flow quickly without touching Talos at all, remove the listener on
the platform side. The node then retries against a closed port indefinitely and
harmlessly.
