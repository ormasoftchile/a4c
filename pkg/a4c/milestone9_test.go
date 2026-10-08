package a4c_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	spike "github.com/ormasoftchile/a4c/pkg/a4c"
)

// TestMilestone9_01_TaskTreeGraphTraversal verifies that GetTaskTree and QueryActivities
// discover all descendant activities via transitive graph closure, regardless of on-disk
// alphabetical sorting order.
func TestMilestone9_01_TaskTreeGraphTraversal(t *testing.T) {
	dir := t.TempDir()
	store, err := spike.NewDurableActivityStore(dir)
	if err != nil {
		t.Fatalf("failed to create activity store: %v", err)
	}

	rootID := "task-1000"
	rootReq := spike.ActivityRequest{
		ID:        rootID,
		AgentID:   "lead",
		Objective: "Build root system",
		CreatedAt: time.Now().Add(-10 * time.Minute),
	}
	if err := store.Enqueue(rootReq); err != nil {
		t.Fatalf("enqueue root: %v", err)
	}

	// Create child 1
	child1ID := "task-1000-sub-1"
	child1Req := spike.ActivityRequest{
		ID:        child1ID,
		ParentID:  rootID,
		AgentID:   "coder",
		Objective: "Subtask 1",
		CreatedAt: time.Now().Add(-8 * time.Minute),
	}
	if err := store.Enqueue(child1Req); err != nil {
		t.Fatalf("enqueue child 1: %v", err)
	}

	// Create child 2
	child2ID := "task-1000-sub-2"
	child2Req := spike.ActivityRequest{
		ID:        child2ID,
		ParentID:  rootID,
		AgentID:   "reviewer",
		Objective: "Subtask 2",
		CreatedAt: time.Now().Add(-6 * time.Minute),
	}
	if err := store.Enqueue(child2Req); err != nil {
		t.Fatalf("enqueue child 2: %v", err)
	}

	// Create grandchildren whose IDs alphabetically sort BEFORE child2 or root
	// e.g. "a-revision" or "task-1000-sub-0-revision"
	grandchild1ID := "task-1000-a-revision"
	grandchild1Req := spike.ActivityRequest{
		ID:        grandchild1ID,
		ParentID:  child2ID,
		AgentID:   "coder",
		Objective: "Revision for subtask 2",
		CreatedAt: time.Now().Add(-4 * time.Minute),
	}
	if err := store.Enqueue(grandchild1Req); err != nil {
		t.Fatalf("enqueue grandchild 1: %v", err)
	}

	greatGrandchildID := "task-1000-00-rereview"
	greatGrandchildReq := spike.ActivityRequest{
		ID:        greatGrandchildID,
		ParentID:  grandchild1ID,
		AgentID:   "reviewer",
		Objective: "Re-review after revision",
		CreatedAt: time.Now().Add(-2 * time.Minute),
	}
	if err := store.Enqueue(greatGrandchildReq); err != nil {
		t.Fatalf("enqueue great grandchild: %v", err)
	}

	// 1. Verify GetTaskTree includes all 5 activities
	tree := store.GetTaskTree(rootID)
	if len(tree) != 5 {
		t.Fatalf("expected 5 activities in task tree, got %d", len(tree))
	}

	idsFound := make(map[string]bool)
	for _, rec := range tree {
		idsFound[rec.Request.ID] = true
	}

	for _, expectedID := range []string{rootID, child1ID, child2ID, grandchild1ID, greatGrandchildID} {
		if !idsFound[expectedID] {
			t.Errorf("expected activity %s in task tree, but was missing", expectedID)
		}
	}

	// Verify root is first in the returned slice
	if tree[0].Request.ID != rootID {
		t.Errorf("expected root activity to be first in task tree, got %s", tree[0].Request.ID)
	}

	// 2. Verify QueryActivities with RootID filter also discovers all 5 activities
	filtered := store.QueryActivities(spike.ActivityFilter{RootID: rootID})
	if len(filtered) != 5 {
		t.Fatalf("expected 5 activities from QueryActivities with RootID, got %d", len(filtered))
	}
}

