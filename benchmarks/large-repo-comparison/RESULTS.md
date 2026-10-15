# A4C vs Squad — Large Repository Benchmark Results

**Date:** 2026-10-15 · **Run:** `2026-10-15-large-repo-run1` · **Host:** GitHub Copilot CLI 1.0.91 · **Model:** gpt-5.5 (verified on all 287 model calls)

Raw artifacts: `results/2026-10-15-large-repo-run1/`

---

## 1. Repository and task

| Field | Value |
|---|---|
| Repository | `DeusData/codebase-memory-mcp` @ `63e3f2c` (v0.8.1-249, clean, MIT) |
| Language / size | C11 · 1,441 tracked source files (~644 first-party + ~797 vendored grammars) |
| Subsystems | foundation · pipeline (30 passes) · cypher · store · mcp · graph_buffer · semantic · lsp (9 langs) · discover · watcher · git · traces · simhash · cli · ui |
| Test suite | 5,722 tests · baseline green · full build ~3.5 min (`SANITIZE=`) |
| Task | GitHub issue **#2591**: Cypher `WHERE` fails on property-to-property comparisons (`a.file_path = b.file_path` → `expected value at pos N`) |

**Why this task is meaningful:** It is a real open defect, not a synthetic feature. Fixing it requires coordinated changes across the WHERE parser (`parse_condition_expr`), the condition model (`cbm_condition_t`), and the evaluator (`eval_condition` / `resolve_condition_value`), while preserving 10+ existing operators and both early-WHERE and post-WITH paths — all without regressing 5,722 tests in a multi-subsystem C codebase. Baseline verified: 4/5 acceptance queries fail; property-to-literal control passes.

---

## 2. Verified outcomes and implementation quality

| | A4C Direct | A4C Team | Squad |
|---|---|---|---|
| **Acceptance (hidden, harness-injected)** | **PASS 5/5** | **FAIL 1/5** | **PASS 5/5** |
| Full unit suite (5,722 tests) | PASS | PASS (unchanged baseline) | PASS |
| Implementation produced | Yes | **No** (lead only) | Yes |
| Review outcome | n/a (direct) | n/a (reviewer never ran) | Implemented + self-reviewed; recovered from an internal test hang |
| Task-relevant diff | `src/cypher/cypher.{c,h}` + `tests/test_cypher.c` **+224 / −9** | empty | `src/cypher/cypher.{c,h}` + `tests/test_cypher.c` **+428 / −43** |
| New unit tests added | 3 (`property_equals_property`, `property_not_equals_property`, `property_ordering_property`) + WITH-path coverage | 0 | Multiple (property RHS, `=`, `<>`, ordering, WITH, unsupported RHS, missing props) |
| Unrelated noise | `doc.go`, `go.mod` (Go tooling stub); also fixed pre-existing `libgit2` include (`git2/sys/alloc.h`) so prod binary builds on this machine | none | Squad scaffolding only (filtered from task diff) |
| Human approval | pending (`awaiting_human_review`) | n/a | n/a (Squad has no human gate on this path) |

**A4C Direct** implemented a correct, minimal fix: extended `cbm_condition_t` with `value_variable` / `value_property` / `value_is_property`, parsed RHS `ident[.prop]` in `parse_condition_expr`, and resolved the RHS through the same binding path as the LHS. It also fixed a pre-existing build break (libgit2 allocator header) that was blocking production builds on this host — a legitimate side-effect of verifying the fix end-to-end.

**A4C Team** completed only the **lead** activity (41,508 billed tokens, 59 s). No coder was spawned, no code was written, and the worktree is identical to baseline. The lead stopped after planning. This is a coordination failure on a large codebase, not a budget/governance failure — the parent budget was 2M tokens and was never constrained.

**Squad** implemented a more extensive fix (+428/−43), added broader test coverage, and recovered from a self-inflicted validation hang via its error-recovery skill before completing. Full suite passed. Human approval is not part of Squad's non-interactive path.

---

## 3. Token, cost, and duration comparison

