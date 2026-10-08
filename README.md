# a4c: Agents for Coding

**a4c** is a persistent, local-first team of AI programmers operating under the deterministic execution authority of **YAWR** and powered by **GitHub Copilot**.

The target user is **one individual software engineer managing their own group of persistent AI programmers**.

```
                           +-------------------------------------+
                           |        Human Software Engineer      |
                           +------------------+------------------+
                                              |
                                              | Task Submission & Decisions
                                              v
+---------------------------------------------------------------------------------------------------+
|                                  YAWR GOVERNANCE & EXECUTION RUNTIME                              |
|                                                                                                   |
|   +-----------------------+     +------------------------+     +------------------------------+   |
|   |  Policy Governor      |     |  Durable Activity DAG  |     |  Durable Organizational      |   |
|   |  - Role tool gates    |     |  - Dependency order    |     |    Memory Store              |   |
|   |  - Budget reservations|     |  - Idempotent claim    |     |  - Verifier-gated promotion  |   |
|   |  - Proposal dedup     |     |  - Human pause/resume  |     |  - Revision tracking         |   |
|   +-----------------------+     +------------------------+     +------------------------------+   |
|                                              |                                                    |
|                                              | Governed Tool Calls & Activities                   |
|                                              v                                                    |
|   +-------------------------------------------------------------------------------------------+   |
|   |                                  Deterministic Verifiers                                  |   |
|   |  - Pre/Post Workspace Double-Hash Invariant (WorkspaceSnapshotID)                         |   |
|   |  - Native Go Test Runner (go test ./... -> ExitCode: 0)                                   |   |
|   |  - Pre-Mutation Intent Journal & Crash Reconciliation                                     |   |
|   +-------------------------------------------------------------------------------------------+   |
+---------------------------------------------------------------------------------------------------+
                                              |
               +------------------------------+------------------------------+
               | (Independent Sessions)      | (Independent Sessions)       | (Independent Sessions)
               v                             v                              v
    +--------------------+        +--------------------+         +--------------------+
    |   Alice (Lead)     |        |    Bob (Coder)     |         | Charlie (Reviewer) |
    |  - Architecture    |        |  - Implementation  |         |  - Code Review     |
    |  - Decomposition   |        |  - Governed Patch  |         |  - Evidence Check  |
    |  - Delegation      |        |  - Native Repair   |         |  - Pass / Reject   |
    +--------------------+        +--------------------+         +--------------------+
               |                             |                              |
               +-----------------------------+------------------------------+
                                             |
                                             v
                           +-----------------------------------+
                           |  Official GitHub Copilot Go SDK   |
                           |  (Local Stdio CLI Connection)     |
                           +-----------------------------------+
```

---

## Core Principles

1. **YAWR is the Sole Execution Authority**:
   Agents own reasoning, hypothesis generation, and peer delegation. YAWR owns durable activity lifecycle, budgets, mutation authorization, and verifiable evidence. There is **no secondary LLM orchestrator**; scheduling is a deterministic Go engine.
2. **Persistent Team Personas, Not Rigid Pipelines**:
   The team consists of persistent personas (`lead`, `coder`, `reviewer`) with configurable charters, specialties, and role-restricted tools. Identities survive CLI restarts.
3. **Idempotent Collaborative Delegation**:
   Agents propose work for teammates (`governed_propose_activity`). YAWR authorizes proposals against parent budgets and deduplicates them across process interruptions and crash recovery.
4. **DAG Dependency Enforcement**:
   Subtasks dynamically declare dependencies (`DependsOn`). Downstream tasks (such as code review) are locked until upstream dependencies (code implementation and native tests) succeed.
5. **Zero Hallucinated Validation**:
   An agent stating "the code is fixed" is never accepted as evidence. Only native compiler and test exit codes (`0`) matching the exact workspace double-hash snapshot authorize transitions.
6. **Shared Organizational Memory with Verifier-Gated Promotion**:
   Model proposals for conventions or constraints are kept in a `proposed` holding state until verified by a test pass, reviewer approval, or human sign-off.
