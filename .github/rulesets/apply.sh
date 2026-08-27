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
#
# --- Renaming or removing a required-check context (read this first) ---
#
# baseline.json's required_status_checks names CI job contexts by string.
# GitHub Rulesets is the *live* source of truth for what a PR needs to pass,
# and this directory is only applied to it manually, after merge. That gap
# means a workflow change that renames/merges/removes a required job cannot
# land together with the matching JSON update:
#   - merge both in one PR: the live ruleset still demands the old context,
#     which no longer exists once the workflow change is live, so that PR
#     (and every later one) can never satisfy required checks again.
#   - apply the JSON first: the live ruleset demands the new context before
#     any workflow run has produced it, blocking every open PR until the
#     workflow change lands.
#
# Do it in three steps instead:
#   1. Edit baseline.json to DROP the doomed context(s) from
#      required_status_checks (don't add the new one(s) yet), merge that
#      change alone, then run `./apply.sh` — this un-blocks PRs without
#      requiring either the old or the new context.
#   2. Merge the workflow PR that actually renames/merges/removes the job(s).
#   3. Edit baseline.json again to ADD the new context(s), merge, then run
#      `./apply.sh` a second time to re-require them.
#
# Related: deployments/.github/workflows/promote-prd.yml hardcodes
# required-check job names into a generated PR body and needs updating in
# the same lockstep if the renamed/removed job is one of those names.

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