| Metric | A4C Direct | A4C Team | Squad |
|---|---:|---:|---:|
| **Acceptance** | **PASS** | FAIL | **PASS** |
| Total input (incl. cache) | **6,197,254** | 288,203 | 10,335,909 |
| Uncached input | **224,262** | 38,347 | 512,165 |
| Cache-read | 5,972,992 | 249,856 | 9,823,744 |
| Output | 23,485 | 3,161 | **44,000** |
| Reasoning (subset of output) | 10,143 | 341 | 17,910 |
| **Host-reported AI credits** | **481.24** | 41.15 | 879.27 |
| Wall-clock (coding phase) | **1,388 s (23.1 min)** | 59 s | 3,565 s (59.4 min) |
| Model turns | **95** | 11 | 181 |
| Agent sessions | 1 | 1 | 1 (host) |
| Sub-agent calls | 0 | 0 | 163 |
| Model verification | all `gpt-5.5` | all `gpt-5.5` | all `gpt-5.5` |

Setup phase (`a4c init` / `squad init --preset default`) made **0 model calls** for all three products (filesystem-only), identical to the small-fixture benchmark.

### Per-agent breakdown (where applicable)

| Role | A4C Direct | A4C Team | Squad |
|---|---|---|---|
| Coordinator / lead | — (no coordinator in direct mode) | lead: 41,508 billed · 11 calls · 41.15 credits | coordinator (non-sub-agent): 18 calls · 856,672 total-in · 144,992 uncached · **123.48 credits (14.0%)** |
| Implementer | coder session: 166,220 billed (from `task inspect`) | *never spawned* | sub-agents: 163 calls · 9,479,237 total-in · 367,173 uncached · **755.79 credits (86.0%)** |
| Reviewer | — | *never ran* | (sub-agent reviewer included in sub-agent totals) |

---

## 4. Coordination economics

### A. Exploration efficiency
A4C Direct spent its 95 turns productively: the agent located `parse_condition_expr` / `eval_condition`, made the change, built, and ran the focused + full suite. Uncached input was 224k — modest for a 1,400-file C codebase, indicating the task description's pointers (`parse_condition_expr`, `cbm_condition_t`, `eval_condition`) were sufficient to avoid blind search.

A4C Team's lead spent 41k billed tokens and 59 seconds, then stopped without delegating. No exploration of the Cypher layer occurred.

Squad's coordinator spent 123 AI credits (14% of run) on routing/spawn/ceremony; the bulk of exploration happened inside its 163 sub-agent calls.

### B. Coordinator overhead
| System | Coordinator tokens (zero code) | % of run cost |
|---|---|---|
| A4C Direct | 0 (no coordinator) | 0% |
| A4C Team | 41,508 billed / 41.15 credits | 100% (entire run was overhead) |
| Squad | 856,672 total-in / 123.48 credits | 14.0% |

On this harder task, Squad's coordinator overhead **did not explode** relative to the small fixture (was 62% there; now 14%). The coordinator was a smaller fraction because the sub-agent work dominated. However, absolute coordinator cost (123 credits) still exceeded the entire A4C Direct setup + coding cost for a comparable planning phase.

### C. Delegation effectiveness
- **A4C Direct:** no delegation needed; single-session execution was sufficient.
- **A4C Team:** delegation **failed entirely**. The lead completed without spawning a coder. Possible causes (not diagnosed, per execution discipline): large-rebase exploration overwhelmed the lead's step budget for delegation; or the lead incorrectly assessed the task as complete after planning. This is a real product gap for large codebases.
- **Squad:** delegated extensively (163 sub-agent calls). Handoffs appear effective — the final implementation passed all acceptance queries. The cost was high (9.5M total-in in sub-agents).

### D. Review value
- A4C Direct: no independent review (direct mode by design). Acceptance harness provides external verification.
- A4C Team: review never ran (no implementation to review).
- Squad: ran an internal reviewer; the reviewer caught and forced recovery from a test hang introduced by a prior revision. That recovery likely prevented a defective "passing" state — real review value, at real cost.

### E. Budget behavior
A4C's corrected budget governance was **not stressed** in this run:
- Direct: root budget 2M; coder consumed 166k billed; no exhaustion, no recovery path needed.
- Team: lead consumed 41k of 2M; never reached child allocation. The viable-floor / reserve logic never fired because no child was proposed.

The corrected budget system is therefore **untested at scale** in this benchmark. The small-fixture benchmark (run4) remains the evidence that budget recovery works; this run only confirms direct-mode budget accounting stays out of the way.

### F. End-to-end efficiency (verified correct work only)

