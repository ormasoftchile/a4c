package a4c_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	spike "github.com/ormasoftchile/a4c/pkg/a4c"
)

// TestDogfood_01_EndToEndWorktreeCollaboration tests full 3-agent collaboration in an isolated worktree.
func TestDogfood_01_EndToEndWorktreeCollaboration(t *testing.T) {
	repoDir := setupTestGitRepo(t)
	stateDir := filepath.Join(repoDir, ".a4c", "worktrees")
	wtMgr, err := spike.NewWorktreeManager(repoDir, stateDir)
	if err != nil {
		t.Fatalf("failed to create WorktreeManager: %v", err)
	}

	dir := t.TempDir()
	store, _ := spike.NewDurableActivityStore(filepath.Join(dir, "activities"))
	memory, _ := spike.NewDurableMemoryStore(filepath.Join(dir, "memory"))
	roster, _ := spike.DefaultTeamRoster(filepath.Join(dir, "roster"))
	gov := spike.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_propose_activity",
		"governed_submit_review", "governed_request_human_decision", "governed_propose_memory",
	})
	journal, _ := spike.NewDurableIntentJournal(filepath.Join(dir, "journal"))
	fakeProvider := spike.NewFakeProvider()
	runner := &spike.NativeGoTestRunner{}

	var executionAgents []string
	var coderWorktreeDir string

	fakeProvider.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		agentID := session.Config().AgentID
		executionAgents = append(executionAgents, agentID)
		activeDir := session.Config().WorkspaceDir

		switch agentID {
		case "lead":
			// Lead delegates to coder and reviewer
			coderID := "lead-coder-task"
			session.Config().OnProposeActivity(spike.ActivityProposal{
				ProposalID:      coderID,
				ProposedAgentID: "coder",
				Objective:       "Implement MathMultiply function",
				RequestedBudget: spike.Budget{Allocated: 5000},
			})
			session.Config().OnProposeActivity(spike.ActivityProposal{
				ProposalID:      "lead-reviewer-task",
				ProposedAgentID: "reviewer",
				Objective:       "Verify MathMultiply implementation",
				RequestedBudget: spike.Budget{Allocated: 3000},
				DependsOn:       []string{coderID},
			})
			return &spike.AgentMessage{Content: "Delegated subtasks"}, nil

		case "coder":
			coderWorktreeDir = activeDir
			// Coder writes Go code in the isolated worktree
			_ = os.WriteFile(filepath.Join(activeDir, "go.mod"), []byte("module example.com/calc\n\ngo 1.22\n"), 0644)
			_ = os.WriteFile(filepath.Join(activeDir, "calc.go"), []byte("package calc\n\nfunc Multiply(a, b int) int { return a * b }\n"), 0644)
			_ = os.WriteFile(filepath.Join(activeDir, "calc_test.go"), []byte("package calc\n\nimport \"testing\"\nfunc TestMultiply(t *testing.T) { if Multiply(3,4) != 12 { t.Fatal(\"err\") } }\n"), 0644)
			return &spike.AgentMessage{Content: "Implemented Multiply"}, nil

		case "reviewer":
			if session.Config().OnSubmitReview != nil {
				session.Config().OnSubmitReview(spike.ReviewResult{
					Approved: true,
					Comments: "Approved. All changes meet specification and native tests pass.",
				})
			}
			return &spike.AgentMessage{Content: "Review approved"}, nil
		}
		return &spike.AgentMessage{Content: "Done"}, nil
	}

	coord := spike.NewTeamCoordinator(repoDir, store, roster, memory, gov, journal, runner, fakeProvider)
	coord.SetWorktreeManager(wtMgr)

	taskReq, err := coord.SubmitTask("Implement Multiply feature", spike.Budget{Allocated: 20000}, nil)
	if err != nil {
		t.Fatalf("failed to submit task: %v", err)
	}

	results, err := coord.RunLoop(context.Background(), 10)
	if err != nil {
		t.Fatalf("run loop error: %v", err)
	}

	if len(results) != 3 { // Lead, Coder, Reviewer
		t.Fatalf("expected 3 completed activities, got %d", len(results))
	}

	// Verify worktree was isolated
	if coderWorktreeDir == repoDir {
		t.Fatalf("coder executed in root repo instead of isolated worktree!")
	}
	if !strings.Contains(coderWorktreeDir, taskReq.ID) {
		t.Fatalf("worktree path %s did not contain task ID %s", coderWorktreeDir, taskReq.ID)
	}

	// Verify root repo was never mutated
	if _, err := os.Stat(filepath.Join(repoDir, "calc.go")); !os.IsNotExist(err) {
		t.Fatalf("root repo was contaminated with calc.go!")
	}

	// Verify reviewer output contains proposed commit
	lastRes := results[len(results)-1]
	if commit, ok := lastRes.Outputs["proposed_commit"].(string); !ok || !strings.Contains(commit, "feat:") {
		t.Fatalf("expected proposed commit in reviewer output, got %+v", lastRes.Outputs)
	}

	// Verify worktree status is released
	wtRec, err := wtMgr.GetWorktree(taskReq.ID)
	if err != nil {
		t.Fatalf("failed to get worktree record: %v", err)
	}
	if wtRec.Status != spike.WorktreeStatusReleased {
		t.Fatalf("expected worktree status released, got %s", wtRec.Status)
	}
}

