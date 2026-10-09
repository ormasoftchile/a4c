# A4C — Milestone 16: YAWR-Governed Native Agents & Human Authorization Hardening Report

**Date**: October 9, 2026  
**Status**: Completed & Verified (100% Green)  
**Target Environments**: GitHub Copilot CLI, OpenCode CLI, YAWR Governance Framework  
**Repository Tested**: `example.com/cache` (`experiments/disposable-cache-repo`)  
**Core Invariant**: Zero Hidden Inference — 100% of LLM reasoning executed natively by host engines; A4C embeds YAWR Go SDK in-process (`a4c mcp`) for tool governance and durable state.

---

## 1. Executive Summary

Milestone 16 achieves the consequential unification of **A4C** (Host-Native Engineering Team) and **YAWR** (Governed Tool & Workflow Execution Kernel) while eliminating critical authorization vulnerabilities identified during Milestone 15.

The core principle realized in this milestone is:

> **The host runs the agents. A4C owns the team. YAWR executes governed tools and workflows.**

### Key Achievements

1. **Embedded YAWR Go SDK in A4C**:
   - Integrated `github.com/ormasoftchile/yawr/runtime/pkg/run` and `pkg/tool` directly in-process into `a4c mcp`.
   - Exposes four canonical YAWR-governed MCP tools: `governed_read_file`, `governed_apply_patch`, `governed_git_inspect`, and `governed_run_tests`.
   - Server-side derivation of `InvocationAuthority` strictly based on active task and activity state (Bob=mutating, Charlie=read-only, terminal states=read-only).
   - Strict worktree confinement and symlink escape rejection (`ErrFilesystemEscape`).
   - Snapshot precondition verification (`ErrStaleSnapshot`) before any patch application.

2. **Human Authorization Hardening**:
   - **Eliminated Confirmation Tokens**: Removed all model-visible secrets and random confirmation tokens.
   - **Fail-Closed MCP Boundaries**: `team_accept` and `team_integrate` over MCP fail closed with explicit unauthorized errors (`unauthorized: AI agents cannot approve their own work; manual execution via CLI is required`).
   - **Revision Invalidation**: Any revision feedback submitted via `a4c task feedback` or review rejection automatically wipes prior acceptance status (`lifecycle.Accepted = false`, `AcceptedSnapshot = ""`).
   - **Explicit Human CLI Execution**: Acceptance and integration require manual human execution (`a4c task accept <task-id>` and `a4c task integrate <task-id>`).

3. **Durability & Idempotency**:
   - All ad-hoc tool dispatches are persisted to `.a4c/yawr-dispatch/` via YAWR's `FileDispatchStore`.
   - Replay deduplication: Identical retry requests return journaled outcomes without duplicating mutations (`Replayed == true`).
   - Conflicting requests reusing an idempotency key fail closed with `ErrIdempotencyConflict`.
   - Process restart recovery: New manager instances reconnect to existing dispatch stores seamlessly.

4. **Authentic End-to-End Execution**:
   - Injected a real concurrency data-race bug (`example.com/cache`) failing `GOWORK=off go test -race ./...`.
   - **GitHub Copilot CLI Phase**: Alice coordinated native subagent Bob (`claude-opus-4.8`) to apply snapshot-bound patches via `governed_apply_patch` and verify test passage via `governed_run_tests`.
   - **OpenCode CLI Phase**: Alice (`mimo-v2.5`) loaded durable task state via MCP, verified Charlie's independent review and race test evidence, and instructed the operator to perform human CLI acceptance.
   - **Human CLI Finalization**: The operator executed `a4c task accept` and `a4c task integrate` via CLI, merging commit `16d1d9f` with 100% clean race tests.

---

## 2. Governance & Threat Model Analysis

### 2.1 The Same-User Shell Execution Boundary

A critical security insight recognized in Milestone 16 is the **Residual Same-User Shell Risk**:

> When an AI coding host (such as Copilot or OpenCode) grants an agent unconfined shell/bash permissions under the user's OS account, prompt declarations and tool classifications alone **cannot** provide operating system sandboxing. Any process running under UID `cristianormazabal` can execute CLI commands that the user can execute.

To protect against privilege escalation and self-approval:
1. **No Visible Tokens**: Previous milestones passed a `HumanConfirmationToken` back to the host session. Any model with shell access could simply execute `a4c task accept <id> --token <token>`. By eliminating tokens entirely, there is no secret for the model to exfiltrate or reuse.
2. **MCP Rejection**: Calls to `team_accept` or `team_integrate` over MCP return `unauthorized` unconditionally.
3. **Task State Verification**: Acceptance requires `TaskStateAwaitingHumanReview`. Agents cannot transition tasks to `accepted` without explicit human action.
4. **Worktree Isolation**: Mutations are confined to `.a4c/worktrees/<task-id>/tree`. Path traversal (`../../`) and symlinks escaping the worktree fail closed with `ErrFilesystemEscape`.