7. **Human Leadership & Durable Escalation**:
   The human engineer can assign work to the team, inspect open questions, approve/reject/reassign decisions, and resume execution.

---

## Team Roster

| Member | Role | Allowed Tools | Specialties |
| :--- | :--- | :--- | :--- |
| **Alice (`lead`)** | Lead Architect & Planner | `governed_read_file`, `governed_propose_activity`, `governed_request_human_decision`, `governed_propose_memory` | Architecture, planning, delegation, triage |
| **Bob (`coder`)** | Senior Software Engineer | `governed_read_file`, `governed_apply_patch`, `governed_propose_memory` | Implementation, refactoring, bugfix, Go |
| **Charlie (`reviewer`)** | Quality & Verification Reviewer | `governed_read_file`, `governed_submit_review` | Code review, security, correctness, verification |

*Non-coder agents (`lead`, `reviewer`) are strictly barred by the Policy Governor from invoking `governed_apply_patch`.*

---

## Installation & Build

```bash
cd /Users/cristianormazabal/Projects/a4c
go build -o bin/a4c ./cmd/a4c
```

---

## CLI Usage

### 1. Initialize Workspace
```bash
./bin/a4c init .
```
Creates `.a4c/` containing the persistent team roster, activity DAG store, organizational memory, and pre-mutation journal.

### 2. Inspect the Team
```bash
./bin/a4c team list
```

### 3. Submit a Feature Request
```bash
./bin/a4c task submit "Implement Multiply(a, b int) int in calc.go and verify tests pass" 50000
```

### 4. Run the Governed Team Loop
Run with simulated provider:
```bash
./bin/a4c run --steps 10
```
Or run with **real GitHub Copilot CLI** via the official Go SDK:
```bash
./bin/a4c run --real-copilot --steps 10
```

### 5. Inspect Workspace Status
```bash
./bin/a4c status
```

### 6. Review & Resolve Human Escalations
```bash
# List open questions from agents
./bin/a4c decisions list

# Resolve decision and resume execution
./bin/a4c decisions resolve dec-12345 "Use SQLite for local storage" --approve
```

### 7. Inspect Verified Organizational Memory
```bash
./bin/a4c memory list
```

---

## Testing & Verification

### Deterministic Unit Test Suite (25 Tests)
All 25 unit tests execute in ~1.5s with zero external API calls:
```bash
go test -v -run "TestSpike|TestExtended|TestTeam" ./pkg/a4c
```

Key test invariants verified:
- `TestTeam_01_PersistentIdentitiesAndRestarts`: Persistent team identities survive process exit and disk reload.
- `TestTeam_02_CollaborativeDelegationAndOrdering`: DAG dependency ordering (Lead -> Coder -> Reviewer).
- `TestTeam_03_DuplicateDelegationPreventionAfterCrash`: Re-running parent does not duplicate child activities or double-spend budget.
- `TestTeam_04_SharedOrganizationalMemory`: Model statements cannot be authoritative without verification; survives restarts.
- `TestTeam_05_IndependentReviewWithEvidence`: Reviewer requires valid native test evidence; fails closed if files tampered post-test.
- `TestTeam_06_HumanEscalationAndResumption`: Human pause/resolve cycle restores activity from `awaiting_human`.
- `TestTeam_07_ToolPrivilegeSeparation`: Governor blocks Lead/Reviewer from mutating code.

### Live GitHub Copilot Integration Suite
Requires local GitHub Copilot CLI (`gh auth status` active):
```bash
RUN_REAL_COPILOT=1 go test -v -run TestRealCopilot ./pkg/a4c
```
Tests verified against live Copilot:
- `TestRealCopilot_Connectivity`: Verifies stdio RPC connection and token streaming.
- `TestRealCopilot_GovernedPatch`: Live model inspects Go code and applies patch via `governed_apply_patch`.
- `TestRealCopilot_MultiAgentTeam`: Full live 2-agent collaboration (Lead delegates -> Coder implements -> native `go test` passes -> state survives restart).
