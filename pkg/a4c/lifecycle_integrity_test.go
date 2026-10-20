package a4c_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ormasoftchile/a4c/pkg/a4c"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func liBegin(t *testing.T, h *serviceTestHarness, strategy a4c.ExecutionStrategy) *a4c.BeginTaskResult {
	t.Helper()
	res, err := h.service.BeginTask(context.Background(), a4c.BeginTaskRequest{
		Objective:    "implement interval algebra",
		BudgetTokens: 10000,
		Strategy:     strategy,
	})
	if err != nil {
		t.Fatalf("BeginTask: %v", err)
	}
	return res
}

func liComplete(t *testing.T, h *serviceTestHarness, taskID, activityID string, success bool) {
	t.Helper()
	_, err := h.service.CompleteActivity(context.Background(), a4c.CompleteActivityRequest{
		TaskID:     taskID,
		ActivityID: activityID,
		Actor:      "bob",
		Summary:    "work finished",
		Success:    success,
	})
	if err != nil {
		t.Fatalf("CompleteActivity(%s): %v", activityID, err)
	}
}

func liAddChild(t *testing.T, h *serviceTestHarness, taskID, childID, agent string) {
	t.Helper()
	if err := h.store.Enqueue(a4c.ActivityRequest{
		ID:       childID,
		ParentID: taskID,
		AgentID:  agent,
		Objective: fmt.Sprintf("subtask %s", childID),
	}); err != nil {
		t.Fatalf("enqueue child: %v", err)
	}
}

// liAddReview records an approved review activity directly in the durable store.
func liAddReview(t *testing.T, h *serviceTestHarness, taskID string) {
	t.Helper()
	id := taskID + "-review-1"
	liAddChild(t, h, taskID, id, "charlie")
	if err := h.store.Complete(id, a4c.ActivityResult{
		ID:     id,
		Status: a4c.StatusCompleted,
		Outputs: map[string]any{
			"reviewer": "charlie",
			"approved": true,
			"verdict":  "approved",
		},
	}); err != nil {
		t.Fatalf("complete review: %v", err)
	}
}

func liAccept(h *serviceTestHarness, taskID string) error {
	_, err := h.service.AcceptTask(context.Background(), a4c.AcceptTaskRequest{
		TaskID:        taskID,
		AcceptedBy:    "human_cli",
		IsInteractive: true,
		Comments:      "benchmark acceptance",
	})
	return err
}

func liIntegrate(h *serviceTestHarness, taskID string) error {
	_, err := h.service.IntegrateTask(context.Background(), a4c.IntegrateTaskRequest{TaskID: taskID})
	return err
}

func liStatus(t *testing.T, h *serviceTestHarness, id string) a4c.ActivityStatus {
	t.Helper()
	rec, err := h.store.Get(id)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", id, err)
	}
	return rec.Status
}

// requireFinalizationRejected asserts the gates refused to finalize the task.
// The precise sentinel depends on which gate fires first (derived state, graph
// validation, or snapshot); all are refusals.
func requireFinalizationRejected(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected rejection, got success", what)
	}
	if !errors.Is(err, a4c.ErrActivityGraphIncomplete) &&
		!errors.Is(err, a4c.ErrTaskNotReadyForReview) &&
		!errors.Is(err, a4c.ErrTaskNotAccepted) &&
		!errors.Is(err, a4c.ErrSnapshotMismatch) {
		t.Fatalf("%s: unexpected error %v", what, err)
	}
}

// ---------------------------------------------------------------------------
// 1-4: approved review must never mask non-terminal or failed work (I1/I3)
// ---------------------------------------------------------------------------

func TestLifecycle_Regress_ApprovedReviewPendingChild_AcceptRejected(t *testing.T) {
	h := setupServiceTestHarness(t)
	task := liBegin(t, h, a4c.StrategyReviewed)
	liComplete(t, h, task.TaskID, task.TaskID, true)
	liAddReview(t, h, task.TaskID)
	liAddChild(t, h, task.TaskID, "child-pending", "bob") // stays pending

	err := liAccept(h, task.TaskID)
	requireFinalizationRejected(t, err, "accept with pending child")
	if st := liStatus(t, h, "child-pending"); st != a4c.StatusPending {
		t.Fatalf("child status = %s, want pending (no silent completion)", st)
	}
}

