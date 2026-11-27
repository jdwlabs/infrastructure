# Runbook: TrueNAS tenant backup datasets

Status: NOT YET EXECUTED. Owner-run on the NAS; no agent applies any of it.
Part of the backup and storage platform (epic `JDWLABS-841`, phase 1).

This runbook creates the storage that every later backup phase writes into:

1. per-tenant encrypted datasets with quotas under `storage/tenants`
2. periodic ZFS snapshot tasks, on those datasets and on `storage/k8s`
3. an NFS share that lets the cluster's dump jobs write to
   `storage/tenants/platform/backup`
4. a daily TrueNAS config export into that tenant

It changes no workload. The dump jobs keep writing to Google Drive until a
separate `deployments` PR points them at the new share. Off-site copies (B2),
the S3 gateway and alerting come in later phases.

**NAS:** `192.168.1.205`, TrueNAS SCALE 25.10.x, pool `storage` (RAIDZ2).
Stay on 25.10.x: version 26 removes the REST API that democratic-csi uses.
See platform ADR-0024.
**Shell:** `ssh -t -i ~/.ssh/id_ed25519_pve truenas_admin@192.168.1.205`.
`sudo` needs the tty. Read-only `zfs`/`zpool`/`midclt ... query` calls work
without it.

Every `midclt` write below mirrors a UI action. Use the UI when the two
disagree. Middleware argument shapes change between releases, so check each
result with the query in its **Verify** step. Do not use raw `zfs create`
or `zfs set` for anything in this runbook: datasets the middleware did not
create have no stored keys, shares or tasks, and the UI shows them wrong.

---

## 0. Pre-checks (read-only)

### 0.1 Version and pool health

```bash
midclt call system.version            # expect TrueNAS-25.10.x
zpool status -x storage               # expect: pool 'storage' is healthy
zpool status storage | grep -E 'scan:|errors:'
zpool list -o name,size,alloc,free,cap,health storage
zpool get feature@encryption storage  # expect enabled or active
```

Stop if the pool is not `ONLINE`, any vdev shows errors, a resilver is
running, or capacity (`cap`) is above 70%. The quotas below total 8 TiB,
so check `free` is comfortably above that. If the last scrub is older than
about five weeks, run one first (**Storage → storage → Scrub**) and let it
finish.

### 0.2 Existing datasets: leave these alone

```bash
zfs list -o name,used,avail,quota,refquota,encryption,mountpoint -d 2 storage
```

Expected: `storage/k8s` (with `vols`, `snaps`, `iscsi/...`), `storage/backup`,
`storage/proxmox`, `storage/minio`, and the system or apps datasets
(`.system`, `ix-apps` or similar). **Nothing in this runbook modifies them.**
The only thing that touches an existing dataset is §3.2, which adds a
snapshot task on `storage/k8s`.

Confirm that `storage/tenants` does not exist yet:

```bash
zfs list storage/tenants 2>&1          # expect: dataset does not exist
```

### 0.3 Existing tasks, shares and cron jobs

Record these first so that you can tell your own additions apart later:

```bash
midclt call pool.snapshottask.query | python3 -c 'import sys,json; [print(t["id"], t["dataset"], t["naming_schema"], t["enabled"]) for t in json.load(sys.stdin)]'
midclt call sharing.nfs.query       | python3 -c 'import sys,json; [print(s["id"], s["path"], s.get("hosts"), s.get("networks")) for s in json.load(sys.stdin)]'
midclt call cronjob.query           | python3 -c 'import sys,json; [print(c["id"], c["user"], c["description"]) for c in json.load(sys.stdin)]'
```

### 0.4 democratic-csi quota mode (gates §3.2)

A snapshot holds the blocks that existed when it was taken. When a PVC
dataset is capped with `quota`, that space counts against the cap: a busy
volume runs out of space as snapshots pile up, and TrueNAS raises its quota
alert. When the cap is `refquota`, only live data counts. The NFS driver is
configured `datasetEnableQuotas: true`, which should set `refquota`, but
confirm that on the live datasets:

```bash
zfs get -r -t filesystem -s local -o name,property,value quota,refquota storage/k8s/vols
zfs get -r -t volume -o name,property,value volsize,refreservation storage/k8s/iscsi
```

Expected:

- every `storage/k8s/vols/<pvc>` has a local `refquota` and no local `quota`
- iSCSI zvols show `refreservation` as `none` or `0`, meaning they are sparse