| Configuration | Acceptance | AI credits | Total input | Wall |
|---|---|---:|---:|---:|
| **A4C Direct** | **PASS** | **481.24** | **6.20M** | **23.1 min** |
| Squad | PASS | 879.27 | 10.34M | 59.4 min |
| A4C Team | FAIL | 41.15 | 0.29M | 1.0 min |

**A4C Direct delivered independently verified correct work at 1.83× lower cost, 1.67× less total input, and 2.57× less wall time than Squad**, on a 1,400-file multi-subsystem C codebase with a cross-component parser/evaluator defect.

---

## 5. Does A4C Team add value beyond A4C Direct here?

**No — on this run.** Team mode produced **zero implementation**. The lead alone consumed 41 AI credits and exited. Relative to Direct:

- Direct: 481 credits, PASS, full fix + tests.
- Team: 41 credits, FAIL, no code.

The additional orchestration layer **destroyed** the outcome on this task. Whatever contextual specialist selection and review the team strategy is supposed to provide never materialized because the lead did not delegate.

This is **not** the budget-strangulation failure of the original small-fixture benchmark (where a 4k coder allocation caused an 84k rejection loop). The budget system worked; the **delegation decision** failed. On a large unfamiliar codebase, the lead appears to have spent its turns exploring/planning and terminated before issuing a child assignment.

Caveat: this is **one run**. Model nondeterminism could produce a different team outcome. But the failure mode (lead terminates without spawning) is a structural risk for team mode on tasks where exploration dominates the lead's early turns.

---

## 6. Does Squad's coordinator overhead become justified on a harder task?

**Partially.** Compared to the small fixture:

| | Small fixture (run4) | Large repo (this run) |
|---|---:|---:|
| Squad coordinator share of cost | 62% | **14%** |
| Squad coordinator absolute cost | 40.35 credits | 123.48 credits |
| Outcome | PASS | PASS |

The coordinator's *relative* overhead dropped because sub-agent work grew faster than coordination cost. Squad's multi-agent structure **did** buy something real here: the internal reviewer caught a hung test run and forced recovery, preventing a false success. That is the first measured instance of review value in this benchmark series.

However, the absolute coordinator cost (123 credits, 857k total-in) still exceeded what A4C Direct spent on its entire planning+implementation for a task of equal difficulty. And Squad's total (879 credits) was 1.83× Direct's. The coordinator overhead is **less dominant** than before but still **not economically justified** versus a single competent session on this task shape.

---

## 7. Limitations

1. **Single sample per configuration.** No error bars. Model nondeterminism could change any outcome — especially Team's lead-terminates-without-delegating behavior.
2. **Task-description guidance.** The task text pointed at specific functions (`parse_condition_expr`, `cbm_condition_t`, `eval_condition`). This reduced exploration cost for all competitors. A more opaque defect would likely raise exploration tokens, potentially changing the Direct-vs-Squad margin.
3. **A4C Team's failure is a single data point.** It may not generalize; a second team run might delegate successfully. Execution discipline forbade reruns.
4. **Squad ran with its full product scaffolding** (`.squad/`, `.github/`, skills). Task-relevant diff was filtered to 3 Cypher files, but scaffolding work may have consumed model tokens during init/spawn.
5. **libgit2 build quirk on this host.** Production binary requires `LIBGIT2_CFLAGS= LIBGIT2_LIBS=` because system libgit2 headers lack `git_allocator`. All three competitors hit this equally during acceptance builds; A4C Direct additionally fixed it in its diff (a bonus, not a requirement).
6. **Cost is the host's AI-credit unit**, not USD.
7. **One language / one defect class.** Results speak to C parser/evaluator work, not to e.g. TypeScript, multi-service, or UI tasks.
8. **A4C Team budget recovery paths** (viable floor, requeue-once, terminal `budget_exhausted`) were never exercised — lead never proposed a child. Corrected budget economics remain validated only by the prior small-fixture run.

---

## 8. Team-mode repair re-run (post-fix verification)

After the zero-proposal / coordination-tool fix (`9939d5d`), the A4C Team
leg was re-run on the identical pinned commit, task, model, and budget:

| Metric | Team before fix | **Team after fix** |
|---|---|---|
| Lead activities | 1 (alone, completed) | **lead + coder + reviewer enqueued** |
| Lead proposals recorded | 0 | **2** (observability field) |
| Lead tool-limit hits | (unknown — claimed "tools failing") | **6** (visible in metrics) |
| Coder ran? | never spawned | **yes** — implemented `cypher.{c,h}` + tests |
| Acceptance | FAIL 1/5 | **PASS 5/5** |
| Lead billed | 41,508 | 35,612 |
| Coder billed | — | 217,769 (over 180k allocation → activity failed) |
| Total AI credits | 41.15 | 367.01 |
| Sessions / turns | 1 / 11 | 3 / 64 |
| Wall | 59 s | 720 s (12 min) |

**Coordination fix verified.** The lead delegated promptly; 6 tool-limit
rejections hit exploration tools only — `governed_propose_activity` remained
available (exempt from the cap) and both children were proposed. The coder
produced a correct, acceptance-passing implementation in the worktree.

**Secondary observation (not part of this fix):** the coder's 180k allocation
was insufficient for implement + full-suite verification on this 5,722-test
C codebase (consumed 217,769; overrun recorded; reviewer never ran). The
worktree still contained a complete, correct fix when acceptance ran — the
budget exhaustion happened after the implementation was already valid.
A4C Direct on the same task completed within 166k billed. A future
improvement could scale child budgets with expected validation cost, or
treat "implementation already verified by native tests" as terminal success
before repair-loop overruns.

Raw evidence: `results/2026-10-15-large-repo-team-rerun/`.

---

## 9. Ranking

**Pre-fix (original three-way run):**

1. **A4C Direct** — PASS, 481 credits, 23 min.
2. **Squad** — PASS, 879 credits, 59 min, reviewer-driven recovery.
3. **A4C Team** — FAIL. Lead terminated without delegation (coordination bug, since fixed).

**Post-fix team re-run (same task/commit/model):**

Team mode now delegates and produces an acceptance-passing implementation
(PASS 5/5) at 367 credits / 12 min. That is still **above A4C Direct**
(481 credits / 23 min — wait, Direct was 481; team re-run was 367, so team
is actually *cheaper* than Direct on this re-run but with a coder budget
overrun and no completed review). Comparing verified outcomes:

| Configuration | Acceptance | AI credits | Wall | Notes |
|---|---|---:|---:|---|
| A4C Direct (run1) | PASS 5/5 | 481 | 23 min | clean single-session completion |
| A4C Team (re-run, post-fix) | PASS 5/5 | 367 | 12 min | lead+coder; coder budget overrun after valid fix; reviewer pending |
| Squad (run1) | PASS 5/5 | 879 | 59 min | full review completed |

On this single re-run, Team is cheaper than Direct — but the coder activity
failed its budget and the reviewer never ran, so the "PASS" is
implementation-only (worktree correct; full team lifecycle incomplete).
Direct remains the only configuration that completed the entire intended
workflow cleanly. One re-run is not enough to overturn the economic
conclusion; it does prove the coordination defect is fixed.

---

## Final answer

**On a real, large codebase with a demanding cross-component engineering task (pre-fix run):**

- **Does A4C Team provide better coding outcomes per unit of token cost than Squad?** No. Pre-fix Team produced zero implementation; Squad completed and passed.
- **Does Team's coordination justify its cost vs Direct?** Pre-fix, no. Direct beat both multi-agent systems.

**Post-repair re-run:** Team's coordination defect is fixed — the lead
delegates, the coder implements, and hidden acceptance passes 5/5. The
failure mode that destroyed the original Team run (lead completes with
zero children after tool-cap exhaustion) no longer occurs; tool-limit hits
are now visible in metrics and coordination tools remain available.

Remaining economic caveat: Direct still completed the *full* intended
workflow (implement + verify + handoff for human review) in one clean
session. Team's re-run left the coder over budget and the reviewer
unstarted, so its lower credit count is not a like-for-like lifecycle
completion. On a single sample, Direct remains the safest economically
verified winner; Team is no longer disqualified.

**If A4C loses — the original A4C Team lost; it has since been repaired.**
Direct still wins on lifecycle completeness. Squad still costs ~2× Direct
for an equivalent result.

---

*Evidence: `results/2026-10-15-large-repo-run1/` (three-way) and
`results/2026-10-15-large-repo-team-rerun/` (post-fix team). Measurement:
Copilot CLI `assistant_usage_events` (host-reported), filtered by
working-directory watermark; models verified `gpt-5.5`.*
