# TrueNAS NAS Integration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the 35.1 TiB TrueNAS box to `pve-cluster-1` as an NFS secondary storage tier — backing Proxmox ISO/backup/migratable-VMs and Kubernetes RWX/bulk PVs — without disturbing the Longhorn default tier.

**Architecture:** Three consuming tiers over one 1GbE LAN. TrueNAS exports ZFS datasets via NFS. Proxmox gets two cluster-wide NFS storages. Kubernetes provisions PVs dynamically through the `democratic-csi` `freenas-api-nfs` driver, which calls the TrueNAS API (key sourced from Vault via ESO) to create child datasets + NFS exports on demand. Longhorn stays the default StorageClass; `truenas-nfs` is opt-in.

**Tech Stack:** TrueNAS SCALE 25.10.4 (`midclt`/ZFS/NFS), Proxmox VE 9.2.3 (`pvesm`/`qm`), Kubernetes + ArgoCD GitOps, democratic-csi Helm chart, External Secrets Operator + Vault, `platformctl` (Go) for Vault seeding.

## Global Constraints

- **Network is 1GbE end-to-end** — NFS only; no iSCSI/SMB. NFS mount option `vers=4.2`.
- **NFS access control:** export to `192.168.1.0/24`, `maproot=root`.
- **Longhorn stays default.** `truenas-nfs` StorageClass MUST be `is-default-class: "false"`, `reclaimPolicy: Retain`.
- **Vault path:** `kv/truenas-csi`, field `api_key`. Field name MUST match the ExternalSecret `property:` and the `platformctl` seed spec `Name`.
- **Repo contract — infrastructure:** never `git push`; never run `terraform apply`/`destroy` autonomously; `kubectl apply`/`delete` out of scope (GitOps only); read-only `kubectl get`/`describe`/`logs` allowed.
- **Repo contract — platform:** drive cluster ops via `platformctl`; specs/plans in `docs/superpowers/` are append-only.
- **TrueNAS sudo:** `truenas_admin` has `sudo ALL` but **`NOPASSWD` is empty** — every `sudo`/`midclt` write is an **operator-run interactive step**, not agent-automatable. Read-only `zfs list`/`midclt call *.query` run without sudo.
- **No secret values in Git.** API key flows Vault → ESO → driver config secret only.

---

### Task 1: TrueNAS datasets, NFS shares, and API key

**Files:** None in-repo. Operational changes on TrueNAS `192.168.1.205`, executed by the operator over an interactive SSH session (`ssh truenas_admin@192.168.1.205`).

**Interfaces:**
- Consumes: nothing.
- Produces:
  - Datasets `storage/proxmox`, `storage/backup`, `storage/k8s`, `storage/k8s/vols`, `storage/k8s/snaps`.
  - Static NFS exports for `/mnt/storage/proxmox` and `/mnt/storage/backup` (the k8s exports are created dynamically by the driver, so no static k8s share).
  - A TrueNAS **API key** string with permission to manage `pool.dataset` and `sharing.nfs` — handed to Task 2 for Vault seeding (value: `api_key`; also note `host=192.168.1.205`, `port=80`, `protocol=http`).

- [ ] **Step 1: Create the datasets** *(operator-run)*

```bash
sudo midclt call pool.dataset.create '{"name": "storage/proxmox"}'
sudo midclt call pool.dataset.create '{"name": "storage/backup"}'
sudo midclt call pool.dataset.create '{"name": "storage/k8s"}'
sudo midclt call pool.dataset.create '{"name": "storage/k8s/vols"}'
sudo midclt call pool.dataset.create '{"name": "storage/k8s/snaps"}'
```

- [ ] **Step 2: Verify the datasets exist** *(agent — read-only, no sudo)*

Run from the workstation:
```bash
ssh -i ~/.ssh/id_ed25519_pve truenas_admin@192.168.1.205 \
  "zfs list -r storage -o name | grep -E 'storage/(proxmox|backup|k8s)'"
```
Expected: 5 lines — `storage/proxmox`, `storage/backup`, `storage/k8s`, `storage/k8s/vols`, `storage/k8s/snaps`.

