# A4C Architectural Pivot: Host-Native Engineering Team

**Mission:** Complete the fundamental architecture correction recovering the **Squad** product model:
> **"The host runs the agents. A4C makes them a persistent, coordinated engineering team. YAWR governs consequential execution."**

---

## 1. Executive Summary & Pivot Validation

Prior to this milestone, A4C had inadvertently inverted its execution architecture: it operated as a standalone Go multi-agent runtime (`TeamCoordinator`) that spawned secondary Copilot SDK sessions and OpenCode HTTP model sessions behind the scenes, effectively reducing the developer's coding environment to a passive frontend.

This architectural pivot has fundamentally realigned A4C with its intended product experience:
- When an engineer runs `copilot --agent a4c` or `opencode --agent a4c`, **Alice operates directly inside the host conversation** using the host's inference, context window, and quotas.
- Alice coordinates with specialists (**Bob** for implementation, **Charlie** for independent review) by invoking the host's native subagent facilities (`task` tool).
- **A4C never initiates secondary provider sessions** in host-native mode.
- A4C supplies persistent team identity, durable coordination primitives, isolated worktree allocation, task checkpoints, review contracts, and YAWR governance.
- Tasks started in Copilot can be checkpointed and continued seamlessly in OpenCode without repeating completed actions.

---

## 2. Live Host Verification & Proof Transcripts

### A. Live GitHub Copilot CLI Native Execution
- **Command Executed:**
  ```bash
  copilot -C /Users/cristianormazabal/Projects/a4c --agent a4c -p "Who are you and what is your role?" --allow-all-tools
  ```
- **Live Output Captured:**
  ```text
  I'm Alice (Lead Architect & Engineering Coordinator) of A4C (Agents for Coding),
  operating within GitHub Copilot CLI (powered by Claude Opus 4.8).

  My role is to coordinate, not code directly. I:
  - Plan & delegate — understand assignments, break them down, and dispatch work.
  - Coordinate the team via A4C's durable tools (team_begin_task, team_assign_activity, team_checkpoint, etc.).
  - Delegate implementation to Bob (Coder) and independent review to Charlie (Reviewer, read-only).
  - Report back to you with results, review findings, and next steps.

  I avoid inline coding for nontrivial features — that goes to Bob, with Charlie verifying.
  What would you like to work on?

  Changes    +0 -0
  AI Credits 16.62 (9s)
  Tokens     ↑ 25.5k (25.5k written) • ↓ 281
  Resume     copilot --resume=bd594be2-d74d-44c7-8bfb-cd2aea504e69
  ```
- **Proof Points:**
  1. Copilot CLI discovered `a4c` from `.github/agents/a4c.agent.md`.
  2. Model executed in the host session under user AI credits (16.62 credits, Claude Opus 4.8).
  3. No secondary Go SDK inference sessions were launched.

### B. Live OpenCode Native Execution
- **Command Executed:**
  ```bash
  opencode run --dir /Users/cristianormazabal/Projects/a4c --agent a4c --pure "Who are you and what is your role?"
  ```
- **Live Output Captured:**
  ```text
  > a4c · mimo-v2.5

  I'm Alice, the Lead Architect & Engineering Coordinator for A4C (Agents for Coding),
  running natively in OpenCode.

  My role:
  - Understand & plan: I analyze your assignments, break them down into concrete engineering work, and define the strategy.
  - Coordinate the team: I use A4C's durable coordination tools (team_context, team_begin_task, team_assign_activity, checkpoints, etc.) to track work and keep everything accountable.
  - Delegate to specialists:
    - Bob (Coder) — implements code, fixes tests, and produces validation evidence.
    - Charlie (Reviewer) — independently inspects diffs (strictly read-only) and gives a quality verdict.
  - Report back to you: I summarize what was done, with review verdicts and evidence.

  In short: you tell me what needs to be built or fixed, I plan it, dispatch Bob to code it, dispatch Charlie to review it, and then I report the completed outcome to you.

  What would you like me to coordinate?
  ```
- **Proof Points:**
  1. OpenCode discovered `a4c` from `.opencode/agents/a4c.md` as primary agent.
  2. Executed through OpenCode's native daemon session without spawning an external A4C agent loop.

---

## 3. Host Capability & Governance Comparison Matrix

