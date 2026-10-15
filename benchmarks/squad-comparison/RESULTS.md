# A4C vs Squad — Measured Token-Efficiency Results

**Date:** 2026-10-14/15 · **Host:** GitHub Copilot CLI 1.0.91 · **Model:**
`gpt-5.5` (verified on every model call of every session and subagent in all
runs) · **Task:** implement interval algebra (`Merge`, `Total`, `Subtract`,
`Complement`) in a disposable Go package with existing code and tests, from an
identical initial git tree (`83c6c3ef88f7…`), verified independently afterward
by hidden acceptance tests injected only after both runs finished.

Raw usage records for every row below are preserved under
`results/2026-10-14-run2/` (A4C team + Squad) and
`results/2026-10-14-run3-a4c-direct/` (A4C direct): host-reported per-call
events from `~/.copilot/session-store.db`, A4C `task inspect --json`, Squad
`--usage-output-file`, product logs, final diffs, and acceptance logs.

## Versions and configurations

| Component | Value |
|---|---|
| A4C | built from workspace commit `1450154` + benchmark harness files |
| Squad | built from `bradygaster/squad` commit `3d0c834d40f11f538df2038428f19735c23729c5` (`1.0.0-build.1`); pre-existing global npm `0.11.0` untouched |
| Copilot CLI | `1.0.91`, `COPILOT_AUTO_UPDATE=false` for both products |
| Model pinning | A4C: `--model gpt-5.5` per activity session. Squad: `--model gpt-5.5` for the coordinator + `defaultModel: gpt-5.5` in `.squad/config.json` (Squad's own documented Layer-0 pin, which instructs the coordinator to pass `model: gpt-5.5` on every `task` spawn). Verified: every `assistant_usage_events.model` in every run is `gpt-5.5`; no fallback occurred. |
| Reasoning effort | CLI default for both (not pinned) |
| A4C invocation | `a4c task start <task> 500000 --strategy <direct\|team> --runtime copilot --model gpt-5.5 --steps 30` |
| Squad invocation | `copilot --agent squad --model gpt-5.5 --yolo --additional-mcp-config @.mcp.json --usage-output-file <f> -p <task>` (Squad's own documented non-interactive flag set; `squad_state` MCP server pinned to the same local squad build) |
| Permissions | A4C: fail-closed handler that auto-approves native tools for the mutating coder inside its isolated worktree; reviewer read-only. Squad: `--yolo`. Equivalent autonomy, each per product design. |
| Measurement | Copilot CLI's `~/.copilot/session-store.db` `assistant_usage_events` (host-reported; includes `task`-tool subagents via `initiator='sub-agent'`), filtered by run working directory + id watermark. Cross-checked against A4C's own metrics (exact match) and Squad's `--usage-output-file` (exact match). No token estimation anywhere. |

## Cold setup phase

| Metric | A4C (`a4c init`) | Squad (`squad init --preset default`) |
|---|---|---|
| Wall time | 0.4 s | 0.8 s |
| Model calls | 0 | 0 |
| Tokens (all kinds) | 0 | 0 |
| Cost | 0 AI credits | 0 AI credits |

Neither product's ordinary init path makes model calls (Squad's optional
LLM "casting" ceremony is an interactive-REPL feature and is not used by the
documented `--preset default` quick start). Squad's init writes a 79 KB
coordinator prompt (`.github/agents/squad.agent.md`), 9 agent charters, MCP
and workflow scaffolding — all filesystem-only. **Neither product pays a
casting cost in its normal non-interactive startup.**

## Coding phase — primary comparison (each product's normal mode)

A4C's documented primary mode for focused single-host tasks is `direct`
(README §"Direct Mode"); Squad has only its coordinator/delegation mode.

| Metric | A4C (direct) | Squad |
|---|---|---|
| **Setup tokens** (model calls) | **0** | **0** |
| **Coding input tokens (uncached)** | **15,813** | **59,829** |
| **Coding cache-read tokens** | **75,776** | **451,584** |
| Cache-write tokens | 0 (none reported by host for gpt-5.5) | 0 |
| **Coding output tokens** | **4,098** | **5,852** |
| **Reasoning tokens** (subset of output — not additive) | **96** | **1,202** |
| **Total reported input tokens** (incl. cache reads) | **91,589** | **511,413** |
| **Setup cost** | **0 AI credits** | **0 AI credits** |
| **Coding cost** (host-reported AI credits) | **23.99** | **70.05** |
| **Total elapsed time** (setup + coding) | **46.9 s** | **82.0 s** |
| **Model turns** (host-reported model calls) | **6** | **18** (8 coordinator + 10 sub-agent) |
| **Subagent sessions** | **0** (1 A4C activity session) | **1** task-tool subagent inside 1 coordinator session |
| Tool calls | partial: 2 file reads self-reported; governed-tool calls 0; native edit/bash not counted by A4C | not reported by host or product on this path (**unavailable**) |
| **Acceptance tests** (hidden, harness-injected) | **PASS** (`ok example.com/sched`) | **PASS** (`ok example.com/sched`) |
| Requirements completed | 4/4 functions + agent's own unit tests | 4/4 functions + agent's own unit tests |
| Unnecessary modifications | none (only `sched.go`, `sched_test.go`) | none in task files (`sched.go`, `sched_test.go`); product scaffolding from init (`.squad/`, `.github/`, `.mcp.json`, etc.) excluded |
| Failed attempts / recovery | none observed | none observed |

## Secondary configuration: A4C team mode (run first, before direct)

The first benchmark leg ran A4C in `team` strategy (lead → coder → reviewer)
as the closest structural mirror of Squad. It **failed the task**:

| Metric | A4C (team) |
|---|---|
| Coding input (uncached / cache-read / total) | 21,357 / 89,088 / 110,445 |
| Output / reasoning | 4,284 / 972 |
| Cost | 27.98 AI credits |
| Wall time | 56.7 s |
| Model turns / sessions | 11 / 2 (lead + coder; reviewer never ran) |
| Acceptance tests | **FAIL** (no implementation; `undefined: Merge`) |

Mechanism (fully evidenced in `results/2026-10-14-run2/a4c-*`): the lead
activity consumed 30,590 tokens (28% of the leg, zero code) and proposed its
coder child with a **4,000-token budget** (reviewer: 2,500). A4C's governor
enforced that budget in-flight — the coder's first model call alone was
12,769 input tokens, so every subsequent governed/native tool call was
rejected with "Budget exceeded"; the strangled coder session burned 84,139
tokens against rejections and produced no changes. This is A4C's governance
operating as designed; the failure is a model budgeting miscalibration inside
that design, not a harness artifact (parent budget was 500,000 tokens).

## Where consumption is concentrated

**Squad:** the coordinator is the dominant cost center. Squad's own
`--usage-output-file` agent breakdown: coordinator ("main") = 8 calls,
319,430 total input tokens (62% of the run), 2,146 output, 40.35 AI credits —
**zero code produced** (routing, spawn ceremony, after-agent ceremonies,
verification); the single coding subagent ("lead", general-purpose) = 10
calls, 191,983 input, 3,706 output, 29.70 AI credits. The 79 KB coordinator
prompt drives a 35,041-token first call and ~35–44k cache-read turns on every
coordinator round-trip. Squad's persistent on-disk context (charter history,
decisions, ceremonies) is read/consulted by the coordinator but the bulk of
measured overhead is the coordinator prompt + ceremony loop itself, not the
memory files.

