# A4C Direct-Only Simplification - Final Acceptance Review (2026-10-21)

**Subject:** commit `559da5c` "Simplify A4C to host-native direct execution"
(task `task-1792613467043446800`, base `850d3cd`).
**Reviewer posture:** independent, evidence-based; the implementation summary
was treated as an unverified claim. No production behavior was changed. One
separate **validation-only** commit adds two missing invariant tests (see F-6).
No paid model calls, no live-host sessions, no benchmarks.

---

## A. Executive verdict

**ACCEPT WITH LIMITATIONS.**

The architectural reduction is correctly implemented: every retired
orchestration responsibility is genuinely gone, every retained governance
guarantee is genuinely enforced, historical records are read-only and remain
gated by their recorded contract, and the reported complexity numbers
reproduce exactly. No blocking defect was found.

One **P1 documentation defect** must be corrected before or at acceptance: the
implementation report states a stronger verification guarantee than the code
provides (F-1). The underlying behavior is not a regression - the host-native
path never enforced independent verification, before or after - but the claim
must not survive into the accepted record unamended.

## B. Regression coverage matrix (Patch 1 / Patch 2)

Source: `pkg/a4c/service.go` (`AcceptTask` ~530-595, `IntegrateTask` ~610-770,
`ValidateTaskGraph` ~1215-1265, `activitySucceeded` ~1199);
`pkg/a4c/store.go`; tests as named. "Exercise" = the test forces the failure
mode and asserts refusal, not merely the happy path.

| # | Invariant | Enforcement source | Surviving test | Exercises failure? | Assessment |
|---|---|---|---|---|---|
| 1 | Incomplete graph cannot be accepted | `ValidateTaskGraph` (AcceptTask gate) | `TestLifecycle_Regress_ApprovedReview{Pending,Running}Child_AcceptRejected`, `TestLifecycle_ReloadIncompleteWorkflow_NoFalseSuccess` | yes | preserved |
| 2 | Incomplete graph cannot be integrated | `IntegrateTask` re-validates graph | `TestLifecycle_Regress_*`, `TestTeamService_IntegrateTask_SuccessAndGuards` | yes | preserved |
| 3 | Failed completion persists as failure | `CompleteActivity` `Success=false` -> `StatusFailed` | `TestLifecycle_FailedCompletion_PersistsFailed` | yes | preserved |
| 4 | Failed activities never count as success | `ValidateTaskGraph` + `activitySucceeded` | `TestLifecycle_Regress_ApprovedReviewFailedChild_AcceptRejected` | yes | preserved |
| 5 | Terminal outcomes idempotent, never flip | `store.Complete`/`Fail` refusal on terminal | `TestStore_DuplicateCompletion_StateConsistent`, `ErrTaskAlreadyIntegrated` in `IntegrateTask_SuccessAndGuards` | yes | preserved |
| 6 | Approved review cannot mask incomplete work (historical) | graph walk, review is no substitute | `TestLifecycle_Regress_ApprovedReview*`, `TestLifecycle_ReviewBeforeChildCompletion_NoAcceptance` | yes | preserved |
| 7 | Pending root blocks accept/integrate | `ValidateTaskGraph` non-terminal case | `TestLifecycle_Regress_TeamRootPendingCompletedChildren_AcceptRejected` | yes | preserved |
| 8 | Post-acceptance activity injection cannot bypass gates | integrate re-validates tree | `TestLifecycle_ActivityAppearsAfterAcceptance_IntegrateRejected` | yes (store-level injection) | preserved |
| 9 | Snapshot mismatch prevents integration | `deriveTaskState` + `ErrSnapshotMismatch` | `TestLifecycle_SnapshotChangedAfterApproval_IntegrateRejected`, `IntegrateTask_SuccessAndGuards` (post-accept commit) | yes | preserved |
| 10 | Snapshot verification fails closed | `snapshotStrict` (hash error is denial) | `TestLifecycle_SnapshotVerificationFails_IntegrateRejected` (worktree removed) | yes | preserved |
| 11 | Git staging/commit failures propagate | `IntegrateTask` add/commit errors fatal | **added** `TestGate_GitCommitFailureDuringIntegrationPropagates` (pre-commit hook injection) | yes | preserved (was untested; see F-6) |
| 12 | Post-commit snapshot re-verification before merge | `IntegrateTask` post-commit compare | `TestTeamService_IntegrateTask_SuccessAndGuards` (content changed during integration path) | partial | preserved (code comment + guard at service.go:722-745; directly exercised only via mismatch case) |
| 13 | Concurrent activity reads cannot fabricate success | store locking + graph revalidation | `TestLifecycle_ConcurrentCompletionAccept_NoFalseSuccess` | yes | preserved |
| 14 | Store persistence errors propagate | `store.persist` errors returned | `TestStore_Complete_PersistenceErrorPropagates` | yes | preserved |
| 15 | Historical invalid graphs readable, never rewritten | `ValidateTaskGraph` read-only; store immutable | `TestLifecycle_HistoricalIntegratedGraph_ReportedNotRewritten`, `TestHistorical_*` (direct_only_test.go) | yes | preserved |