- [ ] **Step 3: Create the two static NFS shares** *(operator-run)*

```bash
sudo midclt call sharing.nfs.create '{
  "path": "/mnt/storage/proxmox",
  "networks": ["192.168.1.0/24"],
  "maproot_user": "root", "maproot_group": "root",
  "comment": "Proxmox VM disks / ISO / templates"
}'
sudo midclt call sharing.nfs.create '{
  "path": "/mnt/storage/backup",
  "networks": ["192.168.1.0/24"],
  "maproot_user": "root", "maproot_group": "root",
  "comment": "Proxmox vzdump backups"
}'
```

- [ ] **Step 4: Verify NFS shares are exported** *(agent — read-only)*

```bash
ssh -i ~/.ssh/id_ed25519_pve truenas_admin@192.168.1.205 \
  "midclt call sharing.nfs.query | python3 -c 'import sys,json;[print(s[\"path\"],s[\"enabled\"]) for s in json.load(sys.stdin)]'"
```
Expected: `/mnt/storage/proxmox True` and `/mnt/storage/backup True`.

- [ ] **Step 5: Create the API key** *(operator-run)*

```bash
sudo midclt call api_key.create '{"name": "democratic-csi"}'
```
Expected: JSON containing a `key` field (the only time the full key is shown — capture it now).
Note: if SCALE 25.10 rejects this schema demanding a user binding, retry with `'{"name":"democratic-csi","username":"root"}'`. Verify the key has dataset + NFS-share API permissions.

- [ ] **Step 6: Verify the API key works** *(agent — read-only, using the new key)*

```bash
curl -sk -H "Authorization: Bearer <API_KEY>" \
  http://192.168.1.205/api/v2.0/pool/dataset/id/storage%2Fk8s | head -c 200
```
Expected: a JSON object describing `storage/k8s` (not a 401). Hand the key to Task 2; do **not** write it to any file.

---

### Task 2: Seed the TrueNAS API key into Vault via `platformctl`

**Files:**
- Modify: `cli/internal/bootstrap/phase4_vault_seed.go` (add to `staticSeedSpecs`, the map at line ~42)
- Test: `cli/internal/bootstrap/phase4_vault_seed_test.go` (add a case if a seed-spec table test exists; otherwise `go test ./...` covers compilation + existing coverage)

**Interfaces:**
- Consumes: API key string from Task 1.
- Produces: Vault secret `kv/truenas-csi` with field `api_key`. The field name `api_key` is the contract consumed by Task 4's ExternalSecret `property: api_key`.

- [ ] **Step 1: Write/extend the failing test**

If `phase4_vault_seed_test.go` already asserts over `staticSeedSpecs`, add:
```go
func TestStaticSeedSpecs_TruenasCSI(t *testing.T) {
	spec, ok := staticSeedSpecs["truenas-csi"]
	if !ok {
		t.Fatal("truenas-csi seed spec missing")
	}
	if spec.Path != "truenas-csi" {
		t.Fatalf("path = %q, want truenas-csi", spec.Path)
	}
	if len(spec.Fields) != 1 || spec.Fields[0].Name != "api_key" {
		t.Fatalf("fields = %+v, want single api_key field", spec.Fields)
	}
	if spec.Fields[0].EnvVar != "PLATFORMCTL_TRUENAS_CSI_API_KEY" {
		t.Fatalf("env = %q", spec.Fields[0].EnvVar)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd cli && go test ./internal/bootstrap/ -run TestStaticSeedSpecs_TruenasCSI -v`
Expected: FAIL — `truenas-csi seed spec missing`.

- [ ] **Step 3: Add the seed spec**

In `cli/internal/bootstrap/phase4_vault_seed.go`, inside `staticSeedSpecs`:
```go
	"truenas-csi": {Path: "truenas-csi", Fields: []seedField{
		{"api_key", "PLATFORMCTL_TRUENAS_CSI_API_KEY", true, false},
	}},
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd cli && go test ./internal/bootstrap/ -run TestStaticSeedSpecs_TruenasCSI -v && go build ./...`
Expected: PASS, build clean.

