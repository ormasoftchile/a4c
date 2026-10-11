package a4c_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ormasoftchile/a4c/pkg/a4c"
)

// Scenario A — Rogue Reviewer:
// Verify that when an implementation exists with uncommitted tracked changes and newly created untracked files,
// a rogue reviewer attempting mutations is rejected without any loss of implementation state.
func TestAdversarial_ScenarioA_RogueReviewerWorkPreservation(t *testing.T) {
	h := setupInvisibleTeamHarness(t)
	ctx := context.Background()

	fake := a4c.NewFakeProvider()
	h.coordinator.RegisterProvider("fake", fake)
	h.coordinator.SetDefaultProvider(fake)

	const trackedContentBefore = "package main\n// uncommitted modification by coder\nfunc Work() string { return \"coder_work\" }\n"
	const untrackedContentBefore = "package main\n// newly created untracked source file\nfunc Untracked() int { return 42 }\n"

	fake.SessionResponseFunc = func(ctx context.Context, session *a4c.FakeSession, prompt string) (*a4c.AgentMessage, error) {
		agentID := session.Config().AgentID
		wtPath := session.Config().WorkspaceDir
		if wtPath == "" {
			wtPath = h.repoDir
		}

		if agentID == "coder" {
			// Coder modifies an existing file without committing it
			_ = os.WriteFile(filepath.Join(wtPath, "work.go"), []byte(trackedContentBefore), 0644)
			// Coder creates a brand new untracked file
			_ = os.WriteFile(filepath.Join(wtPath, "untracked.go"), []byte(untrackedContentBefore), 0644)
			return &a4c.AgentMessage{Content: "Coder completed implementation with uncommitted and untracked files"}, nil
		}

		if agentID == "reviewer" {
			// Malicious/rogue reviewer attempts to mutate files in its workspace
			_ = os.WriteFile(filepath.Join(wtPath, "rogue_file.go"), []byte("package main\n// unauthorized reviewer write\n"), 0644)
			_ = os.WriteFile(filepath.Join(wtPath, "work.go"), []byte("package main\n// tampered by reviewer!\n"), 0644)

			if session.Config().OnSubmitReview != nil {
				session.Config().OnSubmitReview(a4c.ReviewResult{
					Verdict:   a4c.ReviewVerdictApproved,
					Approved:  true,
					Comments:  "Rogue reviewer approves while tampering with code",
					Timestamp: time.Now(),
				})
			}
			return &a4c.AgentMessage{Content: "Tampered and approved [REVIEW_APPROVED]"}, nil
		}
		return &a4c.AgentMessage{Content: "Done"}, nil
	}

	// 1. Coder executes implementation
	coderReq := a4c.ActivityRequest{
		ID:        "rogue-task-coder",
		AgentID:   "coder",
		Objective: "Implement feature with uncommitted and untracked files",
		Budget:    a4c.Budget{Allocated: 50000},
		CreatedAt: time.Now(),
	}
	_ = h.store.Enqueue(coderReq)

	coderRes, err := h.coordinator.Step(ctx)
	if err != nil {
		t.Fatalf("coder step failed: %v", err)
	}
	if coderRes.Status != a4c.StatusCompleted {
		t.Fatalf("expected coder completed, got: %s", coderRes.Status)
	}

	// Capture exact baseline bytes in authoritative implementation worktree
	workGoPath := filepath.Join(h.repoDir, "work.go")
	untrackedGoPath := filepath.Join(h.repoDir, "untracked.go")

	workDataBefore, err := os.ReadFile(workGoPath)
	if err != nil {
		t.Fatalf("failed to read work.go before review: %v", err)
	}
	untrackedDataBefore, err := os.ReadFile(untrackedGoPath)
	if err != nil {
		t.Fatalf("failed to read untracked.go before review: %v", err)
	}

	// 2. Reviewer executes
	revReq := a4c.ActivityRequest{
		ID:        "rogue-task-reviewer",
		AgentID:   "reviewer",
		Objective: "Review feature implementation",
		DependsOn: []string{"rogue-task-coder"},
		Budget:    a4c.Budget{Allocated: 30000},
		CreatedAt: time.Now(),
	}
	_ = h.store.Enqueue(revReq)

	revRes, err := h.coordinator.Step(ctx)
	if err != nil {
		t.Fatalf("reviewer step failed: %v", err)
	}

	// 3. Verification of Reviewer Outcome
	approved, _ := revRes.Outputs["approved"].(bool)
	mutDetected, _ := revRes.Outputs["reviewer_mutation_detected"].(bool)
	verdict, _ := revRes.Outputs["verdict"].(string)

	if approved {
		t.Fatalf("INVARIANT VIOLATION: Review must NOT be approved when reviewer mutates workspace!")
	}
	if !mutDetected {
		t.Fatalf("INVARIANT VIOLATION: Expected reviewer_mutation_detected to be true!")
	}
	if verdict != string(a4c.ReviewVerdictRejected) {
		t.Errorf("expected verdict %s, got %s", a4c.ReviewVerdictRejected, verdict)
	}

	// 4. Verification of Work Preservation in Authoritative Worktree
	workDataAfter, err := os.ReadFile(workGoPath)
	if err != nil {
		t.Fatalf("INVARIANT VIOLATION: work.go was deleted or lost: %v", err)
	}
	if string(workDataAfter) != string(workDataBefore) {
		t.Fatalf("INVARIANT VIOLATION: work.go was corrupted or reset!\nExpected:\n%s\nGot:\n%s",
			string(workDataBefore), string(workDataAfter))
	}

	untrackedDataAfter, err := os.ReadFile(untrackedGoPath)
	if err != nil {
		t.Fatalf("INVARIANT VIOLATION: untracked.go was deleted by recovery logic: %v", err)
	}
	if string(untrackedDataAfter) != string(untrackedDataBefore) {
		t.Fatalf("INVARIANT VIOLATION: untracked.go content altered!\nExpected:\n%s\nGot:\n%s",
			string(untrackedDataBefore), string(untrackedDataAfter))
	}

	// 5. Verify rogue file was NOT written into the authoritative implementation worktree
	rogueInAuthoritative := filepath.Join(h.repoDir, "rogue_file.go")
	if _, err := os.Stat(rogueInAuthoritative); !os.IsNotExist(err) {
		t.Fatalf("INVARIANT VIOLATION: rogue_file.go exists in authoritative worktree!")
	}
}

