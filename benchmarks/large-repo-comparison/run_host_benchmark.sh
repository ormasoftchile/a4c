#!/usr/bin/env bash
# Run the large-repo A4C Team benchmark on an arbitrary host (opencode/antigravity).
# Reuses the pinned repo/task/verification/acceptance. Measures from A4C activity
# metrics (host-agnostic), unlike measure_usage.py which is Copilot-DB-specific.
# Usage: run_host_benchmark.sh <run-id> <runtime> <model> [opencode-endpoint]
set -euo pipefail

RUN_ID=${1:?run-id}
RUNTIME=${2:?runtime: opencode|antigravity}
MODEL=${3:?model}
OC_ENDPOINT=${4:-}

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
A4C_ROOT=$(cd "$SCRIPT_DIR/../.." && pwd)
SRC_REPO=/Volumes/Projects/codebase-memory-mcp
SRC_COMMIT=63e3f2c7a6ba0ae444967d1f777254e9df5e381a
OUT="$SCRIPT_DIR/results/$RUN_ID"
TASK=$(cat "$SCRIPT_DIR/testdata/task.txt")
ACCEPT="$SCRIPT_DIR/testdata/acceptance_where_prop.sh"
BUDGET=2000000
STEPS=50
A4C_BIN="$OUT/a4c"

log() { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*"; }
mkdir -p "$OUT"
log "results dir: $OUT (runtime=$RUNTIME model=$MODEL)"

command -v go >/dev/null || { echo "missing go"; exit 1; }

log "building a4c"
(cd "$A4C_ROOT" && go build -o "$A4C_BIN" ./cmd/a4c)

log "scaffold clone"
rm -rf "$OUT/repo"
git clone -q "$SRC_REPO" "$OUT/repo"
git -C "$OUT/repo" checkout -q "$SRC_COMMIT"
git -C "$OUT/repo" config user.email bench@example.com
git -C "$OUT/repo" config user.name "Benchmark Harness"

log "a4c init + trusted verification contract"
(cd "$OUT/repo" && "$A4C_BIN" init .) > "$OUT/setup.log" 2>&1 || true
cat > "$OUT/repo/.a4c/verify.json" <<'VERIFY'
{
  "command": ["make", "-f", "Makefile.cbm", "SANITIZE=", "LIBGIT2_CFLAGS=", "LIBGIT2_LIBS=", "test"],
  "kind": "make",
  "timeout_seconds": 5400
}
VERIFY

log "phase: A4C Team coding"
t0=$(date +%s)
ENDPOINT_ARGS=()
if [ -n "$OC_ENDPOINT" ]; then ENDPOINT_ARGS=(--opencode-endpoint "$OC_ENDPOINT"); fi
(cd "$OUT/repo" && COPILOT_AUTO_UPDATE=false "$A4C_BIN" task start "$TASK" "$BUDGET" \
  --strategy team --runtime "$RUNTIME" --model "$MODEL" --steps "$STEPS" ${ENDPOINT_ARGS[@]+"${ENDPOINT_ARGS[@]}"}) \
  >"$OUT/coding.log" 2>&1 || true
t1=$(date +%s)
echo $((t1 - t0)) > "$OUT/wall.txt"
task_id=$(grep -m1 '^Task started: ' "$OUT/coding.log" | awk '{print $3}' || true)
echo "$task_id" > "$OUT/task-id.txt"
log "task id: ${task_id:-none}"

log "collecting A4C activity metrics"
python3 "$SCRIPT_DIR/collect_a4c_usage.py" "$OUT/repo/.a4c/activities" "$OUT/usage.json"

if [ -n "$task_id" ]; then
  (cd "$OUT/repo" && "$A4C_BIN" task inspect "$task_id" --json) > "$OUT/task-inspect.json" 2>/dev/null || true
fi

log "phase: acceptance"
BASE=$(git -C "$OUT/repo" rev-parse HEAD)
worktree="$OUT/repo/.a4c/worktrees/$task_id/tree"
verify_dir="$OUT/repo"
if [ -n "$task_id" ] && [ -d "$worktree" ]; then verify_dir="$worktree"; fi
git -C "$verify_dir" add -A 2>/dev/null || true
git -C "$verify_dir" diff --cached "$BASE" > "$OUT/final-diff.patch" 2>/dev/null || true
git -C "$verify_dir" diff --cached --name-only "$BASE" > "$OUT/files-modified.txt" 2>/dev/null || true
ACCEPT_DIR="$verify_dir" bash "$ACCEPT" > "$OUT/acceptance.log" 2>&1 \
  && echo PASS > "$OUT/acceptance.verdict" || echo FAIL > "$OUT/acceptance.verdict"
log "acceptance: $(cat "$OUT/acceptance.verdict")"

# Unattended benchmark completion: judge PASS/FAIL from authoritative persisted
# evidence (implementation + acceptance + agent review approval) without waiting
# for human approval. Production state is recorded separately, never mutated.
python3 "$SCRIPT_DIR/judge_benchmark.py" "$OUT" > "$OUT/benchmark-result.json" 2> "$OUT/benchmark-judge.log" \
  && log "benchmark: PASS" || log "benchmark: FAIL ($(python3 -c "import json;print('; '.join(json.load(open('$OUT/benchmark-result.json')).get('reasons',[])))" 2>/dev/null))"
log "done. results in $OUT"