If a PVC dataset carries `quota`, or a zvol has `refreservation` equal to its
`volsize`, **skip §3.2** and record which datasets on the ticket. Snapshotting
a thick zvol reserves another copy of its referenced data.

### 0.5 UPS: answer before continuing

Once this runbook is done, this NAS holds every local backup, and it has a
history of unexplained reboots. Check whether a UPS is wired to it:

```bash
midclt call service.query '[["service","=","ups"]]' | python3 -c 'import sys,json; [print(s["service"], s["state"], s["enable"]) for s in json.load(sys.stdin)]'
```

Record the answer on the ticket: UPS model, or "none". A UPS is not a
blocker for this runbook. If there is none, file a follow-up. An abrupt power
loss leaves ZFS consistent but loses in-flight writes, and a backup job
interrupted halfway produces a truncated archive.

---

## 1. Backup service user

The dump jobs run as different identities:

| Job | Namespace | uid:gid (verified live) |
|---|---|---|
| `postgres-backup` | `database` | 999:999 |
| `litellm-db-backup` | `ai-sre` | 999:999 |
| `jdwillmsen-minecraft-fwb-prd-backup` | `jdwillmsen-prd` | 1000, fsGroup 2000 |

Re-check before relying on this table. It only reads, nothing changes:

```bash
kubectl --context admin@core -n database get cronjob postgres-backup \
  -o jsonpath='{.spec.jobTemplate.spec.template.spec.securityContext}{"\n"}'
```

Rather than match one of those uids on the NAS (999 is a system uid on
Debian), the share uses **mapall**. The NAS performs every write as one
dedicated local user, whatever uid the pod runs as. Files on the NAS then
have a single owner, and the cluster cannot act as root on the share.

**UI:** **Credentials → Users → Add**

- Username `platform-backup`, full name `Platform backup writer (NFS mapall)`
- UID: accept the suggested value (3000 or above) and **write it down**
- Create a new primary group (`platform-backup`)
- Disable the password. Grant no SMB access, no SSH, no TrueNAS (UI/API)
  access. Shell `nologin`, home `/var/empty`.

**Verify:**

```bash
midclt call user.query '[["username","=","platform-backup"]]' | python3 -c 'import sys,json; u=json.load(sys.stdin)[0]; print(u["uid"], u["group"]["bsdgrp_gid"], u["shell"], u["password_disabled"])'
id platform-backup
```

**Rollback:** **Credentials → Users → platform-backup → Delete**. Do this only
after §4 is rolled back. Keep the group unless nothing else uses it.

---

## 2. Datasets

### 2.1 Layout

```
storage/tenants                    encryption root (key), lz4, atime off
├── platform                       quota 2T, reservation 200G
│   ├── s3                         recordsize 1M
│   ├── backup                     recordsize 1M   ← NFS share (§4)
│   │   └── truenas-config         recordsize 1M, root-only 0700 (§5)
│   └── files                      zstd
├── jdwillmsen                     quota 4T
│   ├── s3 / backup                recordsize 1M
│   └── files                      zstd
├── jdwlabs                        quota 1T    (same three children)
└── dotablaze-tech                 quota 1T    (same three children)
```

The choices behind that layout:

- **One encryption root, at `storage/tenants`.** Every child inherits the
  key, so there is exactly one key to keep safe. The tenants are separated
  by quotas, share permissions and, later, S3 gateway roots, not by
  cryptography. A key per tenant would mean four keys to custody and no
  stronger isolation, because all four live on the same NAS.
- **Key, not passphrase.** A passphrase-encrypted dataset stays locked after
  every reboot until someone types the passphrase in. With this NAS's reboot
  history, backups would then quietly stop landing. A key-encrypted dataset
  unlocks automatically at boot from the key in the TrueNAS config database,
  and the encryption still protects the pulled disks and the raw `zfs send`
  streams. The price is that the key must exist **off the NAS** (§2.4):
  without it, a lost boot device or a lost config makes the data
  unrecoverable.
- **Compression.** `lz4` is inherited by default. Backup and S3 data
  (`.tar.gz`, `pg_dump -Fc`, zstd archives) is already compressed, and lz4
  gives up on incompressible blocks almost for free. The `files` datasets
  hold uncompressed user files, so they get `zstd`.
- **recordsize 1M on `s3` and `backup`.** These hold large sequential
  objects, so 1M records mean fewer blocks, less metadata and better
  compression ratios. `files` keeps the default 128K.