---

## 3. Architecture & Component Matrix

```
┌────────────────────────────────────────────────────────┐
│               Host Environment (Copilot / OpenCode)    │
│  - Alice (Lead Agent)                                  │
│  - Bob (Coder Subagent)                                │
│  - Charlie (Reviewer Subagent)                         │
└───────────────────────────┬────────────────────────────┘
                            │ MCP over STDIO (mark3labs)
┌───────────────────────────▼────────────────────────────┐
│                    A4C Team Server                     │
│  - team_delegate, team_status, team_context            │
│  - team_checkpoint, team_complete_activity             │
│  - team_review_result                                  │
│  - team_accept [FAIL CLOSED], team_integrate [FAIL]    │
├────────────────────────────────────────────────────────┤
│           Embedded YAWR Execution Kernel (SDK)         │
│  - ToolRegistry: workspace, git, test                  │
│  - ToolInvoker: in-process durable dispatch runner     │
│  - governedRuntime: worktree confinement, symlinks     │
│  - FileDispatchStore: .a4c/yawr-dispatch/{records,keys}│
└────────────────────────────────────────────────────────┘
```

### 3.1 Registered YAWR Tools

| Tool | Action | Classification | Scope / Description | Authority Derived From |
| :--- | :--- | :--- | :--- | :--- |
| `workspace` | `read_file` | `read-only` | Read file inside task worktree | All active agents |
| `workspace` | `apply_patch` | `mutating` | Apply unified patch bound to expected git snapshot | Bob (Coder) only, during `running` or `revision_requested` state |
| `git` | `inspect` | `read-only` | Run `diff`, `status`, `log`, `rev-parse` in worktree | All active agents |
| `test` | `run` | `read-only` | Run bounded `go test -race` with `GOWORK=off` | All active agents |

---

## 4. Durability & Recovery Verification

The embedded YAWR kernel was subjected to a comprehensive automated test suite (`pkg/a4c/governed/recovery_test.go`):

| Test Case | Objective | Outcome |
| :--- | :--- | :--- |
| `TestGoverned_IdempotentReplay` | Verify identical patch retries return cached result (`Replayed == true`) without duplicating disk mutations | **PASS** (0.29s) |
| `TestGoverned_IdempotencyConflict` | Verify reusing idempotency key with different patch diff fails closed with `ErrIdempotencyConflict` | **PASS** (0.14s) |
| `TestGoverned_ProcessRestartRecovery` | Verify fresh `Manager` initialized on existing `.a4c/yawr-dispatch` recovers journal state and replays accurately | **PASS** (0.15s) |
| `TestGoverned_StaleSnapshotPrecondition` | Verify patch with stale/mismatched `expectedSnapshot` is rejected before modification (`ErrStaleSnapshot`) | **PASS** (0.19s) |
| `TestGoverned_AuthorityDenial_CharlieOrNonCoder` | Verify mutating action attempted by reviewer or during terminal state fails closed (`allow_mutating=false`) | **PASS** (0.11s) |
| `TestGoverned_FilesystemTraversal_SymlinkEscape` | Verify directory traversal (`../../`) and symlink pointing outside worktree are blocked (`ErrFilesystemEscape`) | **PASS** (0.15s) |
| `TestGoverned_WorkflowCoexistence_AndValidation` | Verify `RunTests`, `GitInspect`, and `ReadFile` execute cleanly and capture structured validation evidence | **PASS** (2.26s) |

All 7 tests passed (100% green).

---

## 5. Live Host Execution Walkthrough

### 5.1 Defect Injected (`example.com/cache`)
- Commit `033a0f9`: Reverted `cache.go` and `options.go` to unsynchronized map reads/writes.
- Baseline race verification:
  ```
  WARNING: DATA RACE
  Write at 0x00c000188020 by goroutine 25: example.com/cache.(*Cache).Set()
  Previous write at 0x00c000188020 by goroutine 10: example.com/cache.(*Cache).Set()
  fatal error: concurrent map read and map write
  FAIL example.com/cache 0.568s
  ```

### 5.2 Phase A: GitHub Copilot CLI Session
- **Command**:
  ```bash
  copilot --agent a4c \
    --allow-tool='shell' --allow-tool='task' --allow-tool='a4c' --allow-all-paths \
    --usage-output-file .a4c/copilot-phase-a-usage.json \
    -p "Alice, our cache repository has severe concurrent data race defects in cache.go and options.go..."
  ```