func TestLifecycle_Regress_ApprovedReviewRunningChild_AcceptRejected(t *testing.T) {
	h := setupServiceTestHarness(t)
	task := liBegin(t, h, a4c.StrategyReviewed)
	liComplete(t, h, task.TaskID, task.TaskID, true)
	liAddReview(t, h, task.TaskID)
	liAddChild(t, h, task.TaskID, "child-running", "bob")
	if _, err := h.store.ClaimNextForAgent("bob"); err != nil {
		t.Fatalf("claim: %v", err)
	}

	requireFinalizationRejected(t, liAccept(h, task.TaskID), "accept with running child")
}

func TestLifecycle_Regress_ApprovedReviewFailedChild_AcceptRejected(t *testing.T) {
	h := setupServiceTestHarness(t)
	task := liBegin(t, h, a4c.StrategyTeam)
	liComplete(t, h, task.TaskID, task.TaskID, true)
	liAddReview(t, h, task.TaskID)
	liAddChild(t, h, task.TaskID, "child-failed", "bob")
	if err := h.store.Fail("child-failed", errors.New("verification failed")); err != nil {
		t.Fatalf("fail child: %v", err)
	}

	err := liAccept(h, task.TaskID)
	requireFinalizationRejected(t, err, "accept with failed child")
	if st := liStatus(t, h, "child-failed"); st != a4c.StatusFailed {
		t.Fatalf("failed child status = %s, want failed", st)
	}
}