// TestDogfood_02_ReviewerRejectionAndBoundedRevisionCycle verifies rejection dispatch and DAG re-review.
func TestDogfood_02_ReviewerRejectionAndBoundedRevisionCycle(t *testing.T) {
	dir := t.TempDir()
	workspaceDir := filepath.Join(dir, "repo")
	_ = os.MkdirAll(workspaceDir, 0755)

	store, _ := spike.NewDurableActivityStore(filepath.Join(dir, "activities"))
	memory, _ := spike.NewDurableMemoryStore(filepath.Join(dir, "memory"))
	roster, _ := spike.DefaultTeamRoster(filepath.Join(dir, "roster"))
	gov := spike.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_propose_activity",
		"governed_submit_review", "governed_request_human_decision", "governed_propose_memory",
	})
	journal, _ := spike.NewDurableIntentJournal(filepath.Join(dir, "journal"))
	fakeProvider := spike.NewFakeProvider()
	runner := &spike.NativeGoTestRunner{}

	reviewAttempts := 0
	fakeProvider.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		agentID := session.Config().AgentID
		switch agentID {
		case "lead":
			session.Config().OnProposeActivity(spike.ActivityProposal{
				ProposalID:      "sub-coder-1",
				ProposedAgentID: "coder",
				Objective:       "Implement Div function",
				RequestedBudget: spike.Budget{Allocated: 5000},
			})
			session.Config().OnProposeActivity(spike.ActivityProposal{
				ProposalID:      "sub-reviewer-1",
				ProposedAgentID: "reviewer",
				Objective:       "Review Div function",
				RequestedBudget: spike.Budget{Allocated: 3000},
				DependsOn:       []string{"sub-coder-1"},
			})
			return &spike.AgentMessage{Content: "Delegated"}, nil

		case "coder":
			// Always writes valid code
			_ = os.WriteFile(filepath.Join(workspaceDir, "go.mod"), []byte("module example.com/calc\n\ngo 1.22\n"), 0644)
			_ = os.WriteFile(filepath.Join(workspaceDir, "calc.go"), []byte("package calc\n\nfunc Div(a, b int) int { return a / b }\n"), 0644)
			_ = os.WriteFile(filepath.Join(workspaceDir, "calc_test.go"), []byte("package calc\n\nimport \"testing\"\nfunc TestDiv(t *testing.T) { if Div(6,2) != 3 { t.Fatal(\"err\") } }\n"), 0644)
			return &spike.AgentMessage{Content: "Div implemented"}, nil

		case "reviewer":
			reviewAttempts++
			if reviewAttempts == 1 {
				// First review rejects: missing division by zero handling
				session.Config().OnSubmitReview(spike.ReviewResult{
					Approved: false,
					Comments: "Missing division by zero check",
				})
				return &spike.AgentMessage{Content: "Rejected initially"}, nil
			}
			// Second review approves
			session.Config().OnSubmitReview(spike.ReviewResult{
				Approved: true,
				Comments: "Approved after revision",
			})
			return &spike.AgentMessage{Content: "Approved"}, nil
		}
		return &spike.AgentMessage{Content: "Done"}, nil
	}

	coord := spike.NewTeamCoordinator(workspaceDir, store, roster, memory, gov, journal, runner, fakeProvider)
	_, _ = coord.SubmitTask("Implement Div function safely", spike.Budget{Allocated: 20000}, nil)

	// Step sequence:
	// Step 1: Lead -> enqueues sub-coder-1 and sub-reviewer-1
	// Step 2: Coder sub-coder-1 -> succeeds with evidence
	// Step 3: Reviewer sub-reviewer-1 -> rejects! Enqueues revision-1 (coder) and rereview-1 (reviewer)
	// Step 4: Coder revision-1 -> succeeds with fresh evidence
	// Step 5: Reviewer rereview-1 -> approves!
	results, err := coord.RunLoop(context.Background(), 10)
	if err != nil {
		t.Fatalf("run loop error: %v", err)
	}

	if len(results) != 5 {
		t.Fatalf("expected 5 steps in revision cycle, got %d", len(results))
	}

	if reviewAttempts != 2 {
		t.Fatalf("expected 2 review attempts, got %d", reviewAttempts)
	}

	finalReviewRes := results[len(results)-1]
	if approved, ok := finalReviewRes.Outputs["approved"].(bool); !ok || !approved {
		t.Fatalf("expected final review to be approved, got %+v", finalReviewRes.Outputs)
	}
}