- **atime off.** Reads should not cause writes. This is inherited by every
  child.
- **Quotas.** These are `quota`, not `refquota`, so snapshots count against
  them. A tenant's quota then bounds what it really costs the pool,
  snapshots included. The consequence is that deleting files frees nothing
  until the snapshots holding them expire, which can take up to 8 weeks
  (§3.1). If a tenant fills up, raise its quota (§2.5). Do not delete
  snapshots to make room.
- **reservation 200G on `platform`.** This guarantees space for the
  production database backups even if another dataset fills the pool.

### 2.2 Create the encryption root (UI)

The UI is the reliable way to create a key-encrypted root: it stores the
generated key in the config database correctly.

**Datasets** → select `storage` → **Add Dataset**

- Name `tenants`, Dataset Preset `Generic`
- **Advanced Options**
  - Encryption: untick **Inherit**, tick **Encryption**, Encryption Type
    **Key**, tick **Generate Key**, Algorithm `AES-256-GCM`
  - Compression `LZ4`, Enable Atime `Off`, Record Size: inherit (128K)
  - Quotas and reservations: leave empty
- **Save**. The UI offers to download the key: **download it** (see §2.4).

**Verify:**

```bash
zfs get -o property,value encryption,keyformat,keylocation,keystatus,encryptionroot,compression,atime storage/tenants
```

Expected: `encryption aes-256-gcm`, `keyformat hex`, `keystatus available`,
`encryptionroot storage/tenants`, `compression lz4`, `atime off`.

### 2.3 Create the tenants and their children

Use the UI (**Datasets → select parent → Add Dataset**, leave Encryption on
**Inherit**) or the shell commands below. Quotas are in bytes: 1 TiB =
1099511627776.

```bash
# tenants
sudo midclt call pool.dataset.create '{"name":"storage/tenants/platform","quota":2199023255552,"reservation":214748364800}'
sudo midclt call pool.dataset.create '{"name":"storage/tenants/jdwillmsen","quota":4398046511104}'
sudo midclt call pool.dataset.create '{"name":"storage/tenants/jdwlabs","quota":1099511627776}'
sudo midclt call pool.dataset.create '{"name":"storage/tenants/dotablaze-tech","quota":1099511627776}'

# children
for t in platform jdwillmsen jdwlabs dotablaze-tech; do
  for c in s3 backup; do
    sudo midclt call pool.dataset.create "{\"name\":\"storage/tenants/$t/$c\",\"recordsize\":\"1M\"}"
  done
  sudo midclt call pool.dataset.create "{\"name\":\"storage/tenants/$t/files\",\"compression\":\"ZSTD\"}"
done

# config-export target (permissions set in §5)
sudo midclt call pool.dataset.create '{"name":"storage/tenants/platform/backup/truenas-config","recordsize":"1M"}'
```

`midclt` creates children of an encrypted parent with inherited encryption
by default. The Verify step below confirms it.

**Verify:**

```bash
zfs list -r -o name,quota,reservation,recordsize,compression,atime,encryptionroot,keystatus storage/tenants
```

Expected: 18 datasets in total, 1 root + 4 tenants + 12 children +
`truenas-config`. Each shows `encryptionroot storage/tenants` and
`keystatus available`, and the quotas, reservation, recordsize and
compression values match §2.1.

### 2.4 Key custody (do not skip)

The key unlocks every tenant's backups. It has to survive the loss of the
NAS, and it must not live only in Vault, because Vault's own backups will be
stored in these datasets.

1. **Datasets** → select `tenants` → **ZFS Encryption** card → **Export
   Key**. The UI downloads `dataset_tenants_key.json` (the name can vary).
   Do this on your own workstation, not in an agent session. Do not export
   it with `midclt call pool.dataset.export_key`, which prints the key to
   the terminal and its scrollback.
2. Store it **offline in two places**:
   - your password manager, as a secure note or attachment
   - a second physical location away from the house, as a printed copy of
     the JSON or an encrypted USB stick, alongside the Vault unseal material
     (`scenarios/vault-unseal-backup.md`)
3. Delete the downloaded file from the workstation's Downloads folder.

**Verify:** open each stored copy and confirm that it names
`storage/tenants` and carries a 64-character hex key. Do not paste the key
anywhere to compare the copies. Record "key stored in 2 places" (not the key)
on the ticket.

