package a4c_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/ormasoftchile/a4c/pkg/a4c"
)

// ---------------------------------------------------------------------------
// Removed-strategy and historical-compatibility regression scenarios (§13).
// The retired execution strategies create nothing; historical Team/Reviewed
// records stay readable, immutable, and unable to bypass lifecycle validation.
// ---------------------------------------------------------------------------

// TestDirectOnly_NoStrategySelectionForNewWork: new submissions cannot request
// Team or Reviewed, and the accepted surface no longer offers a choice.
func TestDirectOnly_NoStrategySelectionForNewWork(t *testing.T) {
	h := setupServiceTestHarness(t)
	ctx := context.Background()

	for _, retired := range []a4c.ExecutionStrategy{a4c.StrategyTeam, a4c.StrategyReviewed} {
		if _, err := h.service.SubmitTask(ctx, a4c.SubmitTaskRequest{
			Objective: "must not exist", Strategy: retired,
		}); err == nil {
			t.Fatalf("SubmitTask accepted retired strategy %q", retired)
		}
		if _, err := h.service.BeginTask(ctx, a4c.BeginTaskRequest{
			Objective: "must not exist", Strategy: retired,
		}); err == nil {
			t.Fatalf("BeginTask accepted retired strategy %q", retired)
		}
		if _, err := h.service.AdoptExistingWork(ctx, a4c.AdoptWorkRequest{
			Objective: "must not exist", Strategy: retired,
		}); err == nil {
			t.Fatalf("AdoptExistingWork accepted retired strategy %q", retired)
		}
	}

	// Direct (or the empty value) remains accepted for compatibility.
	if _, err := h.service.SubmitTask(ctx, a4c.SubmitTaskRequest{
		Objective: "normal work", Strategy: a4c.StrategyDirect,
	}); err != nil {
		t.Fatalf("SubmitTask with legacy 'direct' value failed: %v", err)
	}
	if all, err := h.service.ListTasks(ctx, a4c.TaskFilter{}); err != nil || len(all) != 1 {
		t.Fatalf("expected exactly the one direct task, got %v (%v)", all, err)
	}
}

// TestDirectOnly_NoDelegationSurface: A4C exposes no way to assign worker
// activities or schedule reviews. The interface itself must be gone.
func TestDirectOnly_NoDelegationSurface(t *testing.T) {
	var svc a4c.TeamService = setupServiceTestHarness(t).service
	typ := reflect.TypeOf(&svc).Elem()
	for _, retired := range []string{
		"AssignActivity", "RecordReview", "EscalateTask", "ExecuteTask", "DiscoverAlternativeHosts",
	} {
		if _, ok := typ.MethodByName(retired); ok {
			t.Fatalf("retired method %s is still exposed on the service interface", retired)
		}
	}
}

// TestHistorical_TeamAndReviewedRecordsRemainInspectable fabricates legacy
// task graphs exactly as the retired strategies wrote them and checks that
// they read back unchanged - reported, never rewritten or reinterpreted.
func TestHistorical_TeamAndReviewedRecordsRemainInspectable(t *testing.T) {
	h := setupServiceTestHarness(t)
	ctx := context.Background()

	// Legacy Reviewed task: root + approved review record.
	reviewed := liBegin(t, h)
	liMarkLegacyStrategy(t, h, reviewed.TaskID, a4c.StrategyReviewed)
	liAddReview(t, h, reviewed.TaskID)
	liComplete(t, h, reviewed.TaskID, reviewed.TaskID, true)

	// Legacy Team task: root + delegated child, both completed.
	team := liBegin(t, h)
	liMarkLegacyStrategy(t, h, team.TaskID, a4c.StrategyTeam)
	liAddChild(t, h, team.TaskID, team.TaskID+"-coder", "bob")
	liComplete(t, h, team.TaskID, team.TaskID+"-coder", true)
	liComplete(t, h, team.TaskID, team.TaskID, true)

	for _, tc := range []struct {
		id       string
		strategy a4c.ExecutionStrategy
	}{
		{reviewed.TaskID, a4c.StrategyReviewed},
		{team.TaskID, a4c.StrategyTeam},
	} {
		rec, err := h.store.Get(tc.id)
		if err != nil {
			t.Fatalf("historical record %s unreadable: %v", tc.id, err)
		}
		if rec.Request.Strategy != tc.strategy {
			t.Fatalf("historical strategy rewritten: got %q want %q", rec.Request.Strategy, tc.strategy)
		}
		details, err := h.service.GetTask(ctx, tc.id)
		if err != nil {
			t.Fatalf("GetTask(%s): %v", tc.id, err)
		}
		if details.TaskID != tc.id {
			t.Fatalf("historical task not reported: %+v", details)
		}
	}

	// Graphs stay unmodified by inspection.
	if rec, _ := h.store.Get(team.TaskID); len(h.store.GetTaskTree(team.TaskID)) != 2 {
		t.Fatalf("legacy team graph changed shape: %+v", rec)
	}
}