// Scenario B — Missing, Malformed, Timeout, and Explicit Review Verdicts:
// Verify review fails closed in all non-explicit-approval situations.
func TestAdversarial_ScenarioB_ExplicitReviewFailClosed(t *testing.T) {
	ctx := context.Background()

	setupCase := func(t *testing.T) (*serviceTestHarness, *a4c.FakeProvider) {
		h := setupInvisibleTeamHarness(t)
		fake := a4c.NewFakeProvider()
		h.coordinator.RegisterProvider("fake", fake)
		h.coordinator.SetDefaultProvider(fake)

		// Step 0: Coder implements and tests pass
		fake.SessionResponseFunc = func(ctx context.Context, session *a4c.FakeSession, prompt string) (*a4c.AgentMessage, error) {
			return &a4c.AgentMessage{Content: "Coder completed"}, nil
		}
		coderReq := a4c.ActivityRequest{
			ID:        "b-coder",
			AgentID:   "coder",
			Objective: "Implement B",
			Budget:    a4c.Budget{Allocated: 20000},
			CreatedAt: time.Now(),
		}
		_ = h.store.Enqueue(coderReq)
		_, _ = h.coordinator.Step(ctx)
		return h, fake
	}

	// Case 1: Missing verdict (model returns plain discussion without calling review tool or directive)
	t.Run("MissingVerdict_FailsClosed", func(t *testing.T) {
		h, fake := setupCase(t)
		fake.SessionResponseFunc = func(ctx context.Context, session *a4c.FakeSession, prompt string) (*a4c.AgentMessage, error) {
			return &a4c.AgentMessage{Content: "I have examined the code and it looks interesting. What do you think?"}, nil
		}
		revReq := a4c.ActivityRequest{
			ID:        "b-rev-missing",
			AgentID:   "reviewer",
			Objective: "Review B",
			DependsOn: []string{"b-coder"},
			Budget:    a4c.Budget{Allocated: 15000},
			CreatedAt: time.Now(),
		}
		_ = h.store.Enqueue(revReq)
		res, err := h.coordinator.Step(ctx)
		if err != nil {
			t.Fatalf("step error: %v", err)
		}
		approved, _ := res.Outputs["approved"].(bool)
		verdict, _ := res.Outputs["verdict"].(string)
		if approved {
			t.Fatalf("INVARIANT VIOLATION: Missing review verdict must NOT produce approval!")
		}
		if verdict != string(a4c.ReviewVerdictIncomplete) {
			t.Errorf("expected verdict %s, got: %s", a4c.ReviewVerdictIncomplete, verdict)
		}
	})

	// Case 2: Session error / timeout / cancellation
	t.Run("ExecutionError_FailsClosed", func(t *testing.T) {
		h, fake := setupCase(t)
		fake.SessionResponseFunc = func(ctx context.Context, session *a4c.FakeSession, prompt string) (*a4c.AgentMessage, error) {
			return nil, errors.New("provider timeout during review execution")
		}
		revReq := a4c.ActivityRequest{
			ID:        "b-rev-timeout",
			AgentID:   "reviewer",
			Objective: "Review B",
			DependsOn: []string{"b-coder"},
			Budget:    a4c.Budget{Allocated: 15000},
			CreatedAt: time.Now(),
		}
		_ = h.store.Enqueue(revReq)
		res, err := h.coordinator.Step(ctx)
		if err == nil {
			t.Fatalf("expected error from step on session failure")
		}
		if res != nil {
			approved, _ := res.Outputs["approved"].(bool)
			if approved {
				t.Fatalf("INVARIANT VIOLATION: Session failure must NOT produce approval!")
			}
		}
	})

	// Case 3: Explicit rejection
	t.Run("ExplicitRejection_Rejected", func(t *testing.T) {
		h, fake := setupCase(t)
		fake.SessionResponseFunc = func(ctx context.Context, session *a4c.FakeSession, prompt string) (*a4c.AgentMessage, error) {
			if session.Config().OnSubmitReview != nil {
				session.Config().OnSubmitReview(a4c.ReviewResult{
					Verdict:   a4c.ReviewVerdictRejected,
					Approved:  false,
					Comments:  "Contains unhandled error case",
					Blockers:  []string{"missing nil check"},
					Timestamp: time.Now(),
				})
			}
			return &a4c.AgentMessage{Content: "Rejected [REVIEW_REJECTED: missing nil check]"}, nil
		}
		revReq := a4c.ActivityRequest{
			ID:        "b-rev-reject",
			AgentID:   "reviewer",
			Objective: "Review B",
			DependsOn: []string{"b-coder"},
			Budget:    a4c.Budget{Allocated: 15000},
			CreatedAt: time.Now(),
		}
		_ = h.store.Enqueue(revReq)
		res, err := h.coordinator.Step(ctx)
		if err != nil {
			t.Fatalf("step error: %v", err)
		}
		approved, _ := res.Outputs["approved"].(bool)
		verdict, _ := res.Outputs["verdict"].(string)
		if approved {
			t.Fatalf("INVARIANT VIOLATION: Explicit rejection must NOT produce approval!")
		}
		if verdict != string(a4c.ReviewVerdictRejected) {
			t.Errorf("expected verdict %s, got %s", a4c.ReviewVerdictRejected, verdict)
		}
	})

	// Case 4: Explicit approval with valid evidence
	t.Run("ExplicitApproval_Approved", func(t *testing.T) {
		h, fake := setupCase(t)
		fake.SessionResponseFunc = func(ctx context.Context, session *a4c.FakeSession, prompt string) (*a4c.AgentMessage, error) {
			if session.Config().OnSubmitReview != nil {
				session.Config().OnSubmitReview(a4c.ReviewResult{
					Verdict:   a4c.ReviewVerdictApproved,
					Approved:  true,
					Comments:  "All invariants verified",
					Timestamp: time.Now(),
				})
			}
			return &a4c.AgentMessage{Content: "Approved [REVIEW_APPROVED]"}, nil
		}
		revReq := a4c.ActivityRequest{
			ID:        "b-rev-approve",
			AgentID:   "reviewer",
			Objective: "Review B",
			DependsOn: []string{"b-coder"},
			Budget:    a4c.Budget{Allocated: 15000},
			CreatedAt: time.Now(),
		}
		_ = h.store.Enqueue(revReq)
		res, err := h.coordinator.Step(ctx)
		if err != nil {
			t.Fatalf("step error: %v", err)
		}
		approved, _ := res.Outputs["approved"].(bool)
		verdict, _ := res.Outputs["verdict"].(string)
		if !approved {
			t.Fatalf("expected approval, got not approved: %v", res.Outputs["review_comments"])
		}
		if verdict != string(a4c.ReviewVerdictApproved) {
			t.Errorf("expected verdict %s, got %s", a4c.ReviewVerdictApproved, verdict)
		}
	})
}

