# Secret management

`talops` keeps its sensitive artifacts in an **encrypted vault committed to git**, so
they are shared across machines, backed up off-box, and revocable per-device — without
ever committing plaintext.

## What is protected

| Artifact | Plaintext (gitignored, local) | Encrypted vault (committed) |
| --- | --- | --- |
| Proxmox creds + cluster topology | `terraform/terraform.tfvars` | `terraform/terraform.tfvars.enc.yaml` |
| Terraform state backend creds (MinIO) | env vars / `~/.aws/credentials` (manual) | `terraform/backend-credentials.enc.yaml` |
| MinIO TLS server cert + internal CA keys | uploaded to TrueNAS (manual) | `terraform/backend-tls.enc.yaml` |
| Talos secrets bundle (cluster PKI, tokens) | `clusters/<name>/secrets/secrets.yaml` | `clusters/<name>/vault/secrets.enc.yaml` |
| Talos client config | `clusters/<name>/secrets/talosconfig` | `clusters/<name>/vault/talosconfig.enc.yaml` |
| Reconciler state | `clusters/<name>/state/bootstrap-state.json` | `clusters/<name>/vault/bootstrap-state.enc.yaml` |

The encrypted `*.enc.yaml` files are the **shared source of truth**. The plaintext copies
are a regenerable local cache: `talops` decrypts them on demand and re-encrypts them when
they change. Generated node/base machine configs are derived from the secrets bundle and are
never stored in the vault. `kubeconfig` is merged into your normal `~/.kube/config` and is
re-fetchable from the cluster, so it is not vaulted.

## How it works