// TestHistorical_LegacyReviewedWithoutReviewCannotBeAccepted: incomplete legacy
// work cannot bypass lifecycle validation just because the orchestrator that
// would have produced the review is gone.
func TestHistorical_LegacyReviewedWithoutReviewCannotBeAccepted(t *testing.T) {
	h := setupServiceTestHarness(t)
	task := liBegin(t, h)
	liMarkLegacyStrategy(t, h, task.TaskID, a4c.StrategyReviewed)
	liComplete(t, h, task.TaskID, task.TaskID, true)

	if err := liAccept(h, task.TaskID); err == nil {
		t.Fatalf("legacy reviewed task without its review record was accepted")
	}
	if violations, err := h.service.ValidateTaskGraph(task.TaskID); err != nil || len(violations) == 0 {
		t.Fatalf("missing historical review not reported: %v %v", violations, err)
	}
}

// TestHistorical_IntegratedTaskRetainsOutcome: a historical task that was
// integrated keeps its recorded outcome and cannot be re-integrated.
func TestHistorical_IntegratedTaskRetainsOutcome(t *testing.T) {
	h := setupServiceTestHarness(t)
	ctx := context.Background()
	task := liBegin(t, h)
	liMarkLegacyStrategy(t, h, task.TaskID, a4c.StrategyTeam)
	liComplete(t, h, task.TaskID, task.TaskID, true)

	if err := liAccept(h, task.TaskID); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if _, err := h.service.IntegrateTask(ctx, a4c.IntegrateTaskRequest{TaskID: task.TaskID}); err != nil {
		t.Fatalf("integrate: %v", err)
	}
	details, err := h.service.GetTask(ctx, task.TaskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if details.State != a4c.TaskStateIntegrated {
		t.Fatalf("integrated outcome not retained: %s", details.State)
	}
	if _, err := h.service.IntegrateTask(ctx, a4c.IntegrateTaskRequest{TaskID: task.TaskID}); err == nil {
		t.Fatalf("historical task was re-integrated")
	}
}

// TestHistorical_LegacyMutationsFailSafely: retired operations on historical
// tasks fail explicitly instead of half-working.
func TestHistorical_LegacyMutationsFailSafely(t *testing.T) {
	h := setupServiceTestHarness(t)
	ctx := context.Background()
	task := liBegin(t, h)
	liMarkLegacyStrategy(t, h, task.TaskID, a4c.StrategyTeam)

	if _, err := h.service.SubmitTask(ctx, a4c.SubmitTaskRequest{
		Objective: "x", Strategy: a4c.StrategyTeam,
	}); err == nil || !strings.Contains(err.Error(), "retired") {
		t.Fatalf("retired mutation did not fail explicitly: %v", err)
	}
	// The historical record itself is untouched.
	rec, err := h.store.Get(task.TaskID)
	if err != nil || rec.Request.Strategy != a4c.StrategyTeam {
		t.Fatalf("historical record altered by a failed mutation: %+v %v", rec, err)
	}
}