- [ ] **Step 5: Commit**

```bash
git add cli/internal/bootstrap/phase4_vault_seed.go cli/internal/bootstrap/phase4_vault_seed_test.go
git commit -m "feat(bootstrap): add truenas-csi vault seed spec"
```

- [ ] **Step 6: Seed Vault** *(operator-run — needs the real key + Vault access)*

```bash
export PLATFORMCTL_TRUENAS_CSI_API_KEY='<API_KEY from Task 1>'
platformctl bootstrap seed truenas-csi --json
```
Expected: a `{"...","status":"ok",...}` event. Verify (read-only): the ExternalSecret in Task 4 will later resolve; for now confirm the path exists per existing seed conventions.

---

### Task 3: Proxmox cluster-wide NFS storage

**Files:** None in-repo (Proxmox `storage.cfg` is cluster-replicated; `pvesm add` from any one node applies to all). Run from pve1 (`ssh root@192.168.1.200`).

**Interfaces:**
- Consumes: NFS shares from Task 1 (`/mnt/storage/proxmox`, `/mnt/storage/backup`).
- Produces: Proxmox storages `truenas-vmdisks` (images,iso,vztmpl) and `truenas-backup` (backup), visible on all 5 nodes.

- [ ] **Step 1: Add the two NFS storages** *(operator-run / agent with care — this mutates cluster config)*

```bash
ssh -i ~/.ssh/id_ed25519_pve root@192.168.1.200 \
  "pvesm add nfs truenas-vmdisks --server 192.168.1.205 --export /mnt/storage/proxmox --content images,iso,vztmpl --options vers=4.2"
ssh -i ~/.ssh/id_ed25519_pve root@192.168.1.200 \
  "pvesm add nfs truenas-backup --server 192.168.1.205 --export /mnt/storage/backup --content backup --options vers=4.2 --prune-backups keep-last=3"
```

- [ ] **Step 2: Verify both storages are active on every node** *(agent — read-only)*

```bash
for ip in 200 201 202 203 204; do
  echo "=== pve .$ip ==="
  ssh -i ~/.ssh/id_ed25519_pve root@192.168.1.$ip \
    "pvesm status | grep -E 'truenas-vmdisks|truenas-backup'"
done
```
Expected: on each node, both storages listed with status `active`.

- [ ] **Step 3: Verify a backup writes to the NAS** *(operator-run)*

Pick an existing low-risk guest (e.g. minecraft VMID 1000) and run a one-off backup:
```bash
ssh -i ~/.ssh/id_ed25519_pve root@192.168.1.200 \
  "vzdump 1000 --storage truenas-backup --mode snapshot --compress zstd"
```
Expected: completes `OK`; archive appears in `pvesm list truenas-backup`.

- [ ] **Step 4: Verify live migration across nodes** *(operator-run)*

Create a throwaway VM with its disk on `truenas-vmdisks`, start it, then online-migrate:
```bash
ssh -i ~/.ssh/id_ed25519_pve root@192.168.1.200 \
  "qm create 9999 --name nfs-migrate-test --memory 1024 --net0 virtio,bridge=vmbr0 --scsihw virtio-scsi-single --scsi0 truenas-vmdisks:8 && qm start 9999"
ssh -i ~/.ssh/id_ed25519_pve root@192.168.1.200 "qm migrate 9999 pve2 --online"
```
Expected: migration completes; `qm status 9999` shows `running` on pve2.
Cleanup: `qm stop 9999 && qm destroy 9999`.

---

### Task 4: Kubernetes `democratic-csi` service (platform GitOps)

**Files (platform repo):**
- Create: `tenants/platform/services/democratic-csi/values.yaml`
- Create: `tenants/platform/services/democratic-csi/postInstall/external-secret.yaml`
- Create: `tenants/platform/services/democratic-csi/postInstall/storageclass-truenas-nfs.yaml`
- Modify: `tenants/platform/tenant.yaml` (add namespace, extraSourceRepo, service entry)