A later restore drill proves the key works. Do not lock the dataset now to
test it.

### 2.5 Changing a quota later

**Datasets** → tenant → **Dataset Space Management** → **Edit** →
*Quota for this dataset and all children*. Or:

```bash
sudo midclt call pool.dataset.update storage/tenants/jdwlabs '{"quota":2199023255552}'
```

### 2.6 Rollback

Roll back only while the datasets are empty: `zfs list -r -o name,used
storage/tenants` shows only metadata-sized `used` values. First roll back
§5, §4 and §3.1, in that order, then:

```bash
sudo midclt call pool.dataset.delete storage/tenants '{"recursive": true}'
zfs list storage/tenants 2>&1   # expect: dataset does not exist
```

This destroys all their snapshots too. Once backups have landed, it destroys
backup data: stop and decide again.

---

## 3. Periodic snapshot tasks

TrueNAS assigns snapshots to a task, and decides which ones to prune, by
matching the naming schema. **Give every task its own prefix.** If two tasks
share a schema, they can prune each other's snapshots. The prefixes also let
a rollback select exactly the snapshots one task made.

### 3.1 `storage/tenants`, recursive

**Data Protection → Periodic Snapshot Tasks → Add**, three times:

| Dataset | Recursive | Lifetime | Naming schema | Schedule |
|---|---|---|---|---|
| `storage/tenants` | yes | 48 HOUR | `tenants-hourly-%Y-%m-%d_%H-%M` | hourly at :00 |
| `storage/tenants` | yes | 30 DAY | `tenants-daily-%Y-%m-%d_%H-%M` | daily 00:10 |
| `storage/tenants` | yes | 8 WEEK | `tenants-weekly-%Y-%m-%d_%H-%M` | Sunday 00:20 |

Leave Exclude empty and keep **Allow Taking Empty Snapshots** on (the
default). A future snapshot-age alert can then tell "idle" from "the task
stopped".

Or from the shell:

```bash
mk() { sudo midclt call pool.snapshottask.create "$1"; }
mk '{"dataset":"storage/tenants","recursive":true,"lifetime_value":48,"lifetime_unit":"HOUR","naming_schema":"tenants-hourly-%Y-%m-%d_%H-%M","schedule":{"minute":"0","hour":"*","dom":"*","month":"*","dow":"*"},"enabled":true}'
mk '{"dataset":"storage/tenants","recursive":true,"lifetime_value":30,"lifetime_unit":"DAY","naming_schema":"tenants-daily-%Y-%m-%d_%H-%M","schedule":{"minute":"10","hour":"0","dom":"*","month":"*","dow":"*"},"enabled":true}'
mk '{"dataset":"storage/tenants","recursive":true,"lifetime_value":8,"lifetime_unit":"WEEK","naming_schema":"tenants-weekly-%Y-%m-%d_%H-%M","schedule":{"minute":"20","hour":"0","dom":"*","month":"*","dow":"sun"},"enabled":true}'
```

### 3.2 `storage/k8s`, recursive (only if §0.4 passed)

| Dataset | Recursive | Lifetime | Naming schema | Schedule |
|---|---|---|---|---|
| `storage/k8s` | yes | 24 HOUR | `k8s-hourly-%Y-%m-%d_%H-%M` | hourly at :05 |
| `storage/k8s` | yes | 7 DAY | `k8s-daily-%Y-%m-%d_%H-%M` | daily 00:15 |

```bash
mk '{"dataset":"storage/k8s","recursive":true,"lifetime_value":24,"lifetime_unit":"HOUR","naming_schema":"k8s-hourly-%Y-%m-%d_%H-%M","schedule":{"minute":"5","hour":"*","dom":"*","month":"*","dow":"*"},"enabled":true}'
mk '{"dataset":"storage/k8s","recursive":true,"lifetime_value":7,"lifetime_unit":"DAY","naming_schema":"k8s-daily-%Y-%m-%d_%H-%M","schedule":{"minute":"15","hour":"0","dom":"*","month":"*","dow":"*"},"enabled":true}'
```

These snapshots are crash-consistent only. They give a quick rollback of a
PVC (a Minecraft datadir, for example), not an application-consistent
backup.

**Side effect to know about:** once a PVC dataset has snapshots, the manual
cleanup in `truenas-nfs-storage.md` §4.3 (`pool.dataset.delete ...
'{"recursive": false}'`) fails with "dataset has children/snapshots". Delete
that dataset with `'{"recursive": true}'`, which also removes its snapshots,
after confirming it is the right one.

