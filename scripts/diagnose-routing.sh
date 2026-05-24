#!/usr/bin/env bash
# Diagnose whether amp -x requests went local-OAuth or BYOK-passthrough.
#
# Usage: ./diagnose-routing.sh [thread-id]
#   With no arg: runs a quick smart+rush test then inspects both threads.
#   With thread-id: just inspects that thread.

set -u

inspect_thread() {
  local tid="$1"
  echo "--- Thread $tid ---"
  local data
  data=$(amp threads export "$tid" 2>/dev/null) || { echo "  (export failed)"; return; }
  echo "$data" | jq -r '
    {
      title: (.title // "Untitled"),
      msgs: (.messages | length),
      agentMode: .agentMode,
      models_used: [.messages[] | select(.role=="assistant") | .usage.model // "(none)"],
      stop_reasons: [.messages[] | select(.role=="assistant") | .state.stopReason // ""]
    }
  '
  # Heuristic: if usage.model contains "/" → amp server's labeling (BYOK or amp default).
  # If it's a raw model id like "claude-opus-4-7" → local proxy intercepted.
  local first_model
  first_model=$(echo "$data" | jq -r '[.messages[] | select(.role=="assistant") | .usage.model] | .[0] // "(none)"')
  case "$first_model" in
    */*) echo "  → routed via amp server (BYOK or amp credits)" ;;
    claude-*|gpt-*|gemini-*) echo "  → routed via LOCAL proxy intercept" ;;
    *)   echo "  → unknown ($first_model)" ;;
  esac
}

if [[ $# -ge 1 ]]; then
  inspect_thread "$1"
  exit 0
fi

cd /tmp
echo "=== Running smart (expect: local OAuth) ==="
amp -x "diagnose smart route, reply just OK" 2>&1 | tail -1
TID1=$(amp threads list 2>&1 | head -3 | tail -1 | awk '{print $NF}')

echo
echo "=== Running rush (expect: amp passthrough) ==="
amp --mode rush -x "diagnose rush route, reply just OK" 2>&1 | tail -1
TID2=$(amp threads list 2>&1 | head -3 | tail -1 | awk '{print $NF}')

echo
echo "=== Inspecting threads ==="
inspect_thread "$TID1"
echo
inspect_thread "$TID2"

echo
echo "=== Cross-check: OpenAI dashboard ==="
echo "If rush went via BYOK, you should see a request at platform.openai.com/usage now."
echo "If smart went local, you should see API usage at console.anthropic.com (or Claude Pro usage)."
