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

## 3. Install the certs on TrueNAS

```bash
ssh -i ~/.ssh/id_ed25519_pve truenas_admin@192.168.1.205 'mkdir -p /mnt/storage/minio-certs'
scp -i ~/.ssh/id_ed25519_pve /tmp/minio-certs/public.crt /tmp/minio-certs/private.key \
  truenas_admin@192.168.1.205:/mnt/storage/minio-certs/
ssh -i ~/.ssh/id_ed25519_pve truenas_admin@192.168.1.205 \
  'chmod 600 /mnt/storage/minio-certs/private.key && chmod 644 /mnt/storage/minio-certs/public.crt'
rm -rf /tmp/minio-certs
```

## 4. Update the custom app compose (TrueNAS UI)

TrueNAS UI → **Apps → minio → Edit**. Three changes to the compose YAML — add
`--certs-dir /certs` to the command, mount the certs directory read-only, and
switch the healthcheck to HTTPS (`-k` because the in-container check hits
`localhost` before trust is established):

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
      test: ["CMD", "curl", "-kf", "https://localhost:9000/minio/health/live"]
      interval: 30s
      retries: 3
      timeout: 10s
    volumes:
      - /mnt/storage/minio:/data
      - /mnt/storage/minio-certs:/certs:ro
```

Leave the `environment` values exactly as they are. Save — TrueNAS recreates the
container. If the healthcheck flaps with `curl` missing from the image, use
`["CMD", "mc", "ready", "local", "--insecure"]` instead.

## 5. Verify TLS from the workstation

```bash
curl --cacert terraform/minio-ca.crt -s -o /dev/null -w '%{http_code}\n' \
  https://192.168.1.205:9000/minio/health/live        # expect 200
openssl s_client -connect 192.168.1.205:9000 -CAfile terraform/minio-ca.crt </dev/null 2>/dev/null \
  | openssl x509 -noout -issuer -ext subjectAltName    # expect the jdwlabs minio-state CA + IP SAN
curl -s -o /dev/null -m 5 http://192.168.1.205:9000/minio/health/live || echo "plaintext refused (expected)"
```

## 6. Cut the repo over and re-initialize

Merge the PR that flips `terraform/providers.tf` to `https://` +
`custom_ca_bundle`, update the working copy, then re-init (the backend
configuration changed, so Terraform requires it):

```bash
cd terraform
terraform init -reconfigure
```

Use `-reconfigure`, not `-migrate-state` — the state never moved; only the
endpoint scheme changed.

## 7. Verify state integrity post-cutover

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

## 8. Update the vaulted endpoint reference

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
3–5 to install and verify, and `rm -rf "$tmp"`. Clients keep working through the
swap because the CA is unchanged — no re-init needed.

## Rollback

If TLS misbehaves, edit the app compose back (remove `--certs-dir /certs`, the
certs volume, and revert the healthcheck to
`["CMD", "mc", "ready", "local"]`), and run terraform from a checkout with the
`http://` endpoint (or `terraform init -reconfigure` after reverting
`providers.tf`). State on disk is untouched either way.
