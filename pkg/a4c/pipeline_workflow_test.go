package a4c_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	spike "github.com/ormasoftchile/a4c/pkg/a4c"
)

// TestWorkflow_OperationalFailureDoesNotTriggerCoder reproduces the historical
// defect where a reviewer that failed to submit a verdict (tool/operational
// failure) became "incomplete" and scheduled a coding revision. An operational
// failure must NOT schedule a coder or consume MaxRevisions.
func TestWorkflow_OperationalFailureDoesNotTriggerCoder(t *testing.T) {
	h := newReviewHarness(t, &fakeScriptedRunner{exitCode: 0, output: "ok  \texample.com/calc\t0.01s\n"})
	h.coord.MaxRevisions = 2

	// Coder with real evidence.
	_ = h.store.Enqueue(spike.ActivityRequest{ID: "c1", AgentID: "coder", Objective: "impl", CreatedAt: time.Now()})
	_ = h.store.Complete("c1", spike.ActivityResult{
		ID: "c1", Status: spike.StatusCompleted,
		Outputs: map[string]any{
			"summary": "implemented",
			"validation_evidence": spike.ValidationEvidence{
				EvidenceID: "evi-c1", WorkspaceSnapshotID: "snap", Command: []string{"make", "test"},
				ExitCode: 0, Valid: true, TestsExecuted: 3, CompletedAt: time.Now(),
			},
		},
	})

	// Reviewer session completes but records NO verdict (operational failure).
	h.fake.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		// Deliberately do NOT call OnSubmitReview: simulates a tool/lifecycle
		// failure where the reviewer never submits a verdict.
		return &spike.AgentMessage{Content: "I was unable to submit."}, nil
	}
	_ = h.store.Enqueue(spike.ActivityRequest{ID: "r1", AgentID: "reviewer", Objective: "review", DependsOn: []string{"c1"}, CreatedAt: time.Now()})

	res, err := h.coord.Step(context.Background())
	if err != nil {
		t.Fatalf("step failed: %v", err)
	}
	verdict, _ := res.Outputs["verdict"].(string)
	if verdict != string(spike.ReviewVerdictIncomplete) {
		t.Fatalf("expected incomplete (operational failure), got %q", verdict)
	}
	// No coding revision may be scheduled.
	if _, err := h.store.Get("r1-revision-1"); err == nil {
		t.Fatalf("operational failure must NOT schedule a coding revision")
	}
	// Instead a bounded review retry is scheduled.
	if rec, err := h.store.Get("r1-reviewretry-1"); err != nil || rec == nil {
		t.Fatalf("operational failure should schedule a review retry, got err=%v", err)
	} else if rec.Request.AgentID != "reviewer" {
		t.Fatalf("retry must target the reviewer, got %q", rec.Request.AgentID)
	}
}

// TestWorkflow_IncompleteExhaustionGoesToHuman: repeated operational failures
// terminate at an explicit human decision, never a coder.
func TestWorkflow_IncompleteExhaustionGoesToHuman(t *testing.T) {
	h := newReviewHarness(t, &fakeScriptedRunner{exitCode: 0, output: "ok  \texample.com/calc\t0.01s\n"})
	h.coord.MaxRevisions = 2

	_ = h.store.Enqueue(spike.ActivityRequest{ID: "c1", AgentID: "coder", Objective: "impl", CreatedAt: time.Now()})
	_ = h.store.Complete("c1", spike.ActivityResult{ID: "c1", Status: spike.StatusCompleted, Outputs: map[string]any{"summary": "ok"}})

	attempts := 0
	h.fake.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		attempts++
		// Always fail to submit a verdict.
		return &spike.AgentMessage{Content: "no verdict"}, nil
	}
	_ = h.store.Enqueue(spike.ActivityRequest{ID: "r1", AgentID: "reviewer", Objective: "review", DependsOn: []string{"c1"}, CreatedAt: time.Now()})

	results, err := h.coord.RunLoop(context.Background(), 10)
	if err != nil {
		t.Fatalf("run loop error: %v", err)
	}
	last := results[len(results)-1]
	// Terminal state is a human decision, not a coder revision.
	if _, err := h.store.Get("r1-revision-1"); err == nil {
		t.Fatalf("operational failures must never schedule a coder revision")
	}
	fr, _ := last.Outputs["failure_reason"].(string)
	if fr != "review_operational_failure_exhausted" && last.Status != spike.StatusAwaitingHuman {
		t.Fatalf("expected operational-failure exhaustion at human, got fr=%q status=%s", fr, last.Status)
	}
}

// TestWorkflow_ApprovalPersistedCorrectly: a genuine approval with valid
// evidence is persisted and reaches the approved outcome. The coder runs
// through the real specialist path so the coordinator produces real, snapshot-
// bound evidence.
func TestWorkflow_ApprovalPersistedCorrectly(t *testing.T) {
	h := newReviewHarness(t, &fakeScriptedRunner{exitCode: 0, output: "ok  \texample.com/calc\t0.01s\n"})

	reviewed := 0
	h.fake.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		switch session.Config().AgentID {
		case "coder":
			// Write a file through the real specialist path.
			_ = os.WriteFile(filepath.Join(h.workspace, "calc.go"), []byte("package calc\n\nfunc Div(a, b int) int { return a / b }\n"), 0o644)
			return &spike.AgentMessage{Content: "implemented"}, nil
		case "reviewer":
			reviewed++
			session.Config().OnSubmitReview(spike.ReviewResult{
				Verdict: spike.ReviewVerdictApproved, Approved: true, Comments: "verified", Timestamp: time.Now(),
			})
			return &spike.AgentMessage{Content: "[REVIEW_APPROVED]"}, nil
		}
		return &spike.AgentMessage{Content: "ok"}, nil
	}
	_ = h.store.Enqueue(spike.ActivityRequest{ID: "c1", AgentID: "coder", Objective: "impl", CreatedAt: time.Now()})
	_ = h.store.Enqueue(spike.ActivityRequest{ID: "r1", AgentID: "reviewer", Objective: "review", DependsOn: []string{"c1"}, CreatedAt: time.Now()})

	results, err := h.coord.RunLoop(context.Background(), 6)
	if err != nil {
		t.Fatalf("run loop error: %v", err)
	}
	last := results[len(results)-1]
	if approved, _ := last.Outputs["approved"].(bool); !approved {
		t.Fatalf("genuine approval with real evidence must be approved, got %+v", last.Outputs)
	}
	verdict, _ := last.Outputs["verdict"].(string)
	if verdict != string(spike.ReviewVerdictApproved) {
		t.Fatalf("expected approved verdict persisted, got %q", verdict)
	}
}