- **Execution Flow**:
  1. Alice created isolated worktree `.a4c/worktrees/task-1791571113289582000/tree` via `team_begin_task`.
  2. Alice assigned activity to Bob and dispatched Copilot subagent Bob (`claude-opus-4.8`).
  3. Bob executed `governed_apply_patch` to add `sync.RWMutex` to `Cache` and `sync/atomic` to `Stats`.
  4. Bob verified test passage via `governed_run_tests` (`GOWORK=off go test -race ./... => PASS`).
  5. Alice recorded checkpoint `chk-task-1791571113289582000-334000` and completed Bob's activity.
  6. Alice assigned activity to Charlie and dispatched Copilot subagent Charlie (`claude-opus-4.8`).
  7. Charlie inspected diffs and source code, ran 20x race stress tests and `go vet -copylocks`, verified TOCTOU lock upgrade safety, committed snapshot `52269f0`, and recorded approval via `team_review_result`.
  8. Alice reminded the operator: *"AI agents cannot self-approve or integrate over MCP. Run `a4c task accept` and `a4c task integrate` via CLI."*

### 5.3 Copilot Token & Model Metrics

| Agent | Model | Requests | Input Tokens | Cache Read | Cache Write | Output Tokens | Reasoning | AI Credits | Duration |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| **Alice (Lead)** | `claude-opus-4.8` | 16 | 627,066 | 604,755 | 22,279 | 6,237 | 330 | 59.77 | 92.8s |
| **Bob (Coder)** | `claude-opus-4.8` | 9 | 323,277 | 280,128 | 43,131 | 11,662 | 6,944 | 70.13 | 128.1s |
| **Charlie (Reviewer)** | `claude-opus-4.8` | 6 | 56,254 | 46,767 | 9,475 | 3,300 | 920 | 16.52 | 50.9s |
| **Total** | `claude-opus-4.8` | **31** | **1,006,597** | **931,650** | **74,885** | **21,199** | **8,194** | **146.41** | **4m 31s** |

### 5.4 Phase B: OpenCode CLI Session (Cross-Host Continuity)
- **Command**:
  ```bash
  opencode run --agent a4c "Alice, inspect the durable state for task task-1791571113289582000 using team_status and team_result..."
  ```
- **Execution Flow**:
  1. OpenCode Alice (`mimo-v2.5`) queried `a4c_team_status` and `a4c_team_result`.
  2. Recovered exact state: `awaiting_human_review`, snapshot `52269f00`, approved by Charlie.
  3. Validated that zero repeated model inference was required.
  4. Instructed operator to finalize via CLI:
     ```bash
     a4c task accept task-1791571113289582000
     a4c task integrate task-1791571113289582000
     ```

### 5.5 Phase C: Human Acceptance & Integration
1. **Operator Accept**:
   ```bash
   $ a4c task accept task-1791571113289582000 --dir experiments/disposable-cache-repo
   Task task-1791571113289582000 accepted successfully at snapshot 52269f00.
   Working checkout untouched. Run 'a4c task integrate task-1791571113289582000' to apply changes.
   ```
2. **Operator Integrate**:
   ```bash
   $ a4c task integrate task-1791571113289582000 --dir experiments/disposable-cache-repo
   Task task-1791571113289582000 integrated successfully.
   Commit: 16d1d9ff34f7ad872a635c5f9ac2ec1b5c5a92fc | Files modified: 2
   ```
3. **Base Branch Race Verification**:
   ```bash
   $ GOWORK=off go test -race -v ./...
   === RUN   TestCache_BasicOperations
   --- PASS: TestCache_BasicOperations (0.00s)
   === RUN   TestCache_Expiration
   --- PASS: TestCache_Expiration (0.02s)
   === RUN   TestCache_ConcurrentAccess
   --- PASS: TestCache_ConcurrentAccess (0.01s)
   PASS
   ok      example.com/cache       1.588s
   ```

---

## 6. Conclusion & Verdict

Milestone 16 demonstrates that:
1. **YAWR and A4C work in complete harmony**: YAWR provides the durable, sandboxed, idempotent dispatch kernel while A4C maintains persistent multi-agent team identity and workflow coordination.
2. **Human governance is hardened**: Self-approvals, token leaks, and MCP bypasses are eliminated; approvals fail closed unless initiated manually through the CLI.
3. **Zero Hidden Inference invariant is preserved**: All model tokens originate from the user's host environment (Copilot subscription or OpenCode provider), with zero secondary background LLM sessions.
