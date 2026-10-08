# A4C — Milestone 9 Technical Report: Governance Integrity, Budget Enforcement & Execution Observability

## Executive Summary

Milestone 9 conducted a rigorous root-cause investigation and remediation of the governance, budget enforcement, and observability defects revealed during Milestone 8. 

During Milestone 8, A4C successfully demonstrated live autonomous engineering (implementing `task inspect <task-id>` with multi-agent review and revision cycles), but exposed critical inconsistencies in its accounting and governance controls:
1. A root task allocated 100,000 tokens consumed 359,920 tokens according to the report (and 1,240,960 tokens in reality), yet completed without pausing or requesting human approval.
2. The diagnostic report displayed only a `REJECTED` review decision while declaring the task status `COMPLETED`.
3. In-flight and hierarchical budget reservations were not enforced at runtime.

Milestone 9 directly restored A4C's foundational architectural promise:
> **Agents may reason nondeterministically, but their execution authority, resource consumption, and recorded outcomes must remain governed, durable, and verifiable.**

Zero speculative product features or agent roles were added. All fixes address root architectural causes and are backed by comprehensive automated test coverage and durable verification.

---

## 1. Root Cause Investigations & Architectural Corrections

### 1.1 Root Cause 1: Task Tree Activity Dropping (`GetTaskTree`)
- **Observed Defect:** `a4c task inspect` and `a4c task history` for task `task-1791486047160298000` omitted 6 out of 9 activities. It hid 881,040 tokens from the operator and reported only an early `REJECTED` review, while the task claimed `COMPLETED`.
- **Root Cause:** In [`pkg/a4c/store.go`](../pkg/a4c/store.go), `GetTaskTree` and `QueryActivities` relied on a single-pass loop over `s.order`, which was populated via `os.ReadDir` (alphabetical order). Descendant subtask IDs created during revision cycles (such as `task-...-sub-2-rereview-1-...`) sorted alphabetically *before* their parent subtask (`task-...-sub-2`). Consequently, the single-pass parent-checking loop dropped these subtasks because their parent was not yet in the visited set.
- **Correction:** Replaced the single-pass scan with a transitive graph closure discovery algorithm (`getDescendantIDsLocked`) and stable topological sort by `CreatedAt`.
- **Verification:** Inspecting `task-1791486047160298000` immediately surfaces all 9 activities, reveals the true token consumption of **1,240,960 tokens**, and shows all 4 review decisions culminating in `APPROVED`.

### 1.2 Root Cause 2: Policy Governor State Leak & Unbounded Delegation
- **Observed Defect:** Child activities could spend tokens without checking parent limits, and revision subtasks completely bypassed `PolicyGovernor`.
- **Root Cause:** 
  1. `PolicyGovernor` never tracked `Budget.Consumed`, never deducted spent tokens from parent reservations, and never registered child budgets in `activeBudgets`.
  2. In `TeamCoordinator.Step`, when peer review was rejected, revision subtasks were directly enqueued to `c.Store.Enqueue`, completely bypassing `c.Governor.AuthorizeProposal`.
- **Correction:** 
  1. Implemented `SetBudget`, `CheckBudget`, and `RecordConsumption` in `PolicyGovernor`. Child budgets are registered upon proposal authorization.
  2. Implemented reservation settlement: when an activity completes, its unused allocated reservation is released back to the parent, while actual consumption is recorded against both child and parent.
  3. Governed revision cycles: revision proposals and re-review proposals are routed strictly through `c.Governor.AuthorizeProposal` against the root task budget. If remaining root budget is insufficient, the revision cycle halts and pauses with `StatusAwaitingHuman`.

### 1.3 Root Cause 3: Accounting Domain Divergence & In-Flight Enforcement
- **Observed Defect:** Individual agent sessions consumed 40k–200k tokens despite allocated subtask budgets of 7,000 or 3,500 tokens.
- **Root Cause:** 
  1. The GitHub Copilot multi-turn API re-sends the entire conversation context (~40k tokens) on every tool call roundtrip. A session making 5 tool calls accumulates `40k * 5 = 200k+` provider-billed prompt tokens.
  2. The Lead agent proposed unrealistic micro-budgets (7,000 tokens for Coder and 3,500 for Reviewer) without accounting for multi-turn prompt re-transmission.
  3. There was no in-flight enforcement inside tool callbacks to stop the session once the allocated limit was reached.