// TestLifecycle_Regress_TeamRootPendingCompletedChildren reproduces the exact
// Stage 1 failure: a Team task whose children (incl. approved review) all
// completed while the root activity stayed pending was accepted and integrated.
// The root is required work and must block finalization.
func TestLifecycle_Regress_TeamRootPendingCompletedChildren_AcceptRejected(t *testing.T) {
	h := setupServiceTestHarness(t)
	task := liBegin(t, h, a4c.StrategyTeam)
	liAddChild(t, h, task.TaskID, task.TaskID+"-coder-1", "bob")
	liAddChild(t, h, task.TaskID, task.TaskID+"-reviewer-1", "reviewer")
	liComplete(t, h, task.TaskID, task.TaskID+"-coder-1", true)
	liAddReview(t, h, task.TaskID)
	if err := h.store.Complete(task.TaskID+"-reviewer-1", a4c.ActivityResult{
		ID: task.TaskID + "-reviewer-1", Status: a4c.StatusCompleted,
		Outputs: map[string]any{"approved": true},
	}); err != nil {
		t.Fatalf("complete reviewer child: %v", err)
	}
	// root intentionally left pending

	err := liAccept(h, task.TaskID)
	requireFinalizationRejected(t, err, "accept with pending root")
	if st := liStatus(t, h, task.TaskID); st != a4c.StatusPending {
		t.Fatalf("root status = %s, want pending (not implicitly completed)", st)
	}

	// Legitimate finalization: the lead explicitly completes the root.
	liComplete(t, h, task.TaskID, task.TaskID, true)
	if err := liAccept(h, task.TaskID); err != nil {
		t.Fatalf("accept after explicit root completion: %v", err)
	}
	if err := liIntegrate(h, task.TaskID); err != nil {
		t.Fatalf("integrate: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 5-7: fully completed tasks per strategy finalize (I1/I6)
// ---------------------------------------------------------------------------

func TestLifecycle_DirectTask_FullAcceptIntegrate(t *testing.T) {
	h := setupServiceTestHarness(t)
	task := liBegin(t, h, a4c.StrategyDirect)
	if err := os.WriteFile(filepath.Join(task.WorktreePath, "feature.go"), []byte("package fixture\n"), 0644); err != nil {
		t.Fatal(err)
	}
	liComplete(t, h, task.TaskID, task.TaskID, true)

	if err := liAccept(h, task.TaskID); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if err := liIntegrate(h, task.TaskID); err != nil {
		t.Fatalf("integrate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.repoDir, "feature.go")); err != nil {
		t.Fatalf("integrated file missing in base checkout: %v", err)
	}
}

func TestLifecycle_ReviewedTask_FullAcceptIntegrate(t *testing.T) {
	h := setupServiceTestHarness(t)
	task := liBegin(t, h, a4c.StrategyReviewed)
	liComplete(t, h, task.TaskID, task.TaskID, true)
	liAddReview(t, h, task.TaskID)

	if err := liAccept(h, task.TaskID); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if err := liIntegrate(h, task.TaskID); err != nil {
		t.Fatalf("integrate: %v", err)
	}
}

func TestLifecycle_TeamTask_FullAcceptIntegrate(t *testing.T) {
	h := setupServiceTestHarness(t)
	task := liBegin(t, h, a4c.StrategyTeam)
	liAddChild(t, h, task.TaskID, task.TaskID+"-coder-1", "bob")
	liAddChild(t, h, task.TaskID, task.TaskID+"-charlie-1", "charlie")
	liComplete(t, h, task.TaskID, task.TaskID+"-coder-1", true)
	liComplete(t, h, task.TaskID, task.TaskID+"-charlie-1", true)
	liAddReview(t, h, task.TaskID)
	liComplete(t, h, task.TaskID, task.TaskID, true) // lead finalizes root

	if err := liAccept(h, task.TaskID); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if err := liIntegrate(h, task.TaskID); err != nil {
		t.Fatalf("integrate: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 8-9: failure persistence and store error propagation (I2)
// ---------------------------------------------------------------------------

func TestLifecycle_FailedCompletion_PersistsFailed(t *testing.T) {
	h := setupServiceTestHarness(t)
	task := liBegin(t, h, a4c.StrategyDirect)
	liComplete(t, h, task.TaskID, task.TaskID, false) // reports failure

	rec, err := h.store.Get(task.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != a4c.StatusFailed {
		t.Fatalf("persisted status = %s, want failed", rec.Status)
	}
	if rec.Result == nil || rec.Result.Status != a4c.StatusFailed {
		t.Fatalf("failure result discarded: %+v", rec.Result)
	}
	if rec.Result.Outputs["summary"] == nil {
		t.Fatalf("failure details/evidence not retained: %+v", rec.Result.Outputs)
	}
	requireFinalizationRejected(t, liAccept(h, task.TaskID), "accept for a failed activity")
}

func TestStore_Complete_PersistenceErrorPropagates(t *testing.T) {
	h := setupServiceTestHarness(t)
	task := liBegin(t, h, a4c.StrategyDirect)

	// Completing an activity that does not exist must surface the store error.
	_, err := h.service.CompleteActivity(context.Background(), a4c.CompleteActivityRequest{
		TaskID:     task.TaskID,
		ActivityID: "does-not-exist",
		Actor:      "bob",
		Success:    true,
	})
	if !errors.Is(err, a4c.ErrActivityNotFound) {
		t.Fatalf("CompleteActivity on unknown id: err = %v, want ErrActivityNotFound", err)
	}
	if err := h.store.Complete("does-not-exist", a4c.ActivityResult{Status: a4c.StatusCompleted}); !errors.Is(err, a4c.ErrActivityNotFound) {
		t.Fatalf("store.Complete unknown id: err = %v, want ErrActivityNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// 10-12: integration revalidation and snapshot integrity (I4/I5)
// ---------------------------------------------------------------------------

func TestLifecycle_SnapshotChangedAfterApproval_IntegrateRejected(t *testing.T) {
	h := setupServiceTestHarness(t)
	task := liBegin(t, h, a4c.StrategyDirect)
	if err := os.WriteFile(filepath.Join(task.WorktreePath, "feature.go"), []byte("package fixture\n"), 0644); err != nil {
		t.Fatal(err)
	}
	liComplete(t, h, task.TaskID, task.TaskID, true)
	if err := liAccept(h, task.TaskID); err != nil {
		t.Fatalf("accept: %v", err)
	}

	// Content changes after approval must invalidate integration.
	if err := os.WriteFile(filepath.Join(task.WorktreePath, "feature.go"), []byte("package fixture // changed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := liIntegrate(h, task.TaskID); !errors.Is(err, a4c.ErrSnapshotMismatch) {
		t.Fatalf("integrate after change: err = %v, want ErrSnapshotMismatch", err)
	}
}

func TestLifecycle_SnapshotVerificationFails_IntegrateRejected(t *testing.T) {
	h := setupServiceTestHarness(t)
	task := liBegin(t, h, a4c.StrategyDirect)
	liComplete(t, h, task.TaskID, task.TaskID, true)
	if err := liAccept(h, task.TaskID); err != nil {
		t.Fatalf("accept: %v", err)
	}

	// Make snapshot calculation impossible; integration must fail closed and
	// must not fall back to the accepted (stale) snapshot identity.
	if err := os.RemoveAll(task.WorktreePath); err != nil {
		t.Fatal(err)
	}
	if err := liIntegrate(h, task.TaskID); err == nil {
		t.Fatalf("integrate must fail when snapshot verification fails")
	}
}

func TestLifecycle_ActivityAppearsAfterAcceptance_IntegrateRejected(t *testing.T) {
	h := setupServiceTestHarness(t)
	task := liBegin(t, h, a4c.StrategyTeam)
	liAddReview(t, h, task.TaskID)
	liComplete(t, h, task.TaskID, task.TaskID, true)
	if err := liAccept(h, task.TaskID); err != nil {
		t.Fatalf("accept: %v", err)
	}

	// The service boundary refuses new delegation on a finalized task...
	if _, err := h.service.AssignActivity(context.Background(), a4c.AssignActivityRequest{
		TaskID: task.TaskID, Agent: "bob", Objective: "late work",
	}); !errors.Is(err, a4c.ErrTaskFinalized) {
		t.Fatalf("AssignActivity after accept: err = %v, want ErrTaskFinalized", err)
	}
	// ...and even a directly injected activity invalidates integration.
	liAddChild(t, h, task.TaskID, "late-child", "bob")
	if err := liIntegrate(h, task.TaskID); !errors.Is(err, a4c.ErrActivityGraphIncomplete) {
		t.Fatalf("integrate with late activity: err = %v, want ErrActivityGraphIncomplete", err)
	}
}

// ---------------------------------------------------------------------------
// 13: duplicate completion keeps durable state consistent (I2/§4)
// ---------------------------------------------------------------------------

func TestStore_DuplicateCompletion_StateConsistent(t *testing.T) {
	h := setupServiceTestHarness(t)
	task := liBegin(t, h, a4c.StrategyDirect)
	liComplete(t, h, task.TaskID, task.TaskID, true)

	// Retry of the same outcome is an idempotent no-op.
	liComplete(t, h, task.TaskID, task.TaskID, true)
	if st := liStatus(t, h, task.TaskID); st != a4c.StatusCompleted {
		t.Fatalf("status after duplicate completion = %s, want completed", st)
	}

	// A conflicting outcome never flips terminal state.
	err := h.store.Complete(task.TaskID, a4c.ActivityResult{ID: task.TaskID, Status: a4c.StatusFailed})
	if !errors.Is(err, a4c.ErrActivityTerminal) {
		t.Fatalf("conflicting completion: err = %v, want ErrActivityTerminal", err)
	}
	if st := liStatus(t, h, task.TaskID); st != a4c.StatusCompleted {
		t.Fatalf("status flipped to %s after conflicting completion", st)
	}
}

// ---------------------------------------------------------------------------
// 14: restart preserves non-terminal state, no false success (§4)
// ---------------------------------------------------------------------------

func TestLifecycle_ReloadIncompleteWorkflow_NoFalseSuccess(t *testing.T) {
	h := setupServiceTestHarness(t)
	task := liBegin(t, h, a4c.StrategyTeam)
	liAddReview(t, h, task.TaskID)
	liAddChild(t, h, task.TaskID, "child-pending", "bob")

	// Simulate process restart: reopen durable stores over the same directories.
	store2, err := a4c.NewDurableActivityStore(filepath.Join(h.repoDir, ".a4c", "activities"))
	if err != nil {
		t.Fatalf("reload store: %v", err)
	}
	lifecycle2, err := a4c.NewDurableLifecycleStore(filepath.Join(h.repoDir, ".a4c", "lifecycle"))
	if err != nil {
		t.Fatalf("reload lifecycle: %v", err)
	}
	svc2 := a4c.NewTeamService(h.repoDir, store2, lifecycle2, h.coordinator, h.roster, h.memory, h.journal, h.wtMgr)

	rec, err := store2.Get("child-pending")
	if err != nil || rec.Status != a4c.StatusPending {
		t.Fatalf("reloaded child status = %v (%v), want pending", rec, err)
	}
	if _, err := svc2.AcceptTask(context.Background(), a4c.AcceptTaskRequest{
		TaskID: task.TaskID, AcceptedBy: "human_cli", IsInteractive: true,
	}); err != nil {
		requireFinalizationRejected(t, err, "accept after reload")
	} else {
		t.Fatalf("accept after reload must be rejected")
	}
}

// ---------------------------------------------------------------------------
// 15: historical invalid graphs are reported, never rewritten (§6)
// ---------------------------------------------------------------------------

func TestLifecycle_HistoricalIntegratedGraph_ReportedNotRewritten(t *testing.T) {
	h := setupServiceTestHarness(t)
	task := liBegin(t, h, a4c.StrategyTeam)
	liAddReview(t, h, task.TaskID)
	liAddChild(t, h, task.TaskID, "orphan-pending", "bob")
	liComplete(t, h, task.TaskID, task.TaskID, true)

	// Fabricate a historical record: integrated with an unresolved child
	// (the Stage 1 shape), as written by pre-patch builds.
	if err := h.lifecycleStore.Save(&a4c.HumanLifecycleRecord{
		TaskID:          task.TaskID,
		Accepted:        true,
		AcceptedBy:      "human_cli",
		AcceptedSnapshot: "historic-snapshot",
		Integrated:      true,
		IntegratedCommit: "historic-commit",
	}); err != nil {
		t.Fatalf("save historical lifecycle: %v", err)
	}

	violations, err := h.service.ValidateTaskGraph(task.TaskID)
	if err != nil {
		t.Fatalf("ValidateTaskGraph: %v", err)
	}
	found := false
	for _, v := range violations {
		if v.ActivityID == "orphan-pending" {
			found = true
		}
	}
	if !found {
		t.Fatalf("historical orphan not reported: %+v", violations)
	}

	// Reported, not rewritten.
	rec, err := h.store.Get("orphan-pending")
	if err != nil || rec.Status != a4c.StatusPending {
		t.Fatalf("historical record mutated: %+v (%v)", rec, err)
	}
	if err := liIntegrate(h, task.TaskID); !errors.Is(err, a4c.ErrTaskAlreadyIntegrated) {
		t.Fatalf("re-integrate of historical task: err = %v, want ErrTaskAlreadyIntegrated", err)
	}
}

// ---------------------------------------------------------------------------
// §3: Direct mode does not delegate; other strategies may
// ---------------------------------------------------------------------------

func TestLifecycle_DirectModeDelegationRejected(t *testing.T) {
	h := setupServiceTestHarness(t)
	direct := liBegin(t, h, a4c.StrategyDirect)
	if _, err := h.service.AssignActivity(context.Background(), a4c.AssignActivityRequest{
		TaskID: direct.TaskID, Agent: "bob", Objective: "verify things",
	}); !errors.Is(err, a4c.ErrDelegationNotPermitted) {
		t.Fatalf("direct delegation: err = %v, want ErrDelegationNotPermitted", err)
	}

	team := liBegin(t, h, a4c.StrategyTeam)
	ref, err := h.service.AssignActivity(context.Background(), a4c.AssignActivityRequest{
		TaskID: team.TaskID, Agent: "bob", Objective: "implement things",
	})
	if err != nil {
		t.Fatalf("team delegation must remain possible: %v", err)
	}
	if ref.ActivityID == "" {
		t.Fatalf("delegated activity not created")
	}
}

// ---------------------------------------------------------------------------
// §4: concurrency — completion racing acceptance cannot produce false success
// ---------------------------------------------------------------------------

func TestLifecycle_ConcurrentCompletionAccept_NoFalseSuccess(t *testing.T) {
	for i := 0; i < 20; i++ {
		h := setupServiceTestHarness(t)
		task := liBegin(t, h, a4c.StrategyTeam)
		liAddReview(t, h, task.TaskID)
		liAddChild(t, h, task.TaskID, "racy-child", "bob")

		var wg sync.WaitGroup
		wg.Add(2)
		var acceptErr error
		go func() {
			defer wg.Done()
			acceptErr = liAccept(h, task.TaskID)
		}()
		go func() {
			defer wg.Done()
			liComplete(t, h, task.TaskID, "racy-child", true)
			liComplete(t, h, task.TaskID, task.TaskID, true)
		}()
		wg.Wait()

		if acceptErr != nil {
			continue // rejected is always a valid outcome of the race
		}
		// If acceptance succeeded, every required activity must be terminal-success.
		for _, id := range []string{"racy-child", task.TaskID} {
			rec, err := h.store.Get(id)
			if err != nil || rec.Status != a4c.StatusCompleted {
				t.Fatalf("accepted with non-terminal activity %s (rec=%+v err=%v)", id, rec, err)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// §5 derive consistency cases (I3), asserted through the acceptance gate
// ---------------------------------------------------------------------------

func TestLifecycle_ReviewBeforeChildCompletion_NoAcceptance(t *testing.T) {
	h := setupServiceTestHarness(t)
	task := liBegin(t, h, a4c.StrategyReviewed)
	liComplete(t, h, task.TaskID, task.TaskID, true)
	liAddReview(t, h, task.TaskID)   // reviewer finishes first
	liAddChild(t, h, task.TaskID, "late-review-check", "bob") // child still open

	if err := liAccept(h, task.TaskID); err != nil {
		requireFinalizationRejected(t, err, "review finished before child")
	} else {
		t.Fatalf("review finished before child: acceptance must be rejected")
	}

	// Review is also necessary where the strategy requires it.
	h2 := setupServiceTestHarness(t)
	t2 := liBegin(t, h2, a4c.StrategyReviewed)
	liComplete(t, h2, t2.TaskID, t2.TaskID, true)
	if err := liAccept(h2, t2.TaskID); err != nil {
		requireFinalizationRejected(t, err, "reviewed task without review")
	} else {
		t.Fatalf("reviewed task without review: acceptance must be rejected")
	}
	if violations, err := h2.service.ValidateTaskGraph(t2.TaskID); err != nil || len(violations) == 0 {
		t.Fatalf("missing review not reported: %v %v", violations, err)
	} else if !strings.Contains(violations[0].Reason, "approved review") {
		t.Fatalf("unexpected violation reason: %+v", violations[0])
	}
}

func TestLifecycle_FailureAfterSuccess_TaskFailed(t *testing.T) {
	h := setupServiceTestHarness(t)
	task := liBegin(t, h, a4c.StrategyTeam)
	liAddReview(t, h, task.TaskID)
	liAddChild(t, h, task.TaskID, "ok-child", "bob")
	liAddChild(t, h, task.TaskID, "bad-child", "bob")
	liComplete(t, h, task.TaskID, "ok-child", true)
	liComplete(t, h, task.TaskID, task.TaskID, true)
	if err := h.store.Fail("bad-child", errors.New("regression")); err != nil {
		t.Fatal(err)
	}

	requireFinalizationRejected(t, liAccept(h, task.TaskID), "accept when a required activity failed after another succeeded")
	if st := liStatus(t, h, "ok-child"); st != a4c.StatusCompleted {
		t.Fatalf("successful child disturbed: %s", st)
	}
}
