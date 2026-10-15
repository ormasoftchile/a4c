# A4C vs Squad — Real Token-Efficiency Benchmark

Reproducible comparison of **A4C** against **bradygaster/squad** on one
identical coding task, using **GitHub Copilot CLI** as the shared host and one
pinned model for every agent invocation.

## Components

| Piece | Location |
|---|---|
| Orchestrating live test (harness) | `pkg/a4c/squad_comparison_benchmark_test.go` |
| Fixture repo template | `pkg/a4c/testdata/squad-comparison/fixture/` |
| Task description (identical for both products) | `pkg/a4c/testdata/squad-comparison/task.txt` |
| Hidden acceptance tests (injected post-run) | `pkg/a4c/testdata/squad-comparison/acceptance_sched_test.go` |
| Raw records + reports | `benchmarks/squad-comparison/results/` |
| Final analysis | `benchmarks/squad-comparison/RESULTS.md` |

## What the harness does

1. Scaffolds two byte-identical disposable Go fixture repos (verified by git
   tree hash) in independent working directories.
2. **Cold setup phase** — runs each product's init (`a4c init` /
   `squad init --preset default`), measuring wall time and any host-reported
   model usage (both are filesystem-only; measured model usage is zero).
3. **Coding phase** — runs each product on the same task:
   - A4C: `a4c task start <task> 500000 --strategy <direct|team> --runtime copilot --model gpt-5.5 --steps 30`
   - Squad: `copilot --agent squad --model gpt-5.5 --yolo --additional-mcp-config @.mcp.json --usage-output-file <f> -p <task>`
     (the flag set is Squad's own empirically proven non-interactive
     invocation; `.squad/config.json` gets `defaultModel: gpt-5.5` — Squad's
     documented Layer-0 model pin — and the `.mcp.json` state server is
     pinned to the same local squad build under test).
4. **Measurement** — both products run on GitHub Copilot CLI, which persists
   host-reported per-model-call usage for every session (including `task`
   tool subagents, recorded with `initiator='sub-agent'`) in
   `~/.copilot/session-store.db` (`assistant_usage_events`). One measurement
   source therefore covers both products, including all subagents. A4C's own
   `ExecutionMetrics` (parsed from the Copilot SDK usage stream) are saved
   alongside as a cross-check and match the session store exactly. Tokens are
   never estimated from text lengths. Squad's `--usage-output-file` output is
   also preserved and matches the session store exactly.
5. **Independent verification** — the hidden acceptance test file is injected
   into each final workspace only after both runs finish; `go test ./...` is
   executed by the harness, not trusted from the products. Final diffs are
   captured before injection.

## Reproduce

```bash
# 1. Build squad from source (Node >= 22.5)
git clone https://github.com/bradygaster/squad && cd squad
npm install && npm run build   # → packages/squad-cli/dist/cli-entry.js

# 2. Run the harness (spends real tokens; ~5–15 min)
cd /path/to/a4c
A4C_BENCH_SQUAD_LIVE=1 \
A4C_BENCH_SQUAD_CLI=/path/to/squad/packages/squad-cli/dist/cli-entry.js \
A4C_BENCH_OUT=$(pwd)/benchmarks/squad-comparison/results/$(date +%F)-run \
go test -run TestSquadComparison_LiveBenchmark -v -timeout 2h ./pkg/a4c
```

Optional knobs: `A4C_BENCH_ONLY=a4c|squad` restricts the run to one product;
`A4C_BENCH_A4C_STRATEGY=direct|reviewed|team` selects the A4C strategy
(default `team`).

## Versions tested (2026-10-14/15 runs)

- A4C: built from this workspace, commit `1450154` (+ benchmark harness files)
- Squad: built from `bradygaster/squad` commit
  `3d0c834d40f11f538df2038428f19735c23729c5` (version `1.0.0-build.1`);
  the pre-existing global npm install `@bradygaster/squad-cli@0.11.0` was
  left untouched
- Host: GitHub Copilot CLI `1.0.91`, `COPILOT_AUTO_UPDATE=false` for both
- Model: `gpt-5.5` on every model call of every session and subagent
  (verified from the session store; no other model appears in any run)

See `RESULTS.md` for measured results and analysis.