- **Correction:** 
  1. Separated accounting domains: Added `PeakContextTokens` to `UsageInfo` and `ExecutionMetrics` to distinguish instantaneous context window high-water mark from cumulative multi-turn billed tokens.
  2. In-flight tool budget checks: Added `CheckBudget()` gates inside every governed tool callback (`readFileTool`, `applyPatchTool`, `proposeActivityTool`, `submitReviewTool`, `requestHumanDecisionTool`, `proposeMemoryTool`, structural tools) and the RPC permission handler `permHandler`. If token consumption reaches allocation, tools fail closed with `ErrBudgetExceeded`.
  3. Session-level parity in `FakeSession.Send`: Enforced pre-flight and post-flight budget checks returning `ErrBudgetExceeded`.

### 1.4 Root Cause 4: Missing Hierarchical Enforcement Gates
- **Observed Defect:** `TeamCoordinator` allowed task execution to continue indefinitely even when root budget was severely overrun.
- **Root Cause:** The coordinator recorded consumption in durable store results but never checked tree-wide budget before claiming an activity or after activity execution.
- **Correction:** 
  1. **Pre-flight hierarchical check:** Before executing an activity, `Step` calculates total consumed tokens across the root task tree via `GetTaskTree`. If total consumed reaches or exceeds root allocation, execution immediately halts, marking the activity `StatusAwaitingHuman` and generating a durable `HumanDecisionRequest`.
  2. **Post-flight activity check:** If an activity finishes having consumed more than its allocated budget, it fails with `ErrBudgetExceeded` and records both the failure and the consumed tokens durably via `c.Store.FailWithResult`.
  3. **Revision cycle check:** Before authorizing revisions, if remaining root budget is exhausted, the coordinator escalates to the human engineer via `StatusAwaitingHuman`.

---

## 2. Automated Test Suite (`pkg/a4c/milestone9_test.go`)

A dedicated automated test suite was constructed and verified:

| Test Case | Objective | Result |
| :--- | :--- | :--- |
| `TestMilestone9_01_TaskTreeGraphTraversal` | Validates transitive graph closure in `GetTaskTree` and `QueryActivities` across out-of-order disk entries | **PASS** (0.00s) |
| `TestMilestone9_02_InFlightBudgetRejection` | Validates that tool execution fails with `ErrBudgetExceeded` when session tokens exceed allocation | **PASS** (0.00s) |
| `TestMilestone9_03_PostFlightBudgetRejection` | Validates that an activity overrunning allocation fails post-flight with `ErrBudgetExceeded` and records metrics | **PASS** (0.00s) |
| `TestMilestone9_04_HierarchicalBudgetExhaustionAndPause` | Validates that total tree consumption reaching root allocation triggers `StatusAwaitingHuman` pause | **PASS** (0.54s) |
| `TestMilestone9_05_RevisionCycleBudgetAuthorization` | Validates that review rejection cannot spawn ungoverned revisions when root budget is exhausted | **PASS** (0.70s) |
| `TestMilestone9_06_PeakContextAccounting` | Validates distinct separation of `PeakContextTokens` from cumulative `TotalTokens` | **PASS** (0.00s) |

### Regression Testing
The entire A4C test suite was executed:
```
ok  github.com/ormasoftchile/a4c/pkg/a4c  60.703s
```
Zero regressions across unit tests, spikes, structural benchmarks, and dogfooding suites.

---

## 3. Real Task Provenance Audit (`task-1791486047160298000`)

Following the fix to `GetTaskTree`, running `./bin/a4c task inspect task-1791486047160298000` revealed the complete, uncorrupted audit history of the Milestone 8 dogfooding execution:

```
Task: task-1791486047160298000
Objective: Implement the 'a4c task inspect <task-id>' command...
Status: completed
Execution strategy: team
Assigned agent: lead
Participating agents: coder, lead, reviewer
Tokens: 100000 allocated, 1240960 consumed
Duration: wall-clock 7m46.471s, model 7m46.471s

Activity Tree:
* task-1791486047160298000 [completed] agent=lead (42,677 tokens)
    - task-1791486047160298000-sub-1 [completed] agent=coder (218,721 tokens)
    - task-1791486047160298000-sub-2 [completed] agent=reviewer (98,522 tokens)
        - task-1791486047160298000-sub-2-rereview-1-revision-1 [completed] agent=coder (215,841 tokens)
        - task-1791486047160298000-sub-2-rereview-1 [completed] agent=reviewer (387,978 tokens)
        - task-1791486047160298000-sub-2-rereview-1-rereview-1-revision-1 [completed] agent=coder (192,741 tokens)
        - task-1791486047160298000-sub-2-rereview-1-rereview-1 [completed] agent=reviewer (0 tokens)
        - task-1791486047160298000-sub-2-rereview-1-rereview-1-revision-2 [completed] agent=coder (43,131 tokens)
        - task-1791486047160298000-sub-2-rereview-1-rereview-1-rereview-2 [completed] agent=reviewer (41,352 tokens)

--- Review Decisions ---
1. reviewer on task-1791486047160298000-sub-2: REJECTED (Missing native test evidence)
2. reviewer on task-1791486047160298000-sub-2-rereview-1: REJECTED (Unverified tests)
3. reviewer on task-1791486047160298000-sub-2-rereview-1-rereview-1: REJECTED (Unverified tests)
4. reviewer on task-1791486047160298000-sub-2-rereview-1-rereview-1-rereview-2: APPROVED
```

### Key Audit Findings:
1. **Actual Activity Count:** 9 activities executed, not 3.
2. **Actual Token Consumption:** 1,240,960 tokens consumed across 3 revision cycles.
3. **Review Decision Integrity:** The task status of `COMPLETED` was legitimate because the final review decision was `APPROVED` after 3 revision attempts. The earlier report had appeared inconsistent solely because `GetTaskTree` dropped the later activities.
4. **Enforcement Gap Resolved:** Under Milestone 9's new rules, this task would have automatically paused with `StatusAwaitingHuman` at Activity 4 when total consumption reached 100,000 tokens, requiring explicit human authorization to allocate additional budget for revisions.

---

## 4. Summary of Code Changes

1. **`pkg/a4c/store.go`**:
   - Implemented `getDescendantIDsLocked` using transitive closure discovery.
   - Fixed `GetTaskTree` and `QueryActivities` with stable topological sort by `CreatedAt`.
   - Added `FailWithResult` to preserve token metrics and output context on activity failures.
2. **`pkg/a4c/governor.go`**:
   - Implemented `SetBudget`, `CheckBudget`, and `RecordConsumption`.
   - Added reservation settlement on parent upon child completion.
   - Enforced budget verification in `AuthorizeProposal`.
3. **`pkg/a4c/types.go` & `pkg/a4c/provider.go`**:
   - Added `PeakContextTokens` to `ExecutionMetrics` and `UsageInfo`.
4. **`pkg/a4c/copilot_adapter.go`**:
   - Wired `CheckBudget` into all custom tools and RPC permission handler.
   - Return shared adapter instance to ensure in-flight usage events update the checked state.
5. **`pkg/a4c/fake_provider.go`**:
   - Added budget exhaustion enforcement in `FakeSession.Send`.
6. **`pkg/a4c/team_coordinator.go`**:
   - Registered root task budgets in `SubmitTaskWithStrategy`.
   - Implemented pre-flight hierarchical task tree budget checks pausing with `StatusAwaitingHuman`.
   - Implemented post-flight activity budget enforcement with `FailWithResult`.
   - Routed revision subtasks through `Governor.AuthorizeProposal` and halted exhausted revision cycles with `StatusAwaitingHuman`.
7. **`pkg/a4c/milestone9_test.go`**:
   - Added comprehensive 6-test automated verification suite.

---

## 5. Conclusion

Milestone 9 has successfully audited and resolved the governance integrity defects:
- Task tree graph discovery is verified complete and immune to disk ordering.
- Cumulative multi-turn token consumption is clearly distinguished from peak context window size.
- Runtime budget overruns can no longer proceed silently to completion.
- When an engineering task exhausts its authorized resources, the multi-agent team cleanly pauses, preserves durable evidence, and requests human decision authority.
