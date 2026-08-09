# Runbook: DR drill — restore cluster access + secrets on a clean machine

Status: NOT YET EXECUTABLE — blocked. This document is the drill procedure
and pre-flight gate, prepared ahead of the blockers clearing. It has not been
run, and per its own gate below it cannot succeed yet. Do not attempt to
execute or simulate any step of it from an already-provisioned machine —
doing so proves nothing about clean-machine recoverability (see "Why a real
clean machine" below).

## Objective

Prove — not assume — that a clean machine plus the break-glass age key can
regain full operational control of the `core` cluster: `talosconfig`,
`kubeconfig`, Vault unseal, Terraform state.

## Pre-flight gate

Every item below must be true before starting the drill. As of this
writing, it is not.

| Gate | Status | Source |
| --- | --- | --- |
| Break-glass SOPS age key is stored somewhere other than this operator's primary workstation disk | **NOT MET** | `JDWLABS-305` — the recovery private key currently sits at `F:/Dev/secrets/jdwlabs-infra-age-recovery.key`, same physical disk as the primary device key. `JDWLABS-305`'s own description states it explicitly: "Blocks: DR restore drill (Phase 2) — a same-disk 'break-glass' key doesn't actually prove restorability from a clean machine." |
| Vault unseal material (root token + unseal key) has an offline, cluster-independent backup that has actually been captured and pushed | **NOT MET** | `JDWLABS-66` — the capture mechanism exists (`scenarios/vault-unseal-backup.sh`, `scenarios/vault-unseal-backup.md`, branch `chore/JDWLABS-66-offline-secrets-backup`) but the human-run capture step has never been executed against the live cluster, and that branch has not been pushed or merged. `JDWLABS-66` itself is `In Progress`, not `Done`. |
| Talos PKI is vaulted | MET | `JDWLABS-133` (Done) — `clusters/core/vault/{secrets,talosconfig,bootstrap-state}.enc.yaml` committed. |
| Second age recipient exists in `.sops.yaml` | MET on paper, **not independently verified by this drill** | `JDWLABS-132` (Done) marks the recipient added, but `JDWLABS-305` found the private half is not actually independent (see above) — a second *public* recipient without an independently-stored *private* key does not satisfy this drill's premise. |
| Terraform remote state backend is reachable with vaulted credentials | MET | `JDWLABS-135` (Done) — MinIO backend + `terraform/backend-credentials.enc.yaml`. |

Two of five gates are unmet. Both trace to key-custody problems, not
mechanism problems — the scripts and vault structures this drill exercises
already exist; the material they'd need to restore is not actually stored
independently of the machine being tested. Running this drill today would
either (a) use the primary key from the same disk, which doesn't test
anything, or (b) fail at the Vault-unseal step with no backup to recover.

**Do not run this drill until both `JDWLABS-66` and `JDWLABS-305` are
closed.** `JDWLABS-136`'s Jira "blocked by" links currently only list
`JDWLABS-132/133/135` (all Done) — that link set is stale relative to the
dependency `JDWLABS-305` itself declares in prose. Whoever picks up this
ticket should add `JDWLABS-66` and `JDWLABS-305` as formal blockers so the
board reflects reality.

## Why a real clean machine

This drill cannot be meaningfully executed from a machine that already has
cluster access, an existing `~/.config/sops/age/keys.txt`, or a populated
`~/.kube/config` / `~/.talos/`. The entire point is to prove recovery from
*zero* prior state using only: the break-glass age key, the git remotes, and
whatever a human can retrieve from wherever the break-glass key and any
password-manager-stored credentials actually live. An agent session running
inside this repo's existing checkout, on a workstation that already has
`kubectl`/`talosctl` configured, cannot exercise this — every credential the
drill is supposed to prove recoverable is already sitting in that session's
environment. This is why the drill must be run by a human, on a genuinely
separate physical or virtual machine, and why no part of it should be
simulated here.

## Drill procedure (once the gate above is fully MET)

Run on a machine with **no** prior jdwlabs state: no cloned repos, no
`~/.config/sops/age/`, no `~/.kube/config`, no `~/.talos/`. The primary
workstation and its keys are not touched or referenced during the drill —
only the break-glass key, retrieved from its offline location.

Start a timer before step 1; stop it after step 8. Record both the total
duration and a per-step timestamp in the results log (template below).

1. **Clone repos** — `platform`, `infrastructure` (and `deployments` if
   tenant-level secrets are in scope). No special access needed; both are
   plain `git clone` of public-to-org repos.
2. **Install prerequisites** — `sops`, `age`, `talosctl`, `kubectl`,
   `terraform`, `talops` (build from `infrastructure/bootstrap/` or fetch a
   release, per `infrastructure/AGENTS.md`).
3. **Install the break-glass age key** — retrieve the private key from its
   offline store (per the relocated location `JDWLABS-305` establishes) and
   place it at `~/.config/sops/age/keys.txt` (or set `SOPS_AGE_KEY_FILE`).
   This is the one step that intentionally uses material the primary
   workstation does not hold.
4. **Hydrate the Talos secrets vault** — from `infrastructure/`, run
   `talops secrets hydrate` (or `talops secrets status` first to confirm the
   break-glass key is recognized as a valid recipient). Confirm
   `clusters/core/secrets/secrets.yaml` and `clusters/core/secrets/talosconfig`
   materialize.
5. **Verify Talos access** — `talosctl --talosconfig clusters/core/secrets/talosconfig health`
   against the cluster's real endpoint (see `infrastructure/docs/host-addressing.md`
   for node IPs — this doc is plaintext/non-secret).