**Interfaces:**
- Consumes: Vault `kv/truenas-csi` field `api_key` (Task 2); dataset `storage/k8s/vols` + `storage/k8s/snaps` and API key (Task 1).
- Produces: StorageClass `truenas-nfs` (non-default, Retain, RWX+RWO).

- [ ] **Step 1: Pin the chart version**

```bash
helm repo add democratic-csi https://democratic-csi.github.io/charts/ && helm repo update
helm search repo democratic-csi/democratic-csi --versions | head -3
```
Use the latest stable tag in place of `<CHART_VERSION>` below (candidate at time of writing: `0.14.7`).

- [ ] **Step 2: Register namespace, repo, and service in `tenant.yaml`**

Add to `namespaces:`:
```yaml
  - name: democratic-csi
    quotaTier: platform
    networkPolicy: platform
```
Add to `project.extraSourceRepos:`:
```yaml
    - 'https://democratic-csi.github.io/charts/'
```
Add to `services:` (Wave 1, alongside longhorn):
```yaml
  - name: democratic-csi
    chart: democratic-csi
    repo: https://democratic-csi.github.io/charts/
    revision: <CHART_VERSION>
    namespace: democratic-csi
    postInstall: true
    syncWave: 1
```

- [ ] **Step 3: Write `values.yaml`**

```yaml
csiDriver:
  name: "org.democratic-csi.nfs"

# Driver config is supplied as a rendered secret (api key from Vault via ESO),
# so it is never committed to Git. See postInstall/external-secret.yaml.
driver:
  existingConfigSecret: democratic-csi-driver-config

storageClasses: []   # StorageClass managed declaratively in postInstall

controller:
  driver:
    image: democratic-csi/democratic-csi
node:
  driver:
    image: democratic-csi/democratic-csi
```