**Interrupted cannot masquerade as success** (implementation invariant):
`ValidateTaskGraph` `StatusInterrupted` case; **added**
`TestGate_InterruptedRootCannotBeAcceptedOrIntegrated` asserts accept and
integrate are both refused and the state stays `interrupted` (was enforced but
only indirectly tested).

### Patch 2

| Guarantee | Source / test | Assessment |
|---|---|---|
| Interrupted tasks keep failure/recovery semantics | `store.InterruptWithResult`, `TestRecovery_InterruptedRootAbandonsChildren`, `TestRecovery_RejectAbandonsChildren`, `TestGate_Interrupted...` | preserved |
| Completion metrics optional | `CompleteActivity` (telemetry never blocks completion); `TestMCP_CompleteActivityOptionalUsageTelemetry` | preserved |
| Missing telemetry != measured zero | `metricsFromHostUsage` absent-field semantics; `TestMetrics_MissingUsageIsAbsentNotZero`, `TestMetrics_UnreportedFieldsStayUnknown` | preserved |
| CLI flag parsing cannot create accidental tasks | `runTask` validation; `TestTaskHelpNeverCreatesTask`, `TestTaskFlagLikeObjectiveRejected`, `TestTaskUnsupportedOptionRejected` | preserved |
| Host tool instructions match tool surface | `TestRosterExport_CharterMatchesServiceContract`, `TestMCPServer_ToolDiscovery` (retired tools absent), `TestPrimaryPromptsDoNotAdvertiseRetiredModes` | preserved |
| Worktree instructions authoritative | charter "work ONLY in the returned worktree"; `TestWorkspaceIsolation_*`, `TestGovernedFile_RejectsEscapes` | preserved |
| Knowledge never mandatory | `knowledgeCoverageWarnings` is warnings-only in `AcceptTask`; `TestKnowledgeLifecycle_MissingAssessmentWarns` | preserved |

**One weakened-but-not-missing area:** `TestIntegrationPreflight_FullChain`
lost its reviewer-bundle/tool-cap/diff-hygiene sections with the reviewer
retirement. The retained half (verification classification -> durable log ->
evidence record) and `TestIntegrationPreflight_ReplayPreservedBenchmarkFailure`
still pin the "vacuous `go test` is a gap, never a pass" classifier. Those
assertions currently protect machinery that is unwired in production (F-2).

## C. Host handoff assessment

The 9-step autonomous flow exists on every host without any deleted component:

| Step | Copilot | OpenCode | Antigravity | Backing implementation |
|---|---|---|---|---|
| 1. Begin task | `team_begin_task` | `tools.a4c.team_begin_task` | same via MCP | `BeginTask` (service), MCP `team_begin_task` |
| 2. Authoritative worktree | `BeginTaskResult.WorktreePath` / `team_status` | same | same | `WorktreeManager`, `GetWorktreePath` |
| 3. Work in worktree | charter invariant | same | same | governed path containment (`ApplyWorkspaceFile`) or host-native edits |
| 4. Native tools + subagents | charter tools list (`bash`,`powershell`) | `subagent: allow` in permissions | skill text | no A4C-side restriction exists |
| 5. Verify | charter "run the tests ONCE" | same | same | **agent-executed** (see F-1) |
| 6. Record completion/failure | `team_complete_activity` | same | same | `CompleteActivity` (success/failure explicit) |
| 7. Preserve evidence | summary + `validation_evidence` + `usage` params | same | same | recorded in `ActivityResult.Outputs`, surfaced by `inspect`/`checkpoint` |
| 8. Human acceptance | `a4c task accept` | same | same | human-gated, token/interactive-bound |
| 9. Guarded integration | `a4c task integrate` | same | same | `IntegrateTask` gates |

