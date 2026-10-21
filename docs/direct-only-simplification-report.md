# A4C Direct-Only Simplification - Deliverables (2026-10-21)

**Baseline:** `850d3cd` (Stage 3 frozen product). **Commit:** see `H` below.
**Scope:** retire `StrategyReviewed` and `StrategyTeam`; make Direct implicit and
the only execution model; delete A4C-owned delegation, coordination, reviewer
orchestration, rosters, and host-session launchers; preserve the deterministic
lifecycle, evidence, worktree isolation, human acceptance, safe integration,
and historical record readability.

---

## A. Dependency and deletion inventory

The full per-subsystem inventory (original responsibility, retain/simplify/
delete/legacy decision, reason) is `docs/direct-only-dependency-map.md`,
produced before implementation. Actual actions taken:

| Subsystem | Decision | Action taken |
|---|---|---|
| `ExecutionStrategy` + consts | Legacy | Moved to `legacy_types.go`, documented as record data; no dispatch |
| `EffectiveStrategy`/`ParseExecutionStrategy`/`a4c task mode`/`DefaultStrategy` (`workspace_config.go`) | Delete | Removed entirely; `--strategy reviewed\|team` fails explicitly (`rejectRetiredStrategy`) |
| `EscalateTask`, `EscalateTaskRequest` | Delete | Removed from service, types, CLI |
| `team_coordinator.go` (Step/RunLoop/proposals/enforceMandatoryReview/review bundles/...) | Delete | File removed; shared `LocalEffectReconciler` kept in `reconcile.go` |
| Provider/launcher layer (`provider.go`, copilot/opencode/antigravity adapters, `opencode_async.go`, `fake_provider.go`, `provider_failure.go`, `worker_host.go`, `programmer_agent.go`, `session_ref.go`) | Delete | Files removed; `task start --runtime`, `task run`, `a4c host` daemon gone |
| `handoff.go` (`CoderHandoff`, `HandoffBuilder`) | Delete | Removed; `BuildContinuationPrompt`/`CheckpointFromInput` kept in `checkpoint_prompt.go` (recovery evidence) |
| `team.go` (roster), `roster_export.go` roster/roles | Delete | `TeamRoster`, `AgentIdentity`, role charters (bob/charlie) removed; install/export/doctor kept (`probeHosts`) |
| Reviewer orchestration (`RecordReview`, `hasApprovedReviewRecord` (reactivated as historical reader), `strategyRequiresReview`, `reviewedSnapshot`, `coderCompletionSnapshot`, MCP `team_review_result`) | Delete | Removed; review gating only for LEGACY reviewed records (see F) |
| MCP `team_delegate`, `team_assign_activity`, `team_review_result` | Delete | Tools unregistered and unadvertised; calls fail closed |
| MCP role gating (`NewServerWithRole`, `addAgentTool(forLead,...)`) | Delete | One tool surface (`NewServer`), 11 tools |
| CLI `a4c team`, `status team/decisions`, `task run/escalate/decide/worker/loop/mode`, `--reviewer-*`, `--agent-runtimes`, `--specialist`, `--runtime/--model` launch flags | Delete | Subcommands return explicit retirement errors; usage strings rewritten |
| `PolicyGovernor` child budgets (`SetBudgetFloor`, `MinChildBudget`, `ParentReserve`, `ReallocateChild`, `AuthorizeProposal`) | Delete | Removed; root budget accounting + tool authorization kept |
| Types `AssignActivityRequest`, `ActivityRef`, `TeamMemberInfo`, `RecordReviewRequest`, `HostReviewResult`, `UsageInfo` (launcher), `ReviewVerdict/Result/Finding` | Delete | Removed (except `UsageInfo`/`ProviderFailureKind`, kept as legacy record data) |
| `TestRunner`/`ProjectTestRunner`, `verification.go`, `AnalyzeGoTestOutput`, `WriteDurableTestLog` | Retain | Shared verification evidence (moved into `verification.go`) |
| Lifecycle gates (`ValidateTaskGraph`, `snapshotStrict`, accept/integrate/reject, `deriveTaskState`), stores, journal, memory, knowledge, worktree, locks, `governed_file`, `tool_governance`, `capability`, `host_contract` (data), audit | Retain | Unchanged except review clause scoped to legacy records |
| History (`history.go`) | Historical | Unchanged; strategy/review fields read-only; empty strategy reports as direct-by-construction (not a reinterpretation of legacy values) |
| Orchestration tests (28 files: team/review/role/provider/milestone-era) | Delete | Removed; shared regressions adapted; new `direct_only_test.go` covers removed-strategy + historical scenarios |