6. **Regenerate/merge kubeconfig** — `talosctl kubeconfig` using the
   hydrated `talosconfig`; confirm `kubectl get nodes` returns Ready nodes
   for all control-plane and worker nodes.
7. **Recover Vault unseal material** — decrypt
   `clusters/core/vault/vault-unseal.enc.yaml` per the restore procedure in
   `scenarios/vault-unseal-backup.md` §Restore, using the break-glass key.
   Confirm the recovered root token authenticates against the live Vault and
   the unseal key matches Vault's current seal status
   (`vault status` — read-only, via `platformctl` per `platform/AGENTS.md`'s
   binary contract, not raw `vault`).
8. **Verify Terraform state access** — decrypt
   `terraform/backend-credentials.enc.yaml`, export the MinIO credentials,
   run `terraform init` / `terraform plan` (plan only — never `apply` from
   an unverified drill machine) against the live remote state and confirm it
   reads cleanly with no drift beyond what's expected.

Do not perform any destructive or mutating action (`terraform apply`,
`talosctl reset`, `kubectl delete`, Vault re-init) during the drill. This is
a read/verify exercise only.

## Results log template

Fill in during the actual drill run — this table is empty until then.

| Step | Start | End | Result | Gap / friction found |
| --- | --- | --- | --- | --- |
| 1. Clone repos | | | | |
| 2. Install prerequisites | | | | |
| 3. Install break-glass key | | | | |
| 4. Hydrate Talos vault | | | | |
| 5. Verify Talos access | | | | |
| 6. Regenerate kubeconfig | | | | |
| 7. Recover Vault unseal material | | | | |
| 8. Verify Terraform state access | | | | |
| **Total duration** | | | | |

Every gap found gets its own ticket (per `JDWLABS-136`'s DoD) rather than a
note buried in this table — the table is for triage, not the permanent
record.

## Definition of done (from JDWLABS-136)

- [ ] `kubectl get nodes` + `talosctl health` + Vault unseal succeed from a
      genuinely clean machine, primary workstation untouched throughout
- [ ] Drill duration and every gap found are documented and ticketed

Neither can be checked off by this document. This runbook only removes the
"what do I actually do" ambiguity for whoever runs the drill once
`JDWLABS-66` and `JDWLABS-305` close.
