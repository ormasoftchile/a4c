# A4C Architectural Reconciliation Audit: Host-Native Engineering Team Pivot

## Executive Summary

This audit reconciles A4C's existing codebase against the **Host-Native Engineering Team** product architecture.

A4C was originally conceived after studying **Squad**—the engineering team agent that operates natively inside GitHub Copilot (`copilot --agent squad` / `.github/agents/squad.agent.md`). The intended developer experience has always been:

```bash
copilot --agent a4c
```

The human engineer interacts with Alice (Engineering Lead) inside their active coding agent environment. Alice coordinates with Bob (Coder) and Charlie (Reviewer) using the host's native subagent mechanisms, while A4C supplies persistent team identity, coordination semantics, durable memory, checkpoints, review contracts, and governance.

Over Milestones 1–14, A4C developed an inverted execution architecture where A4C ran as a standalone Go daemon that spawned secondary LLM sessions behind the scenes. This audit classifies every major component, isolates the inverted execution assumptions, and defines the dual-mode operational contract (Mode A: Host-Native vs. Mode B: Headless).

---

## Component Classification Matrix

| Component | File / Package | Classification | Analysis & Adaptation Strategy |
|---|---|---|---|
| **Durable Activity Store** | `pkg/a4c/store.go` | **Reuse Unchanged** | Authoritative task/activity DAG and persistence. Both host-native and headless modes enqueue, claim, checkpoint, and complete activities here. |
| **Durable Lifecycle Store** | `pkg/a4c/lifecycle_store.go` | **Reuse Unchanged** | Atomically stores non-derivable human decisions (revisions, acceptance, integration, rejection). Fully multi-process safe. |
| **Durable Intent Journal** | `pkg/a4c/journal.go` | **Reuse Unchanged** | Pre-mutation intent logging and idempotency ledger. Authoritative integration target for YAWR. |
| **Durable Memory Store** | `pkg/a4c/memory.go` | **Reuse Unchanged** | Verified organizational conventions and architectural decisions across sessions and hosts. |
| **Worktree Manager** | `pkg/a4c/worktree.go` | **Reuse Unchanged** | Isolated Git worktree lifecycle per task. Protects developer checkout; provides exact snapshot commit IDs for review. |
| **Task History & Audit** | `pkg/a4c/history.go` | **Reuse Unchanged** | Deterministic reconstruction of task DAG, timeline, token metrics, and validation evidence. |
| **Team Roster & Roles** | `pkg/a4c/roster.go` | **Adapt for Host-Native** | Host-neutral definition of Alice, Bob, Charlie charters and tools. Add generator for Copilot `.github/agents/` and OpenCode `.opencode/agents/`. |
| **Shared Team Service** | `pkg/a4c/service.go` | **Adapt for Host-Native** | Expand from headless orchestration into a host-native coordination service. Add native activity registration, checkpointing, and reporting APIs alongside headless execution. |
| **MCP Server & Tools** | `pkg/a4c/mcp/server.go` | **Adapt for Host-Native** | Reframe tool semantics from "run full autonomous LLM loop in Go" to "record and coordinate native agent operations" (`team_context`, `team_begin_task`, `team_checkpoint`, `team_complete_activity`, `team_diff`, `team_feedback`, `team_accept`, `team_integrate`). Preserve backwards compatibility. |
| **Worker Host** | `pkg/a4c/worker_host.go` | **Preserve for Headless Only** | Retain for explicit headless background processing (`a4c host` / `a4c run --daemon`). Must never be triggered silently by host-native interactions. |
| **Team Coordinator** | `pkg/a4c/team_coordinator.go` | **Preserve for Headless Only** | Retain Go-managed `Step`/`RunLoop` for deterministic testing, benchmarks, and headless runs. Host-native agents execute via the host's own LLM loop. |
| **Copilot SDK Adapter** | `pkg/a4c/copilot_provider.go` | **Preserve for Headless Only** | Retain for headless mode and automated integration tests. |
| **OpenCode HTTP Adapter** | `pkg/a4c/opencode_adapter.go` | **Preserve for Headless Only** | Retain for headless mode and automated conformance tests. |
| **Policy Governor** | `pkg/a4c/governor.go` | **Adapt for Host-Native** | Retain for headless mode; prepare as adapter for host tool capability contexts until YAWR SDK is embedded. Explicitly label host-native tool confinement limitations. |
| **A4C CLI** | `cmd/a4c/main.go` | **Adapt for Host-Native** | Add diagnostic and installation commands (`a4c install copilot`, `a4c install opencode`, `a4c doctor`) while retaining task inspect/history/acceptance. |