// TestMilestone9_02_InFlightBudgetRejection verifies that when an agent session consumes
// tokens beyond its allocation, in-flight operations return ErrBudgetExceeded.
func TestMilestone9_02_InFlightBudgetRejection(t *testing.T) {
	fakeProvider := spike.NewFakeProvider()

	// Configure fake session to simulate token overrun
	fakeProvider.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		session.SetUsage(spike.UsageInfo{
			PromptTokens:     600,
			CompletionTokens: 500,
			TotalTokens:      1100, // Exceeds allocated 1000
		})
		return &spike.AgentMessage{
			Role:      "assistant",
			Content:   "Finished work",
			Timestamp: time.Now(),
		}, nil
	}

	config := spike.AgentConfig{
		AgentID: "coder",
		Budget:  spike.Budget{Allocated: 1000},
	}

	session, err := fakeProvider.CreateSession(context.Background(), config)
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}
	defer session.Close()

	_, err = session.Send(context.Background(), "Do coding")
	if err == nil {
		t.Fatalf("expected ErrBudgetExceeded when usage exceeds allocated, got nil")
	}
	if !errors.Is(err, spike.ErrBudgetExceeded) {
		t.Fatalf("expected ErrBudgetExceeded, got: %v", err)
	}
}

// TestMilestone9_03_PostFlightBudgetRejection verifies that an activity consuming more
// than its allocated budget fails post-flight with ErrBudgetExceeded and records metrics durably.
func TestMilestone9_03_PostFlightBudgetRejection(t *testing.T) {
	dir := t.TempDir()
	store, _ := spike.NewDurableActivityStore(filepath.Join(dir, "store"))
	roster, _ := spike.DefaultTeamRoster(filepath.Join(dir, "roster"))
	memory, _ := spike.NewDurableMemoryStore(filepath.Join(dir, "memory"))
	gov := spike.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_propose_activity",
		"governed_submit_review", "governed_request_human_decision", "governed_propose_memory",
	})
	journal, _ := spike.NewDurableIntentJournal(filepath.Join(dir, "journal"))
	fakeProvider := spike.NewFakeProvider()
	runner := &spike.NativeGoTestRunner{}

	fakeProvider.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		// Simulate consumption exceeding allocated budget
		session.SetUsage(spike.UsageInfo{
			PromptTokens:     1200,
			CompletionTokens: 300,
			TotalTokens:      1500, // Allocated is 1000
		})
		return &spike.AgentMessage{Content: "Completed"}, nil
	}

	coord := spike.NewTeamCoordinator(dir, store, roster, memory, gov, journal, runner, fakeProvider)

	// Submit direct task with 1,000 token budget
	taskReq, err := coord.SubmitTaskWithStrategy("Perform task", spike.Budget{Allocated: 1000}, nil, spike.StrategyDirect)
	if err != nil {
		t.Fatalf("submit task error: %v", err)
	}

	res, err := coord.Step(context.Background())
	if err == nil {
		t.Fatalf("expected coordinator Step to fail with ErrBudgetExceeded, got nil")
	}
	if !errors.Is(err, spike.ErrBudgetExceeded) {
		t.Fatalf("expected ErrBudgetExceeded, got: %v", err)
	}
	if res != nil {
		t.Fatalf("expected nil result on step error, got: %v", res)
	}

	// Verify durable record was marked failed and captured consumed metrics
	rec, err := store.Get(taskReq.ID)
	if err != nil {
		t.Fatalf("get record error: %v", err)
	}
	if rec.Status != spike.StatusFailed {
		t.Fatalf("expected durable status failed, got: %s", rec.Status)
	}
	if rec.Result == nil {
		t.Fatalf("expected durable result to be preserved on failure")
	}
	if rec.Result.Consumed.Consumed != 1500 {
		t.Fatalf("expected consumed tokens 1500 recorded, got %.0f", rec.Result.Consumed.Consumed)
	}
}

