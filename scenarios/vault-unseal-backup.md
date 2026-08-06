# Runbook: Vault unseal material backup + restore

Status: MECHANISM READY — the capture step is human-run (it touches real
secret material), same gating as `terraform apply` in this repo.

## Why

`core`'s Vault runs with `-key-shares=1 -key-threshold=1`: a single Shamir
key unseals it, and the `vault-auto-unseal` CronJob
(`platform/tenants/platform/services/vault/postInstall/vault-unseal-cronjob.yaml`)
reads that key from the `vault-unseal-keys` k8s Secret to re-seal every 2
minutes. The root token and the same key also live in the `vault-init` k8s
Secret (`platformctl bootstrap` phase 3 writes both — see
`platform/cli/internal/bootstrap/phase3_vault_init.go`), plus a local,
gitignored copy at `.secrets/vault-init.json` on whichever workstation ran
bootstrap.

All three copies are **in-cluster or single-host**. If the cluster is lost
(the scenario this whole backup effort exists for), the k8s Secrets go with
it, and if the workstation disk is lost too, so does the `.secrets/` copy.
Vault cannot hold a backup of its own unseal keys — that's circular by
construction — so this material needs a location that depends on neither the
cluster nor one machine.

This is the piece of `JDWLABS-66` not already covered by the Talos secrets
vault (`JDWLABS-133`, done): that work committed `clusters/core/vault/{secrets,talosconfig,bootstrap-state}.enc.yaml`,
which covers Talos's own PKI (cluster CA, etcd CA, service-account key,
bootstrap tokens) — Talos generates the Kubernetes cluster CA as part of that
same `secrets.yaml` bundle, so the k8s CA is covered too. Vault's root token
and unseal key are a distinct secret, owned by the `platform` repo's
bootstrap flow, and were never in scope for JDWLABS-133.

## What this backs up

- `vault-init` Secret (namespace `vault`, key `vault-init.json`): root token
  + full Shamir key set as JSON.
- `vault-unseal-keys` Secret (namespace `vault`, key `unseal_key_1`): the
  single key the auto-unseal CronJob actually uses. Captured separately from
  the JSON blob above in case the two secrets ever drift (they're written by
  two different `upsertSecret` calls in the same `Apply()`, not atomically).

## Where it lives

`scenarios/vault-unseal-backup.sh` reads both Secrets via `kubectl get` (read
only — this repo's agent contract permits read-only `kubectl get`), writes a
combined plaintext JSON to `clusters/<cluster>/secrets/vault-unseal.json`
(gitignored, same local-cache convention as the Talos secrets bundle), then
SOPS-encrypts it to `clusters/<cluster>/vault/vault-unseal.enc.yaml` using
this repo's existing `.sops.yaml` creation rule — the same dual age
recipients (workstation key + the offline break-glass key from
`docs/secrets.md` "Break-glass key") already used for the Talos vault.

That reuse is what makes the location genuinely independent: the encrypted
file is committed to git (mirrored wherever the repo is cloned, including off
this cluster/network entirely) and its break-glass decryption key is already
required to be held offline, outside any repo device. Losing the cluster
loses neither.

## Running the backup

Human-run, from a `kubectl` context pointed at the live cluster, with `sops`/
`age`/`jq` installed (same prerequisites as `docs/secrets.md`):

```bash
./scenarios/vault-unseal-backup.sh core
git diff --stat clusters/core/vault/vault-unseal.enc.yaml   # review before committing
git add clusters/core/vault/vault-unseal.enc.yaml
git commit -m "chore(secrets): back up vault unseal material for core"
git push
```

Re-run after any Vault re-init (root token or unseal key rotation) — the
script always overwrites the encrypted file with the current live secrets,
matching the SOPS-blob-diff behavior of `talops secrets seal`.

## Restore

Order matters: the cluster (and Talos secrets) must exist and be reachable
before Vault can be unsealed, so this always runs after a Talos-level
restore, never instead of one.

1. Decrypt the backup:
   ```bash
   sops decrypt clusters/core/vault/vault-unseal.enc.yaml > /tmp/vault-unseal.json
   ```
2. If Vault was freshly re-initialized (new cluster, new Vault pods, no prior
   state) rather than merely sealed: the *old* root token and unseal key are
   only useful if Vault's underlying storage (the Longhorn PV backing its
   data dir) survived. If the PV is gone, this backup cannot recover Vault's
   *contents* — only re-authenticates to whatever Vault instance now exists.
   Losing the PV means every secret Vault held must be reseeded from
   elsewhere (ExternalSecrets sources, `platform/scripts/vault-seed.ps1`,
   etc.) — this backup does not substitute for that.
3. If the PV survived and Vault just needs re-sealing/re-authenticating:
   ```bash
   ROOT_TOKEN=$(jq -r '.vault_init.root_token' /tmp/vault-unseal.json)
   UNSEAL_KEY=$(jq -r '.unseal_keys.unseal_key_1' /tmp/vault-unseal.json)

   kubectl create secret generic vault-init -n vault \
     --from-literal=vault-init.json="$(jq -c '.vault_init' /tmp/vault-unseal.json)" \
     --dry-run=client -o yaml | kubectl apply -f -
   kubectl create secret generic vault-unseal-keys -n vault \
     --from-literal=unseal_key_1="$UNSEAL_KEY" \
     --dry-run=client -o yaml | kubectl apply -f -

   vault operator unseal "$UNSEAL_KEY"   # per Vault pod, or wait for the auto-unseal CronJob's next tick
   ```
4. Confirm: `vault status` reports unsealed on every pod; `platformctl
   bootstrap status` (or equivalent) reports the vault-init phase as done.
5. `rm /tmp/vault-unseal.json` when finished — it is a decrypted secret on
   local disk.

**This restore procedure has not yet been rehearsed end-to-end against a real
cluster.** Treat step 3 as the documented intent, not a verified sequence,
until it's run once for real — track that as a follow-up before relying on it
in an actual incident.

## Should `talops` (or `platformctl`) manage this automatically?

Recommendation: **not `talops`, and not yet `platformctl` either — keep this
manual for now.**

`talops`'s existing vault (`bootstrap/internal/secrets/vault.go`) is a
hydrate/seal cycle over *local plaintext files that talops itself manages* —
it never reaches into a live cluster's k8s Secrets, and the vault-init/
vault-unseal-keys Secrets aren't Talos-level material at all; they belong to
`platform`'s bootstrap flow (`phase3_vault_init.go`). Bolting live-cluster
reads onto `talops` would blur a boundary this repo's `AGENTS.md` draws
deliberately (`infrastructure` provisions the cluster; `platform` configures
what runs on it).

The cleaner long-term split, if this is worth automating later: a
`platformctl` command that dumps `vault-init.json` + `unseal_key_1` as JSON to
stdout (it already owns the shape and namespace of both Secrets, and
`platform/AGENTS.md` makes it the only sanctioned interface for cluster reads
there) — piped into `sops encrypt` here. That keeps "read the live secret"
in the repo that owns it and "encrypt + store off-cluster" in the repo that
already has the vault mechanism for it. Filed as a follow-up rather than
built now, since it's a `platform`-repo change and out of scope for this
ticket.
