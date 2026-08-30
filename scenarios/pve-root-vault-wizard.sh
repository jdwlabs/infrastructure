#!/usr/bin/env bash
#
# pve-root-vault-wizard.sh
# Usage: ./pve-root-vault-wizard.sh [cluster-name]
#
# Interactive walkthrough: set a root PAM password on each Proxmox host
# (pve1-pve5) and store it in this cluster's Vault at
# kv/pve-hosts/<host>/root, so a pve host whose SSH key trust has died (the
# 2026-08-27 pve3 outage: pmxcfs died, taking every SSH key path with it,
# and no retrievable root password existed anywhere) has a remote-recovery
# path that doesn't depend on /etc/pve staying alive. See
# scenarios/host-remote-power-recovery.md and
# scenarios/pve-stale-node-ip-corosync.md for how this credential is used
# once it exists.
#
# This is a HUMAN-RUN script, same gating as `terraform apply` and
# scenarios/vault-unseal-backup.sh in this repo — it sets real root
# credentials on production hypervisors. The wizard automates the Vault
# read/write/verify plumbing; it deliberately does NOT capture, transmit,
# or log the password you type at each host's own `passwd` prompt — you
# type that directly into your own SSH session. It DOES ask you to paste
# that same password once, via a hidden prompt, so it can hand it to
# `vault kv put` over stdin (never as a CLI arg, never echoed, never
# written to disk) — that capture is unavoidable if the password is going
# to end up in Vault at all.
#
# Everything above the "STAGES" marker is the wizard library: do not hand-edit
# it. Author the per-step stages below the marker.

set -euo pipefail

# ──────────────────────────────────────────────────────────────────────────
# Wizard library — delightful, consistent UX. Identical across every wizard.
# ──────────────────────────────────────────────────────────────────────────

if [[ -t 1 ]] && command -v tput >/dev/null 2>&1 && [[ "$(tput colors 2>/dev/null || echo 0)" -ge 8 ]]; then
  BOLD=$(tput bold); DIM=$(tput dim); RESET=$(tput sgr0)
  BLUE=$(tput setaf 4); GREEN=$(tput setaf 2); YELLOW=$(tput setaf 3); RED=$(tput setaf 1)
else
  BOLD=""; DIM=""; RESET=""; BLUE=""; GREEN=""; YELLOW=""; RED=""
fi

# Author sets this at the top of the stages section.
TOTAL_STAGES=0

_STAGE_INDEX=0
ENV_FILE="${ENV_FILE:-.env}"
WRITTEN_ENV=()    # KEYs written to ENV_FILE this run
WRITTEN_SECRET=() # secret NAMEs set this run
SKIPPED=()        # things we couldn't do (e.g. gh missing)

# _clear — wipe the terminal so only the current step is on screen. No-op when
# output isn't a terminal, so piped logs stay readable.
_clear() {
  [[ -t 1 ]] || return 0
  if command -v tput >/dev/null 2>&1; then tput clear; else printf '\033[2J\033[3J\033[H'; fi
}

# banner "Title" — opening frame: what this wizard does.
banner() {
  _clear
  printf '\n%s%s  %s%s\n' "$BOLD" "$BLUE" "$1" "$RESET"
  printf '%s  %s stages%s\n\n' "$DIM" "$TOTAL_STAGES" "$RESET"
  printf '%s  You drive the browser; this wizard tells you exactly what to do and\n' "$DIM"
  printf '  captures the values you copy back. Stop any time with Ctrl-C and re-run\n'
  printf '  later — it remembers values already saved.%s\n' "$RESET"
  pause "Ready to start?"
}

# stage "Name" — clear the screen, then announce a stage and show progress.
# Clearing keeps only the current step on screen.
stage() {
  _clear
  _STAGE_INDEX=$((_STAGE_INDEX + 1))
  printf '\n%s%s▸ Stage %s/%s · %s%s\n' \
    "$BOLD" "$BLUE" "$_STAGE_INDEX" "$TOTAL_STAGES" "$1" "$RESET"
}

