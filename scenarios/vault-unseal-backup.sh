#!/bin/bash
#
# vault-unseal-backup.sh
# Usage: ./vault-unseal-backup.sh [cluster-name]
#
# Captures the in-cluster Vault root token + unseal key(s) and encrypts them
# into this repo's SOPS+age vault, alongside the Talos secrets bundle. See
# scenarios/vault-unseal-backup.md for the full rationale and restore steps.
#
# This is a HUMAN-RUN script (it touches real secret material) — same gating
# as `terraform apply` in this repo. It only reads from the cluster
# (`kubectl get secret`) and never writes back to it.

set -e

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$SCRIPT_DIR/.."
CLUSTER="${1:-core}"
NS="vault"

PLAIN_DIR="$REPO_ROOT/clusters/$CLUSTER/secrets"
PLAIN_FILE="$PLAIN_DIR/vault-unseal.json"
VAULT_DIR="$REPO_ROOT/clusters/$CLUSTER/vault"
ENC_FILE="$VAULT_DIR/vault-unseal.enc.yaml"

for bin in kubectl sops jq; do
    if ! command -v "$bin" >/dev/null 2>&1; then
        echo -e "${RED}Error: $bin not found in PATH${NC}"
        exit 1
    fi
done

echo -e "${YELLOW}[1/3] Reading vault-init and vault-unseal-keys secrets (namespace: $NS)...${NC}"

if ! kubectl get secret vault-init -n "$NS" >/dev/null 2>&1; then
    echo -e "${RED}Error: secret vault-init not found in namespace $NS — is kubectl pointed at the right cluster?${NC}"
    exit 1
fi
if ! kubectl get secret vault-unseal-keys -n "$NS" >/dev/null 2>&1; then
    echo -e "${RED}Error: secret vault-unseal-keys not found in namespace $NS${NC}"
    exit 1
fi

# vault-init holds the full platformctl VaultInitResult (root_token + keys) as
# a single JSON blob under key "vault-init.json". vault-unseal-keys holds the
# same Shamir key(s) split into individual keys (currently just unseal_key_1,
# since vault-init phase runs with -key-shares=1 -key-threshold=1) — captured
# separately here in case the two secrets ever drift.
VAULT_INIT_JSON=$(kubectl get secret vault-init -n "$NS" -o jsonpath='{.data.vault-init\.json}' | base64 -d)
UNSEAL_KEY_1=$(kubectl get secret vault-unseal-keys -n "$NS" -o jsonpath='{.data.unseal_key_1}' | base64 -d)

echo -e "${GREEN}✓ Done${NC}"

echo -e "${YELLOW}[2/3] Writing plaintext working copy (gitignored: clusters/*/secrets/)...${NC}"
mkdir -p "$PLAIN_DIR"
jq -n \
    --argjson vault_init "$VAULT_INIT_JSON" \
    --arg unseal_key_1 "$UNSEAL_KEY_1" \
    --arg captured_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    '{vault_init: $vault_init, unseal_keys: {unseal_key_1: $unseal_key_1}, captured_at: $captured_at}' \
    > "$PLAIN_FILE"
chmod 600 "$PLAIN_FILE"
echo -e "${GREEN}✓ Done${NC}"

echo -e "${YELLOW}[3/3] Encrypting into the vault (clusters/$CLUSTER/vault/)...${NC}"
mkdir -p "$VAULT_DIR"
sops encrypt --input-type json --output-type yaml \
    --filename-override "$ENC_FILE" "$PLAIN_FILE" > "$ENC_FILE"
echo -e "${GREEN}✓ Done${NC}"

echo ""
echo -e "${GREEN}=== Captured $ENC_FILE ===${NC}"
echo "Review, then commit and push:"
echo "  git add $ENC_FILE"
echo "  git commit -m \"chore(secrets): back up vault unseal material for $CLUSTER\""
echo ""
echo -e "${YELLOW}Nothing was decrypted or printed above beyond what was written to $ENC_FILE.${NC}"
echo "The plaintext working copy at $PLAIN_FILE is gitignored — delete it when done"
echo "if you want it off this disk (talops-managed files support 'lock'; this one doesn't yet, see the runbook)."
