# Direct-Only Dependency Map (pre-implementation, read-only)

**Baseline:** `P:\Projects\a4c` `main` @ `850d3cd` (== Stage 3 frozen product baseline), clean.
**Scope:** classify every subsystem as Retain / Simplify / Delete / Historical.
**Status:** inventory only - no code changed.

## A. Strategy abstraction

| Item | Location | Class | Notes |
|---|---|---|---|
| `ExecutionStrategy`, `StrategyDirect/Reviewed/Team` | `types.go:58-64` | **Historical** | keep type for persisted records; no dispatch |
| `EffectiveStrategy`, `ParseExecutionStrategy`, `--strategy` flag | `cmd/a4c/main.go:668-899,1154-1198` | **Delete** | explicit failure for legacy `reviewed\|team` values |
| `a4c task mode` / `DefaultStrategy` | `main.go:1730-1790`, `workspace_config.go` | **Delete** | strategy selection no longer exists |
| `EscalateTask` (strategy escalation) | `service.go:~1236`, `types.go:391` | **Delete** | escalation = switching strategies |
| `SubmitTaskRequest.Strategy`, `--specialist` | `service_types.go:70`, `main.go` | **Simplify** | direct is implicit |
| Usage/charter text advertising strategies | `main.go:668-672`, charters | **Simplify** | |

## B. Reviewed execution / AI review

| Item | Location | Class |
|---|---|---|
| `RecordReview`, `HostReviewResult`, `hasApprovedReviewRecord`, `strategyRequiresReview`, `isReviewRecord`, `reviewedSnapshot` | `service.go` | **Delete** (snapshot helper merges into generic path) |
| `enforceMandatoryReview`, `isReviewerAgent`, `hasReviewerChild` | `team_coordinator.go` | **Delete** |
| `ReviewVerdict/ReviewResult/ReviewFinding`, review consts | `types.go:251-278` | **Historical** (records) / Delete (API) |
| MCP `team_review_result` | `mcp/server.go:227` | **Delete** (optional review reports ride existing evidence fields) |
| Reviewer charters `charlie.*`, coder charters `bob.*` | `.github/agents/`, `.opencode/agents/` | **Delete** |
| Review gating in accept/integrate/derive-task-state | `service.go`, `history.go` | **Delete**; human gates stay |
| Tests: `review_contract`, `review_evidence`, `reviewer_boundary`, `opencode_review_integrity`, `review_benchmark_validation` | `pkg/a4c/*_test.go` | **Delete** |

## C. Team orchestration / delegation / A4C-managed sessions

| Item | Location | Class |
|---|---|---|
| `SubmitTaskWithStrategy`, `Step`, `RunLoop`, proposal/child enqueue, `RecoverInterruptedActivities`, `ListPendingDecisions`/`ResolveDecision` | `team_coordinator.go` | **Delete** (coordinator loops) |
| `NewTeamService`/`TeamServiceImpl` orchestration branches (`ExecuteTask` loop, `AssignActivity`) | `service.go` | **Simplify** |
| Child-budget machinery: `PolicyGovernor.SetBudgetFloor/ReallocateChild/MinChildBudget` | `governor.go` | **Simplify** (root budget accounting retained) |
| Roster: `lead/coder/reviewer.json` export, role charters | `roster_export.go`, `team.go` | **Delete** (roster/roles) |
| `programmer_agent.go`, `worker_host.go`, `CoderHandoff`/`handoff.go` role path, `ActivityProposal`, `TeamMemberInfo`, `TeamContextResult`, `AssignActivityRequest` | various | **Delete/Simplify** |
| MCP `team_delegate`, `team_assign_activity` | `mcp/server.go:70,157` | **Delete** |
| MCP role gating `addAgentTool(forLead,forCoder,forReviewer)` | `mcp/server.go:53` | **Simplify** |
| CLI `a4c team`, `task worker`, `task loop`, `task decide`, `status team`, `decisions` | `main.go` | **Delete** |
| **Host session launchers (OPEN BOUNDARY)**: `provider.go`, `copilot_adapter.go`, `opencode_adapter.go`, `opencode_async.go`, `antigravity_adapter.go`, `worker_host.go`, `fake_provider.go`, `provider_failure.go`, `session_ref.go`, `task start --runtime`, `task run` | `pkg/a4c/*`, `main.go` | **Delete (proposed)** — A4C-managed agent sessions duplicate host orchestration; host-native flow (Stage 3) never used them. **Needs your confirmation** |
| Tests: `team_test`, `team_budget_economics`, `team_no_delegation`, `invisible_team`, `worker_host`, `benchmark`, `squad_comparison_benchmark`, `milestone12*`, `milestone13_full_team_live`, provider/live tests | `pkg/a4c/*_test.go` | **Delete** (orchestration-specific) |

## D. Shared governance — RETAIN unchanged (or trivially simplified)

Task identity/lifecycle (`service.go` submit/begin/list/status/result), `store.go`
(durable activities), `lifecycle_store.go`, `history.go` (execution history),
`intent_journal.go`, `memory.go`, `knowledge_dir.go`, `task_integrity.go`,
`task_resolve.go`, `worktree.go` (isolation), `workspace_snapshot.go`,
`workspace_lock*.go`, `verification.go` + `test_runner.go` (verification
evidence), `governed_file.go`, `tool_governance.go`, `capability.go`,
`host_contract.go`, evidence/audit paths, human acceptance
(`AcceptTask`/`IntegrateTask`/`RejectTask`/`SubmitFeedback`), MCP governance
tools (`team_begin_task`, `team_complete_activity`, `team_checkpoint`,
`team_apply_patch`, `team_resume`, `team_list/status/result/diff/feedback/context`),
host install/export/doctor (`ExportCopilotAgents`, `ExportOpenCodeAgents`,
`ExportAntigravityCustomization`, `RegisterAntigravityMCP`, `RunDoctor`).

**Invariants that must survive (Patch 1/2):** failed work never recorded
successful; terminal outcomes idempotent; incomplete work unaccept­able/
unintegrable; snapshot verification fails closed; store failures propagate;
integration requires human acceptance + validated snapshot.

## E. Historical compatibility (read-only)

Persisted `strategy` field (`history.go:41`, activity records), review
verdicts in stored records, `deriveTaskState`'s current `StrategyDirect`
special-case (`history.go:235`) — must keep reading old records **without
reinterpreting Team/Reviewed tasks as Direct**.

## F. Docs & examples

`README.md` (20 orchestration refs), `.agents/skills/a4c/SKILL.md`,
`docs/copilot-mcp-setup.md`, `docs/opencode-integration-contract.md` →
**Simplify**. `docs/milestone-*`, `architectural-reconciliation-report.md` →
**Historical** (keep as record; no rewrite).

## Open decisions (need your call)

1. **Host session launchers** (C, open boundary): delete `--runtime` provider
   path entirely (recommended per §2), or keep as a convenience launcher?
2. **MCP tool names**: keep `team_*` names (zero blast radius on charters/host
   configs) or rename to `a4c_*` with the old names as thin aliases?
3. **Headless `task run`/`loop` + fake provider**: delete with the coordinator
   loops (recommended - "do not build a replacement orchestration engine"),
   or keep a single-activity executor for CI?
