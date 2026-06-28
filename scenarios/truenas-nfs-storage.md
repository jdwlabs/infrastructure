# TrueNAS NFS Storage: Operator Runbook

This runbook covers day-two operations for the TrueNAS NFS storage tier integrated
into `pve-cluster-1`. The integration adds a 35.1 TiB RAIDZ2 pool as a shared NFS
secondary tier for Proxmox (ISOs, templates, backups, and live-migratable VMs) and
for Kubernetes bulk/RWX workloads via `democratic-csi`.

**TrueNAS address:** `192.168.1.205`  
**Proxmox cluster:** `pve1`–`pve5` (`192.168.1.200`–`204`)  
**Network:** 1GbE end-to-end — NFS is the only supported protocol here.

---

## 1. TrueNAS Layout

### ZFS Datasets

All datasets live under ZFS pool `storage` (RAIDZ2, 35.1 TiB usable).

| Dataset | Mount path | Purpose |
|---------|-----------|---------|
| `storage/proxmox` | `/mnt/storage/proxmox` | Proxmox VM disk images, ISOs, container templates |
| `storage/backup` | `/mnt/storage/backup` | Proxmox vzdump backup archives |
| `storage/k8s` | `/mnt/storage/k8s` | Parent dataset for Kubernetes — not directly mounted |
| `storage/k8s/vols` | `/mnt/storage/k8s/vols` | Root for dynamically provisioned PV child datasets |
| `storage/k8s/snaps` | `/mnt/storage/k8s/snaps` | Root for detached snapshot datasets |

### NFS Shares

Two **static** shares are created during initial setup — they persist indefinitely:

| Share path | Consumer | NFS options |
|-----------|---------|-------------|
| `/mnt/storage/proxmox` | Proxmox `truenas-vmdisks` | `192.168.1.0/24`, `maproot=root`, NFSv4.2 |
| `/mnt/storage/backup` | Proxmox `truenas-backup` | `192.168.1.0/24`, `maproot=root`, NFSv4.2 |

