#!/usr/bin/env bash
#
# tailscale-subnet-router-wizard.sh
# Usage: ./tailscale-subnet-router-wizard.sh
#
# Interactive walkthrough for the human-run steps in
# scenarios/tailscale-subnet-router.md — install Tailscale on the HAProxy VM,
# advertise the LAN route, approve it in the Tailscale admin console, and
# verify from an off-LAN device. Nothing in this script can be run by an
# agent: it needs SSH access to a specific VM, a login the console only
# hands to a human, and a second physical device on another network.
#
# Purely a guide — it opens URLs and pauses for confirmation but never
# touches the VM, the router, or Talos itself. Rollback at any point is
# `tailscale down` on the VM; nothing here does anything that isn't already
# described (with its own rollback) in scenarios/tailscale-subnet-router.md.
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

TOTAL_STAGES=4

# Not using the shared banner() here: it claims progress is remembered across
# a Ctrl-C and re-run ("it remembers values already saved"), which is true
# for wizards that call ask/write_env but false for this one — no stage here
# captures or persists anything. That claim would be actively misleading in
# Stage 3+, after Tailscale is already installed on the production VM: a
# human re-running expecting to resume would land back at Stage 1 and could
# assume Stage 2 still needs redoing, or skip re-confirming what they already
# did. Authoring a custom opening block instead of overriding the library.
_clear
printf '\n%s%s  %s%s\n' "$BOLD" "$BLUE" "Tailscale subnet router — off-LAN cluster admin" "$RESET"
printf '%s  %s stages%s\n\n' "$DIM" "$TOTAL_STAGES" "$RESET"
printf '%s  This is a guide only: it opens URLs, prints the exact commands to run\n' "$DIM"
printf '  yourself, and pauses for confirmation. It never touches the VM, the\n'
printf '  router, or Talos config — and it does not capture or remember\n'
printf '  anything. Stopping partway through (Ctrl-C or closing the terminal)\n'
printf '  and re-running starts over from Stage 1; re-confirm what you already\n'
printf '  did rather than assuming it is remembered, especially after Stage 2\n'
printf '  once Tailscale is installed on the VM.%s\n' "$RESET"
pause "Ready to start?"

# ── Stage 1: prerequisites ─────────────────────────────────────────────────
stage "Prerequisites"
say "This walks through scenarios/tailscale-subnet-router.md end to end."
say "Read that file first if any step below is unclear — it has the full"
say "rationale for every flag and every rollback."
confirm "SSH access to the HAProxy VM (192.168.1.199) as the admin user?" \
  || { warn "Get SSH access first, then re-run this wizard."; exit 1; }
confirm "Owner/admin access to the Tailscale admin console?" \
  || { warn "Get console access first, then re-run this wizard."; exit 1; }
confirm "A second device already on the tailnet, on a DIFFERENT network than \
the LAN (a phone hotspot is enough), with no hosts-file shortcut for \
cluster.jdwlabs.com, sitting ready?" \
  || { warn "That device is the only thing that proves the result — get it ready, then re-run."; exit 1; }
note "If that off-LAN device is Linux, subnet routes aren't accepted by"
note "default — run 'sudo tailscale set --accept-routes' on it first."

# ── Stage 2: install and advertise, on the VM ──────────────────────────────
stage "Install Tailscale and advertise the route (on the HAProxy VM)"
say "From the LAN, SSH to the VM and run:"
step "ssh <admin-user>@192.168.1.199"
step "curl -fsSL https://tailscale.com/install.sh | sh"
step "sudo tailscale up --advertise-routes=192.168.1.0/24 --accept-dns=false"
note "--accept-dns=false is load-bearing: it keeps the VM's own name"
note "resolution on the LAN gateway instead of letting Tailscale rewrite"
note "/etc/resolv.conf on the host fronting both admin APIs."
say "The command prints a login URL — open it and approve the machine into"
say "the tailnet."
say "Advertising the route does not activate it — it's a pending request"
say "in the admin console until the next stage."
warn "Rollback at any point: sudo tailscale down on the VM."
pause "Press Enter once 'tailscale up' has completed and the machine is joined."

# ── Stage 3: approve the route — human-only, admin console ────────────────
stage "Approve the route in the Tailscale admin console"
say "This step has no API or CLI path from this repo — it is a deliberate,"
say "manual per-machine consent gate. A node advertising a route it"
say "shouldn't carry is a routing hijack, so the console never auto-approves."
open_url "https://login.tailscale.com/admin/machines"
step "Find the HAProxy VM's row (hostname printed by 'tailscale up' above)."
step "Click the row → under 'Subnet routes', approve the pending 192.168.1.0/24 route."
confirm "Route approved in the console?" \
  || { warn "Nothing routes until this is approved — re-run this wizard when it's done."; exit 1; }
note "Optional, to survive a reinstall without repeating this click: add an"
note "autoApprovers entry to the tailnet policy file. See"
note "docs/tailscale-subnet-router.md, Step 2, for the exact JSON and the"
note "matching 'tailscale up --advertise-tags=tag:lan-router' flag."
note "The policy file lives in the Tailscale control plane, not this repo —"
note "there is no Terraform resource for it."

# ── Stage 4: verify off-LAN — Definition of Done ───────────────────────────
stage "Verify off-LAN and capture evidence"
warn "Run every command below from the OFF-LAN device only — not from a LAN"
warn "machine, and not from a workstation with an existing hosts-file"
warn "shortcut for the cluster. All four must hold."
say ""
say "3a — route is advertised and active:"
step 'tailscale status --json | jq ".Peer[] | select(.PrimaryRoutes) | {name: .HostName, online: .Online, routes: .PrimaryRoutes}"'
note "Expect one entry: the HAProxy VM, online: true, routes containing 192.168.1.0/24."
say ""
say "3b — kubectl reaches the cluster (confirm no hosts-file entry first):"
step "kubectl get nodes"
note "Expect all 8 nodes, Ready. No resolver for cluster.jdwlabs.com off-LAN"
note "yet — a hosts entry or --server https://192.168.1.199:6443 is expected."
say ""
say "3c — talosctl reaches the Talos API:"
step "talosctl -e 192.168.1.199 version"
note "Add '-n <cp-ip>' if the bare -e form doesn't resolve a node."
say ""
say "3d — no new WAN port opened (failure to connect IS the pass):"
step 'for p in 6443 50000 22 9000; do timeout 5 bash -c "</dev/tcp/104.53.12.62/${p}" 2>/dev/null && echo "${p} OPEN - investigate" || echo "${p} refused"; done'
note "Expect 'refused' on all four. An 'OPEN' result is a live exposure,"
note "not a runbook error — stop and treat it as an incident."
say ""
confirm "All four checks passed?" \
  || warn "Don't close this out — see scenarios/tailscale-subnet-router.md for troubleshooting."
say "Paste each command's output as its own comment on the ticket tracking"
say "this work, in the same order as above."
pause "Press Enter once the evidence is recorded."

finish
note "Full rationale, the optional CIDR-allowlist follow-up, and rebuild"
note "parity notes live in docs/tailscale-subnet-router.md."