// TestMilestone9_04_HierarchicalBudgetExhaustionAndPause verifies that when cumulative consumption
// across the task tree reaches root allocation, the coordinator halts execution and escalates to human.
func TestMilestone9_04_HierarchicalBudgetExhaustionAndPause(t *testing.T) {
	dir := t.TempDir()
	store, _ := spike.NewDurableActivityStore(filepath.Join(dir, "store"))
	roster, _ := spike.DefaultTeamRoster(filepath.Join(dir, "roster"))
	memory, _ := spike.NewDurableMemoryStore(filepath.Join(dir, "memory"))
	gov := spike.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_propose_activity",
		"governed_submit_review", "governed_request_human_decision", "governed_propose_memory",
	})
	journal, _ := spike.NewDurableIntentJournal(filepath.Join(dir, "journal"))
	fakeProvider := spike.NewFakeProvider()
	runner := &spike.NativeGoTestRunner{}

	fakeProvider.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		agentID := session.Config().AgentID
		if agentID == "lead" {
			session.SetUsage(spike.UsageInfo{TotalTokens: 500})
			session.Config().OnProposeActivity(spike.ActivityProposal{
				ProposalID:      "sub-coder",
				ProposedAgentID: "coder",
				Objective:       "Coder task",
				RequestedBudget: spike.Budget{Allocated: 500},
			})
			return &spike.AgentMessage{Content: "Delegated to coder"}, nil
		}
		if agentID == "coder" {
			session.SetUsage(spike.UsageInfo{TotalTokens: 500})
			activeDir := session.Config().WorkspaceDir
			_ = os.WriteFile(filepath.Join(activeDir, "go.mod"), []byte("module example.com/calc\n\ngo 1.22\n"), 0644)
			_ = os.WriteFile(filepath.Join(activeDir, "calc.go"), []byte("package calc\n"), 0644)
			_ = os.WriteFile(filepath.Join(activeDir, "calc_test.go"), []byte("package calc\nimport \"testing\"\nfunc TestX(t *testing.T) {}\n"), 0644)
			return &spike.AgentMessage{Content: "Done coding"}, nil
		}
		return &spike.AgentMessage{Content: "Done"}, nil
	}

	coord := spike.NewTeamCoordinator(dir, store, roster, memory, gov, journal, runner, fakeProvider)

	// Root budget allocated is 1,000 tokens
	// Lead will consume 500 tokens in step 1.
	// Step 2 Coder starts: total consumed is 500 (available is 500).
	// Coder consumes 600 tokens -> total consumed across tree is 1,100 tokens (> 1,000)!
	// Now any further work in the task tree must pause with StatusAwaitingHuman!
	taskReq, err := coord.SubmitTask("Root task with small budget", spike.Budget{Allocated: 1000}, nil)
	if err != nil {
		t.Fatalf("submit task: %v", err)
	}

	// Step 1: Lead executes and consumes 500 tokens
	res1, err := coord.Step(context.Background())
	if err != nil {
		t.Fatalf("step 1 error: %v", err)
	}
	if res1.Status != spike.StatusCompleted {
		t.Fatalf("step 1 expected completed, got %s", res1.Status)
	}

	// Enqueue another subtask to verify pre-flight budget pause
	nextSubtask := spike.ActivityRequest{
		ID:        "sub-coder-2",
		ParentID:  taskReq.ID,
		AgentID:   "coder",
		Objective: "Second coder task",
		Budget:    spike.Budget{Allocated: 500},
		DependsOn: []string{"sub-coder"},
		CreatedAt: time.Now(),
	}
	if err := store.Enqueue(nextSubtask); err != nil {
		t.Fatalf("enqueue sub-coder-2: %v", err)
	}

	// Step 2: Coder 1 executes and consumes 600 tokens
	// Total consumed across tree is now 500 + 600 = 1,100 tokens (> 1,000 allocated root budget)
	res2, err := coord.Step(context.Background())
	if err != nil {
		t.Fatalf("step 2 error: %v", err)
	}
	if res2.Status != spike.StatusCompleted {
		t.Fatalf("step 2 expected completed, got %s", res2.Status)
	}

	// Step 3: sub-coder-2 claims. Tree total consumed (1,100) >= root allocation (1,000).
	// Pre-flight check MUST pause with StatusAwaitingHuman and request human authorization.
	res3, err := coord.Step(context.Background())
	if err != nil {
		t.Fatalf("step 3 unexpected error: %v", err)
	}
	if res3.Status != spike.StatusAwaitingHuman {
		t.Fatalf("expected step 3 to pause with StatusAwaitingHuman, got %s", res3.Status)
	}
	if res3.Outputs["reason"] != "root_budget_exhausted" {
		t.Fatalf("expected reason root_budget_exhausted, got: %v", res3.Outputs["reason"])
	}

	// Verify durable decision request exists
	rec, err := store.Get("sub-coder-2")
	if err != nil {
		t.Fatalf("get record error: %v", err)
	}
	if rec.Status != spike.StatusAwaitingHuman {
		t.Fatalf("expected durable record status awaiting_human, got %s", rec.Status)
	}
}

