#!/usr/bin/env bash
# Large-repo three-way benchmark: A4C Direct vs A4C Team vs Squad.
# Usage: run_benchmark.sh <run-id>
set -euo pipefail

RUN_ID=${1:?usage: run_benchmark.sh <run-id>}
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
A4C_ROOT=$(cd "$SCRIPT_DIR/../.." && pwd)
SRC_REPO=/Volumes/Projects/codebase-memory-mcp
SRC_COMMIT=63e3f2c7a6ba0ae444967d1f777254e9df5e381a
SQUAD_CLI=/var/folders/wp/qzs8rgx10jnfrk2vkxd_slb40000gn/T/opencode/bench/squad/packages/squad-cli/dist/cli-entry.js
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

# ---- preflight ----
for t in git go copilot sqlite3 node python3 make cc; do
  command -v "$t" >/dev/null || { echo "missing tool: $t"; exit 1; }
done
test -f "$USAGE_DB" || { echo "no copilot session store"; exit 1; }
test -f "$SQUAD_CLI" || { echo "squad CLI missing at $SQUAD_CLI"; exit 1; }
copilot --version | tee "$OUT/copilot-version.txt"

log "building a4c"
(cd "$A4C_ROOT" && go build -o "$A4C_BIN" ./cmd/a4c)

# ---- scaffold three identical clones ----
for name in a4c-direct a4c-team squad; do
  dir="$OUT/$name"
  log "scaffold $name"
  rm -rf "$dir"
  git clone -q "$SRC_REPO" "$dir"
  git -C "$dir" checkout -q "$SRC_COMMIT"
  git -C "$dir" config user.email bench@example.com
  git -C "$dir" config user.name "Benchmark Harness"
  # strip any leftover build artifacts from the source clone
  rm -rf "$dir/build"
done

tree_a=$(git -C "$OUT/a4c-direct" rev-parse HEAD^{tree})
tree_b=$(git -C "$OUT/a4c-team" rev-parse HEAD^{tree})
tree_c=$(git -C "$OUT/squad" rev-parse HEAD^{tree})
test "$tree_a" = "$tree_b" && test "$tree_a" = "$tree_c" \
  || { echo "trees differ: $tree_a $tree_b $tree_c"; exit 1; }
log "identical trees: $tree_a"

# ---- helper: watermark / measure ----
wm() { python3 "$MEASURE" watermark "$USAGE_DB"; }
measure() { # measure <dir> <outjson>
  local dir="$1" outjson="$2" wmfile="$3"
  python3 "$MEASURE" collect "$USAGE_DB" "$(cat "$wmfile")" "$dir" "$outjson" \
    | tee "${outjson%.json}-summary.json"
}

run_a4c() { # run_a4c <dir> <strategy> <logfile>
  local dir="$1" strategy="$2" logfile="$3"
  local wmf="$OUT/wm-$strategy.tmp"
  wm > "$wmf"
  local t0 t1
  t0=$(date +%s)
  (cd "$dir" && COPILOT_AUTO_UPDATE=false "$A4C_BIN" task start "$TASK" "$BUDGET" \
    --strategy "$strategy" --runtime copilot --model "$MODEL" --steps "$STEPS") \
    >"$logfile" 2>&1 || true
  t1=$(date +%s)
  echo $((t1 - t0)) > "$OUT/wall-$strategy.txt"
  local task_id
  task_id=$(grep -m1 '^Task started: ' "$logfile" | awk '{print $3}' || true)
  echo "$task_id" > "$OUT/task-id-$strategy.txt"
  measure "$dir" "$OUT/$strategy-usage.json" "$wmf"
  if [ -n "$task_id" ]; then
    (cd "$dir" && "$A4C_BIN" task inspect "$task_id" --json) > "$OUT/$strategy-task-inspect.json" 2>/dev/null || true
  fi
}

run_squad() {
  local dir="$1" logfile="$2"
  local wmf="$OUT/wm-squad.tmp"
  # pin model + MCP to local squad build
  python3 - <<PY
import json, os
cfg = {"defaultModel": "$MODEL"}
os.makedirs("$dir/.squad", exist_ok=True)
with open("$dir/.squad/config.json", "w") as f:
    json.dump(cfg, f, indent=2)
mcp = {"mcpServers": {"squad_state": {"command": "node", "args": ["$SQUAD_CLI", "state-server"]}}}
with open("$dir/.mcp.json", "w") as f:
    json.dump(mcp, f, indent=2)
PY
  wm > "$wmf"
  local t0 t1
  t0=$(date +%s)
  (cd "$dir" && COPILOT_AUTO_UPDATE=false copilot \
    --agent squad --model "$MODEL" --yolo \
    --additional-mcp-config "@$dir/.mcp.json" \
    --usage-output-file "$OUT/squad-usage-output.json" \
    -p "$TASK") >"$logfile" 2>&1 || true
  t1=$(date +%s)
  echo $((t1 - t0)) > "$OUT/wall-squad.txt"
  measure "$dir" "$OUT/squad-usage.json" "$wmf"
  (cd "$dir" && node "$SQUAD_CLI" cost --all) > "$OUT/squad-cost.txt" 2>/dev/null || true
}