// Scenario C — False Validation Evidence:
// Verify that injected incomplete metadata, failed test records, or stale snapshots are never fabricated as valid success.
func TestAdversarial_ScenarioC_NoFabricatedValidationEvidence(t *testing.T) {
	ctx := context.Background()

	// Case 1: Incomplete metadata (snapshot string present, but authoritative evidence object missing)
	t.Run("IncompleteMetadata_Rejected", func(t *testing.T) {
		h := setupInvisibleTeamHarness(t)
		fake := a4c.NewFakeProvider()
		h.coordinator.RegisterProvider("fake", fake)
		h.coordinator.SetDefaultProvider(fake)
		fake.SessionResponseFunc = func(ctx context.Context, session *a4c.FakeSession, prompt string) (*a4c.AgentMessage, error) {
			if session.Config().OnSubmitReview != nil {
				session.Config().OnSubmitReview(a4c.ReviewResult{
					Verdict:   a4c.ReviewVerdictApproved,
					Approved:  true,
					Comments:  "Attempting approval",
					Timestamp: time.Now(),
				})
			}
			return &a4c.AgentMessage{Content: "Approve [REVIEW_APPROVED]"}, nil
		}

		coderReq := a4c.ActivityRequest{
			ID:        "c1-coder",
			AgentID:   "coder",
			Objective: "Write code",
			CreatedAt: time.Now(),
		}
		_ = h.store.Enqueue(coderReq)
		// Fake completed result with incomplete metadata
		_ = h.store.Complete("c1-coder", a4c.ActivityResult{
			ID:     "c1-coder",
			Status: a4c.StatusCompleted,
			Outputs: map[string]any{
				"snapshot_id": "snap-fabricated-without-evidence",
			},
		})

		revReq := a4c.ActivityRequest{
			ID:        "c1-reviewer",
			AgentID:   "reviewer",
			Objective: "Review C1",
			DependsOn: []string{"c1-coder"},
			CreatedAt: time.Now(),
		}
		_ = h.store.Enqueue(revReq)

		res, err := h.coordinator.Step(ctx)
		if err != nil {
			t.Fatalf("reviewer step failed: %v", err)
		}
		approved, _ := res.Outputs["approved"].(bool)
		verdict, _ := res.Outputs["verdict"].(string)
		if approved {
			t.Fatalf("INVARIANT VIOLATION: Review must NOT approve incomplete validation metadata!")
		}
		if verdict != string(a4c.ReviewVerdictRejected) && verdict != string(a4c.ReviewVerdictIncomplete) {
			t.Errorf("expected rejected/incomplete, got: %s", verdict)
		}
	})

	// Case 2: Failed test evidence (authoritative evidence explicitly recorded exit code 1)
	t.Run("FailedTestEvidence_Rejected", func(t *testing.T) {
		h := setupInvisibleTeamHarness(t)
		fake := a4c.NewFakeProvider()
		h.coordinator.RegisterProvider("fake", fake)
		h.coordinator.SetDefaultProvider(fake)
		fake.SessionResponseFunc = func(ctx context.Context, session *a4c.FakeSession, prompt string) (*a4c.AgentMessage, error) {
			if session.Config().OnSubmitReview != nil {
				session.Config().OnSubmitReview(a4c.ReviewResult{
					Verdict:   a4c.ReviewVerdictApproved,
					Approved:  true,
					Comments:  "Attempting approval",
					Timestamp: time.Now(),
				})
			}
			return &a4c.AgentMessage{Content: "Approve [REVIEW_APPROVED]"}, nil
		}

		currentSnap, _ := a4c.NewWorkspaceSnapshotter().ComputeSnapshot(h.repoDir)

		coderReq := a4c.ActivityRequest{
			ID:        "c2-coder",
			AgentID:   "coder",
			Objective: "Failing test activity",
			CreatedAt: time.Now(),
		}
		_ = h.store.Enqueue(coderReq)

		failedEvidence := a4c.ValidationEvidence{
			EvidenceID:          "evi-c2-failed",
			ActivityID:          "c2-coder",
			WorkspaceSnapshotID: currentSnap,
			Command:             []string{"go", "test", "./..."},
			ExitCode:            1, // FAILED
			Valid:               false,
			StartedAt:           time.Now(),
			CompletedAt:         time.Now(),
		}

		_ = h.store.Complete("c2-coder", a4c.ActivityResult{
			ID:          "c2-coder",
			Status:      a4c.StatusCompleted,
			EvidenceIDs: []string{failedEvidence.EvidenceID},
			Outputs: map[string]any{
				"snapshot_id":         currentSnap,
				"validation_evidence": failedEvidence,
			},
		})

		revReq := a4c.ActivityRequest{
			ID:        "c2-reviewer",
			AgentID:   "reviewer",
			Objective: "Review C2",
			DependsOn: []string{"c2-coder"},
			CreatedAt: time.Now(),
		}
		_ = h.store.Enqueue(revReq)

		res, err := h.coordinator.Step(ctx)
		if err != nil {
			t.Fatalf("reviewer step failed: %v", err)
		}
		approved, _ := res.Outputs["approved"].(bool)
		comments, _ := res.Outputs["review_comments"].(string)
		if approved {
			t.Fatalf("INVARIANT VIOLATION: Review must NEVER approve failed test evidence!")
		}
		if !strings.Contains(comments, "failing") && !strings.Contains(comments, "exit code") {
			t.Errorf("expected comments to cite failing test, got: %s", comments)
		}
	})
}

