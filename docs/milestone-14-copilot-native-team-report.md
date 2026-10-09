# A4C Milestone 14: Copilot-Native Engineering Team, MCP Interface & Durable Task Lifecycle Report

## Executive Summary

Milestone 14 transforms A4C from a CLI-driven autonomous engineering system into a **persistent personal engineering team operated directly from GitHub Copilot**, while maintaining full CLI equivalence for testing, automation, and operational debugging.

Prior milestones established a durable multi-agent architecture: single-writer workspace ownership, hierarchical token budgets, crash recovery, independent review, isolated worktrees, and provider independence across GitHub Copilot and OpenCode. However, operating A4C required switching contexts to a terminal to invoke CLI commands.

Milestone 14 eliminates this friction by exposing A4C as a standard **Model Context Protocol (MCP)** server. A software engineer can now converse directly with their team inside GitHub Copilot:

> *"Alice, investigate the failing tests in this repository. Delegate the implementation to Bob, have Charlie review the changes, and report the result."*

All execution, policy governance, worktree isolation, and durable persistence guarantees established in Milestones 1–13 remain completely intact and active.

---

## Architectural Principles & User Corrections

In accordance with approved design specifications and critical corrections, Milestone 14 enforces the following architectural principles:

### Correction 1: Derived Execution State, Persisted Human Facts
- **Execution state is never duplicated.** The authoritative execution engine remains `DurableActivityStore` and `TeamCoordinator`.
- Technical execution states (`queued`, `running`, `awaiting_decision`, `failed`, `awaiting_human_review`) are **derived dynamically** from the underlying activity execution DAG.
- Only human-facing lifecycle facts that cannot be derived—review feedback, explicit acceptance, rejection reasons, and integration confirmations—are persisted in atomic files (`.a4c/lifecycle/<task-id>.json` via `HumanLifecycleRecord`).

### Correction 2: Strict Separation of Acceptance vs. Integration
- **`AcceptTask` records explicit human approval of an exact reviewed snapshot SHA.** It **never** touches or mutates the developer's working checkout.
- **`IntegrateTask` separately applies the approved changes** to the base repository working checkout. Integration enforces two mandatory fail-closed guards:
  1. *Snapshot Identity Guard (`ErrSnapshotMismatch`)*: Verifies that the task worktree commit has not changed since human acceptance was granted. Any post-acceptance commits immediately invalidate authorization.
  2. *Clean Working Tree Guard (`ErrWorkingTreeDirty`)*: Verifies that the developer's working checkout is clean (excluding internal `.a4c` state) before applying changes or merging branches.

### Correction 3: Production MCP Go SDK
- Built on the maintained and battle-tested Go MCP SDK: `github.com/mark3labs/mcp-go v1.2.0`.
- Supports both high-performance in-process testing via `client.NewInProcessClient` and robust OS `stdio` streaming via `server.ServeStdio`.

### Correction 4: Single Worker Ownership
- One durable worker host (`WorkerHost`) owns task execution and crash recovery using exclusive kernel file locks (`.a4c/workspace.lock`).
- CLI commands and MCP server requests act as lightweight clients of the same shared service (`TeamService`), reading and enqueueing work safely without acquiring the execution lock.

### Correction 5: YAWR Abstraction Boundary
- Preserves existing YAWR execution paths and policy governors; avoids premature abstraction refactoring while YAWR's ad-hoc tool API matures independently.

---

## Technical Architecture & Implementation

### 1. Shared Application Service Layer (`TeamService`)
Located in `pkg/a4c/service.go` and `pkg/a4c/service_types.go`, `TeamService` provides the single source of truth for all operational consumers:

```go
type TeamService interface {
    SubmitTask(ctx context.Context, req SubmitTaskRequest) (*TaskRef, error)
    GetTask(ctx context.Context, taskID string) (*TaskDetails, error)
    ListTasks(ctx context.Context, filter TaskFilter) ([]TaskSummary, error)
    GetTaskDiff(ctx context.Context, taskID string) (*DiffResult, error)
    SubmitFeedback(ctx context.Context, req FeedbackRequest) (*TaskRef, error)
    AcceptTask(ctx context.Context, req AcceptTaskRequest) (*AcceptResult, error)
    IntegrateTask(ctx context.Context, req IntegrateTaskRequest) (*IntegrateResult, error)
    RejectTask(ctx context.Context, req RejectTaskRequest) (*RejectResult, error)
    ResumeTask(ctx context.Context, taskID string) error
    ExecuteTask(ctx context.Context, taskID string, maxSteps int) ([]*ActivityResult, error)
}
```

### 2. The 8 Canonical MCP Tools (`pkg/a4c/mcp/server.go`)
The MCP server exposes 8 high-level tools designed for conversational workflows:

| MCP Tool | Description | Inputs | Output |
|---|---|---|---|
| `team_delegate` | Submit assignment to the engineering team | `objective`, `budget_tokens`, `strategy`, `runtime`, `model` | `TaskRef` (`task_id`, `state`, `created_at`) |
| `team_list` | Query active and historical tasks | `state`, `runtime`, `limit` | Array of `TaskSummary` |
| `team_status` | Inspect live audit trail, active agent, metrics | `task_id` | `TaskDetails` |
| `team_result` | Retrieve final outputs, review verdict, evidence | `task_id` | `ResultSummary` |
| `team_diff` | View isolated worktree git diffs before sign-off | `task_id` | `DiffResult` (files, +/-, patch) |
| `team_feedback` | Submit review critique and request revisions | `task_id`, `feedback`, `target_agent` | `TaskRef` (`revision_requested`) |
| `team_accept` | Record explicit approval of exact snapshot | `task_id`, `comments` | `AcceptResult` (checkout untouched) |
| `team_integrate` | Apply approved changes to working checkout | `task_id` | `IntegrateResult` (commit SHA, files modified) |

### 3. Durable Worker Host (`pkg/a4c/worker_host.go`)
`WorkerHost` manages the execution loop:
- Acquires `.a4c/workspace.lock` on startup; releases on exit.
- Runs crash recovery and intent reconciliation via `DurableIntentJournal.ReconcileUnfinished`.
- Bounded (`--steps N`) or continuous daemon (`--poll-ms 250`) execution modes.
- Traps OS `SIGINT` / `SIGTERM` signals for clean, zero-corruption shutdown.

---

## Verification & Conformance Evidence

All test suites pass 100% with zero regressions across the codebase.

### Test Matrix Summary

| Test Suite | Purpose | Status | Execution Time |
|---|---|---|---|
| `pkg/a4c/service_test.go` | Unit verification of `TeamService`, state derivation, acceptance immutability, integration guards | **PASS** | 1.49s |
| `pkg/a4c/worker_host_test.go` | Single-writer lock enforcement, bounded stepping, graceful shutdown | **PASS** | 1.02s |
| `pkg/a4c/mcp/server_test.go` | Tool registration, in-process MCP client dispatch, error schemas | **PASS** | 2.69s |
| `pkg/a4c/mcp_parity_test.go` | CLI/MCP cross-boundary equivalence, lock contention, stdio subprocess | **PASS** | 1.05s |
| `pkg/a4c/...` (Full Suite) | Regression tests across all prior milestones (M1–M13) | **PASS** | 39.38s |

### Key Empirical Findings Validated

1. **Acceptance Immutability**:
   `TestTeamService_AcceptTask_DoesNotModifyWorkingCheckout` verifies byte-for-byte that calling `AcceptTask` does not alter files in the working directory.
2. **Snapshot Identity Guard**:
   `TestTeamService_IntegrateTask_SuccessAndGuards` commits a change in the task worktree after acceptance; verifies that `IntegrateTask` rejects integration with `ErrSnapshotMismatch` until the worktree is reset to the accepted commit.
3. **Dirty Working Tree Guard**:
   Verifies that uncommitted edits in the developer's working tree cause `IntegrateTask` to fail closed with `ErrWorkingTreeDirty`.
4. **Lock Contention Under Concurrency**:
   `TestMilestone14_WorkerHost_ConcurrencyAndLocking` verifies that while a worker host holds the kernel lock, a second worker process is immediately rejected with `ErrWorkspaceLocked`, while concurrent task submissions to the durable store succeed cleanly.
5. **CLI / MCP Equivalence**:
   `TestMilestone14_CLI_MCP_Parity` submits a task via the `a4c` CLI binary, inspects it via an MCP client, marks completion, accepts via MCP, and integrates via CLI. The state remains completely synchronized across process boundaries.

---

## User Experience: Conversational vs. CLI Parity

A developer can use both interfaces interchangeably:

```
                  ┌───────────────────────────────┐
                  │    GitHub Copilot / Claude    │
                  └──────────────┬────────────────┘
                                 │ JSON-RPC (MCP)
                                 ▼
┌─────────────────────────┐  ┌────────────────────┐
│         CLI User        │  │     a4c mcp        │
│  (a4c task / a4c host)  │  │  (mark3labs/mcp-go)│
└────────────┬────────────┘  └─────────┬──────────┘
             │                         │
             └───────────┬─────────────┘
                         ▼
             ┌─────────────────────────┐
             │       TeamService       │
             └───────────┬─────────────┘
                         ▼
   ┌───────────────────────────────────────────────┐
   │             Durable Infrastructure            │
   │  .a4c/activities/     .a4c/lifecycle/         │
   │  .a4c/worktrees/      .a4c/workspace.lock     │
   └───────────────────────────────────────────────┘
```

---

## Conclusion & Production Readiness

Milestone 14 achieves complete **Copilot-native personal engineering team operations**:
- Developers can assign, inspect, review, critique, accept, and integrate complex engineering work directly inside GitHub Copilot chat.
- The CLI remains fully supported with identical ergonomics and authoritative durable persistence.
- Zero duplication of execution state machine.
- Total separation between review acceptance and checkout integration.
- A4C is ready for daily production dogfooding inside VS Code and GitHub Copilot.