- **Encryption:** [SOPS](https://github.com/getsops/sops) with [age](https://github.com/FiloSottile/age) recipients. Each operator/device has its own age keypair; only the **public** keys live in the repo (`.sops.yaml`). Private keys never leave the device.
- **Hydrate (auto, on every command):** decrypts the vault into the plaintext working files when they are missing or older than the vault.
- **Seal (auto, after a successful command):** re-encrypts changed plaintext back into the vault. Unchanged files are skipped, so git stays clean. Read-only commands (`status`, `plan`, `version`, the `secrets` group) never auto-seal. Set `TALOPS_NO_AUTOSEAL=1` to disable entirely while debugging.
- **Auto-lock (opt-in):** set `TALOPS_AUTOLOCK=1` to wipe the plaintext working copies after a successful run (failed runs keep them for debugging). Off by default.
- **Legacy fallback:** if `sops` is not installed, `talops` logs a warning and operates on plaintext only — the vault is simply disabled.

**Multi-device safety:**

- `talops` anchors to the repository root before running, so the vault always lives at
  `<repo>/clusters/<name>/vault` regardless of the directory you invoke it from — you can't
  accidentally create a second vault under `bootstrap/`.
- Before operating, `talops` refuses to run if your local branch is **behind its upstream**
  (the vault may be stale) — run `git pull` first, or pass `--allow-stale-vault` to override.
  It also warns if you have uncommitted vault changes left from a previous run. The
  behind-check uses your last-fetched ref; pass `--fetch` to run `git fetch` first for an
  accurate count.
- Concurrency is ultimately enforced by git: a `git push` of updated vault files is rejected
  if another device pushed first. Operate one device at a time, `git pull` before starting,
  and commit + push the `.enc.yaml` changes after.

## Prerequisites

Install `sops` and `age`, then generate your device key:

```bash
age-keygen -o ~/.config/sops/age/keys.txt   # prints your public key (age1...)
```

`talops` finds the key via `SOPS_AGE_KEY_FILE` or `~/.config/sops/age/keys.txt`.

## First-time setup (greenfield repo)

```bash
age-keygen -o ~/.config/sops/age/keys.txt
talops secrets add-device <your-age-public-key>   # creates .sops.yaml
talops secrets seal                               # encrypt existing plaintext into the vault
git add .sops.yaml clusters terraform && git commit -m "chore(secrets): initialize vault"
```

## Onboarding a new device

1. On the new device: `git clone` the repo and `age-keygen -o ~/.config/sops/age/keys.txt`; copy the printed public key.
2. On a device that already has access:
   ```bash
   talops secrets add-device <new-device-public-key>   # re-keys every vault file
   git commit -am "chore(secrets): authorize <device>" && git push
   ```
3. On the new device: `git pull`. Any `talops` command now hydrates automatically.

## Revoking a device

Remove its public key from `.sops.yaml` and re-key the remaining vault (e.g. by running
`talops secrets add-device` for any still-trusted key, which re-keys every file), then commit
and push.

Removing a recipient stops it decrypting *future* versions, but a previously-cloned device
already saw the secrets — so for a true revocation, **rotate the underlying credentials**:
regenerate the Proxmox API token and, if the cluster permits it, re-key the Talos secrets
bundle. Treat this as a manual, reviewed operation.

## Break-glass key

Generate one extra age key, store its **private** half offline (password manager / hardware
token), and keep it as a permanent recipient. If every device key is lost, the vault is
otherwise unrecoverable.

## Terraform remote state backend credentials

Terraform state lives in an S3-compatible MinIO bucket on the TrueNAS host
(`https://192.168.1.205:9000`, bucket `terraform-state`, native lockfile locking).
The backend block in `terraform/providers.tf` carries no credentials — Terraform
resolves them through the standard AWS chain. The canonical copy of the access
key is `terraform/backend-credentials.enc.yaml` (same SOPS/age vault flow, but
hydrated manually — `talops` does not manage this file).

The endpoint is TLS-only, serving a certificate issued by an internal CA. The
public CA certificate is committed in plaintext as `terraform/minio-ca.crt` and
wired into the backend via `custom_ca_bundle`, so `terraform init`/`plan` needs
no per-machine trust setup. Other S3 clients (`mc`, `rclone`, `aws s3`) reach
the same endpoint by exporting `AWS_CA_BUNDLE=terraform/minio-ca.crt` or the
tool's equivalent. The server cert/key and the CA private key are vaulted in
`terraform/backend-tls.enc.yaml`; cert rotation and the TrueNAS-side setup are
covered by `scenarios/minio-tls-state-backend.md`.

Setting up a new machine:

```bash
sops -d terraform/backend-credentials.enc.yaml   # read access_key_id / secret_access_key
```

Then either export them for the shell that runs `terraform`/`talops`:

```bash
export AWS_ACCESS_KEY_ID=<access_key_id>
export AWS_SECRET_ACCESS_KEY=<secret_access_key>
```

or install them once as the `default` profile in `~/.aws/credentials`:

```ini
[default]
aws_access_key_id = <access_key_id>
aws_secret_access_key = <secret_access_key>
```

The env vars win over the profile when both are present. The file also holds the
MinIO root credentials (break-glass; day-to-day access uses the scoped
`terraform-state-rw` key). Never write these values into `*.tf`, `*.tfvars`, or
shell history — and never `source`/`eval` a decrypted secrets file.

**One-time migration for existing machines:** a working directory whose
`.terraform/` was initialized against the old local backend must be
re-initialized once (`talops` skips init when `.terraform/` exists, and plan
fails with "Backend initialization required" until this is done). From
`terraform/`, run:

```bash
terraform init -migrate-state   # move local terraform.tfstate into MinIO
```

or, if the state already lives in the bucket, delete `.terraform/` and re-run
`terraform init`.

## Command reference

| Command | Purpose |
| --- | --- |
| `talops secrets status` | Show recipients and per-artifact plaintext/encrypted state |
| `talops secrets hydrate` | Decrypt the vault into plaintext working files |
| `talops secrets seal` | Encrypt changed plaintext into the vault |
| `talops secrets lock` | Seal, then remove the plaintext working copies |
| `talops secrets edit <name>` | Edit `tfvars`/`secrets`/`talosconfig`/`state` in `$EDITOR` and re-seal |
| `talops secrets add-device <pubkey>` | Authorize an age key and re-key the vault |