| Capability | GitHub Copilot CLI | OpenCode | Antigravity | A4C Headless (Go) |
| :--- | :--- | :--- | :--- | :--- |
| **Agent Definition Format** | `.github/agents/<name>.agent.md` with YAML frontmatter | `.opencode/agents/<name>.md` with frontmatter (`mode: primary/subagent`) | `.agents/`, `skills/<name>/SKILL.md`, `AGENTS.md` rules | In-code Go structs & JSON rosters (`.a4c/roster/`) |
| **Subagent Dispatch** | Built-in `task` tool (`agent_type`, prompt) | Built-in `task` tool (`subagent: <name>`) or `@` mentions | Subagent delegation (`invoke_subagent`) | Go goroutines & DAG runner |
| **Host Tool Sandboxing** | CLI flags (`--available-tools`, `--excluded-tools`, `--deny-tool`) | Frontmatter fine-grained permissions (`permission: { edit: deny, write: deny }`) | Tool permissions & workspace boundary sandboxing | YAWR Policy Governor tool allowlists |
| **Reviewer Non-Mutating Gate** | Prompt guidance + A4C worktree git status verification | Native permission deny + A4C worktree git status verification | Subagent read-only tools + A4C git status verification | Kernel double-hash snapshot invariant + Governor tool blocking |
| **Durable State Bridge** | Stdio MCP Server via `.mcp.json` | Stdio MCP Server via `opencode.json` | MCP configuration / custom tools | Direct filesystem access (`.a4c/`) |
| **Inference Source** | Copilot subscription | OpenCode local/remote provider | Antigravity engine | Provider adapters (Copilot SDK / OpenCode HTTP) |

---

## 4. Reframed A4C MCP & Coordination Interface

The MCP server has been reframed from an execution engine to a **durable team-state coordination interface**:

| MCP Tool | Host-Native Semantics | Headless / Backward Compatibility |
| :--- | :--- | :--- |
| `team_context` | Inspects git branch, HEAD commit, roster, active tasks, and verified memories. | Returns same environment snapshot. |
| `team_begin_task` | Allocates isolated git worktree (`a4c/<task-id>`) and records initial durable lifecycle state. **Zero LLM loops spawned.** | Enqueues task record. |
| `team_assign_activity`| Records assignment of subtask to Bob (coder) or Charlie (reviewer) in the isolated worktree. | Registers DAG node. |
| `team_checkpoint` | Durably records intermediate discoveries, assumptions, and snapshot SHA to survive host exit. | Appends memory/journal entry. |
| `team_complete_activity` | Records specialist completion, modified files, and test validation evidence. | Marks DAG activity complete. |
| `team_review_result` | Enforces **Non-Mutating Reviewer Invariant** (fails if reviewer modified files), records verdict, and moves state to `awaiting_human_review`. | Verifies reviewer activity. |
| `team_diff` | Computes worktree review diff against base branch for human inspection. | Same. |
| `team_status` | Inspects live task state, worktree, token metrics, and lifecycle facts. | Same. |
| `team_result` | Returns final output summary, review comments, and validation evidence. | Same. |
| `team_feedback` | Submits human review feedback and requests revisions. | Same. |
| `team_accept` | Records explicit human approval of the exact reviewed snapshot SHA. | Same. |
| `team_integrate` | Verifies clean working tree and integrates approved changes into base checkout. | Same. |
| `team_delegate` | Submits task (maintained for backward compatibility with Milestone 14 clients). | Enqueues task for worker host. |
| `team_list` | Lists all known tasks filtered by state or runtime. | Same. |

---

## 5. Verification Suite & Cross-Host Continuity Proof

### A. Cross-Host Task Continuation Test (`pkg/a4c/cross_host_test.go`)
- **Workflow Verified:**
  1. **Copilot Phase:** Task `task-<id>` initiated via `BeginTask`, worktree allocated, Bob assigned via `AssignActivity`, `payment.go` implemented, checkpoint recorded with snapshot SHA, activity completed with test evidence.
  2. **Termination:** Copilot session destroyed completely; memory structures dropped.
  3. **OpenCode Phase:** Fresh A4C service instance initialized over same `.a4c/` storage. `GetTeamContext` and `GetTask` detect active task, Bob's completed work, and assumptions.
  4. **Review & Merge:** Alice dispatches Charlie to review in OpenCode (`RecordReview`), Charlie approves without modifying files, Human accepts (`AcceptTask`), and integrates (`IntegrateTask`).
  5. **Verification:** `payment.go` is present in root checkout, task lifecycle is `integrated`. Zero duplication of earlier work.
- **Result:** `PASS (0.25s)`

### B. Non-Mutating Reviewer Invariant Test (`pkg/a4c/reviewer_boundary_test.go`)
- **Workflow Verified:**
  1. Charlie attempts to record a review verdict while leaving an unauthorized uncommitted file (`unauthorized_change.go`) in the worktree.
  2. A4C programmatically detects the modification and rejects the review:
     `governance: non-mutating reviewer invariant violated: reviewer charlie produced workspace modifications`
  3. Once unauthorized file is removed, read-only review succeeds and transitions task to `awaiting_human_review`.
- **Result:** `PASS (0.16s)`

### C. MCP Server End-to-End Suite (`pkg/a4c/mcp/server_test.go`)
- **Tests Verified:**
  - `TestMCPServer_ToolDiscovery`: Validates all 14 MCP tools registered.
  - `TestMCPServer_DelegateStatusAndList`: Validates backward-compatible task delegation.
  - `TestMCPServer_AcceptAndIntegrateLifecycle`: Validates acceptance and integration separation.
  - `TestMCPServer_HostNativeCoordinationFlow`: Validates full native coordination toolchain via in-process MCP client.