### 3.3 Verify

```bash
midclt call pool.snapshottask.query | python3 -c 'import sys,json; [print(t["id"], t["dataset"], t["naming_schema"], t["lifetime_value"], t["lifetime_unit"], t["schedule"], t["enabled"]) for t in json.load(sys.stdin)]'
```

Five new tasks, with the schedules shown above. The UI's task list should
also show each schedule in words: confirm that the weekly one says Sunday.
After the next full hour:

```bash
zfs list -t snapshot -o name,creation -s creation -r storage/tenants | grep tenants-hourly | tail -3
zfs list -t snapshot -o name,creation -s creation -r storage/k8s     | grep k8s-hourly     | tail -3
```

Every dataset under each root has a snapshot from the last hour. After
48 hours, the oldest `tenants-hourly-*` should be about 48 hours old, which
shows pruning works.

Watch the TrueNAS alert bell (top right) for a day. A new "quota" alert on
a `storage/k8s/vols/*` dataset means §0.4 missed something: disable the
`k8s-*` tasks and investigate.

### 3.4 Rollback

**Data Protection → Periodic Snapshot Tasks** → the task → **Delete** (or
toggle **Enabled** off). Shell: `sudo midclt call pool.snapshottask.delete
<id>`. Deleting a task keeps the snapshots it already made. To remove them,
filtered strictly by that task's prefix:

```bash
zfs list -H -t snapshot -o name -r storage/k8s | grep '@k8s-hourly-' > /tmp/snaps.txt
wc -l /tmp/snaps.txt; head /tmp/snaps.txt        # read it before destroying
xargs -r -n1 sudo zfs destroy < /tmp/snaps.txt
```

---

## 4. NFS share: `storage/tenants/platform/backup`

### 4.1 Allowed clients

The kubelet mounts NFS volumes on the node itself, so the source address is
the **node IP**, not a pod IP. Current nodes:

```bash
kubectl --context admin@core get nodes -o wide | awk 'NR>1{print $1, $6}'
```

When this was written there were eight: 192.168.1.78, .87, .98, .125, .130,
.163, .165 and .241. Re-run the command and use its output, not this list.

List each node IP under **Hosts** rather than allowing `192.168.1.0/24`. The
Proxmox shares already allow the whole /24, but this share will hold every
tenant's production database dumps, so nothing else on the LAN should be
able to mount it. The control-plane IPs are pinned
(`scenarios/cp-static-ips.md`); worker IPs come from the reconciler. **When a
node is added, or a worker changes address, add it here**, or jobs scheduled
on it will fail to mount.

### 4.2 Directory ownership

```bash
sudo chown platform-backup:platform-backup /mnt/storage/tenants/platform/backup
sudo chmod 0770 /mnt/storage/tenants/platform/backup
```

UI equivalent: **Datasets** → `platform/backup` → **Permissions → Edit** →
User and Group `platform-backup`, mode 770. Do not apply recursively:
`truenas-config` gets its own permissions in §5.

### 4.3 Create the share

**Shares → Unix (NFS) Shares → Add**

- Path `/mnt/storage/tenants/platform/backup`
- Description `platform backup dumps (k8s nodes only)`
- Hosts: each node IP from §4.1. Networks: none.
- Advanced Options:
  - **Mapall User** `platform-backup`, **Mapall Group** `platform-backup`
  - Maproot: leave empty (it cannot be combined with mapall)
  - Security `SYS`; not read-only
- **Save**. If asked to restart NFS, accept: the existing Proxmox shares
  reconnect on their own. Do this outside the Proxmox vzdump and Minecraft
  backup windows (04:00).

Or from the shell:

```bash
sudo midclt call sharing.nfs.create '{"path":"/mnt/storage/tenants/platform/backup","comment":"platform backup dumps (k8s nodes only)","hosts":["192.168.1.78","192.168.1.87","192.168.1.98","192.168.1.125","192.168.1.130","192.168.1.163","192.168.1.165","192.168.1.241"],"mapall_user":"platform-backup","mapall_group":"platform-backup","ro":false,"enabled":true}'
```

### 4.4 Verify

On the NAS:

```bash
midclt call sharing.nfs.query '[["path","=","/mnt/storage/tenants/platform/backup"]]' | python3 -c 'import sys,json; s=json.load(sys.stdin)[0]; [print(k, s.get(k)) for k in ("id","path","hosts","networks","mapall_user","mapall_group","maproot_user","ro","enabled")]'
stat -c '%U:%G %a' /mnt/storage/tenants/platform/backup   # expect platform-backup:platform-backup 770
```

From the cluster, a throwaway pod writes as uid 999, the dump jobs' identity.
This step creates and then deletes a namespace, which is a live change,
so you run it:

```bash
kubectl --context admin@core create namespace nfs-smoke
cat <<'EOF' | kubectl --context admin@core apply -f -
apiVersion: v1
kind: Pod
metadata: {name: nfs-smoke, namespace: nfs-smoke}
spec:
  restartPolicy: Never
  securityContext: {runAsUser: 999, runAsGroup: 999, runAsNonRoot: true, seccompProfile: {type: RuntimeDefault}}
  containers:
    - name: t
      image: busybox:1.36
      securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}, readOnlyRootFilesystem: true}
      command: [sh, -c, 'set -e; echo ok > /b/.smoke && cat /b/.smoke && rm /b/.smoke; ls /b/truenas-config 2>&1 || true']
      volumeMounts: [{name: b, mountPath: /b}]
  volumes:
    - name: b
      nfs: {server: 192.168.1.205, path: /mnt/storage/tenants/platform/backup}
EOF
kubectl --context admin@core -n nfs-smoke wait pod/nfs-smoke --for=jsonpath='{.status.phase}'=Succeeded --timeout=120s
kubectl --context admin@core -n nfs-smoke logs nfs-smoke
kubectl --context admin@core delete namespace nfs-smoke
```

Expected logs: `ok`, then `Permission denied` for `truenas-config` (or an
empty listing if the NFS server does not cross into child datasets). Either
way, no `truenas-config-*.tar` names may appear. If the pod sticks in
`ContainerCreating`, `kubectl describe` shows the mount error: usually the
node's IP is missing from Hosts.

### 4.5 Rollback

**Shares → Unix (NFS) Shares** → the share → **Delete**, or `sudo midclt call
sharing.nfs.delete <id>`. The dataset and its contents remain. First make
sure no CronJob mounts it:

```bash
kubectl --context admin@core get pv -o wide | grep tenants/platform/backup
```

---

## 5. TrueNAS config export

TrueNAS already writes a daily config copy to the system dataset, but
without the **secret seed** (`pwenc_secret`). Without the seed, a restored
config loses every stored credential **and the `storage/tenants` dataset
key**. This task keeps 30 daily tarballs with the seed. They have the same
layout as **System → General Settings → Manage Configuration → Download File
→ Export Password Secret Seed**, so the UI's **Upload File** accepts them
directly.

A copy stored on the NAS protects against a bad config change or a reinstall
onto a new boot device. It does not protect against losing the pool: that is
the job of the off-site phase, which ships this directory to B2, and of the
key you exported in §2.4.

### 5.1 Lock the directory down

The tarball holds the secret seed, which decrypts every credential TrueNAS
stores. It must not be readable over the share:

```bash
sudo chown root:root /mnt/storage/tenants/platform/backup/truenas-config
sudo chmod 0700      /mnt/storage/tenants/platform/backup/truenas-config
stat -c '%U:%G %a' /mnt/storage/tenants/platform/backup/truenas-config   # root:root 700
```

### 5.2 Install the script

Write this file as root:
`/mnt/storage/tenants/platform/backup/truenas-config/truenas-config-export.sh`.
The script lives on the encrypted dataset, so if the dataset is locked the
cron task fails loudly instead of writing elsewhere.

```sh
#!/bin/sh
set -eu
dest=/mnt/storage/tenants/platform/backup/truenas-config
keep=30
stamp=$(date -u +%Y%m%dT%H%M%SZ)
umask 077

# Staged inside the encrypted dataset so the seed never touches /tmp.
work=$(mktemp -d "$dest/.work.XXXXXX")
trap 'rm -rf "$work"' EXIT

# Copying freenas-v1.db while the middleware writes to it can capture a torn
# page; the SQLite online-backup API takes a consistent copy.
python3 - "$work/freenas-v1.db" <<'PY'
import sqlite3, sys
src = sqlite3.connect("file:/data/freenas-v1.db?mode=ro", uri=True)
dst = sqlite3.connect(sys.argv[1])
src.backup(dst)
dst.close()
src.close()
PY
cp /data/pwenc_secret "$work/pwenc_secret"

# Plain tar, not gzip: Upload File expects the same format as the UI export.
tar -C "$work" -cf "$dest/.truenas-config-$stamp.tar" freenas-v1.db pwenc_secret
mv "$dest/.truenas-config-$stamp.tar" "$dest/truenas-config-$stamp.tar"

# Count-based retention, so a stalled cron never ages out the last good copy.
ls -1t "$dest"/truenas-config-*.tar | tail -n +$((keep + 1)) | xargs -r rm --
```