# say "..." — a plain instruction line.
say()  { printf '  %s\n' "$1"; }
# step "..." — a numbered-feeling action the human takes in the browser.
step() { printf '  %s•%s %s\n' "$BLUE" "$RESET" "$1"; }
note() { printf '  %s%s%s\n' "$DIM" "$1" "$RESET"; }
warn() { printf '  %s⚠ %s%s\n' "$YELLOW" "$1" "$RESET"; }

# open_url URL — open in the human's browser, cross-platform incl. WSL.
open_url() {
  local url="$1"
  printf '  %s↗ opening%s %s\n' "$GREEN" "$RESET" "$url"
  { if   command -v wslview     >/dev/null 2>&1; then wslview "$url"
    elif command -v explorer.exe >/dev/null 2>&1; then explorer.exe "$url"
    elif command -v xdg-open    >/dev/null 2>&1; then xdg-open "$url"
    elif command -v open        >/dev/null 2>&1; then open "$url"
    else warn "couldn't open a browser — visit it manually: $url"; fi
  } >/dev/null 2>&1 || warn "couldn't open a browser — visit it manually: $url"
}

# pause "msg" — wait for the human to confirm they've done the manual part.
pause() {
  printf '  %s%s%s ' "$DIM" "${1:-Press Enter to continue}" "$RESET"
  read -r _ || true
}

# confirm "question" — y/N gate; returns success on yes.
confirm() {
  local reply=""
  printf '  %s? %s [y/N] ' "$YELLOW" "$1"
  read -r reply || true
  [[ "$reply" =~ ^[Yy] ]]
}

# _existing KEY — current value of KEY in ENV_FILE, if any.
_existing() {
  [[ -f "$ENV_FILE" ]] || return 1
  local line; line=$(grep -E "^${1}=" "$ENV_FILE" | tail -n1) || return 1
  printf '%s' "${line#*=}"
}

# ask KEY "Prompt" — read a value into $KEY. Offers the existing .env value as
# a default on re-runs (Enter keeps it). Visible input (non-secret).
ask() {
  local key="$1" prompt="$2" current input
  current=$(_existing "$key" || true)
  if [[ -n "$current" ]]; then
    printf '  %s%s%s %s[Enter keeps current]%s ' "$BOLD" "$prompt" "$RESET" "$DIM" "$RESET"
  else
    printf '  %s%s%s ' "$BOLD" "$prompt" "$RESET"
  fi
  read -r input || true
  [[ -z "$input" && -n "$current" ]] && input="$current"
  printf -v "$key" '%s' "$input"
}

# ask_secret KEY "Prompt" — like ask, but input is hidden.
ask_secret() {
  local key="$1" prompt="$2" current input
  current=$(_existing "$key" || true)
  if [[ -n "$current" ]]; then
    printf '  %s%s%s %s[Enter keeps current]%s ' "$BOLD" "$prompt" "$RESET" "$DIM" "$RESET"
  else
    printf '  %s%s%s ' "$BOLD" "$prompt" "$RESET"
  fi
  read -rs input || true
  printf '\n'
  [[ -z "$input" && -n "$current" ]] && input="$current"
  printf -v "$key" '%s' "$input"
}

# write_env KEY VALUE — upsert KEY=VALUE into ENV_FILE (creates it; replaces
# any existing line). Idempotent.
write_env() {
  local key="$1" value="$2" tmp
  touch "$ENV_FILE"
  tmp=$(mktemp)
  grep -vE "^${key}=" "$ENV_FILE" > "$tmp" || true
  printf '%s=%s\n' "$key" "$value" >> "$tmp"
  mv "$tmp" "$ENV_FILE"
  WRITTEN_ENV+=("$key")
  printf '  %s✓ wrote%s %s → %s\n' "$GREEN" "$RESET" "$key" "$ENV_FILE"
}