// Scenario D — Stale Approval:
// Verify that if a task is approved at snapshot S1, modifying the worktree to S2 invalidates approval and denies integration.
func TestAdversarial_ScenarioD_StaleApprovalDeniesIntegration(t *testing.T) {
	h := setupInvisibleTeamHarness(t)
	ctx := context.Background()

	// 1. Submit and complete task
	ref, err := h.service.SubmitTask(ctx, a4c.SubmitTaskRequest{
		Objective: "Feature D",
		Strategy:  a4c.StrategyDirect,
	})
	if err != nil {
		t.Fatalf("submit failed: %v", err)
	}

	fake := a4c.NewFakeProvider()
	h.coordinator.RegisterProvider("fake", fake)
	h.coordinator.SetDefaultProvider(fake)
	fake.SessionResponseFunc = func(ctx context.Context, session *a4c.FakeSession, prompt string) (*a4c.AgentMessage, error) {
		return &a4c.AgentMessage{Content: "Feature D implemented"}, nil
	}

	_, err = h.service.ExecuteTask(ctx, ref.TaskID, 1)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	// 2. Human accepts task at snapshot S1
	acceptRes, err := h.service.AcceptTask(ctx, a4c.AcceptTaskRequest{
		TaskID:        ref.TaskID,
		Comments:      "Approved at S1",
		IsInteractive: true,
	})
	if err != nil {
		t.Fatalf("accept failed: %v", err)
	}
	s1 := acceptRes.AcceptedSnapshot

	// 3. Unauthorized mutation after approval!
	wtRecord, err := h.wtMgr.GetWorktree(ref.TaskID)
	if err != nil {
		t.Fatalf("get worktree failed: %v", err)
	}
	_ = os.WriteFile(filepath.Join(wtRecord.WorktreePath, "stale_mutation.go"), []byte("package main\n// unapproved post-acceptance mutation\n"), 0644)

	// 4. Attempt integration
	_, err = h.service.IntegrateTask(ctx, a4c.IntegrateTaskRequest{
		TaskID: ref.TaskID,
	})
	if err == nil {
		t.Fatalf("INVARIANT VIOLATION: Integration must be DENIED when worktree was modified after acceptance!")
	}
	if !errors.Is(err, a4c.ErrSnapshotMismatch) {
		t.Errorf("expected ErrSnapshotMismatch, got: %v", err)
	}

	// 5. Verify base working checkout was NOT modified
	staleInRoot := filepath.Join(h.repoDir, "stale_mutation.go")
	if _, err := os.Stat(staleInRoot); !os.IsNotExist(err) {
		t.Errorf("INVARIANT VIOLATION: Post-approval mutation leaked into base repository checkout!")
	}
	_ = s1
}

