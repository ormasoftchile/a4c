#!/bin/bash
# Hidden acceptance for Cypher property-to-property WHERE comparisons.
# Verifies the #2591 defect is fixed without regressions.
# Usage: ACCEPT_DIR=/path/to/worktree bash acceptance_where_prop.sh
set -euo pipefail

WORKTREE=${ACCEPT_DIR:?ACCEPT_DIR must point at the modified working tree}
BIN="$WORKTREE/build/c/codebase-memory-mcp"
DEMO=$(mktemp -d /tmp/cbm-accept-XXXXXX)
PROJ="accept-demo"
PASS=0
FAIL=0

cleanup() { rm -rf "$DEMO"; }
trap cleanup EXIT

echo "== build production binary (no libgit2; env has incompatible headers) =="
make -C "$WORKTREE" -f Makefile.cbm SANITIZE= LIBGIT2_CFLAGS= LIBGIT2_LIBS= cbm \
  >/tmp/cbm-accept-build.log 2>&1 \
  || { echo "BUILD FAILED"; tail -40 /tmp/cbm-accept-build.log; exit 1; }
test -x "$BIN" || { echo "binary missing at $BIN"; exit 1; }
echo "build: OK"

echo "== full unit test suite =="
make -C "$WORKTREE" -f Makefile.cbm SANITIZE= LIBGIT2_CFLAGS= LIBGIT2_LIBS= test \
  >/tmp/cbm-accept-test.log 2>&1 \
  && echo "unit tests: PASS ($(grep -c '^PASS' /tmp/cbm-accept-test.log || true) lines)" \
  || { echo "unit tests: FAIL"; tail -60 /tmp/cbm-accept-test.log; exit 1; }

# Confirm the suite actually ran the cypher tests
grep -q 'test_cypher\|suite_cypher\|cypher_exec_where' /tmp/cbm-accept-test.log \
  || grep -q 'passed' /tmp/cbm-accept-test.log \
  || echo "warning: could not confirm cypher suite ran (check log)"

echo "== fixture + index =="
mkdir -p "$DEMO"
cat > "$DEMO/demo.go" <<'EOF'
package demo

func Helper() int { return 1 }

func Caller() int { return Helper() }

func Cross() int { return Helper() }
EOF
git -C "$DEMO" init -q
git -C "$DEMO" add -A
git -C "$DEMO" commit -qm accept

"$BIN" cli index_repository "{\"repo_path\":\"$DEMO\",\"mode\":\"full\",\"name\":\"$PROJ\"}" \
  >/tmp/cbm-accept-index.log 2>&1 \
  || { echo "INDEX FAILED"; tail -20 /tmp/cbm-accept-index.log; exit 1; }

query() {
  local q="$1" label="$2" expect_rows="$3"
  local out rows err
  set +e
  out=$("$BIN" cli query_graph "{\"project\":\"$PROJ\",\"query\":$q}" 2>/tmp/cbm-accept-qerr.log)
  local rc=$?
  set -e
  if [ $rc -ne 0 ] || printf '%s' "$out" | grep -q 'expected value'; then
    err=$(grep -v 'mem.init' /tmp/cbm-accept-qerr.log | tail -1)
    echo "FAIL $label: query failed (${err:-no output})"
    FAIL=$((FAIL+1)); return
  fi
  rows=$(printf '%s' "$out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('total',-1))" 2>/dev/null || echo -1)
  if [ "$rows" = "$expect_rows" ]; then
    echo "PASS $label (rows=$rows)"
    PASS=$((PASS+1))
  else
    echo "FAIL $label: expected $expect_rows rows, got $rows"
    printf '%s\n' "$out" | head -c 400; echo
    FAIL=$((FAIL+1))
  fi
}

echo "== acceptance queries (2 CALLS edges, both same-file) =="
query '"MATCH (a)-[:CALLS]->(b) WHERE a.file_path = b.file_path RETURN a.name, b.name"' \
  "same-file (a.file_path = b.file_path)" 2
query '"MATCH (a)-[:CALLS]->(b) WHERE a.file_path <> b.file_path RETURN a.name, b.name"' \
  "cross-file (a.file_path <> b.file_path)" 0
query '"MATCH (a)-[:CALLS]->(b) WHERE a.name <> b.name RETURN a.name, b.name"' \
  "name inequality (a.name <> b.name)" 2
query '"MATCH (a)-[:CALLS]->(b) WITH a, b WHERE a.file_path = b.file_path RETURN a.name"' \
  "post-WITH same-file" 2
query '"MATCH (a)-[:CALLS]->(b) WHERE b.file_path = '"'"'demo.go'"'"' RETURN a.name, b.name"' \
  "property-to-literal control" 2

echo
echo "ACCEPTANCE: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ] || exit 1
exit 0