// TestDogfood_03_MandatoryEvidenceEnforcement verifies model approval cannot override missing native evidence.
func TestDogfood_03_MandatoryEvidenceEnforcement(t *testing.T) {
	dir := t.TempDir()
	workspaceDir := filepath.Join(dir, "repo")
	_ = os.MkdirAll(workspaceDir, 0755)

	store, _ := spike.NewDurableActivityStore(filepath.Join(dir, "activities"))
	memory, _ := spike.NewDurableMemoryStore(filepath.Join(dir, "memory"))
	roster, _ := spike.DefaultTeamRoster(filepath.Join(dir, "roster"))
	gov := spike.NewPolicyGovernor([]string{"governed_submit_review"})
	journal, _ := spike.NewDurableIntentJournal(filepath.Join(dir, "journal"))
	fakeProvider := spike.NewFakeProvider()
	runner := &spike.NativeGoTestRunner{}

	fakeProvider.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		session.Config().OnSubmitReview(spike.ReviewResult{
			Approved: true,
			Comments: "LGTM!",
		})
		return &spike.AgentMessage{Content: "Approved"}, nil
	}

	coord := spike.NewTeamCoordinator(workspaceDir, store, roster, memory, gov, journal, runner, fakeProvider)
	
	// Enqueue reviewer activity directly WITHOUT any prior coder run / evidence
	revReq := spike.ActivityRequest{
		ID:        "rev-no-evidence",
		AgentID:   "reviewer",
		Objective: "Review unvalidated code",
		Budget:    spike.Budget{Allocated: 1000},
		CreatedAt: time.Now(),
	}
	_ = store.Enqueue(revReq)

	res, err := coord.Step(context.Background())
	if err != nil {
		t.Fatalf("step failed: %v", err)
	}

	// Reviewer approval must be overridden to false because no native evidence exists
	approved, _ := res.Outputs["approved"].(bool)
	if approved {
		t.Fatalf("coordinator allowed reviewer approval without native test evidence!")
	}
	comments, _ := res.Outputs["review_comments"].(string)
	if !strings.Contains(comments, "no valid native test evidence") {
		t.Fatalf("expected evidence rejection comment, got %q", comments)
	}
}