// Scenario E — Unauthorized Acceptance and Trust Boundary:
// Verify agent self-acceptance and non-interactive unauthenticated acceptance are strictly rejected.
func TestAdversarial_ScenarioE_HumanAuthorizationBoundary(t *testing.T) {
	h := setupInvisibleTeamHarness(t)
	ctx := context.Background()

	ref, err := h.service.SubmitTask(ctx, a4c.SubmitTaskRequest{
		Objective: "Feature E",
		Strategy:  a4c.StrategyDirect,
	})
	if err != nil {
		t.Fatalf("submit failed: %v", err)
	}

	fake := a4c.NewFakeProvider()
	h.coordinator.RegisterProvider("fake", fake)
	h.coordinator.SetDefaultProvider(fake)
	fake.SessionResponseFunc = func(ctx context.Context, session *a4c.FakeSession, prompt string) (*a4c.AgentMessage, error) {
		return &a4c.AgentMessage{Content: "Feature E implemented"}, nil
	}
	_, _ = h.service.ExecuteTask(ctx, ref.TaskID, 1)

	// Case 1: Agent tries to self-accept with agent role ("coder", "reviewer", "lead")
	for _, agentRole := range []string{"coder", "reviewer", "lead", "agent"} {
		_, err := h.service.AcceptTask(ctx, a4c.AcceptTaskRequest{
			TaskID:        ref.TaskID,
			AcceptedBy:    agentRole,
			IsInteractive: true,
		})
		if err == nil {
			t.Fatalf("INVARIANT VIOLATION: Agent role %q must NOT be authorized to accept task!", agentRole)
		}
		if !strings.Contains(err.Error(), "unauthorized") {
			t.Errorf("expected unauthorized error for role %s, got: %v", agentRole, err)
		}
	}

	// Case 2: Automated non-interactive invocation without token is rejected
	_, err = h.service.AcceptTask(ctx, a4c.AcceptTaskRequest{
		TaskID:            ref.TaskID,
		AcceptedBy:        "human_cli",
		IsInteractive:     false,
		ConfirmationToken: "",
	})
	if err == nil {
		t.Fatalf("INVARIANT VIOLATION: Non-interactive accept without confirmation token must be rejected!")
	}
	if !strings.Contains(err.Error(), "non-interactive invocation requires explicit human authorization token") {
		t.Errorf("expected token requirement error, got: %v", err)
	}

	// Case 3: Out-of-band confirmation token succeeds
	res, err := h.service.AcceptTask(ctx, a4c.AcceptTaskRequest{
		TaskID:            ref.TaskID,
		AcceptedBy:        "human_cli",
		ConfirmationToken: "human-out-of-band-secret-auth",
		IsInteractive:     false,
	})
	if err != nil {
		t.Fatalf("accept with out-of-band token failed: %v", err)
	}
	if res.AuthorizationMethod != "out_of_band_token" {
		t.Errorf("expected auth method out_of_band_token, got: %s", res.AuthorizationMethod)
	}
}