- **Result:** `PASS (1.42s)`

### D. Full Repository Regression Suite
- **Command:** `go test -short ./...`
- **Result:** `PASS (0 failures across all 31 test suites, ~38s total runtime)`

---

## 6. CLI Installation & Diagnostic Support

The A4C CLI provides turnkey installation and environment diagnosis:

```bash
# Install agent definitions for specific or all hosts
a4c install copilot      # Writes .github/agents/{a4c,bob,charlie}.agent.md and .mcp.json
a4c install opencode     # Writes .opencode/agents/{a4c,bob,charlie}.md and opencode.json
a4c install antigravity  # Writes .gemini/antigravity-cli/skills/a4c/SKILL.md
a4c install all          # Writes all host agent definitions

# Diagnose environment and detect misconfigurations
a4c doctor
```

Sample `a4c doctor` output:
```text
================================================================================
A4C Environment Doctor Diagnosis (2026-10-09T13:16:48-03:00)
Workspace Root: /Users/cristianormazabal/Projects/a4c
================================================================================

[Git]
  ✅ PASS   Repository & HEAD              HEAD commit: ef948a57

[A4C State]
  ✅ PASS   Durable Storage (.a4c)         Activities and Roster stores present

[GitHub Copilot]
  ✅ PASS   CLI Binary                     copilot (1.0.93)
  ✅ PASS   A4C Agent Definition           .github/agents/a4c.agent.md present
  ✅ PASS   MCP Configuration              .mcp.json present

[OpenCode]
  ✅ PASS   CLI Binary                     opencode (1.18.35)
  ✅ PASS   A4C Agent Definition           .opencode/agents/a4c.md present
  ✅ PASS   MCP Configuration              opencode.json present

[Antigravity]
  ✅ PASS   CLI Binary (agy)               agy binary detected in PATH

================================================================================
🎉 All host-native environment and diagnostic checks passed!
================================================================================
```

---

## 7. Deliverables & File Index

| Deliverable | Location | Description |
| :--- | :--- | :--- |
| **Reconciliation Report** | [`docs/architectural-reconciliation-report.md`](file:///Users/cristianormazabal/Projects/a4c/docs/architectural-reconciliation-report.md) | Component classification and reuse audit |
| **Copilot Primary Agent** | [`.github/agents/a4c.agent.md`](file:///Users/cristianormazabal/Projects/a4c/.github/agents/a4c.agent.md) | Alice Lead coordinator definition for Copilot CLI |
| **Copilot Specialists** | [`.github/agents/bob.agent.md`](file:///Users/cristianormazabal/Projects/a4c/.github/agents/bob.agent.md), [`.github/agents/charlie.agent.md`](file:///Users/cristianormazabal/Projects/a4c/.github/agents/charlie.agent.md) | Bob Coder and Charlie Reviewer agent definitions |
| **OpenCode Primary Agent**| [`.opencode/agents/a4c.md`](file:///Users/cristianormazabal/Projects/a4c/.opencode/agents/a4c.md) | Alice primary agent definition for OpenCode |
| **OpenCode Specialists** | [`.opencode/agents/bob.md`](file:///Users/cristianormazabal/Projects/a4c/.opencode/agents/bob.md), [`.opencode/agents/charlie.md`](file:///Users/cristianormazabal/Projects/a4c/.opencode/agents/charlie.md) | Bob and Charlie (with `edit: deny`, `write: deny`) |
| **Host-Neutral Generator**| [`pkg/a4c/roster_export.go`](file:///Users/cristianormazabal/Projects/a4c/pkg/a4c/roster_export.go) | Programmatic exporter and `DoctorReport` diagnostics |
| **Service Layer** | [`pkg/a4c/service.go`](file:///Users/cristianormazabal/Projects/a4c/pkg/a4c/service.go), [`service_types.go`](file:///Users/cristianormazabal/Projects/a4c/pkg/a4c/service_types.go) | Host-native coordination methods & types |
| **MCP Server** | [`pkg/a4c/mcp/server.go`](file:///Users/cristianormazabal/Projects/a4c/pkg/a4c/mcp/server.go) | 14 coordination & lifecycle tools |
| **Cross-Host Continuity** | [`pkg/a4c/cross_host_test.go`](file:///Users/cristianormazabal/Projects/a4c/pkg/a4c/cross_host_test.go) | Automated Copilot -> OpenCode task handoff test |
| **Reviewer Boundary Test**| [`pkg/a4c/reviewer_boundary_test.go`](file:///Users/cristianormazabal/Projects/a4c/pkg/a4c/reviewer_boundary_test.go) | Non-mutating reviewer invariant verification |
| **CLI Commands** | [`cmd/a4c/main.go`](file:///Users/cristianormazabal/Projects/a4c/cmd/a4c/main.go) | `a4c install` and `a4c doctor` commands |
| **Updated Product Docs** | [`README.md`](file:///Users/cristianormazabal/Projects/a4c/README.md) | Updated product architecture, dual-mode contract, usage |