// TestDogfood_04_StaleEvidenceWorkspaceTampering verifies rejection if files change after validation.
func TestDogfood_04_StaleEvidenceWorkspaceTampering(t *testing.T) {
	dir := t.TempDir()
	workspaceDir := filepath.Join(dir, "repo")
	_ = os.MkdirAll(workspaceDir, 0755)

	store, _ := spike.NewDurableActivityStore(filepath.Join(dir, "activities"))
	memory, _ := spike.NewDurableMemoryStore(filepath.Join(dir, "memory"))
	roster, _ := spike.DefaultTeamRoster(filepath.Join(dir, "roster"))
	gov := spike.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_propose_activity",
		"governed_submit_review", "governed_request_human_decision", "governed_propose_memory",
	})
	journal, _ := spike.NewDurableIntentJournal(filepath.Join(dir, "journal"))
	fakeProvider := spike.NewFakeProvider()
	runner := &spike.NativeGoTestRunner{}

	fakeProvider.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		agentID := session.Config().AgentID
		if agentID == "coder" {
			_ = os.WriteFile(filepath.Join(workspaceDir, "go.mod"), []byte("module example.com/calc\n\ngo 1.22\n"), 0644)
			_ = os.WriteFile(filepath.Join(workspaceDir, "calc.go"), []byte("package calc\n\nfunc Ok() bool { return true }\n"), 0644)
			_ = os.WriteFile(filepath.Join(workspaceDir, "calc_test.go"), []byte("package calc\n\nimport \"testing\"\nfunc TestOk(t *testing.T) {}\n"), 0644)
			return &spike.AgentMessage{Content: "Coder done"}, nil
		}
		if agentID == "reviewer" {
			session.Config().OnSubmitReview(spike.ReviewResult{
				Approved: true,
				Comments: "Looks great",
			})
			return &spike.AgentMessage{Content: "Reviewed"}, nil
		}
		return &spike.AgentMessage{Content: "Done"}, nil
	}

	coord := spike.NewTeamCoordinator(workspaceDir, store, roster, memory, gov, journal, runner, fakeProvider)

	// Step 1: Coder runs and produces valid evidence
	coderReq := spike.ActivityRequest{
		ID:        "coder-tamper",
		AgentID:   "coder",
		Objective: "Write Ok function",
		Budget:    spike.Budget{Allocated: 2000},
		CreatedAt: time.Now(),
	}
	_ = store.Enqueue(coderReq)
	_, err := coord.Step(context.Background())
	if err != nil {
		t.Fatalf("coder step failed: %v", err)
	}

	// Tamper with file in workspace AFTER validation
	_ = os.WriteFile(filepath.Join(workspaceDir, "calc.go"), []byte("package calc\n// TAMPERED\n"), 0644)

	// Step 2: Reviewer runs
	revReq := spike.ActivityRequest{
		ID:        "rev-tamper",
		AgentID:   "reviewer",
		Objective: "Review Ok function",
		Budget:    spike.Budget{Allocated: 2000},
		DependsOn: []string{"coder-tamper"},
		CreatedAt: time.Now(),
	}
	_ = store.Enqueue(revReq)

	resRev, err := coord.Step(context.Background())
	if err != nil {
		t.Fatalf("reviewer step failed: %v", err)
	}

	// Reviewer must reject because snapshot hash mismatch
	approved, _ := resRev.Outputs["approved"].(bool)
	if approved {
		t.Fatalf("expected reviewer rejection due to workspace tampering after validation!")
	}
	comments, _ := resRev.Outputs["review_comments"].(string)
	if !strings.Contains(comments, "workspace modified after validation") {
		t.Fatalf("expected tampering comment, got: %q", comments)
	}
}

