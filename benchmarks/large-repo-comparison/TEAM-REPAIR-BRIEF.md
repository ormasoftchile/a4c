# A4C Team Mode — Repair Brief (large-repo benchmark failure)

**Date:** 2026-10-15 · **Evidence:** `results/2026-10-15-large-repo-run1/` · **Source task:** `testdata/task.txt`

## Symptom

On the large-repo task (Cypher WHERE property-to-property fix in `codebase-memory-mcp`), A4C Team mode completed with **only the lead activity**. No coder was spawned. No code was written. Worktree identical to baseline. Acceptance FAILED 1/5.

```
Executed 1 activities for task task-1792069670665567000:
  -> [task-1792069670665567000] Agent: lead       Status: completed    Consumed: 41508 tokens

Current State: awaiting_human_review
```

Lead's final summary (from `team-task-inspect.json`):

> **Blocked:** I inspected the relevant Cypher surfaces, but the **required delegation tools are failing**, so I can't hand this to the coder/reviewer agents or implement directly under the active Lead Architect role.
>
> **Handoff ready:** change `src/cypher/cypher.h` (`cbm_condition_t`) and `src/cypher/cypher.c` (`parse_condition_expr`, cleanup/free paths, `resolve_condition_value`/`eval_condition`)…

The lead produced a **correct handoff specification** but never successfully delegated. Budget was 2M tokens; only 41k consumed. This was **not** a budget/governance failure.

## Root cause (coordinator code)

In `pkg/a4c/team_coordinator.go`, the lead-completion path **always** marks the activity completed, even with zero proposals:

```go
if isLead {
    for idx, prop := range proposedActivities {
        // authorize + enqueue children...
    }
    _ = c.Store.Complete(req.ID, *result)  // ← runs even when len(proposedActivities)==0
}
```

There is **no guard** that treats "lead finished with zero delegated children" as a failure or escalation. The task therefore ends in `awaiting_human_review` with nothing implemented, while the lead's own text says delegation failed.

Secondary observation (not yet diagnosed): the lead claimed `governed_propose_activity` was failing. The tool body itself only records a proposal via callback and returns success — so either:
1. The model never actually invoked the tool (and hallucinated the failure), or
2. Tool invocation failed for another reason (`checkToolLimits`, Governor `AuthorizeToolInvocation`, session tool-registry mismatch), or
3. The tool was invoked and recorded, but `proposedActivities` was empty at Step time (callback not wired, or proposals cleared).

Diagnostics needed during repair:
- Confirm `OnProposeActivity` is wired for the lead session in team mode with copilot runtime.
- Log every `governed_propose_activity` invocation (success and error) to the activity outputs / store.
- Log `checkToolLimits` and `AuthorizeToolInvocation` failures for the lead agent.
- Confirm the lead's tool registry actually includes `governed_propose_activity` in this runtime path (`copilot_adapter.go` ~line 423, 678).

## Required fix (minimal, no redesign)

1. **Zero-proposal guard:** If a lead activity completes with `len(proposedActivities) == 0`, do **not** mark it `completed`. Mark it `failed` with `failure_reason: "no_delegation"` (or `awaiting_human` with an explicit decision request), including the lead's summary text in outputs so the handoff is not lost.

2. **Delegation-tool observability:** When `governed_propose_activity` is invoked, record in the activity result (or a durable log) whether the tool was called, with what payload, and whether Governor/limit checks passed. This makes "tools are failing" verifiable from artifacts.

3. **Tool-availability assertion (defensive):** Before starting a lead session in team mode, assert the session's allowed-tools set contains `governed_propose_activity`. If not, fail the activity immediately with a clear configuration error rather than letting the model burn turns discovering it.

4. **Do not** change budget floors, recovery, or governance semantics. This is a coordination-completion bug, not a budget bug.

5. **Do not** force the lead to implement (that would collapse team mode into direct mode). The lead must delegate; if it cannot, that must be an explicit failure, not a silent success.

## Verification (after repair)

Re-run **only** the A4C Team leg of the large-repo benchmark against the same pinned commit / task / model:

```bash
# from /Volumes/Projects/a4c
A4C_BENCH_ONLY=... # or use the shell harness:
bash benchmarks/large-repo-comparison/run_benchmark.sh $(date +%F)-large-repo-team-rerun
# then restrict analysis to the a4c-team artifacts, or extend the harness with A4C_BENCH_ONLY=a4c-team
```

**Pass criteria:**
- Lead spawns at least one coder child (activity count ≥ 2).
- Acceptance: 5/5 (same hidden queries as the original run).
- Full unit suite of the target repo still passes.
- If the lead again claims tool failure, the new observability fields show exactly which check failed.

Also re-run the existing deterministic suite to ensure no regression:
`go test -short ./pkg/a4c -run 'TestBudgetEconomics|TestTeam|TestCopilotAdapter_Budget|TestGovernance'`

## Reference numbers (this failed run)

| Metric | Value |
|---|---|
| Lead activities | 1 (completed) |
| Coder / reviewer | 0 / 0 |
| Lead billed | 41,508 tokens |
| AI credits | 41.15 |
| Wall | 59 s |
| Acceptance | FAIL 1/5 (property-to-literal only) |
| Parent budget | 2,000,000 (unused) |

Compare: A4C Direct on the same task **PASS 5/5** at 481 credits / 23 min.
