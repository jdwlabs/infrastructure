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
# With API_KEY set, BASE_URL must be https:// or a plain-HTTP localhost or
# 127.0.0.1 port-forward; anything else fails before the request is sent.
#
# Needs curl and jq. Exit 0 on a parsed get_time call, 1 on anything else,
# with `result: FAIL` and the reason on stdout so an agent reading only stdout
# still sees why.
set -euo pipefail

BASE_URL="${BASE_URL:-http://192.168.1.50:8000/v1}"
MODEL="${MODEL:-local-chat}"
TIMEOUT="${TIMEOUT:-60}"

fail() {
  echo "result: FAIL"
  echo "reason: $1"
  exit 1
}

# A reason is one line: a server's error page is flattened and cut short.
excerpt() {
  local s
  s=$(tr -s '[:space:]' ' ' <"$1")
  if [ "${#s}" -gt 500 ]; then
    s="${s:0:500}..."
  fi
  printf '%s' "$s"
}

for tool in curl jq; do
  command -v "$tool" >/dev/null 2>&1 || fail "${tool} is not installed"
done

url="${BASE_URL%/}/chat/completions"

auth=()
if [ -n "${API_KEY:-}" ]; then
  # A bearer key over plain HTTP crosses the network readable. Loopback is
  # the exception: that is a kubectl port-forward, whose tunnel is encrypted.
  # An "@" is refused first: curl reads what precedes it as credentials, so
  # http://localhost:x@host looks like loopback here and connects to host.
  case "$BASE_URL" in
    https://*) ;;
    *@*) fail "refusing to send API_KEY over plain HTTP to ${BASE_URL}: an \"@\" in the URL can hide the real host; use https:// or a localhost port-forward" ;;
    http://localhost[:/]* | http://127.0.0.1[:/]*) ;;
    *) fail "refusing to send API_KEY over plain HTTP to ${BASE_URL}; use https:// or a localhost port-forward" ;;
  esac
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
}') || fail "could not build the request body with jq"

tmp=$(mktemp -d) || fail "could not create a temporary directory"
trap 'rm -rf "$tmp"' EXIT

# The body goes to a file and the status and duration to another, so an HTTP
# error keeps the server's message and stderr holds only curl's own failure.
# ${auth[@]+...} because bash before 4.4 treats an empty array as unset.
if ! err=$(curl -sS --max-time "$TIMEOUT" ${auth[@]+"${auth[@]}"} \
  -H 'Content-Type: application/json' -d "$body" \
  -o "$tmp/body" -w '%{http_code} %{time_total}\n' "$url" 2>&1 >"$tmp/meta"); then
  fail "request to ${url} failed: ${err}"
fi
read -r status secs <"$tmp/meta" || fail "curl reported no HTTP status for ${url}"

case "$status" in
  2??) ;;
  *) fail "HTTP ${status} from ${url}: $(excerpt "$tmp/body")" ;;
esac

if ! fields=$(jq -rc '[
    .model // "-",
    .choices[0].message.tool_calls[0].function.name // "",
    (.choices[0].message.content | tojson)
  ] | .[]' "$tmp/body" 2>/dev/null) || [ -z "$fields" ]; then
  fail "response from ${url} is not a chat completion: $(excerpt "$tmp/body")"
fi
{
  read -r served
  read -r name
  read -r content
} <<<"$fields"

echo "base_url: ${BASE_URL}"
echo "model: ${MODEL}"
echo "served_by: ${served}"
echo "seconds: ${secs}"
if [ "$name" = "get_time" ]; then
  echo "result: PASS"
  exit 0
fi
fail "no parsed get_time call; content was: ${content}"
