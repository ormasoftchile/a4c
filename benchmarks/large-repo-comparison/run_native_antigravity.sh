#!/usr/bin/env bash
# Native Antigravity CLI baseline: run the same task directly through agy's
# headless agent interface (NOT A4C). Records whether review/tests are
# self-reported vs independently performed.
# Usage: run_native_antigravity.sh <run-id> <model>
set -euo pipefail

RUN_ID=${1:?run-id}
MODEL=${2:-gemini-4-argon-medium}

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
SRC_REPO=/Volumes/Projects/codebase-memory-mcp
SRC_COMMIT=63e3f2c7a6ba0ae444967d1f777254e9df5e381a
OUT="$SCRIPT_DIR/results/$RUN_ID"
TASK=$(cat "$SCRIPT_DIR/testdata/task.txt")
ACCEPT="$SCRIPT_DIR/testdata/acceptance_where_prop.sh"

log() { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*"; }
mkdir -p "$OUT"
log "results dir: $OUT (native antigravity model=$MODEL)"

log "scaffold clone"
rm -rf "$OUT/repo"
git clone -q "$SRC_REPO" "$OUT/repo"
git -C "$OUT/repo" checkout -q "$SRC_COMMIT"
git -C "$OUT/repo" config user.email bench@example.com
git -C "$OUT/repo" config user.name "Benchmark Harness"

# Build the native-agent prompt: same task + an explicit instruction to run the
# C test suite and report its result (so we can see whether it self-verifies).
PROMPT="$TASK

Implement this fix directly in the repository. After implementing, run the C test suite with: make -f Makefile.cbm SANITIZE= LIBGIT2_CFLAGS= LIBGIT2_LIBS= test
Then report what you changed and the test results."

log "phase: native antigravity coding"
t0=$(date +%s)
# -p attaches the prompt; bounded by print-timeout (90 min).
(cd "$OUT/repo" && agy -p "$PROMPT" --output-format json --dangerously-skip-permissions \
  --model "$MODEL" --print-timeout 5400s > "$OUT/agent-output.json" 2> "$OUT/agent-stderr.log") || true
t1=$(date +%s)
echo $((t1 - t0)) > "$OUT/wall.txt"
log "agent finished in $((t1 - t0))s"

# Capture usage from the JSON output.
python3 - "$OUT/agent-output.json" "$OUT/usage.json" <<'PY'
import json, sys
raw = open(sys.argv[1]).read()
try:
    d = json.loads(raw)
except Exception:
    d = {"_parse_error": True, "_raw_head": raw[:500]}
usage = d.get("usage", {})
out = {
    "native_antigravity": True,
    "usage": usage,
    "num_turns": d.get("num_turns"),
    "duration_seconds": d.get("duration_seconds"),
    "status": d.get("status"),
    "response_excerpt": str(d.get("response", ""))[:2000],
}
json.dump(out, open(sys.argv[2], "w"), indent=2)
print(json.dumps({k: out[k] for k in ("usage", "num_turns", "duration_seconds", "status")}, indent=2))
PY

log "phase: acceptance"
BASE=$(git -C "$OUT/repo" rev-parse HEAD)
git -C "$OUT/repo" add -A 2>/dev/null || true
git -C "$OUT/repo" diff --cached "$BASE" > "$OUT/final-diff.patch" 2>/dev/null || true
git -C "$OUT/repo" diff --cached --name-only "$BASE" > "$OUT/files-modified.txt" 2>/dev/null || true
ACCEPT_DIR="$OUT/repo" bash "$ACCEPT" > "$OUT/acceptance.log" 2>&1 \
  && echo PASS > "$OUT/acceptance.verdict" || echo FAIL > "$OUT/acceptance.verdict"
log "acceptance: $(cat "$OUT/acceptance.verdict")"
log "done. results in $OUT"
