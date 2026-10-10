# A4C: Agents for Coding — Host-Native Personal Engineering Team

**A4C** turns your existing coding agent into a **persistent, coordinated personal software engineering team**.

Operating on the principle:
> **The host runs the agents. A4C makes them a persistent, coordinated personal software engineering team with Git worktree isolation and independent verification.**

A4C was inspired by **Squad** (the engineering team agent that operates inside GitHub Copilot). Instead of acting as a competing standalone conversational application, A4C operates natively inside **GitHub Copilot CLI**, **OpenCode**, and **Antigravity**.

---

## Architecture Overview

```text
Human Software Engineer
       │
       ▼
GitHub Copilot CLI / OpenCode / Antigravity
       │
       ▼
A4C Coordinator Agent (Alice — Lead Architect)
       ├──► Host-native Bob subagent (Coder)
       ├──► Host-native Charlie subagent (Reviewer — Read-Only)
       └──► Other host-native specialists
       │
       ▼
A4C Durable Coordination & State Layer (.a4c)
       ├──► team_context          (environment, roster, active tasks)
       ├──► team_begin_task       (isolated git worktree allocation)
       ├──► team_assign_activity  (specialist task delegation)
       ├──► team_checkpoint       (intermediate progress & snapshot persistence)
       ├──► team_complete_activity(evidence-bound completion)
       ├──► team_review_result    (non-mutating review verdict verification)
       ├──► team_accept           (explicit human snapshot sign-off)
       └──► team_integrate        (conflict-verified base repository merge)
       │
       ▼
Git Worktree Isolation & Host-Native Command/Test Execution
```

### Critical Architectural Invariants
1. **Zero Secondary Inference in Host-Native Mode**:
   Starting A4C inside Copilot or OpenCode does **NOT** cause A4C to initiate secondary SDK-managed model sessions. The hosting agent environment supplies reasoning, conversation, and native subagent dispatch.
2. **Non-Mutating Reviewer Invariant**:
   Charlie (Reviewer) is strictly read-only. A4C programmatically verifies git status before recording review verdicts; any reviewer modifications cause immediate rejection.
3. **Cross-Host Task Portability**:
   Engineering tasks, activity progress, checkpoints, and review states survive host restarts and transfer seamlessly across Copilot, OpenCode, and Antigravity.

---

## Dual-Mode Contract

| Property | Mode A: Host-Native (Default Interactive) | Mode B: Headless (Automated CI / Benchmarking) |
| :--- | :--- | :--- |
| **Primary Interface** | `copilot --agent a4c` / `opencode --agent a4c` | `a4c run` / `a4c host` / Go test runner |
| **Inference Source** | Host coding agent subscription & quotas | Direct Copilot SDK / OpenCode HTTP daemon |
| **Agent Execution** | Host native subagents (`task` tool) | Standalone Go `TeamCoordinator` loop |
| **Durable State** | A4C MCP Server (`a4c mcp`) & `.a4c/` | A4C Activity Store & Intent Journal |
| **Reviewer Gate** | Host permissions + A4C worktree diff validation | Policy Governor tool restrictions |

---

## Installation & Setup

Build the A4C CLI:
```bash
go build -o bin/a4c ./cmd/a4c
```

### 1. Initialize Workspace
```bash
./bin/a4c init .
```
Initializes `.a4c/` storage for activities, persistent team roster, organizational memory, and journals.

### 2. Install Host-Native Agent Definitions
Install for GitHub Copilot:
```bash
./bin/a4c install copilot
```
*Creates `.github/agents/a4c.agent.md`, `bob.agent.md`, `charlie.agent.md`, and `.mcp.json`.*

Install for OpenCode:
```bash
./bin/a4c install opencode
```
*Creates `.opencode/agents/a4c.md`, `bob.md`, `charlie.md`, and `opencode.json`.*

Install for all supported hosts:
```bash
./bin/a4c install all
```

### 3. Diagnose Environment
Verify your configuration across git, durable stores, Copilot CLI, OpenCode, and Antigravity:
```bash
./bin/a4c doctor
```

---

## Interactive Host Usage

### Running inside GitHub Copilot
Launch Copilot with the A4C agent:
```bash
copilot --agent a4c
```
Interact naturally:
> *"Alice, investigate the failing test in calculation_test.go. Delegate the fix to Bob, have Charlie review the change, and report the result."*

Alice will:
1. Call `team_context` to inspect current team state and active tasks.
2. Call `team_begin_task` to create an isolated worktree branch.
3. Call `team_assign_activity` and dispatch Bob via Copilot's native `task` tool.
4. Call `team_checkpoint` to record intermediate progress.
5. Call `team_complete_activity` when Bob finishes with passing test evidence.
6. Call `team_assign_activity` and dispatch Charlie via Copilot's native `task` tool for independent, read-only review.
7. Call `team_review_result` to record the approved verdict.
8. Report results directly to you in the Copilot chat.

### Running inside OpenCode
Launch OpenCode with the A4C primary agent:
```bash
opencode run --agent a4c "Investigate test failures, delegate to Bob, and have Charlie review."
```
OpenCode delegates to `@bob` and `@charlie` using its native subagent facility with strict permission boundaries (`edit: deny`, `write: deny` for Charlie).

---

## Cross-Host Task Continuity

A4C allows work started in one environment to be seamlessly continued in another:

1. **Start in Copilot**: Alice and Bob begin a complex refactoring in Copilot and record a checkpoint via `team_checkpoint`.
2. **Host Session Ends**: The developer exits Copilot.
3. **Resume in OpenCode**: The developer launches OpenCode (`opencode --agent a4c`). Alice reads `.a4c/` state via `team_context` and resumes the exact task ID, recognizing already-completed work without re-running earlier steps.
4. **Independent Review & Merge**: Charlie reviews the changes, the developer approves via `a4c task accept <task-id>`, and integrates via `a4c task integrate <task-id>`.

---

## CLI Management & Diagnostics

The CLI remains fully functional for inspection, auditing, and headless automation:

```bash
# Inspect task status, worktree, and lifecycle state
./bin/a4c task inspect <task-id> [--json]

# View review diffs before approval
./bin/a4c task diff <task-id>

# Submit human review feedback
./bin/a4c task feedback <task-id> "Ensure nil checks are present"

# Record explicit human approval of reviewed snapshot
./bin/a4c task accept <task-id> "Approved for merge"

# Integrate approved changes into base repository
./bin/a4c task integrate <task-id>

# Run MCP server manually
./bin/a4c mcp
```

---

## Verification & Test Suite

Run unit and integration tests:
```bash
go test -short ./...
```

Run cross-host continuity test:
```bash
go test -v -run TestCrossHostContinuity ./pkg/a4c/...
```

Run reviewer boundary enforcement test:
```bash
go test -v -run TestReviewerBoundaryEnforcement ./pkg/a4c/...
```

Run MCP host-native flow test:
```bash
go test -v -run TestMCPServer_HostNativeCoordinationFlow ./pkg/a4c/mcp/...
```