// TestDogfood_05_MaxRevisionsBoundedStop verifies that repeated rejections stop at MaxRevisions.
func TestDogfood_05_MaxRevisionsBoundedStop(t *testing.T) {
	repoDir := setupTestGitRepo(t)
	stateDir := filepath.Join(repoDir, ".a4c", "worktrees")
	wtMgr, _ := spike.NewWorktreeManager(repoDir, stateDir)

	dir := t.TempDir()
	store, _ := spike.NewDurableActivityStore(filepath.Join(dir, "activities"))
	memory, _ := spike.NewDurableMemoryStore(filepath.Join(dir, "memory"))
	roster, _ := spike.DefaultTeamRoster(filepath.Join(dir, "roster"))
	gov := spike.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_propose_activity",
		"governed_submit_review", "governed_request_human_decision", "governed_propose_memory",
	})
	journal, _ := spike.NewDurableIntentJournal(filepath.Join(dir, "journal"))
	fakeProvider := spike.NewFakeProvider()
	runner := &spike.NativeGoTestRunner{}

	fakeProvider.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		agentID := session.Config().AgentID
		activeDir := session.Config().WorkspaceDir
		if agentID == "lead" {
			session.Config().OnProposeActivity(spike.ActivityProposal{
				ProposalID:      "sub-coder",
				ProposedAgentID: "coder",
				Objective:       "Implement something",
				RequestedBudget: spike.Budget{Allocated: 2000},
			})
			session.Config().OnProposeActivity(spike.ActivityProposal{
				ProposalID:      "sub-reviewer",
				ProposedAgentID: "reviewer",
				Objective:       "Review something",
				RequestedBudget: spike.Budget{Allocated: 1000},
				DependsOn:       []string{"sub-coder"},
			})
			return &spike.AgentMessage{Content: "Delegated"}, nil
		}
		if agentID == "coder" {
			_ = os.WriteFile(filepath.Join(activeDir, "go.mod"), []byte("module example.com/calc\n\ngo 1.22\n"), 0644)
			_ = os.WriteFile(filepath.Join(activeDir, "calc.go"), []byte("package calc\n"), 0644)
			_ = os.WriteFile(filepath.Join(activeDir, "calc_test.go"), []byte("package calc\nimport \"testing\"\nfunc TestX(t *testing.T) {}\n"), 0644)
			return &spike.AgentMessage{Content: "Coded"}, nil
		}
		if agentID == "reviewer" {
			// Always reject!
			session.Config().OnSubmitReview(spike.ReviewResult{
				Approved: false,
				Comments: "Never approved",
			})
			return &spike.AgentMessage{Content: "Rejected"}, nil
		}
		return &spike.AgentMessage{Content: "Done"}, nil
	}

	coord := spike.NewTeamCoordinator(repoDir, store, roster, memory, gov, journal, runner, fakeProvider)
	coord.SetWorktreeManager(wtMgr)
	coord.MaxRevisions = 2

	taskReq, _ := coord.SubmitTask("Task that fails review", spike.Budget{Allocated: 10000}, nil)

	// Step sequence with MaxRevisions=2:
	// 1: Lead
	// 2: Coder
	// 3: Reviewer 1 (rejects -> enqueues revision 1 and rereview 1)
	// 4: Coder revision 1
	// 5: Reviewer 2 (rejects -> enqueues revision 2 and rereview 2)
	// 6: Coder revision 2
	// 7: Reviewer 3 (rejects -> max revisions reached, no more enqueued!)
	results, err := coord.RunLoop(context.Background(), 20)
	if err != nil {
		t.Fatalf("run loop error: %v", err)
	}

	if len(results) != 7 {
		t.Fatalf("expected 7 steps before bounded halt, got %d", len(results))
	}

	// Worktree should be released with failed status
	wtRec, _ := wtMgr.GetWorktree(taskReq.ID)
	if wtRec.Status != spike.WorktreeStatusFailed {
		t.Fatalf("expected worktree status failed after max revisions exceeded, got %s", wtRec.Status)
	}
}

