# A4C vs Squad — Large Repository Benchmark

**Date:** 2026-10-15 · **Status:** complete · **Results:** see `RESULTS.md`

## 1. Selected repository

| Field | Value |
|---|---|
| Repository | `DeusData/codebase-memory-mcp` |
| Local clone | `/Volumes/Projects/codebase-memory-mcp` |
| Commit | `63e3f2c7a6ba0ae444967d1f777254e9df5e381a` (v0.8.1-249, clean) |
| Language | C11 (+ vendored tree-sitter grammars, SQLite, mimalloc) |
| Tracked source files | 1,441 (747 `.c` + 631 `.h`; ~644 first-party, ~797 vendored) |
| Subsystems | foundation, pipeline (30 passes), cypher, store, mcp, graph_buffer, semantic, lsp (9 langs), discover, watcher, git, traces, simhash, cli, ui |
| License | MIT (public OSS — no proprietary code sent to the model host) |
| Build | `make -f Makefile.cbm SANITIZE= cbm` (prod) / `test` (full suite) |
| Test suite | 5,722 tests, all passing on baseline (`SANITIZE=` build ~3.5 min; incremental relink ~23 s) |

**Rationale:** Substantial multi-subsystem C codebase with a reliable, reproducible
test harness. Build is feasible on this machine in minutes (not hours). Clean
pinned public revision. Far more complex than the previous Go `sched` fixture:
import resolution, 30 pipeline passes, Cypher query engine, multi-language LSP.

Rejected alternatives:
- chromium/firefox/gcc/llvm/kibana/vscode/linux — too large to build/test in benchmark budget
- tree-sitter — 295 source files; full `make test` requires network fixture fetch; smaller multi-subsystem surface
- evtx-rust — dirty working tree (161 files)
- codetopo / cli-replay — own projects; proprietary risk for Copilot

## 2. Selected task

**GitHub issue #2591:** `query_graph: a WHERE comparison between two properties
(a.file_path = b.file_path) fails with "expected value at pos N"`

A real, open, cross-component defect in the Cypher query engine. Requires:

1. Understanding the recursive-descent WHERE parser (`parse_condition_expr`)
2. Extending the condition model (`cbm_condition_t` only stores a literal value)
3. Changing the evaluator (`eval_condition` / `resolve_condition_value`) to
   resolve a property right-hand side against the binding
4. Preserving all existing WHERE operators and paths (early WHERE + post-WITH)
5. Adding unit tests without regressing 5,722 existing tests

**Baseline verified on this exact commit:**

| Query | Result |
|---|---|
| `WHERE a.file_path = b.file_path` | `expected value at pos 44` (FAIL) |
| `WHERE a.name <> b.name` | `expected value at pos 40` (FAIL) |
| `WHERE b.file_path = 'demo.go'` | works (control) |

Full task text: `testdata/task.txt`
Hidden acceptance: `testdata/acceptance_where_prop.sh` (build + full suite + 5 CLI queries)

## 3. Competitors

| Config | Invocation |
|---|---|
| A — A4C Direct | `a4c task start <task> 2000000 --strategy direct --runtime copilot --model gpt-5.5 --steps 50` |
| B — A4C Team | `a4c task start <task> 2000000 --strategy team --runtime copilot --model gpt-5.5 --steps 50` |
| C — Squad | `copilot --agent squad --model gpt-5.5 --yolo --additional-mcp-config @.mcp.json --usage-output-file <f> -p <task>` |

- Host: GitHub Copilot CLI 1.0.91, `COPILOT_AUTO_UPDATE=false`
- Model: gpt-5.5 on every call (verified from session store)
- Squad: built from `bradygaster/squad` @ `3d0c834` (1.0.0-build.1)
- Identical starting trees (git clone of the pinned commit into three dirs)
- Equivalent fail-closed tool permissions for A4C; `--yolo` for Squad
- One attempt each; failures and limit-reached are recorded as-is

## 4. Execution

```bash
# from /Volumes/Projects/a4c
RUN_ID=$(date +%F)-large-repo-run1
benchmarks/large-repo-comparison/run_benchmark.sh "$RUN_ID"
```

Results land in `benchmarks/large-repo-comparison/results/$RUN_ID/`.