// Scenario F — Normal Mixed-Host Execution:
// Verify implementation on coder host, independent review on reviewer host, snapshot binding,
// transition to awaiting human review, and no automatic impersonation of human acceptance.
func TestAdversarial_ScenarioF_NormalMixedHostExecution(t *testing.T) {
	h := setupInvisibleTeamHarness(t)
	ctx := context.Background()

	coderHost := a4c.NewFakeProvider()
	reviewerHost := a4c.NewFakeProvider()
	h.coordinator.RegisterProvider("opencode", coderHost)
	h.coordinator.RegisterProvider("copilot", reviewerHost)

	coderRan := false
	coderHost.SessionResponseFunc = func(ctx context.Context, session *a4c.FakeSession, prompt string) (*a4c.AgentMessage, error) {
		coderRan = true
		wtPath := session.Config().WorkspaceDir
		_ = os.WriteFile(filepath.Join(wtPath, "mixed_feature.go"), []byte("package main\nfunc MixedFeature() {}\n"), 0644)
		return &a4c.AgentMessage{Content: "Feature written by opencode coder"}, nil
	}

	reviewerRan := false
	reviewerHost.SessionResponseFunc = func(ctx context.Context, session *a4c.FakeSession, prompt string) (*a4c.AgentMessage, error) {
		reviewerRan = true
		if session.Config().OnSubmitReview != nil {
			session.Config().OnSubmitReview(a4c.ReviewResult{
				Verdict:   a4c.ReviewVerdictApproved,
				Approved:  true,
				Comments:  "Verified independently by copilot reviewer",
				Timestamp: time.Now(),
			})
		}
		return &a4c.AgentMessage{Content: "Review approved [REVIEW_APPROVED]"}, nil
	}

	ref, err := h.service.SubmitTask(ctx, a4c.SubmitTaskRequest{
		Objective: "Mixed host feature F",
		Strategy:  a4c.StrategyReviewed,
		Runtime:   "opencode",
		Inputs: map[string]any{
			"agent_runtimes": map[string]string{
				"coder":    "opencode",
				"reviewer": "copilot",
			},
			"reviewer_runtime": "copilot",
		},
	})
	if err != nil {
		t.Fatalf("submit failed: %v", err)
	}

	// Execute coder and reviewer steps
	results, err := h.service.ExecuteTask(ctx, ref.TaskID, 5)
	if err != nil {
		t.Fatalf("execute task failed: %v", err)
	}

	if !coderRan {
		t.Errorf("expected opencode coder to execute")
	}
	if !reviewerRan {
		t.Errorf("expected copilot reviewer to execute")
	}

	if len(results) < 2 {
		t.Fatalf("expected at least 2 activity results, got %d", len(results))
	}

	// Verify task transitioned to awaiting_human_review
	taskDetails, err := h.service.GetTask(ctx, ref.TaskID)
	if err != nil {
		t.Fatalf("get task failed: %v", err)
	}
	if taskDetails.State != a4c.TaskStateAwaitingHumanReview {
		t.Fatalf("expected state %s, got %s", a4c.TaskStateAwaitingHumanReview, taskDetails.State)
	}

	// Verify INVARIANT: No automatic human acceptance or integration occurred!
	// Task must not be integrated
	intRes, err := h.service.IntegrateTask(ctx, a4c.IntegrateTaskRequest{TaskID: ref.TaskID})
	if err == nil {
		t.Fatalf("INVARIANT VIOLATION: Task must NOT be integrated before explicit human acceptance!")
	}
	_ = intRes
}