**A4C direct:** one coder session; a ~12.3k-token charter/system first call,
then a short tool loop. Orchestration overhead is negligible (no coordinator
turns exist). A4C's durable state (`.a4c/` stores, snapshots, budgets) costs
no model tokens.

**A4C team:** the lead (coordinator) consumed 30,590 tokens with zero code —
comparable *per-token* ceremony overhead to Squad's coordinator, but the run
ended in failure so it cannot be scored as an economical mode.

## Answers to the benchmark questions

1. **Configurations:** see table above (exact commands, versions, pins).
2. **Concentration:** Squad — coordinator ceremonies (62% of all input
   tokens, zero code). A4C direct — the coding session itself. A4C team —
   lead ceremony + governance-strangled coder retry loop.
3. **Squad coordinator/context overhead:** yes, measurable and dominant:
   319,430 input tokens / 40.35 AI credits of coordinator work with zero code
   on a task whose entire coding output was 3,706 tokens. Roughly 2.2× the
   coding agent's own consumption.
4. **A4C governance/coordination overhead:** negligible in direct mode (no
   coordinator); in team mode, comparable ceremony overhead to Squad's
   coordinator **plus** a strict-budget failure mode that prevented
   completion on this run.
5. **Which is more economical for this task:** **A4C (direct mode)** — it
   delivered the same verified correctness as Squad using **5.6× less total
   input** (91,589 vs 511,413), **3.8× less uncached input** (15,813 vs
   59,829), 1.4× less output, **2.9× less host-reported cost** (23.99 vs 70.05
   AI credits), and 1.7× less wall time. A4C team mode was **not**
   economical: it burned 110k input tokens and failed the task.
6. **Limitations of one run:** single sample per configuration — no error
   bars; model nondeterminism could change Squad's spawn choices or the A4C
   lead's budgeting (a different team-mode run might not under-budget the
   coder); cache-read volume is order- and prefix-dependent; reasoning effort
   was not pinned; one small Go task; Squad ran from a HEAD source build
   rather than the npm 1.0.0 release; A4C `reviewed` strategy was not
   benchmarked; tool-call counts are not reliably host-recorded on either
   path; cost is the host's AI-credit billing unit, not USD.

## Final question

> For the same engineering task, on the same model and host, does A4C
> consume fewer tokens than Squad while delivering equivalent correctness?

**Measured answer: yes — in A4C's normal (direct) mode.** On this task, same
host, same pinned model, same fixture, independently verified: A4C consumed
91,589 total input tokens / 23.99 AI credits and passed all hidden acceptance
tests; Squad consumed 511,413 total input tokens / 70.05 AI credits and also
passed. A4C's extra tokens were almost entirely productive coding work;
Squad's majority (62%) was coordinator ceremony. **In A4C's team mode the
answer is no:** it failed to deliver working code (lead under-budgeted the
coder to 4,000 tokens; governance enforced it), so its lower token count is
not an efficiency victory.

## Run integrity notes

- One preliminary execution (harness debugging) was discarded before analysis
  due to two harness bugs (A4C argv splitting, usage-attribution exact-path
  match); both were fixed and the reported runs re-executed from fresh,
  identical fixture repos. The discarded run's host events remain in the
  session store but outside all reported watermarks.
- A4C's own `ExecutionMetrics` match the host session store exactly for every
  activity in every run (prompt/completion/cached/reasoning), and Squad's
  `--usage-output-file` matches the session store exactly — three independent
  readouts agree.
- Acceptance tests were validated against a reference implementation before
  the benchmark; neither product ever saw them.
- `squad cost` (Squad's own report command) shows "No token usage data found"
  on its primary non-interactive path — Squad's SDK usage ledger is not
  engaged when running via `copilot --agent squad -p`; the host session store
  and `--usage-output-file` are the reliable sources there.
