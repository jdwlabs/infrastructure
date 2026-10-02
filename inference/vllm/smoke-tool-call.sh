#!/usr/bin/env bash
# One OpenAI-style tool-call round trip, pass or fail.
#
# The vLLM health gate already proves the server parses a tool call, but only
# under the served name and straight at the host. This runs the same request
# through whatever sits in front of it, so a trial can show that the path a
# consumer actually uses still returns parsed tool_calls: point BASE_URL at
# LiteLLM and MODEL at sre-investigator-local to check the SRE tier.
#
#   inference/vllm/smoke-tool-call.sh                     # the host, as local-chat
#   BASE_URL=<litellm>/v1 MODEL=sre-investigator-local API_KEY=... \
#     inference/vllm/smoke-tool-call.sh                   # the SRE tier
#
# Exit 0 on a parsed get_time call, 1 on anything else, with the reason on
# stdout so an agent reading only stdout still sees why.
set -euo pipefail

BASE_URL="${BASE_URL:-http://192.168.1.50:8000/v1}"
MODEL="${MODEL:-local-chat}"
TIMEOUT="${TIMEOUT:-60}"

auth=()
if [ -n "${API_KEY:-}" ]; then
  auth=(-H "Authorization: Bearer ${API_KEY}")
fi

body=$(jq -n --arg model "$MODEL" '{
  model: $model,
  messages: [
    {role: "system", content: "You must answer by calling the get_time tool."},
    {role: "user", content: "What time is it? Call get_time."}
  ],
  tools: [{type: "function", function: {
    name: "get_time",
    description: "Returns the current time.",
    parameters: {type: "object", properties: {}}
  }}],
  tool_choice: "auto",
  max_tokens: 128
}')

start=$(date +%s.%N)
if ! resp=$(curl -fsS --max-time "$TIMEOUT" "${auth[@]}" \
  -H 'Content-Type: application/json' -d "$body" "${BASE_URL%/}/chat/completions" 2>&1); then
  echo "result: FAIL"
  echo "reason: request to ${BASE_URL} failed: ${resp}"
  exit 1
fi
secs=$(echo "$(date +%s.%N) - $start" | bc)

name=$(jq -r '.choices[0].message.tool_calls[0].function.name // empty' <<<"$resp")
echo "base_url: ${BASE_URL}"
echo "model: ${MODEL}"
echo "served_by: $(jq -r '.model // "-"' <<<"$resp")"
echo "seconds: ${secs}"
if [ "$name" = "get_time" ]; then
  echo "result: PASS"
  exit 0
fi
echo "result: FAIL"
echo "reason: no parsed get_time call; content was: $(jq -c '.choices[0].message.content' <<<"$resp")"
exit 1