capture_diff() { # capture_diff <dir> <basecommit> <name>
  local dir="$1" base="$2" name="$3"
  git -C "$dir" add -A 2>/dev/null || true
  git -C "$dir" diff --cached "$base" > "$OUT/$name-final-diff.patch" 2>/dev/null || true
  # files examined/modified
  git -C "$dir" diff --cached --name-only "$base" > "$OUT/$name-files-modified.txt" 2>/dev/null || true
}

run_accept() { # run_accept <dir> <name>
  local dir="$1" name="$2"
  ACCEPT_DIR="$dir" bash "$ACCEPT" > "$OUT/$name-acceptance.log" 2>&1 \
    && echo PASS > "$OUT/$name-acceptance.verdict" \
    || echo FAIL > "$OUT/$name-acceptance.verdict"
  log "$name acceptance: $(cat "$OUT/$name-acceptance.verdict")"
}

# ---- PHASE 1: cold setup (filesystem only, expect 0 model calls) ----
log "phase 1a: a4c-direct init"
w=$(wm)
(cd "$OUT/a4c-direct" && "$A4C_BIN" init .) > "$OUT/a4c-direct-setup.log" 2>&1 || true
measure "$OUT/a4c-direct" "$OUT/a4c-direct-setup-usage.json" <(echo "$w")

log "phase 1b: a4c-team init"
w=$(wm)
(cd "$OUT/a4c-team" && "$A4C_BIN" init .) > "$OUT/a4c-team-setup.log" 2>&1 || true
measure "$OUT/a4c-team" "$OUT/a4c-team-setup-usage.json" <(echo "$w")

log "phase 1c: squad init"
w=$(wm)
(cd "$OUT/squad" && node "$SQUAD_CLI" init --preset default) > "$OUT/squad-setup.log" 2>&1 || true
measure "$OUT/squad" "$OUT/squad-setup-usage.json" <(echo "$w")

# ---- PHASE 2: coding (sequential, 10s cooldown between) ----
log "phase 2a: A4C Direct"
run_a4c "$OUT/a4c-direct" direct "$OUT/a4c-direct-coding.log"

log "cooldown 10s"
sleep 10

log "phase 2b: A4C Team"
run_a4c "$OUT/a4c-team" team "$OUT/a4c-team-coding.log"

log "cooldown 10s"
sleep 10

log "phase 2c: Squad"
run_squad "$OUT/squad" "$OUT/squad-coding.log"

# ---- PHASE 3: diffs + acceptance ----
BASE=$(git -C "$OUT/a4c-direct" rev-parse HEAD)
log "phase 3: diffs + acceptance (base $BASE)"

# A4C worktrees (team/direct live in .a4c/worktrees)
for strategy in direct team; do
  dir="$OUT/a4c-$strategy"
  task_id=$(cat "$OUT/task-id-$strategy.txt" 2>/dev/null || true)
  worktree="$dir/.a4c/worktrees/$task_id/tree"
  if [ -n "$task_id" ] && [ -d "$worktree" ]; then
    capture_diff "$worktree" "$BASE" "a4c-$strategy"
    run_accept "$worktree" "a4c-$strategy"
  else
    log "no worktree for $strategy (task_id=$task_id); using base checkout"
    capture_diff "$dir" "$BASE" "a4c-$strategy"
    run_accept "$dir" "a4c-$strategy"
  fi
done

capture_diff "$OUT/squad" "$BASE" "squad"
# filter product scaffolding from squad task diff
git -C "$OUT/squad" diff --cached --name-only "$BASE" \
  | grep -v -E '^\.squad/|^\.github/|^\.copilot/|^\.vscode/|^\.mcp\.json$|^\.gitignore$|^\.gitattributes$' \
  > "$OUT/squad-files-task-modified.txt" 2>/dev/null || true
run_accept "$OUT/squad" "squad"

# ---- metadata ----
python3 - <<PY
import json, subprocess, os
out = "$OUT"
def sh(*a):
    return subprocess.check_output(a, text=True).strip()
meta = {
  "run_id": "$RUN_ID",
  "src_repo": "$SRC_REPO",
  "base_commit": sh("git", "-C", "$SRC_REPO", "rev-parse", "HEAD"),
  "base_tree": "$tree_a",
  "model": "$MODEL",
  "budget_tokens": int("$BUDGET"),
  "max_steps": int("$STEPS"),
  "copilot": open(os.path.join(out, "copilot-version.txt")).read().strip(),
  "squad_cli": "$SQUAD_CLI",
  "task_file": "testdata/task.txt",
}
with open(os.path.join(out, "run-metadata.json"), "w") as f:
    json.dump(meta, f, indent=2)
print(json.dumps(meta, indent=2))
PY

log "done. results in $OUT"
