#!/usr/bin/env bash
# Re-run ONLY the A4C Team leg of the large-repo benchmark after a fix.
# Usage: run_team_rerun.sh <run-id>
set -euo pipefail

RUN_ID=${1:?usage: run_team_rerun.sh <run-id>}
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
A4C_ROOT=$(cd "$SCRIPT_DIR/../.." && pwd)
SRC_REPO=/Volumes/Projects/codebase-memory-mcp
SRC_COMMIT=63e3f2c7a6ba0ae444967d1f777254e9df5e381a
OUT="$SCRIPT_DIR/results/$RUN_ID"
TASK=$(cat "$SCRIPT_DIR/testdata/task.txt")
MEASURE="$SCRIPT_DIR/measure_usage.py"
ACCEPT="$SCRIPT_DIR/testdata/acceptance_where_prop.sh"
USAGE_DB="$HOME/.copilot/session-store.db"
MODEL=gpt-5.5
BUDGET=2000000
STEPS=50
A4C_BIN="$OUT/a4c"

log() { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*"; }
mkdir -p "$OUT"
log "results dir: $OUT"

command -v go >/dev/null || { echo "missing go"; exit 1; }
command -v copilot >/dev/null || { echo "missing copilot"; exit 1; }
test -f "$USAGE_DB" || { echo "no copilot session store"; exit 1; }

log "building a4c from current HEAD ($(git -C "$A4C_ROOT" rev-parse --short HEAD))"
(cd "$A4C_ROOT" && go build -o "$A4C_BIN" ./cmd/a4c)

log "scaffold a4c-team clone"
rm -rf "$OUT/a4c-team"
git clone -q "$SRC_REPO" "$OUT/a4c-team"
git -C "$OUT/a4c-team" checkout -q "$SRC_COMMIT"
git -C "$OUT/a4c-team" config user.email bench@example.com
git -C "$OUT/a4c-team" config user.name "Benchmark Harness"

wm() { python3 "$MEASURE" watermark "$USAGE_DB"; }

log "phase 1: a4c init"
w=$(wm)
(cd "$OUT/a4c-team" && "$A4C_BIN" init .) > "$OUT/a4c-team-setup.log" 2>&1 || true
# Author the trusted project verification contract BEFORE the coder runs. This
# is trusted configuration (authored by the harness), not a coder-generated
# file, so it pins the C test command regardless of any files the coder adds.
# LIBGIT2_* are emptied because this environment has incompatible libgit2
# headers (same configuration the independent acceptance harness uses).
cat > "$OUT/a4c-team/.a4c/verify.json" <<'VERIFY'
{
  "command": ["make", "-f", "Makefile.cbm", "SANITIZE=", "LIBGIT2_CFLAGS=", "LIBGIT2_LIBS=", "test"],
  "kind": "make",
  "timeout_seconds": 5400
}
VERIFY
python3 "$MEASURE" collect "$USAGE_DB" "$w" "$OUT/a4c-team" "$OUT/a4c-team-setup-usage.json" >/dev/null

log "phase 2: A4C Team coding"
w=$(wm)
t0=$(date +%s)
(cd "$OUT/a4c-team" && COPILOT_AUTO_UPDATE=false "$A4C_BIN" task start "$TASK" "$BUDGET" \
  --strategy team --runtime copilot --model "$MODEL" --steps "$STEPS") \
  >"$OUT/a4c-team-coding.log" 2>&1 || true
t1=$(date +%s)
echo $((t1 - t0)) > "$OUT/wall-team.txt"
task_id=$(grep -m1 '^Task started: ' "$OUT/a4c-team-coding.log" | awk '{print $3}' || true)
echo "$task_id" > "$OUT/task-id-team.txt"
log "task id: ${task_id:-none}"
python3 "$MEASURE" collect "$USAGE_DB" "$w" "$OUT/a4c-team" "$OUT/a4c-team-usage.json" \
  | tee "$OUT/a4c-team-usage-summary.json"
if [ -n "$task_id" ]; then
  (cd "$OUT/a4c-team" && "$A4C_BIN" task inspect "$task_id" --json) \
    > "$OUT/team-task-inspect.json" 2>/dev/null || true
fi

log "phase 3: acceptance"
BASE=$(git -C "$OUT/a4c-team" rev-parse HEAD)
worktree="$OUT/a4c-team/.a4c/worktrees/$task_id/tree"
verify_dir="$OUT/a4c-team"
if [ -n "$task_id" ] && [ -d "$worktree" ]; then
  verify_dir="$worktree"
fi
git -C "$verify_dir" add -A 2>/dev/null || true
git -C "$verify_dir" diff --cached "$BASE" > "$OUT/a4c-team-final-diff.patch" 2>/dev/null || true
git -C "$verify_dir" diff --cached --name-only "$BASE" > "$OUT/a4c-team-files-modified.txt" 2>/dev/null || true

ACCEPT_DIR="$verify_dir" bash "$ACCEPT" > "$OUT/a4c-team-acceptance.log" 2>&1 \
  && echo PASS > "$OUT/a4c-team-acceptance.verdict" \
  || echo FAIL > "$OUT/a4c-team-acceptance.verdict"
log "acceptance: $(cat "$OUT/a4c-team-acceptance.verdict")"

log "done. results in $OUT"