No step references providers, the coordinator, or reviewer infrastructure.
Charters and MCP schemas match the surviving implementation (tool list = 11;
`team_delegate`/`team_assign_activity`/`team_review_result` absent from
discovery and fail closed when called). **Unverified live assumptions** (no paid
calls authorized): real-host tool-name resolution (`a4c-team_*` on Copilot),
Antigravity skill invocation + `agy mcp add a4c a4c mcp` registration behavior,
and OpenCode's code-execution call form. These were validated statically and by
deterministic charter-contract tests only.

## D. Historical compatibility

- Team/Reviewed records: readable and immutable (`TestHistorical_
  TeamAndReviewedRecordsRemainInspectable`), original strategy preserved
  (`liMarkLegacyStrategy` fabricates exact historical shapes at store level).
- Legacy Reviewed contract still enforced: approved review required before
  acceptance (`TestHistorical_LegacyReviewedWithoutReviewCannotBeAccepted`,
  `TestLifecycle_ReviewBeforeChildCompletion_NoAcceptance`); new tasks carry no
  strategy and are never subject to it.
- Outcomes not rewritten: `TestLifecycle_HistoricalIntegratedGraph_
  ReportedNotRewritten`, `TestHistorical_IntegratedTaskRetainsOutcome`
  (re-integration refused).
- Unsupported mutations fail explicitly: `rejectRetiredStrategy`
  (`TestHistorical_LegacyMutationsFailSafely`).
- Interrupted/abandoned legacy children: preserved with explicit reasons
  (`TestRecovery_*`), never fabricated into success.
- Export: `TestRenderLegacyRecordMarkdown` keeps historical records readable
  through the knowledge export path.
- No coordinator is kept alive to finish legacy orchestration; the policy is
  read-only inspection + safe rejection, as the mission allows.

## E. Complexity audit (independently recomputed from git)

| Measure | `850d3cd` | `559da5c` | Delta | Report claim | Match |
|---|---:|---:|---:|---|---|
| Production Go files | 45 | 33 | -12 | 45->33 | yes |
| Production lines | 17,921 | 9,491 | -8,430 (-47%) | 17,921->9,491 | yes |
| Test files | 75 | 33 | -42 | 75->33 | yes |
| Test lines | 18,625 | 5,493 | -13,132 (-71%) | 18,625->5,493 | yes |
| Total Go lines | 36,546 | 14,984 | -21,562 (-59%) | 36,546->14,984 | yes |
| MCP tools | 14 | 11 | -3 | 14->11 | yes |
| CLI roots | 10 | 5 | -5 | 10->5 | yes |

Deleted production files (15) all map to retired responsibilities: the
coordinator (`team_coordinator.go`), launcher/provider layer (9 files),
role handoffs (`handoff.go`), roster (`team.go`), headless runner
(`test_runner.go`), strategy config (`workspace_config.go`). Added files (4)
are shared keepers (`reconcile.go`, `checkpoint_prompt.go`, `legacy_types.go`)
plus the new regression file. This is an actual architectural reduction, not
command hiding: the dispatch, session, delegation, and review code paths no
longer exist anywhere in the binary.

**Residual dead code (verified by reference scan):**
- `ProjectTestRunner`/`TestRunner`/`ProjectAwareRunner`/`AnalyzeGoTestOutput`
  + `WriteDurableTestLog`: zero production callers (definition-only), exercised
  by tests. The trusted verification contract machinery exists but is unwired
  (F-2).
- `ValidateAgainstEvidence` + `ErrStaleValidationEvidence`: zero callers, zero
  tests (their tests were deleted with the coordinator) (F-2).
- Type leftovers: `AgentIdentity`, `ReviewResult`, `ReviewFinding`, and the
  `ReviewVerdict` type (the derived `TaskDetails.ReviewVerdict` STRING field is
  live historical reporting and correctly kept) (F-3).

Obsolete strategy references remaining are legitimate legacy data only:
`legacy_types.go` (enum), `rejectRetiredStrategy`, `history.go` reporting,
`liMarkLegacyStrategy`/`direct_only_test.go` fixtures, and retired-flag
rejection in the CLI.

## F. Validation results