```bash
sudo chmod 0700 /mnt/storage/tenants/platform/backup/truenas-config/truenas-config-export.sh
```

### 5.3 Create the cron task

**System → Advanced Settings → Cron Jobs → Add**

- Description `truenas-config-export (with secret seed, keep 30)`
- Command `/mnt/storage/tenants/platform/backup/truenas-config/truenas-config-export.sh`
- Run As User `root`
- Schedule: daily at 03:30, which runs before the snapshot and backup
  windows and is never at the same minute as a snapshot
- Untick **Hide Standard Error**, so failures reach the TrueNAS alert email
- Enabled

### 5.4 Verify

Run it once by hand, from **Cron Jobs → ⋮ → Run Now** or
`sudo /mnt/.../truenas-config-export.sh`, then:

```bash
sudo ls -l /mnt/storage/tenants/platform/backup/truenas-config/
f=$(sudo sh -c 'ls -1t /mnt/storage/tenants/platform/backup/truenas-config/truenas-config-*.tar | head -1')
sudo tar -tvf "$f"                      # freenas-v1.db and pwenc_secret, both non-zero
sudo sh -c "d=\$(mktemp -d /mnt/storage/tenants/platform/backup/truenas-config/.chk.XXXXXX); tar -C \$d -xf '$f' freenas-v1.db; python3 -c \"import sqlite3,sys; print(sqlite3.connect(sys.argv[1]).execute('pragma integrity_check').fetchone()[0])\" \$d/freenas-v1.db; rm -rf \$d"
# expect: ok
```

The next day, check that a second tarball appeared with no alert. After
31 days, confirm the count stays at 30:
`sudo sh -c 'ls /mnt/.../truenas-config/truenas-config-*.tar | wc -l'`.

### 5.5 Rollback

Delete the cron job (**System → Advanced Settings → Cron Jobs → Delete**).
Keep the tarballs unless they are wanted gone. To remove them, run
`sudo rm /mnt/storage/tenants/platform/backup/truenas-config/truenas-config-*.tar`.
They also persist in the `tenants-*` snapshots until those expire.

---

## 6. Final checklist

Tick each line, then paste the ticked list (no keys, no secrets) as a comment
on the ticket:

```
[ ] 0.1  25.10.x, pool ONLINE, no errors, cap < 70%, scrub recent
[ ] 0.2  storage/k8s, backup, proxmox, minio untouched (zfs list before == after, except new snapshots on k8s)
[ ] 0.4  k8s vols use refquota; zvols sparse  (or §3.2 skipped, reason recorded)
[ ] 0.5  UPS: ______________ (model / none → follow-up filed)
[ ] 1    platform-backup user, uid ____, no login, no password
[ ] 2.3  18 datasets under storage/tenants, encryptionroot storage/tenants, keystatus available
[ ] 2.3  quotas 2T / 4T / 1T / 1T, platform reservation 200G, recordsize 1M on s3+backup, zstd on files, atime off
[ ] 2.4  tenants key exported, stored offline in 2 places, download deleted
[ ] 3.3  5 snapshot tasks, distinct prefixes; hourly snapshots present on tenants and k8s
[ ] 3.3  no new quota alerts on storage/k8s/vols after 24 h
[ ] 4.4  share restricted to current node IPs, mapall platform-backup, smoke pod wrote as uid 999
[ ] 4.4  smoke pod could not list truenas-config tarballs; nfs-smoke namespace deleted
[ ] 5.4  config tarball present, contains db + seed, integrity_check ok
[ ] 5.4  day 2: second tarball, no cron alert
```

What follows, each in its own ticket under the epic: pointing the dump jobs
at this share (a `deployments` PR), the off-site copy to B2, the S3 gateway on
the NAS, and alerts on tenant quota and snapshot age.
