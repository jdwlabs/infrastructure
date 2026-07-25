# MinIO Terraform State Backend: TLS Cutover Runbook

This runbook switches the Terraform state backend — MinIO running on the TrueNAS
host — from plaintext HTTP to TLS. The certificate material already exists in the
repo vault (`terraform/backend-tls.enc.yaml`): an internal CA (public half
committed as `terraform/minio-ca.crt`) and a server certificate for
`192.168.1.205` with `127.0.0.1`/`localhost` SANs, valid until 2029-07-23.

**TrueNAS address:** `192.168.1.205` (SSH as `truenas_admin`, key `~/.ssh/id_ed25519_pve`)
**MinIO:** TrueNAS SCALE custom app `minio` (docker compose), S3 API on `:9000`,
console on `:9001`, data at `/mnt/storage/minio`

**Cutover window:** MinIO serves either HTTP or HTTPS, never both. From the
moment the app restarts with certs until the repo change is merged and
`terraform init -reconfigure` is run, a checkout pointing at the old `http://`
endpoint cannot reach state. Do all steps in one sitting.

---

## 1. Capture a pre-cutover state baseline (workstation, before touching TrueNAS)

From a checkout still on the `http://` endpoint, with backend credentials in the
environment (see `docs/secrets.md`):

```bash
cd terraform
terraform state pull > /tmp/state-before.json
sha256sum /tmp/state-before.json
grep -E '"(serial|lineage)"' /tmp/state-before.json
```

Record the checksum, `serial`, and `lineage`.

## 2. Hydrate the server cert and key from the vault (workstation)

MinIO expects a certs directory containing `public.crt` and `private.key`:

```bash
mkdir -p /tmp/minio-certs
sops decrypt --extract '["server_certificate"]' terraform/backend-tls.enc.yaml > /tmp/minio-certs/public.crt
sops decrypt --extract '["server_private_key"]' terraform/backend-tls.enc.yaml > /tmp/minio-certs/private.key
```

Never `cat`/`source` the decrypted key; it goes straight to files and then to
TrueNAS.

## 3. Create the certs dataset

The certs directory must be a **ZFS dataset**, not an ordinary directory. TrueNAS
validates custom-app host paths against real datasets, and a path that is not one
does not merely get rejected — the app silently discards the entire `volumes:`
block on save, taking the working `/data` mount with it. Docker then honours the
`VOLUME /data` declaration in the MinIO image by creating an anonymous volume,
and MinIO initialises an empty backend against it. The service comes up healthy,
serving nothing, and the scoped access key appears not to exist because MinIO's
IAM database lives in `.minio.sys` on the unmounted volume.

TrueNAS UI → **Datasets** → select `storage` → **Add Dataset**, name
`minio-certs`, preset `Generic`, defaults otherwise.

Confirm it is genuinely a dataset before going further — `ls` cannot tell a
dataset mountpoint from a directory, so ask ZFS:

```bash
ssh -i ~/.ssh/id_ed25519_pve truenas_admin@192.168.1.205 'zfs list -o name,mountpoint | grep minio'
```

Both `storage/minio` and `storage/minio-certs` must appear. If `minio-certs` is
absent, stop — the compose edit in step 5 will strip the mounts.

## 4. Install the certs on TrueNAS

Everything under `/mnt/storage` is `root:root` and `truenas_admin` (uid 950) has
no write there, so the certs are staged in the home directory and moved with
sudo. MinIO's data dir is root-owned too — the container runs as root, so the
installed certs are root-owned to match. `ssh -t` is required for the sudo
password prompt to render.

```bash
scp -i ~/.ssh/id_ed25519_pve /tmp/minio-certs/public.crt /tmp/minio-certs/private.key \
  truenas_admin@192.168.1.205:~/
ssh -t -i ~/.ssh/id_ed25519_pve truenas_admin@192.168.1.205 '
  sudo mv ~/public.crt ~/private.key /mnt/storage/minio-certs/ &&
  sudo chown root:root /mnt/storage/minio-certs/public.crt /mnt/storage/minio-certs/private.key &&
  sudo chmod 600 /mnt/storage/minio-certs/private.key &&
  sudo chmod 644 /mnt/storage/minio-certs/public.crt &&
  sudo ls -l /mnt/storage/minio-certs
'
rm -rf /tmp/minio-certs
```

The `&&` chain stops before anything moves if sudo fails. `sudo mv` (not `cp`)
is what clears the staged key from the home directory — if the chain breaks
partway, check `ls ~/private.key` on the host and remove it before retrying.

## 5. Update the custom app compose (TrueNAS UI)

TrueNAS UI → **Apps → minio → Edit**. Select the whole document and replace it —
pasting into the existing text produces duplicate mapping keys and the save is
rejected. Three changes: add `--certs-dir /certs` to the command, mount the certs
dataset read-only, and relax the healthcheck's certificate verification (it hits
`localhost` before any trust is established inside the container):

```yaml
services:
  minio:
    image: minio/minio:RELEASE.2025-09-07T16-13-09Z
    command: ["server", "/data", "--certs-dir", "/certs", "--console-address", ":9001"]
    environment:
      MINIO_ROOT_USER: "<keep existing value>"
      MINIO_ROOT_PASSWORD: "<keep existing value>"
    ports:
      - "9000:9000"
      - "9001:9001"
    restart: unless-stopped
    healthcheck:
      test: ["CMD", "mc", "ready", "local", "--insecure"]
      interval: 30s
      retries: 3
      timeout: 10s
    volumes:
      - /mnt/storage/minio:/data
      - /mnt/storage/minio-certs:/certs:ro
```