# set_secret NAME VALUE — set a GitHub Actions repo secret via gh. Falls back
# to a warning (and records it) if gh is unavailable or unauthenticated.
set_secret() {
  local name="$1" value="$2"
  if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
    if printf '%s' "$value" | gh secret set "$name" >/dev/null 2>&1; then
      WRITTEN_SECRET+=("$name")
      printf '  %s✓ set%s GitHub secret %s\n' "$GREEN" "$RESET" "$name"
      return
    fi
  fi
  SKIPPED+=("GitHub secret $name (set it manually: gh secret set $name)")
  warn "skipped GitHub secret $name — gh not ready; set it later"
}

# set_var NAME VALUE — set a GitHub Actions repo variable (non-secret).
set_var() {
  local name="$1" value="$2"
  if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
    if gh variable set "$name" --body "$value" >/dev/null 2>&1; then
      printf '  %s✓ set%s GitHub variable %s\n' "$GREEN" "$RESET" "$name"
      return
    fi
  fi
  SKIPPED+=("GitHub variable $name")
  warn "skipped GitHub variable $name — gh not ready; set it later"
}

# finish — clear, then a closing summary of everything configured.
finish() {
  _clear
  printf '\n%s%s  ✓ Setup complete%s\n' "$BOLD" "$GREEN" "$RESET"
  (( ${#WRITTEN_ENV[@]} ))    && note "wrote ${#WRITTEN_ENV[@]} value(s) to $ENV_FILE: ${WRITTEN_ENV[*]}"
  (( ${#WRITTEN_SECRET[@]} )) && note "set ${#WRITTEN_SECRET[@]} GitHub secret(s): ${WRITTEN_SECRET[*]}"
  if (( ${#SKIPPED[@]} )); then
    printf '\n'; warn "still to do by hand:"
    for s in "${SKIPPED[@]}"; do note "  - $s"; done
  fi
  printf '\n'
}

# ──────────────────────────────────────────────────────────────────────────
# STAGES — author this section. One stage() per step the human takes.
# Replace the example below. Set TOTAL_STAGES to match the stages you write.
# ──────────────────────────────────────────────────────────────────────────

CLUSTER="${1:-core}"
VAULT_NAMESPACE="${VAULT_NAMESPACE:-vault}"
VAULT_POD="${VAULT_POD:-platform-vault-0}"

TOTAL_STAGES=6

# Not using the shared banner() here: it claims progress is remembered across
# a Ctrl-C and re-run, which is true for the Vault side of this wizard (a
# re-run just overwrites kv/pve-hosts/<host>/root with whatever you paste
# next) but not for the host side — a host whose password you already set
# doesn't "remember" that from this script's point of view, and re-running
# will ask you to confirm it again rather than silently skipping it. That's
# intentional (see "Step A" below), but the generic banner claim would be
# misleading here. Authoring a custom opening block instead of overriding
# the library.
_clear
printf '\n%s%s  %s%s\n' "$BOLD" "$BLUE" "pve root PAM password → Vault" "$RESET"
printf '%s  %s stages%s\n\n' "$DIM" "$TOTAL_STAGES" "$RESET"
printf '%s  This sets a REAL root password on each production Proxmox host and\n' "$DIM"
printf '  stores it in this cluster'"'"'s Vault. Nothing here can be undone\n'
printf '  automatically. You drive every mutating step (passwd, the SSH\n'
printf '  password test) yourself, at your own terminal — the wizard only\n'
printf '  handles the Vault plumbing and never sees the password you type at\n'
printf '  a host'"'"'s own prompt. Stop any time with Ctrl-C and re-run later;\n'
printf '  it starts over from Stage 1 and re-confirms each host rather than\n'
printf '  assuming a prior partial run is still accurate.%s\n' "$RESET"
pause "Ready to start?"

# ── Stage 1: prerequisites + Vault auth ────────────────────────────────────
stage "Prerequisites and Vault access"
say "This walks scenarios/host-remote-power-recovery.md's proposed fix end"
say "to end: a root PAM password on pve1-pve5, stored in Vault at"
say "kv/pve-hosts/<host>/root so a pmxcfs-dead node still has a remote"
say "recovery path."
confirm "kubectl is pointed at the live cluster ('kubectl get pods -n ${VAULT_NAMESPACE}' shows ${VAULT_POD} Running)?" \
  || { warn "Fix kubectl access first, then re-run this wizard."; exit 1; }
confirm "SSH key access to pve1-pve5 as root works today (this wizard needs it to reach each host)?" \
  || { warn "Get SSH key access first — see docs/host-addressing.md — then re-run."; exit 1; }

say ""
say "Checking Vault is reachable and unsealed..."
if VSTATUS=$(kubectl exec -n "$VAULT_NAMESPACE" "$VAULT_POD" -- vault status 2>&1); then
  note "Vault reachable."
else
  warn "Could not reach Vault via 'kubectl exec -n $VAULT_NAMESPACE $VAULT_POD -- vault status':"
  note "$VSTATUS"
  warn "Fix cluster/Vault access first — see scenarios/vault-unseal-backup.md — then re-run."
  exit 1
fi

say "Checking whether this session is already authenticated to Vault..."
if kubectl exec -n "$VAULT_NAMESPACE" "$VAULT_POD" -- vault kv list kv/ >/dev/null 2>&1; then
  note "Already authenticated inside ${VAULT_POD} — nothing to do."
else
  warn "Not authenticated yet — logging in with the root token from this"
  warn "repo's sealed backup (scenarios/vault-unseal-backup.md). The token"
  warn "flows straight through a pipe below; this wizard never stores or"
  warn "prints it."
  step "sops decrypt --output-type json clusters/${CLUSTER}/vault/vault-unseal.enc.yaml | jq -r '.vault_init.root_token' | kubectl exec -i -n ${VAULT_NAMESPACE} ${VAULT_POD} -- vault login -"
  if sops decrypt --output-type json "clusters/${CLUSTER}/vault/vault-unseal.enc.yaml" 2>/dev/null \
      | jq -r '.vault_init.root_token' \
      | kubectl exec -i -n "$VAULT_NAMESPACE" "$VAULT_POD" -- vault login - >/dev/null 2>&1; then
    note "Logged in to Vault."
  else
    warn "Automatic login failed. Log in by hand, then re-run this wizard:"
    note "  kubectl exec -it -n ${VAULT_NAMESPACE} ${VAULT_POD} -- vault login"
    note "(paste the root token at its prompt — see scenarios/vault-unseal-backup.md"
    note "for how to decrypt it if you don't have it handy)."
    exit 1
  fi
fi

# do_host HOST IP — one full pass (set, store, verify, prove) for one pve
# host. Uses 'return' rather than 'exit' on a recoverable per-host problem so
# the wizard keeps going through the rest of the fleet; every skip is
# recorded in SKIPPED and shown again in the closing summary.
do_host() {
  local host="$1" ip="$2"
  local vault_path="kv/pve-hosts/${host}/root"

  stage "root PAM password — ${host} (${ip})"

  say "Step A — set the root PAM password on ${host} yourself."
  say "This wizard is not capturing or transmitting this password. Run the"
  say "SSH session below in this terminal (or a separate one, if you'd"
  say "rather keep this wizard's prompts visible) and type the new password"
  say "directly at the host's own 'passwd' prompt."
  step "ssh root@${ip}"
  step "passwd    # choose a strong password, confirm it, then 'exit'"
  note "Needs ${host}'s pmxcfs-backed root SSH key to still work. If key auth"
  note "is already broken (the same pmxcfs-death failure mode this credential"
  note "exists to cover), this step can't bootstrap itself that way; get"
  note "console/physical access to set the first password on that host, then"
  note "come back and re-run this wizard just to store it."
  if ! confirm "Did 'passwd' succeed on ${host} and did you exit the session?"; then
    warn "Skipping ${host} for now — re-run this wizard once the password is set."
    SKIPPED+=("${host}: root PAM password not set")
    return
  fi

  say ""
  say "Step B — store that password in Vault at ${vault_path}."
  note "Input is hidden and goes straight to 'vault kv put' over stdin — this"
  note "wizard never echoes it, logs it, or writes it to disk."
  local pw=""
  ask_secret pw "Paste the password you just set on ${host}:"
  if [[ -z "$pw" ]]; then
    warn "Empty input — skipping ${host}."
    SKIPPED+=("${host}: no password entered for Vault write")
    return
  fi
  local write_err=""
  if write_err=$(printf '%s' "$pw" \
      | kubectl exec -i -n "$VAULT_NAMESPACE" "$VAULT_POD" -- vault kv put "$vault_path" password=- 2>&1 >/dev/null); then
    printf '  %s✓ wrote%s %s\n' "$GREEN" "$RESET" "$vault_path"
  else
    warn "Vault write failed for ${host}:"
    note "  $write_err"
    warn "Leaving it for you to retry by hand:"
    note "  kubectl exec -i -n ${VAULT_NAMESPACE} ${VAULT_POD} -- vault kv put ${vault_path} password=-"
    SKIPPED+=("${host}: vault kv put ${vault_path} failed")
    pw=""
    return
  fi

  say ""
  say "Step C — verify the write by reading it back. The value itself is"
  say "never printed, only whether it matches what was just set."
  local readback=""
  readback=$(kubectl exec -n "$VAULT_NAMESPACE" "$VAULT_POD" -- vault kv get -field=password "$vault_path" 2>/dev/null || true)
  if [[ -n "$readback" && "$readback" == "$pw" ]]; then
    printf '  %s✓ verified%s %s matches what was just set\n' "$GREEN" "$RESET" "$vault_path"
  else
    warn "Read-back did not match (or the read failed) for ${host} — check"
    warn "${vault_path} by hand: kubectl exec -n ${VAULT_NAMESPACE} ${VAULT_POD} -- vault kv get ${vault_path}"
    SKIPPED+=("${host}: vault read-back did not match")
  fi
  pw=""; readback=""   # scrub the in-memory copies now that verification is done

  say ""
  say "Step D — prove password SSH actually works on ${host}."
  say "This opens a real SSH connection with key auth turned off, so sshd"
  say "must prompt YOU for the password — type it at SSH's own prompt, not"
  say "into this wizard."
  if ssh -o PreferredAuthentications=password -o PubkeyAuthentication=no \
      -o NumberOfPasswordPrompts=1 -o ConnectTimeout=10 \
      "root@${ip}" 'echo pve-root-vault-wizard: password SSH OK'; then
    printf '  %s✓ verified%s password SSH works on %s\n' "$GREEN" "$RESET" "$host"
  else
    warn "Password SSH test failed (wrong password, PAM/sshd config, or you"
    warn "cancelled) for ${host}. Retry by hand:"
    note "  ssh -o PreferredAuthentications=password -o PubkeyAuthentication=no root@${ip}"
    SKIPPED+=("${host}: password SSH test not confirmed")
  fi
}

# ── Stages 2-6: one per host ────────────────────────────────────────────────
PVE_HOSTS=(
  "pve1 192.168.1.200"
  "pve2 192.168.1.201"
  "pve3 192.168.1.202"
  "pve4 192.168.1.203"
  "pve5 192.168.1.204"
)
for entry in "${PVE_HOSTS[@]}"; do
  read -r HOST IP <<< "$entry"
  do_host "$HOST" "$IP"
done

finish
note "Retrieval for an actual incident (SSH key auth dead because pmxcfs"
note "died) is documented in"
note "scenarios/host-remote-power-recovery.md and"
note "scenarios/pve-stale-node-ip-corosync.md — both reference"
note "kv/pve-hosts/<host>/root as the first thing to try before assuming"
note "physical access is required."
note "IPs/hostnames above come from docs/host-addressing.md — check there"
note "first if this wizard's table ever looks stale."