// TestDogfood_06_TaskHistoryAggregation tests deterministic reconstruction of task audit history.
func TestDogfood_06_TaskHistoryAggregation(t *testing.T) {
	dir := t.TempDir()
	repoDir := filepath.Join(dir, "repo")
	_ = os.MkdirAll(repoDir, 0755)

	store, _ := spike.NewDurableActivityStore(filepath.Join(dir, "activities"))
	memory, _ := spike.NewDurableMemoryStore(filepath.Join(dir, "memory"))
	roster, _ := spike.DefaultTeamRoster(filepath.Join(dir, "roster"))
	gov := spike.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_propose_activity",
		"governed_submit_review", "governed_request_human_decision", "governed_propose_memory",
	})
	journal, _ := spike.NewDurableIntentJournal(filepath.Join(dir, "journal"))
	fakeProvider := spike.NewFakeProvider()
	runner := &spike.NativeGoTestRunner{}

	fakeProvider.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		agentID := session.Config().AgentID
		switch agentID {
		case "lead":
			session.Config().OnProposeActivity(spike.ActivityProposal{
				ProposalID:      "coder-task",
				ProposedAgentID: "coder",
				Objective:       "Implement Subtract",
				RequestedBudget: spike.Budget{Allocated: 3000},
			})
			session.Config().OnProposeActivity(spike.ActivityProposal{
				ProposalID:      "reviewer-task",
				ProposedAgentID: "reviewer",
				Objective:       "Review Subtract",
				RequestedBudget: spike.Budget{Allocated: 2000},
				DependsOn:       []string{"coder-task"},
			})
			return &spike.AgentMessage{Content: "Delegated subtasks"}, nil
		case "coder":
			_ = os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte("module example.com/calc\n\ngo 1.22\n"), 0644)
			_ = os.WriteFile(filepath.Join(repoDir, "calc.go"), []byte("package calc\nfunc Sub(a, b int) int { return a - b }\n"), 0644)
			_ = os.WriteFile(filepath.Join(repoDir, "calc_test.go"), []byte("package calc\nimport \"testing\"\nfunc TestSub(t *testing.T) {}\n"), 0644)
			return &spike.AgentMessage{Content: "Implemented"}, nil
		case "reviewer":
			session.Config().OnSubmitReview(spike.ReviewResult{
				Approved: true,
				Comments: "Approved cleanly",
			})
			return &spike.AgentMessage{Content: "Reviewed"}, nil
		}
		return &spike.AgentMessage{Content: "Done"}, nil
	}

	coord := spike.NewTeamCoordinator(repoDir, store, roster, memory, gov, journal, runner, fakeProvider)
	taskReq, _ := coord.SubmitTask("Implement Subtract feature", spike.Budget{Allocated: 15000}, nil)
	_, _ = coord.RunLoop(context.Background(), 10)

	// Build task history
	history, err := spike.BuildTaskHistory(taskReq.ID, store, journal, nil, memory)
	if err != nil {
		t.Fatalf("failed to build task history: %v", err)
	}

	if history.TaskID != taskReq.ID {
		t.Fatalf("expected TaskID %s, got %s", taskReq.ID, history.TaskID)
	}
	if history.OriginalObjective != "Implement Subtract feature" {
		t.Fatalf("expected objective 'Implement Subtract feature', got %q", history.OriginalObjective)
	}
	if len(history.Activities) != 3 {
		t.Fatalf("expected 3 activities in history, got %d", len(history.Activities))
	}
	if len(history.ReviewDecisions) != 1 || !history.ReviewDecisions[0].Approved {
		t.Fatalf("expected 1 approved review decision in history, got %+v", history.ReviewDecisions)
	}
	if !strings.Contains(history.FinalOutcome, "successfully implemented") {
		t.Fatalf("expected successful final outcome, got %q", history.FinalOutcome)
	}
}

// TestDogfood_07_TaskHistoryFormatting verifies human-readable audit formatting.
func TestDogfood_07_TaskHistoryFormatting(t *testing.T) {
	report := &spike.TaskHistoryReport{
		TaskID:              "task-dogfood-707",
		OriginalObjective:   "Build task history command",
		CreatedAt:           time.Now(),
		AllocatedBudget:     10000,
		ConsumedBudget:      3500,
		Status:              spike.StatusCompleted,
		Branch:              "a4c/task-dogfood-707",
		ParticipatingAgents: []string{"lead", "coder", "reviewer"},
		Activities: []spike.ActivityHistoryEntry{
			{
				ActivityID: "act-1",
				AgentID:    "lead",
				Objective:  "Architect plan",
				Status:     spike.StatusCompleted,
			},
			{
				ActivityID:  "act-2",
				AgentID:     "coder",
				Objective:   "Implement CLI command",
				Status:      spike.StatusCompleted,
				EvidenceIDs: []string{"evi-999"},
			},
		},
		ReviewDecisions: []spike.ReviewHistoryEntry{
			{
				ActivityID: "act-3",
				ReviewerID: "reviewer",
				Approved:   true,
				Comments:   "Quality approved",
			},
		},
		FinalOutcome: "Task successfully implemented, verified natively, and reviewed.",
	}

	formatted := report.FormatHumanReadable()

	requiredSubstrings := []string{
		"A4C Task Activity History: task-dogfood-707",
		"Original Objective:  Build task history command",
		"Git Worktree Branch: a4c/task-dogfood-707",
		"Activity DAG & Transitions",
		"Peer Review Decisions",
		"Quality approved",
		"Final Outcome",
		"successfully implemented",
	}

	for _, sub := range requiredSubstrings {
		if !strings.Contains(formatted, sub) {
			t.Fatalf("formatted output missing substring %q:\n%s", sub, formatted)
		}
	}
}