---

## Identification of Inverted Execution Assumptions

The following code locations currently assume that A4C itself owns the LLM execution loop:

1. **`TeamCoordinator.Step` (`team_coordinator.go:568`)**:
   ```go
   session, err := c.resolveProviderLocked(req).CreateSession(ctx, config)
   ...
   resp, err := session.Send(ctx, prompt)
   ```
   *Inversion*: Calls an external model provider SDK/HTTP endpoint to produce assistant messages instead of letting Copilot/OpenCode run the conversation.
   *Correction*: In Host-Native mode, the host (Copilot/OpenCode) sends prompts and receives model completions. A4C provides prompt context and records the resulting state through MCP tool calls.

2. **`TeamServiceImpl.SubmitTask` & `ExecuteTask` (`service.go:73, 560`)**:
   `ExecuteTask` calls `coordinator.Step(ctx)` to run the LLM.
   *Correction*: In Host-Native mode, submitting/beginning a task registers the assignment; the native agent executes it immediately in the host, or Alice calls `team_assign_activity` and spawns a native subagent.

3. **`WorkerHost.Run` (`worker_host.go:87`)**:
   Polls `c.coordinator.Step(ctx)` continuously.
   *Correction*: Clearly separate `WorkerHost` as an optional background daemon for Headless Mode (`a4c host`). Host-native operations do not require a running worker host.

4. **`MCP team_delegate` (`mcp/server.go:107`)**:
   Currently assumes `team_delegate` returns a queued task that a background Go worker will claim and execute via secondary SDK sessions.
   *Correction*: In Host-Native mode, `team_delegate` registers the assignment and returns the active coordination context so that Alice can immediately invoke native subagents or perform initial triage.

---

## Dual-Mode Execution Architecture

```
┌────────────────────────────────────────────────────────────────────────┐
│                        MODE A: HOST-NATIVE (PRIMARY)                   │
├────────────────────────────────────────────────────────────────────────┤
│ Host (Copilot / OpenCode) owns:                                        │
│   • Model Inference & Quotas (User's active subscription)              │
│   • Main Chat UI & Conversation Turn Loop                              │
│   • Subagent Spawning (e.g. task tool / native subagents)              │
│                                                                        │
│ A4C (MCP / CLI) owns:                                                  │
│   • Team Identity (Alice Lead, Bob Coder, Charlie Reviewer)            │
│   • Durable Tasks, Activities & DAG (.a4c/activities)                  │
│   • Memory & Checkpoints (.a4c/memory, .a4c/lifecycle)                 │
│   • Worktree Isolation (.a4c/worktrees)                                │
│   • Diff Inspection, Review Verdicts, Acceptance & Integration         │
└────────────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────────────┐
│                        MODE B: HEADLESS (OPTIONAL)                     │
├────────────────────────────────────────────────────────────────────────┤
│ Explicit CLI execution (a4c run, a4c host, automated test suites):     │
│   • A4C TeamCoordinator drives the loop                                │
│   • Uses Copilot SDK or OpenCode HTTP provider                         │
│   • Single-writer kernel lock (.a4c/workspace.lock)                     │
│   • Used for benchmarks, unattended CI, regression testing             │
└────────────────────────────────────────────────────────────────────────┘
```