## B. Final architecture

```
   Copilot / OpenCode / Antigravity
            |  native coding execution (optional native subagents - opaque to A4C)
            v
   +-------------------------+
   |          A4C            |  task lifecycle, isolated worktree, durable
   |                         |  evidence, execution history, verification
   |                         |  results, human acceptance, snapshot
   |                         |  validation, safe integration
   +-------------------------+
            v
   Verified repository
```

Task lifecycle: `created -> worktree -> host executes -> result recorded
(failed/interrupted stops here, honestly) -> verification evidence -> human
acceptance -> snapshot validation -> safe integration`.

Ownership: the host owns execution and subagents; A4C owns identity, isolation,
records, evidence, acceptance, integration authorization, auditability. One
A4C task = one governed engineering outcome; a single root activity per task.

## C. Implementation summary

Production: `team_coordinator.go`, `provider.go`, `copilot_adapter.go`,
`opencode_adapter.go`, `opencode_async.go`, `antigravity_adapter.go`,
`fake_provider.go`, `provider_failure.go`, `worker_host.go`,
`programmer_agent.go`, `session_ref.go`, `handoff.go`, `team.go`,
`test_runner.go`, `workspace_config.go` **deleted**; `reconcile.go` and
`checkpoint_prompt.go` **added** (shared reconciler; checkpoint/continuation);
`service.go`, `service_types.go`, `types.go`, `governor.go`, `history.go`,
`host_contract.go`, `verification.go`, `governed_file.go`, `roster_export.go`,
`mcp/server.go`, `cmd/a4c/main.go` **simplified**. Chartres regenerated
(`.github/agents/a4c*.agent.md`, `.opencode/agents/a4c*.md`,
`.agents/skills/a4c/SKILL.md`); role charters (bob/charlie) removed.

Compatibility decisions:
- `team_*` MCP names kept as the smallest compatible surface (no rename).
- `--strategy direct` accepted; `reviewed|team` fail explicitly with a
  retirement message (CLI and MCP).
- `task start` now creates the task + worktree and prints the human handoff
  (it no longer launches a host session).
- Historical records: `strategy`, `agent_id`, review outputs, proposals, and
  interruption fields are legacy data, read-only and never rewritten.

## D. Regression validation

```
go build ./...                      PASS
go vet ./...                        PASS
go test -short ./...                PASS (cmd, cmd/a4c, pkg/a4c, pkg/a4c/knowledge, pkg/a4c/mcp)
go test -race -short ./pkg/a4c/...  PASS
```

Deterministic and offline; no paid model calls; no live-host integration tests
run (they exist only as historical files and were not executed - reported as
environmental scope, not hidden skips).

Covered scenarios (§13): task lifecycle (create without strategy, isolated
worktree, root completion success/failure, resume, terminal idempotency,
incomplete rejection); governance (snapshot mismatch, verification not
hideable, human acceptance required, integration on independently validated
state, stale-snapshot refusal, concurrency without false success);
removed strategies (Team/Reviewed submissions rejected, no delegation or
reviewer scheduling surface, no reachable coordinator, no retired capability in
help/charters); historical compatibility (Team/Reviewed records inspectable,
integrated outcomes retained, legacy graphs unmodified, legacy mutations fail
explicitly, legacy reviewed contract still enforced); host integration
(charters match tool surfaces, Antigravity skill + `agy mcp add a4c a4c mcp`
registration kept, Copilot `bash`+`powershell` whitelist kept (P3.2), native
subagents unrestricted, no retired strategy in generated instructions).