Leave the `environment` values exactly as they are. Save — TrueNAS recreates the
container. `mc` ships in the image and the existing healthcheck already uses it;
`--insecure` is the smallest change that keeps it working against the new TLS
listener. A `curl`-based check is not a safe substitute — `curl` is not
guaranteed to be present.

**Verify the mounts before trusting anything else.** A green app status and a
passing healthcheck do not prove the volumes attached — `--insecure` passes
either way, and MinIO serves a freshly formatted empty backend perfectly
happily:

```bash
ssh -t -i ~/.ssh/id_ed25519_pve truenas_admin@192.168.1.205 '
  C=$(sudo docker ps -q --filter name=minio)
  sudo docker inspect -f "{{range .Mounts}}{{.Type}} | {{.Source}} -> {{.Destination}}{{println}}{{end}}" $C
  sudo docker exec $C ls -l /certs
  sudo docker logs --tail 30 $C 2>&1 | grep -iE "formatting|^API:"
'
```

All four must hold: two **`bind`** lines (a `volume` type line means the mounts
were stripped — revert immediately), `public.crt` + `private.key` visible under
`/certs`, **no** `Formatting 1st pool` line, and `API: https://`.

## 6. Verify TLS from the workstation

```bash
openssl s_client -connect 192.168.1.205:9000 -CAfile terraform/minio-ca.crt </dev/null 2>/dev/null \
  | openssl x509 -noout -subject -issuer -ext subjectAltName   # expect CN=192.168.1.205, jdwlabs minio-state CA, IP SAN
openssl s_client -connect 192.168.1.205:9000 -CAfile terraform/minio-ca.crt </dev/null 2>/dev/null \
  | grep "Verify return code"                                  # expect 0 (ok)
curl -s -o /dev/null -m 5 -w '%{http_code}\n' http://192.168.1.205:9000/minio/health/live  # expect 400 — plaintext refused
```

Use `openssl`, not `curl --cacert`, as the trust check on Windows. Git Bash ships
a Schannel-backed curl that ignores a PEM passed to `--cacert` and consults the
Windows certificate store instead, so it fails with exit 60 against a correctly
served internal-CA certificate. Terraform is unaffected — it uses Go's TLS stack
and reads `custom_ca_bundle` directly.

## 7. Cut the repo over and re-initialize

Merge the PR that flips `terraform/providers.tf` to `https://` +
`custom_ca_bundle`, update the working copy, then re-init (the backend
configuration changed, so Terraform requires it):

```bash
cd terraform
terraform init -reconfigure
```

Use `-reconfigure`, not `-migrate-state` — the state never moved; only the
endpoint scheme changed.

## 8. Verify state integrity post-cutover

```bash
terraform state pull > /tmp/state-after.json
sha256sum /tmp/state-before.json /tmp/state-after.json
```

The checksums must match (identical `serial`, `lineage`, and contents — nothing
wrote state in between). Then confirm locking and a clean read end-to-end:

```bash
terraform plan -lock-timeout=30s
```

Expect "No changes" (or only known drift) and no lock errors. Clean up
`/tmp/state-*.json` afterwards.

Add `-refresh=false` if the provider refresh is slow — locking is still
exercised, and a plan killed by an impatient timeout strands a lock that must
then be cleared with `terraform force-unlock <id>`.

## 9. Update the vaulted endpoint reference

`terraform/backend-credentials.enc.yaml` records the endpoint alongside the
access keys — keep it accurate:

```bash
sops set terraform/backend-credentials.enc.yaml '["s3_endpoint"]' '"https://192.168.1.205:9000"'
```

Commit that change through the normal PR flow.

---

## Certificate rotation (before 2029-07-23)

The CA (valid to 2036) stays; only the server cert is reissued. From the repo
root on a workstation with the vault key:

```bash
tmp=$(mktemp -d)
sops decrypt --extract '["ca_certificate"]' terraform/backend-tls.enc.yaml > "$tmp/ca.crt"
sops decrypt --extract '["ca_private_key"]' terraform/backend-tls.enc.yaml > "$tmp/ca.key"
openssl ecparam -name prime256v1 -genkey -noout -out "$tmp/server.key"
openssl req -new -key "$tmp/server.key" -subj "/CN=192.168.1.205" -out "$tmp/server.csr"
printf 'basicConstraints=CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\nsubjectAltName=IP:192.168.1.205,IP:127.0.0.1,DNS:localhost\n' > "$tmp/san.cnf"
openssl x509 -req -in "$tmp/server.csr" -CA "$tmp/ca.crt" -CAkey "$tmp/ca.key" \
  -days 1095 -sha256 -extfile "$tmp/san.cnf" -out "$tmp/server.crt"
```

(On Git Bash, prefix openssl commands with `MSYS_NO_PATHCONV=1` so `-subj`
paths are not mangled.)

Then: update `server_certificate`/`server_private_key` in
`terraform/backend-tls.enc.yaml` (`sops edit`, paste from `$tmp`), repeat steps
4 and 6 to install and verify, and `rm -rf "$tmp"`. The dataset from step 3
already exists, so it is not recreated. Clients keep working through the swap
because the CA is unchanged — no re-init needed.

## Rollback

If TLS misbehaves, edit the app compose back (remove `--certs-dir /certs`, the
certs volume, and revert the healthcheck to
`["CMD", "mc", "ready", "local"]`), and run terraform from a checkout with the
`http://` endpoint (or `terraform init -reconfigure` after reverting
`providers.tf`). State on disk is untouched either way.