The `storage/k8s` dataset has **no static NFS share**. The `democratic-csi`
`freenas-api-nfs` driver creates child dataset + NFS export pairs on demand via
the TrueNAS API each time a PVC is provisioned. The same driver tears them down
during deletion (but because the StorageClass uses `reclaimPolicy: Retain`, the
driver will not delete a volume when the PVC is deleted — see
[Section 4: Kubernetes PVC operations](#4-kubernetes-pvc-operations)).

### API Key

A single TrueNAS API key named `democratic-csi` is used by the CSI driver. It is
stored in Vault at path `kv/truenas-csi`, field `api_key`, and is never written to
Git. External Secrets Operator syncs it into the `democratic-csi` namespace as
`democratic-csi-driver-config`. Rotate it using the procedure in
[Section 2](#2-rotating-the-truenas-api-key).

---

## 2. Rotating the TrueNAS API Key

Rotate when the key is compromised, as part of a scheduled credential rotation, or
after personnel changes. All steps except the Vault seed require an interactive SSH
session to TrueNAS (the `truenas_admin` user requires a tty for `sudo`).

### Step 1 — Create a new API key on TrueNAS (interactive, requires sudo tty)

```bash
ssh -i ~/.ssh/id_ed25519_pve truenas_admin@192.168.1.205
sudo midclt call api_key.create '{"name": "democratic-csi-new"}'
```

The response JSON contains a `key` field. This is the only time the full key is
shown — copy it immediately.

If TrueNAS SCALE 25.10 requires a user binding, use:

```bash
sudo midclt call api_key.create '{"name": "democratic-csi-new", "username": "root"}'
```

### Step 2 — Verify the new key works before cutting over

```bash
curl -s -H "Authorization: Bearer <NEW_KEY>" \
  http://192.168.1.205/api/v2.0/pool/dataset/id/storage%2Fk8s | head -c 200
```

Expected: a JSON object describing `storage/k8s`. A `401` means the key lacks the
required permissions.

### Step 3 — Reseed Vault

```bash
export PLATFORMCTL_TRUENAS_CSI_API_KEY='<NEW_KEY>'
platformctl bootstrap seed truenas-csi --json
```

Vault path `kv/truenas-csi` field `api_key` is now updated. Do not commit or log
the key value.

### Step 4 — Force ESO to refresh

ESO polls on a 1-minute `refreshInterval`, so the new secret will propagate
automatically within a minute. To force it immediately:

```bash
kubectl annotate externalsecret democratic-csi-driver-config \
  -n democratic-csi \
  force-sync="$(date +%s)" --overwrite
```

Watch the secret sync status:

```bash
kubectl get externalsecret democratic-csi-driver-config -n democratic-csi -w
```

Expected: `READY=True`, `STATUS=SecretSynced`.

### Step 5 — Restart the CSI driver pods to pick up the new secret

The driver reads the secret at pod startup. Rolling-restart the controller:

```bash
kubectl rollout restart deployment -n democratic-csi
kubectl rollout status deployment -n democratic-csi
```

### Step 6 — Delete the old API key on TrueNAS (interactive)

First find its ID:

```bash
ssh -i ~/.ssh/id_ed25519_pve truenas_admin@192.168.1.205
midclt call api_key.query | python3 -c \
  'import sys, json; [print(k["id"], k["name"]) for k in json.load(sys.stdin)]'
```

Then delete the old key by ID:

```bash
sudo midclt call api_key.delete <OLD_ID>
```

---

## 3. Proxmox Storage Operations

### 3.1 Storage Map

Two cluster-wide NFS storages are registered in Proxmox (visible on all five nodes):

| Storage ID | Backing dataset | Content types | Purpose |
|-----------|----------------|---------------|---------|
| `truenas-vmdisks` | `storage/proxmox` | `images`, `iso`, `vztmpl` | VM disk images, uploaded ISOs, container templates |
| `truenas-backup` | `storage/backup` | `backup` | `vzdump` archives; pruning: keep-last=3 |

Because these storages are cluster-wide, any VM whose disk resides on
`truenas-vmdisks` can be live-migrated between Proxmox nodes without copying data.
This is the primary reason the storage exists: Talos VM disks intentionally stay on
local NVMe (see [Section 5](#5-important-constraints)), but future non-Talos VMs can
use `truenas-vmdisks` to gain live-migration capability.

### 3.2 Taking a Backup (vzdump)

Run from any Proxmox node (pve1–pve5); the backup lands on the NAS regardless of
which node initiates it:

```bash
ssh -i ~/.ssh/id_ed25519_pve root@192.168.1.200
vzdump <VMID> --storage truenas-backup --mode snapshot --compress zstd
```

Verify the archive was written:

```bash
pvesm list truenas-backup
```

### 3.3 Restoring a Backup

```bash
# List available archives
pvesm list truenas-backup

# Restore to a new VMID on the same or a different node
qmrestore truenas-backup:backup/<ARCHIVE_FILENAME> <NEW_VMID> \
  --storage truenas-vmdisks
```

The restored VM's disk is placed on `truenas-vmdisks` so it is immediately
live-migratable.

### 3.4 Uploading ISOs and Templates

In the Proxmox web UI: **Datacenter → Storage → truenas-vmdisks → ISO Images** (or
**CT Templates**). Alternatively from a node:

```bash
scp ~/my-image.iso root@192.168.1.200:/mnt/pve/truenas-vmdisks/template/iso/
```

---

## 4. Kubernetes PVC Operations

### 4.1 Choosing a StorageClass

| | `longhorn` (default) | `truenas-nfs` |
|-|---------------------|--------------|
| Backing | On-node NVMe, replicated across nodes | TrueNAS HDD pool, 35 TiB, over 1GbE NFS |
| Speed | Fast (local NVMe, replication in-cluster) | Slower (1GbE link; HDD latency) |
| Durability | In-cluster replication (configurable replicas) | RAIDZ2 on TrueNAS |
| Access modes | RWO | RWO and RWX |
| Best for | Databases, caches, latency-sensitive apps | Shared config/media, bulk data, RWX mounts |
| Reclaim | Delete (PVC delete removes the volume) | Retain (see below) |

Use `longhorn` unless you need RWX (ReadWriteMany) access from multiple pods
simultaneously, or you need more raw capacity than the per-node NVMe tier provides.
Do not use `truenas-nfs` for any workload that sits in the latency-sensitive path
(databases, Vault storage backend, etc.) — at 1GbE over HDD the latency profile
is incompatible with those workloads.

### 4.2 Creating a PVC on `truenas-nfs`

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: my-shared-data
  namespace: my-namespace
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: truenas-nfs
  resources:
    requests:
      storage: 50Gi
```

When the PVC is created, `democratic-csi` calls the TrueNAS API to create a child
ZFS dataset under `storage/k8s/vols` and a corresponding NFS export. The PVC
transitions to `Bound` once the dataset and export are created (typically within
a few seconds).

### 4.3 Deleting a PVC — Manual Cleanup Required

The `truenas-nfs` StorageClass has `reclaimPolicy: Retain`. This means that when
a PVC is deleted, the backing PersistentVolume and its TrueNAS dataset are
**not deleted automatically** — they are retained to prevent accidental data loss.

After deleting a PVC you no longer need, perform the following cleanup:

**1. Find the orphaned PV:**

```bash
kubectl get pv | grep Released
```

**2. Identify the TrueNAS dataset name from the PV spec:**

```bash
kubectl describe pv <PV_NAME> | grep -A5 'Volume Handle'
```

The dataset name is embedded in the volume handle (typically
`storage/k8s/vols/<pvc-uid>`).

**3. Delete the PV object:**

```bash
kubectl delete pv <PV_NAME>
```

**4. Remove the TrueNAS dataset and its NFS export (interactive sudo):**

```bash
ssh -i ~/.ssh/id_ed25519_pve truenas_admin@192.168.1.205
# The dataset name comes from step 2
sudo midclt call sharing.nfs.query \
  | python3 -c 'import sys,json; \
    [print(s["id"], s["path"]) for s in json.load(sys.stdin) \
    if "storage/k8s/vols" in s["path"]]'
sudo midclt call sharing.nfs.delete <NFS_SHARE_ID>
sudo midclt call pool.dataset.delete 'storage/k8s/vols/<PVC_UID>' '{"recursive": false}'
```

Caution: Verify the `pool.dataset.delete` argument form against your TrueNAS SCALE version before running; API signatures can change between releases.

Skipping this step leaves orphaned datasets and NFS exports on TrueNAS. They do not
harm anything immediately, but they consume space and accumulate without bound.

---

## 5. Important Constraints

### Talos VM Disks Stay on Local NVMe

The Talos control plane and worker VMs (VMIDs 200–202, 300–303) have their disks
on per-node local NVMe (`local-lvm`). This is intentional and must not change.

Talos VMs back Longhorn storage replicas. Moving a Talos VM disk to a 1GbE NFS
share would route every Longhorn I/O over the same 1GbE link as the NFS traffic,
collapsing the throughput available to in-cluster workloads. The local NVMe disks
are kept separate to preserve Longhorn performance.

As a result, Talos VMs cannot be live-migrated between Proxmox nodes — this is
expected and acceptable. Live migration is available for VMs whose disks are
explicitly placed on `truenas-vmdisks`.

### 1GbE is the Throughput Ceiling

All nodes, including TrueNAS, are connected at 1GbE. Aggregate cluster NFS
throughput is bounded at roughly 118 MB/s. Multiple simultaneous NFS consumers
(Proxmox backup, multiple PVCs) share this bandwidth. Do not schedule
throughput-intensive workloads to `truenas-nfs` concurrently with scheduled Proxmox
backup jobs.

---

## 6. Verifying the Integration is Healthy

```bash
# Proxmox: both storages active on all nodes
for ip in 200 201 202 203 204; do
  echo "=== pve.$ip ==="
  ssh -i ~/.ssh/id_ed25519_pve root@192.168.1.$ip \
    "pvesm status | grep -E 'truenas-vmdisks|truenas-backup'"
done

# Kubernetes: CSI driver running, StorageClass present
kubectl get pods -n democratic-csi
kubectl get storageclass truenas-nfs

# ESO: secret synced
kubectl get externalsecret democratic-csi-driver-config -n democratic-csi

# TrueNAS: datasets exist (read-only, no sudo)
ssh -i ~/.ssh/id_ed25519_pve truenas_admin@192.168.1.205 \
  "zfs list -r storage -o name | grep -E 'storage/(proxmox|backup|k8s)'"
```