- [ ] **Step 4: Write `postInstall/external-secret.yaml`** (renders the full driver config with the key injected — mirrors longhorn's templated secret)

```yaml
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: democratic-csi-driver-config
  namespace: democratic-csi
spec:
  refreshInterval: 1m
  secretStoreRef:
    name: vault
    kind: ClusterSecretStore
  target:
    name: democratic-csi-driver-config
    template:
      data:
        driver-config-file.yaml: |
          driver: freenas-api-nfs
          httpConnection:
            protocol: http
            host: 192.168.1.205
            port: 80
            apiKey: "{{ .api_key }}"
            allowInsecure: true
          zfs:
            datasetParentName: storage/k8s/vols
            detachedSnapshotsDatasetParentName: storage/k8s/snaps
            datasetEnableQuotas: true
            datasetEnableReservation: false
          nfs:
            shareHost: 192.168.1.205
            shareAlldirs: false
            shareAllowedNetworks:
              - 192.168.1.0/24
            shareMaprootUser: root
            shareMaprootGroup: root
  data:
    - secretKey: api_key
      remoteRef:
        key: truenas-csi
        property: api_key
```

- [ ] **Step 5: Write `postInstall/storageclass-truenas-nfs.yaml`**

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: truenas-nfs
  annotations:
    # NFS-backed capacity tier on the TrueNAS RAIDZ2 pool (35TiB HDD over 1GbE).
    # Non-default and Retain: Longhorn remains the default fast tier; this class
    # is opt-in for bulk and RWX workloads whose data must survive PVC churn.
    storageclass.kubernetes.io/is-default-class: "false"
provisioner: org.democratic-csi.nfs
allowVolumeExpansion: true
reclaimPolicy: Retain
volumeBindingMode: Immediate
parameters:
  fsType: nfs
  detachedVolumesFromSnapshots: "false"
```

- [ ] **Step 6: Validate manifests locally**

Run (in platform repo):
```bash
yamllint tenants/platform/services/democratic-csi/ tenants/platform/tenant.yaml
platformctl tenants validate
```
Expected: no errors; tenant validation passes.

- [ ] **Step 7: Commit (platform repo)**

```bash
git add tenants/platform/services/democratic-csi/ tenants/platform/tenant.yaml
git commit -m "feat(storage): add democratic-csi truenas-nfs StorageClass"
```

- [ ] **Step 8: Verify ArgoCD sync after merge** *(agent — read-only)*

After the platform PR merges and ArgoCD syncs:
```bash
kubectl get pods -n democratic-csi
kubectl get storageclass truenas-nfs
kubectl get externalsecret -n democratic-csi democratic-csi-driver-config
```
Expected: CSI controller + node pods `Running`; `truenas-nfs` present (NOT marked default); ExternalSecret `SecretSynced=True`.

- [ ] **Step 9: Verify a RWX PVC binds and is shared across nodes** *(operator-run — validation exception to the GitOps-only rule; throwaway, deleted immediately)*

Apply this temporary manifest by hand (`kubectl apply -f`), confirm, then delete:
```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata: { name: truenas-rwx-test, namespace: default }
spec:
  accessModes: ["ReadWriteMany"]
  storageClassName: truenas-nfs
  resources: { requests: { storage: 1Gi } }
```
Verify: `kubectl get pvc truenas-rwx-test` → `Bound`; on TrueNAS a child dataset appears under `storage/k8s/vols`. Mount it write/read from two pods scheduled on different nodes.
Cleanup: `kubectl delete pvc truenas-rwx-test` (Retain → manually remove the orphaned PV + TrueNAS dataset).

---

### Task 5: Documentation

**Files (infrastructure repo):**
- Create: `scenarios/truenas-nfs-storage.md`
- Modify: `docs/ARCHITECTURE.md` (add a storage tier section)

**Interfaces:**
- Consumes: the working integration from Tasks 1–4.
- Produces: operator runbook + architecture record.

- [ ] **Step 1: Write the runbook `scenarios/truenas-nfs-storage.md`**

Cover, as a step-by-step runbook matching the style of existing `scenarios/`:
- TrueNAS dataset/share/API-key layout (the 5 datasets, 2 static shares, dynamic k8s shares).
- Rotating the API key: `midclt call api_key.create`, reseed `platformctl bootstrap seed truenas-csi`, ESO refresh.
- Proxmox storage map (`truenas-vmdisks`, `truenas-backup`) + how to take/restore a vzdump.
- k8s: when to choose `truenas-nfs` vs default `longhorn`; Retain reclaim implies manual PV/dataset cleanup.

- [ ] **Step 2: Add a storage tier section to `docs/ARCHITECTURE.md`**

Document the two-tier model: Longhorn (default, on-node NVMe, replicated) vs TrueNAS NFS (`truenas-nfs`, capacity, RWX), and the rule that Talos VM disks stay on local NVMe.

- [ ] **Step 3: Commit (infrastructure repo)**

```bash
git add scenarios/truenas-nfs-storage.md docs/ARCHITECTURE.md
git commit -m "docs: add TrueNAS NFS storage runbook and architecture section"
```

---

## Self-Review

**Spec coverage:**
- TrueNAS datasets/shares/API-key → Task 1. ✓
- API key → Vault → Task 2. ✓
- Proxmox `truenas-vmdisks` + `truenas-backup`, backup + live-migrate checks → Task 3. ✓
- democratic-csi service, ESO secret, `truenas-nfs` StorageClass, tenant registration → Task 4. ✓
- Longhorn stays default; Talos VMs stay on local NVMe → enforced in Global Constraints + Task 4 SC annotation + Task 5 doc. ✓
- Docs/runbook → Task 5. ✓
- Acceptance criteria: each maps to a verification step (datasets→1.2, shares→1.4, Vault key→2.6, Proxmox visible→3.2, backup→3.3, live-migrate→3.4, SC non-default→4.8, RWX bind→4.9). ✓

**Open items deliberately deferred to implementation (verification steps, not placeholders):** democratic-csi chart version pin (Task 4 Step 1); TrueNAS 25.10 `api_key.create` schema may require `username` (Task 1 Step 5 fallback noted). Both have explicit resolve-and-verify steps.

**Out of scope (from spec):** pve5 idle capacity repurpose, SLOG SSD, storage VLAN/bond, codifying TrueNAS/Proxmox steps as IaC.