```
go build ./...                      PASS
go vet ./...                        PASS
go test -short ./...                PASS (cmd, cmd/a4c, pkg/a4c, pkg/a4c/knowledge, pkg/a4c/mcp)
go test -race -short ./pkg/a4c/...  PASS
go test -short -run TestGate_ ./pkg/a4c/   PASS (both validation-only tests)
```

Deterministic and offline. **Environmental skips:** none (the commit-failure
injection probe would skip explicitly if a platform did not execute git hooks;
it fired here, so the assertion ran). **Not run by design:** live-host
integration tests and paid model calls (unauthorized for this review). The
`gofmt -l` listing on this tree is the repository's known CRLF convention
(untouched files are listed too) and is not formatting damage.

## G. Findings

**F-1 (P1) - The deliverable overstates the verification guarantee; independent
verification is ADVISORY in the host-native flow.**
Evidence: no production code path invokes verification or validates a
`ValidationEvidence` record (`ProjectTestRunner`, `ValidateAgainstEvidence` are
definition-only in production); `AcceptTask`/`IntegrateTask` gate on graph
status + snapshot identity + cleanliness; `CompleteActivity` records
agent-reported `Success` and an optional evidence ID without checking it.
`docs/direct-only-simplification-report.md` (section D) claims "verification
not hideable" and "integration on independently validated state" - the second
is true of *state*, the first is not established. Reproducible scenario: a host
agent runs `go test`, sees failures, calls `team_complete_activity` with
`success:true`; the gates accept and integrate. Not a regression: in `850d3cd`
`ValidateAgainstEvidence` was called only from `team_coordinator.go:2383,2401`
(the deleted headless `Step` path) - the host-native path had the same
property. The actual guarantee is: **verification evidence is recorded and
surfaced to the human; human acceptance is the authority; graph, snapshot, and
integration integrity are enforced.** Minimum corrective work: amend those two
sentences in the report (not done here - documentation of an accepted
deliverable should not be rewritten by its reviewer without authorization).

**F-2 (P2) - Verification machinery is retained but unwired.**
`ProjectTestRunner` (trusted `verify.json` contract, vacuous-evidence
classification, revision-bound evidence), `WriteDurableTestLog`, and
`ValidateAgainstEvidence` are dead in production (tests only). Either wire the
runner into the completion/accept path (a behavior change - out of scope) or
document it explicitly as a library available for future enforcement.

**F-3 (P2) - Legacy type leftovers.** `AgentIdentity`, `ReviewResult`,
`ReviewFinding`, `ReviewVerdict` type are unused declarations. Harmless;
candidates for deletion.

**F-4 (Informational) - MCP `success` parameter defaults to `true`.**
`server.go` `GetBool("success", true)`: an agent that omits the flag records a
successful completion. Pre-existing contract; the charters mandate explicit
values. A fail-closed default would be stricter but changes the host contract.

**F-5 (Informational) - Evidence shape is record-level, not artifact-level.**
With `WriteDurableTestLog` unwired, agent evidence lives in completion fields
(`summary`, `validation_evidence`, `usage`) rather than hashed artifacts under
`.a4c/evidence/`. Inspectable and durable; weaker than artifact attestation.

**F-6 (Informational) - Validation-only tests added.** Two Patch 1 invariants
had no direct test after the simplification (interrupted-not-acceptable;
git-commit-failure-propagates). Both were enforced by the implementation; the
tests are added in a separate, clearly labeled commit
(`validation-only: pin interrupted-not-acceptable and commit-failure-propagation
gates`) and pass. The implementation commit `559da5c` is unmodified.

## H. Acceptance recommendation

**Accept and integrate `559da5c`,** with F-1's two-sentence documentation
correction applied at or immediately after acceptance (or accepted knowingly
with this review as the authoritative statement of guarantees).

The simplified implementation preserves the guarantees A4C is responsible for -
durability, inspectability, recoverability, governance, and safe integration -
without any dependency on the deleted orchestration. The one guarantee it does
*not* provide is automated verification gating of agent-reported success; that
limitation is pre-existing on the host-native path, is documented here, and is
compensated by mandatory human acceptance.

**Minimum corrective work if the human prefers it before acceptance:** amend
`docs/direct-only-simplification-report.md` section D/F wording per F-1.

---

### Acceptance authorization (operator)

Review complete; nothing has been accepted or integrated. If the verdict is
acceptable, the commands remain:

```powershell
a4c task accept task-1792613467043446800
a4c task integrate task-1792613467043446800
```