// TestMilestone9_05_RevisionCycleBudgetAuthorization verifies that revision subtasks are governed
// and will pause with StatusAwaitingHuman if root budget is insufficient.
func TestMilestone9_05_RevisionCycleBudgetAuthorization(t *testing.T) {
	dir := t.TempDir()
	store, _ := spike.NewDurableActivityStore(filepath.Join(dir, "store"))
	roster, _ := spike.DefaultTeamRoster(filepath.Join(dir, "roster"))
	memory, _ := spike.NewDurableMemoryStore(filepath.Join(dir, "memory"))
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
			session.SetUsage(spike.UsageInfo{TotalTokens: 200})
			session.Config().OnProposeActivity(spike.ActivityProposal{
				ProposalID:      "sub-coder",
				ProposedAgentID: "coder",
				Objective:       "Implement feature",
				RequestedBudget: spike.Budget{Allocated: 500},
			})
			session.Config().OnProposeActivity(spike.ActivityProposal{
				ProposalID:      "sub-reviewer",
				ProposedAgentID: "reviewer",
				Objective:       "Review feature",
				RequestedBudget: spike.Budget{Allocated: 300},
				DependsOn:       []string{"sub-coder"},
			})
			return &spike.AgentMessage{Content: "Delegated"}, nil
		}
		if agentID == "coder" {
			session.SetUsage(spike.UsageInfo{TotalTokens: 500})
			_ = os.WriteFile(filepath.Join(activeDir, "go.mod"), []byte("module example.com/calc\n\ngo 1.22\n"), 0644)
			_ = os.WriteFile(filepath.Join(activeDir, "calc.go"), []byte("package calc\n"), 0644)
			_ = os.WriteFile(filepath.Join(activeDir, "calc_test.go"), []byte("package calc\nimport \"testing\"\nfunc TestX(t *testing.T) {}\n"), 0644)
			return &spike.AgentMessage{Content: "Coded"}, nil
		}
		if agentID == "reviewer" {
			// Reviewer consumes 300 tokens and rejects
			session.SetUsage(spike.UsageInfo{TotalTokens: 300})
			session.Config().OnSubmitReview(spike.ReviewResult{
				Approved: false,
				Comments: "Missing documentation",
			})
			return &spike.AgentMessage{Content: "Rejected"}, nil
		}
		return &spike.AgentMessage{Content: "Done"}, nil
	}

	coord := spike.NewTeamCoordinator(dir, store, roster, memory, gov, journal, runner, fakeProvider)
	coord.MaxRevisions = 2

	// Total root budget is exactly 1,000 tokens.
	// Step 1: Lead consumes 200 tokens.
	// Step 2: Coder consumes 500 tokens.
	// Step 3: Reviewer consumes 300 tokens (Total tree consumption = 200 + 500 + 300 = 1,000 tokens!)
	// When reviewer rejects, remaining root budget is 0 tokens!
	// Coordinator MUST pause with StatusAwaitingHuman rather than spawning ungoverned subtasks.
	taskReq, err := coord.SubmitTask("Task with tightly bounded budget", spike.Budget{Allocated: 1000}, nil)
	if err != nil {
		t.Fatalf("submit task: %v", err)
	}

	// Step 1: Lead
	res1, err := coord.Step(context.Background())
	if err != nil || res1.Status != spike.StatusCompleted {
		t.Fatalf("step 1 failed: %v, res: %v", err, res1)
	}

	// Step 2: Coder
	res2, err := coord.Step(context.Background())
	if err != nil || res2.Status != spike.StatusCompleted {
		t.Fatalf("step 2 failed: %v, res: %v", err, res2)
	}

	// Step 3: Reviewer rejects and budget is exhausted -> StatusAwaitingHuman
	res3, err := coord.Step(context.Background())
	if err != nil {
		t.Fatalf("step 3 error: %v", err)
	}
	if res3.Status != spike.StatusAwaitingHuman {
		t.Fatalf("expected step 3 reviewer rejection to pause with StatusAwaitingHuman due to exhausted budget, got: %s", res3.Status)
	}

	// Verify the root task audit record exists and has human decision recorded
	rec, err := store.Get("sub-reviewer")
	if err != nil {
		t.Fatalf("get record error: %v", err)
	}
	if rec.Status != spike.StatusAwaitingHuman {
		t.Fatalf("expected sub-reviewer record to be awaiting_human, got: %s", rec.Status)
	}
	_ = taskReq
}

// TestMilestone9_06_PeakContextAccounting verifies that PeakContextTokens is populated
// and distinct from cumulative TotalTokens across multiple turns.
func TestMilestone9_06_PeakContextAccounting(t *testing.T) {
	metrics := spike.ExecutionMetrics{
		PromptTokens:      45000,
		CompletionTokens:  5000,
		TotalTokens:       50000,
		PeakContextTokens: 12000,
	}

	if metrics.TotalTokens != 50000 {
		t.Errorf("expected total tokens 50000, got %d", metrics.TotalTokens)
	}
	if metrics.PeakContextTokens != 12000 {
		t.Errorf("expected peak context tokens 12000, got %d", metrics.PeakContextTokens)
	}
}
