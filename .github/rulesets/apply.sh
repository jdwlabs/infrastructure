#!/usr/bin/env bash
# Applies the ruleset exports in this directory to GitHub.
#
# Rulesets are managed as code here: edit the JSON, merge via PR, then run
# this script. Requires gh (authenticated with admin on the repo) and jq.
#
#   ./apply.sh            # apply every *.json in this directory
#   ./apply.sh --dry-run  # print what would change without writing
#
# Re-export after any out-of-band UI change with:
#   gh api repos/jdwlabs/infrastructure/rulesets/<id> | jq . > <name>.json
#
# Matching is by the "id" field embedded in each export: if that id still
# exists live, the ruleset is updated in place (PUT); otherwise it is
# recreated (POST) — re-export afterwards so the file carries the new id.

set -euo pipefail

REPO="jdwlabs/infrastructure"
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DRY_RUN="${1:-}"

count=0
for file in "$DIR"/*.json; do
  name=$(jq -r '.name' "$file")
  id=$(jq -r '.id // empty' "$file")
  # Strip read-only fields the API rejects or ignores on write.
  payload=$(jq 'del(.id, .node_id, .source, .source_type, .created_at, .updated_at, .current_user_can_bypass, ._links)' "$file")

  if [ -n "$id" ] && gh api "repos/$REPO/rulesets/$id" >/dev/null 2>&1; then
    method="PUT"; endpoint="repos/$REPO/rulesets/$id"
  else
    method="POST"; endpoint="repos/$REPO/rulesets"
  fi

  if [ "$DRY_RUN" = "--dry-run" ]; then
    echo "would $method $endpoint  ($name from ${file##*/})"
  else
    printf '%s' "$payload" | gh api -X "$method" "$endpoint" --input - >/dev/null
    echo "$method $endpoint  ($name from ${file##*/})"
  fi
  count=$((count + 1))
done

echo "done: $count ruleset file(s) processed for $REPO"
