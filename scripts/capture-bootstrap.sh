#!/usr/bin/env bash
set -euo pipefail

# Capture Amp Neo/Rivet bootstrap frames through amp-proxy's built-in dump path.
# This script intentionally uses AMP_PROXY_RIVET_PASSTHROUGH=1 so the proxy is a
# transparent bridge to actors.ampcode.com and does not inject local inference.

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DUMP_ROOT="${AMP_PROXY_DUMP_ROOT:-$HOME/.amp-proxy/bootstrap-captures}"
OUT_FILE="${AMP_PROXY_BOOTSTRAP_OUT:-$ROOT_DIR/testdata/bootstrap/smart-mode.jsonl}"
PROMPT="${AMP_PROXY_BOOTSTRAP_PROMPT:-say hi}"
MODE="${AMP_PROXY_BOOTSTRAP_MODE:-smart}"
PROXY_BIN="${AMP_PROXY_BIN:-$ROOT_DIR/amp-proxy}"
LOG_FILE="${AMP_PROXY_BOOTSTRAP_LOG:-$DUMP_ROOT/amp-proxy-passthrough.log}"

mkdir -p "$DUMP_ROOT" "$(dirname "$OUT_FILE")"

if lsof -nP -iTCP:9317 -sTCP:LISTEN >/dev/null 2>&1; then
  cat >&2 <<'MSG'
Port 9317 is already listening. AMP_PROXY_RIVET_PASSTHROUGH is read at process
startup, so stop the running AmpProxy app first (menu bar Quit, or kill it) and
rerun this script.
MSG
  lsof -nP -iTCP:9317 -sTCP:LISTEN >&2 || true
  exit 1
fi

if [[ ! -x "$PROXY_BIN" ]]; then
  echo "[capture] $PROXY_BIN not found; building temporary binary with go build" >&2
  (cd "$ROOT_DIR" && go build -o "$PROXY_BIN" .)
fi

echo "[capture] dump root: $DUMP_ROOT" >&2
echo "[capture] output:    $OUT_FILE" >&2
echo "[capture] starting amp-proxy in passthrough mode" >&2
(
  cd "$ROOT_DIR"
  AMP_PROXY_RIVET_PASSTHROUGH=1 \
  AMP_PROXY_DUMP_DIR="$DUMP_ROOT" \
  "$PROXY_BIN"
) >"$LOG_FILE" 2>&1 &
PROXY_PID=$!

cleanup() {
  if kill -0 "$PROXY_PID" >/dev/null 2>&1; then
    kill "$PROXY_PID" >/dev/null 2>&1 || true
    wait "$PROXY_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

for _ in {1..80}; do
  if lsof -nP -iTCP:9317 -sTCP:LISTEN >/dev/null 2>&1; then
    break
  fi
  sleep 0.25
done

if ! lsof -nP -iTCP:9317 -sTCP:LISTEN >/dev/null 2>&1; then
  echo "[capture] amp-proxy did not start; see $LOG_FILE" >&2
  exit 1
fi

cat >&2 <<MSG
[capture] amp-proxy is running with:
  AMP_PROXY_RIVET_PASSTHROUGH=1
  AMP_PROXY_DUMP_DIR=$DUMP_ROOT

In another terminal, run a single smart-mode Neo request, for example:
  amp -x "$PROMPT" --mode "$MODE"

Press Enter here after the command has connected and produced output.
MSG
read -r _

LATEST="$(find "$DUMP_ROOT" -mindepth 1 -maxdepth 1 -type d -print | sort | tail -n 1 || true)"
TIMELINE="${LATEST:+$LATEST/timeline.jsonl}"
if [[ -z "${LATEST}" || ! -s "$TIMELINE" ]]; then
  cat >"$OUT_FILE" <<PENDING
PENDING: no timeline.jsonl was captured. Start amp-proxy with AMP_PROXY_RIVET_PASSTHROUGH=1 AMP_PROXY_DUMP_DIR=$DUMP_ROOT, run: amp -x "$PROMPT" --mode "$MODE", then rerun extraction against the newest dump session.
PENDING
  echo "[capture] no timeline found; wrote PENDING marker to $OUT_FILE" >&2
  exit 0
fi

python3 - "$TIMELINE" "$OUT_FILE" <<'PY'
import json
import re
import sys
from copy import deepcopy

timeline, out_file = sys.argv[1], sys.argv[2]
server_frames = []
seen_user_msg = False
jwt_re = re.compile(r'([A-Za-z0-9_-]+)\.([A-Za-z0-9_-]+)\.([A-Za-z0-9_-]+)')

def redact(value):
    if isinstance(value, dict):
        return {k: redact(v) for k, v in value.items()}
    if isinstance(value, list):
        return [redact(v) for v in value]
    if isinstance(value, str):
        if value.count('.') == 2 and jwt_re.fullmatch(value):
            return '[REDACTED].[REDACTED].[REDACTED]'
        return jwt_re.sub('[REDACTED].[REDACTED].[REDACTED]', value)
    return value

with open(timeline, 'r', encoding='utf-8') as f:
    for line in f:
        line = line.strip()
        if not line:
            continue
        try:
            evt = json.loads(line)
        except json.JSONDecodeError:
            continue
        if evt.get('kind') != 'ws_frame':
            continue
        frame = evt.get('frame')
        if not isinstance(frame, dict):
            continue
        direction = evt.get('direction')
        ftype = frame.get('type')
        if direction == '>' and ftype == 'client_append_user_msg':
            seen_user_msg = True
            break
        if direction == '<':
            cleaned = redact(deepcopy(frame))
            server_frames.append(cleaned)

with open(out_file, 'w', encoding='utf-8') as out:
    if server_frames:
        for frame in server_frames:
            out.write(json.dumps(frame, ensure_ascii=False, separators=(',', ':')) + '\n')
    else:
        out.write('PENDING: timeline had no server-to-client bootstrap frames before client_append_user_msg. Keep the raw dump and inspect direction/type ordering manually.\n')

print(len(server_frames))
PY

COUNT="$(grep -cv '^PENDING:' "$OUT_FILE" || true)"
echo "[capture] wrote $COUNT frame(s) to $OUT_FILE" >&2
echo "[capture] raw timeline: $TIMELINE" >&2