## E. Compatibility assessment

| Surface | Change |
|---|---|
| Existing Direct clients | Compatible: `task submit/start`, MCP `team_begin_task/complete_activity/...` unchanged in name and meaning |
| CLI | `run/loop/worker/decide/escalate/mode` and `a4c team/host` retired (explicit errors); usage/help rewritten; `--strategy reviewed\|team` rejected explicitly |
| MCP contracts | 11 tools remain; 3 orchestration tools removed (fail closed); `team_context` no longer returns `team_members`; `team_begin_task` no longer accepts `coordinator`/`runtime`/`strategy>direct` |
| Host installations | Regenerated charters; `a4c install`/`init` behavior preserved (Antigravity skill + MCP registration; Copilot shell-tool whitelists) |
| Durable task records | Forward/backward readable; no migration; no rewriting |
| Historical Team/Reviewed tasks | Inspectable; legacy reviewed contract enforced; cannot be created anew |
| Human acceptance | Unchanged and authoritative |
| Integration | Unchanged gates (graph terminality, snapshot validation, clean base); worktree deliverable lands on integration |

## F. Complexity reduction report

| Measure | Before | After | Delta |
|---|---:|---:|---:|
| Production Go files | 45 | 33 | **-12** |
| Production lines | 17,921 | 9,491 | **-8,430 (-47%)** |
| Test files | 75 | 33 | **-42** |
| Test lines | 18,625 | 5,493 | **-13,132 (-71%)** |
| Total Go lines | 36,546 | 14,984 | **-21,562 (-59%)** |
| Public CLI commands | 10 root/sub families incl. team, host, decisions, decide, mode | 5 (`init`, `install`, `task`, `mcp`, `status`) | **-5** |
| Task subcommands | 19 | 13 | **-6** |
| MCP tools | 14 | 11 | **-3** (`team_delegate`, `team_assign_activity`, `team_review_result`) |
| Service interface methods | 24 | 17 | **-7** (Escalate, Execute, Assign, RecordReview, DiscoverAlternativeHosts, Coordinator, ...) |
| Strategy-specific interfaces | 3 strategies + dispatch + workspace default | 0 active (1 legacy enum for records) | **eliminated** |
| Go module dependencies | unchanged (no new deps; `mcp-go` retained for the server) | - | **0 added** |
| Agent charters | 5 role charters + 5 strict | 2 + 2 (a4c, a4c-strict per host) + skill | **-6** |

Net production diff: **+1,006 / -25,709 lines across 95 files** (deletions
include tests and docs regenerated).

## G. Residual risks

1. **Historical graphs keep role semantics** for validation of legacy
   `reviewed` records (approved-review requirement). If a future need contradicts
   this, it is a data-compatibility decision, not a product feature.
2. **`task start` behavior changed** (no longer executes): any external script
   relying on `start` to run a host session must launch the host itself. The
   old path duplicated host orchestration and is gone.
3. **MCP tools keep `team_*` names** for compatibility; renaming remains
   possible later and is cosmetic.
4. **The worktree-escape case is enforced at the integration gate** (dirty base
   refusal) rather than by supervising the agent - correct for host-owned
   execution, but an escaped write is visible only at acceptance/integration.
5. **Live-host conformance** (Copilot/OpenCode/Antigravity charters against real
   host tool surfaces) was verified by the deterministic charter-contract tests;
   live re-commissioning was not run (no paid model calls allowed in this
   initiative).

## H. Commit

`Simplify A4C to host-native direct execution` - see the worktree branch
`a4c/task-1792613467043446800`. Human acceptance and integration are the
operator's: `a4c task diff`, `a4c task accept`, `a4c task integrate`.
