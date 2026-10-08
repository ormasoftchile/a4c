package a4c_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	spike "github.com/ormasoftchile/a4c/pkg/a4c"
)

// Mock external service with state tracking
type MockExternalService struct {
	createdPRs map[string]string // branch -> prID
	failOnCall bool
}

func NewMockExternalService() *MockExternalService {
	return &MockExternalService{
		createdPRs: make(map[string]string),
	}
}

func (m *MockExternalService) CreatePR(branch string) (string, error) {
	if m.failOnCall {
		return "", fmt.Errorf("network crash during PR creation")
	}
	prID := fmt.Sprintf("pr-%s-101", branch)
	m.createdPRs[branch] = prID
	return prID, nil
}

func (m *MockExternalService) Reconcile(ctx context.Context, intent spike.IntentRecord) (bool, string, error) {
	if intent.Action == "create_pr" {
		if prID, exists := m.createdPRs[intent.Target]; exists {
			return true, prID, nil
		}
	}
	return false, "", nil
}

// Test 1: Normal Run
// Architect -> Coder -> Reviewer completes with state durably persisted.
func TestSpike_01_NormalRun(t *testing.T) {
	dir := t.TempDir()
	store, err := spike.NewDurableActivityStore(filepath.Join(dir, "activities"))
	if err != nil {
		t.Fatal(err)
	}

	activities := []spike.ActivityRequest{
		{ID: "act-1", AgentID: "architect", Objective: "Plan feature", CreatedAt: time.Now()},
		{ID: "act-2", AgentID: "coder", Objective: "Implement feature", CreatedAt: time.Now()},
		{ID: "act-3", AgentID: "reviewer", Objective: "Review PR diff", CreatedAt: time.Now()},
	}

	for _, a := range activities {
		if err := store.Enqueue(a); err != nil {
			t.Fatal(err)
		}
	}

	// Execution loop
	for store.HasPending() {
		claimed, err := store.ClaimNext()
		if err != nil {
			t.Fatal(err)
		}
		if claimed == nil {
			break
		}
		res := spike.ActivityResult{
			ID:     claimed.ID,
			Status: spike.StatusCompleted,
			Outputs: map[string]any{
				"verdict": "success",
			},
		}
		if err := store.Complete(claimed.ID, res); err != nil {
			t.Fatal(err)
		}
	}

	if store.CompletedCount() != 3 {
		t.Fatalf("expected 3 completed activities, got %d", store.CompletedCount())
	}
}

// Test 2: Crash Between Activities
// Simulate SIGKILL between act-1 and act-2. Restart must skip completed act-1 and continue act-2.
func TestSpike_02_CrashBetweenActivities(t *testing.T) {
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "activities")

	// Process 1: Enqueues 3 activities, completes act-1, then dies
	{
		store1, err := spike.NewDurableActivityStore(storeDir)
		if err != nil {
			t.Fatal(err)
		}
		_ = store1.Enqueue(spike.ActivityRequest{ID: "act-1", AgentID: "architect", Objective: "Plan"})
		_ = store1.Enqueue(spike.ActivityRequest{ID: "act-2", AgentID: "coder", Objective: "Implement"})
		_ = store1.Enqueue(spike.ActivityRequest{ID: "act-3", AgentID: "reviewer", Objective: "Review"})

		claimed, _ := store1.ClaimNext()
		if claimed.ID != "act-1" {
			t.Fatalf("expected act-1, got %s", claimed.ID)
		}
		_ = store1.Complete(claimed.ID, spike.ActivityResult{ID: claimed.ID, Status: spike.StatusCompleted})

		// Simulated SIGKILL: process terminates without touching act-2 or act-3
	}

	// Process 2: Restarts from disk
	{
		store2, err := spike.NewDurableActivityStore(storeDir)
		if err != nil {
			t.Fatal(err)
		}

		// Verify act-1 is already completed
		rec1, err := store2.Get("act-1")
		if err != nil || rec1.Status != spike.StatusCompleted {
			t.Fatalf("act-1 should be completed, got status %v", rec1.Status)
		}

		// Next claim must be act-2, NOT act-1
		claimed, err := store2.ClaimNext()
		if err != nil || claimed == nil || claimed.ID != "act-2" {
			t.Fatalf("expected to resume at act-2, got %v", claimed)
		}
		_ = store2.Complete(claimed.ID, spike.ActivityResult{ID: claimed.ID, Status: spike.StatusCompleted})

		claimed3, _ := store2.ClaimNext()
		if claimed3 == nil || claimed3.ID != "act-3" {
			t.Fatalf("expected act-3, got %v", claimed3)
		}
		_ = store2.Complete(claimed3.ID, spike.ActivityResult{ID: claimed3.ID, Status: spike.StatusCompleted})

		if store2.CompletedCount() != 3 {
			t.Fatalf("expected 3 completed activities, got %d", store2.CompletedCount())
		}
	}
}

// Test 3: Crash During Mutation
// Intent is persisted to disk, mutation occurs externally, but process is SIGKILLed before outcome is written.
// Restart detects unfinished intent and blocks raw retry without reconciliation.
func TestSpike_03_CrashDuringMutation(t *testing.T) {
	dir := t.TempDir()
	journalDir := filepath.Join(dir, "journal")
	external := NewMockExternalService()

	intent := spike.IntentRecord{
		IntentID:    "intent-pr-42",
		ActivityID:  "act-pr",
		Action:      "create_pr",
		Target:      "feature-x",
		PayloadHash: "sha256-payload",
	}

	// Process 1: Persists intent, executes external PR, then receives SIGKILL before writing outcome
	{
		journal1, err := spike.NewDurableIntentJournal(journalDir)
		if err != nil {
			t.Fatal(err)
		}

		if err := journal1.RecordIntent(intent); err != nil {
			t.Fatal(err)
		}

		// External mutation executes!
		_, err = external.CreatePR("feature-x")
		if err != nil {
			t.Fatal(err)
		}

		// Simulated SIGKILL: process crashes immediately! Outcome is NEVER written.
	}

	// Process 2: Restarts from disk
	{
		journal2, err := spike.NewDurableIntentJournal(journalDir)
		if err != nil {
			t.Fatal(err)
		}

		unfinished := journal2.FindUnfinishedIntents()
		if len(unfinished) != 1 {
			t.Fatalf("expected 1 unfinished intent, got %d", len(unfinished))
		}
		if unfinished[0].Status != spike.IntentPersisted {
			t.Fatalf("expected IntentPersisted status, got %s", unfinished[0].Status)
		}
	}
}

// Test 4: Reconciliation
// Restart runs reconciliation against the external service. It discovers PR already exists and adopts it
// WITHOUT duplicating the PR creation call.
func TestSpike_04_Reconciliation(t *testing.T) {
	dir := t.TempDir()
	journalDir := filepath.Join(dir, "journal")
	external := NewMockExternalService()

	// External PR was already created during prior run
	prID, _ := external.CreatePR("feature-reconcile")

	// Journal has intent in uncommitted state
	journal, err := spike.NewDurableIntentJournal(journalDir)
	if err != nil {
		t.Fatal(err)
	}

	intent := spike.IntentRecord{
		IntentID:   "intent-rec-1",
		ActivityID: "act-create-pr",
		Action:     "create_pr",
		Target:     "feature-reconcile",
	}
	_ = journal.RecordIntent(intent)

	// Reconcile on startup
	resolved, err := journal.ReconcileUnfinished(context.Background(), external)
	if err != nil {
		t.Fatal(err)
	}

	if len(resolved) != 1 {
		t.Fatalf("expected 1 resolved intent, got %d", len(resolved))
	}
	if resolved[0].ExternalID != prID {
		t.Fatalf("expected adopted PR ID %s, got %s", prID, resolved[0].ExternalID)
	}
	if resolved[0].Status != spike.IntentCommitted {
		t.Fatalf("expected status committed, got %s", resolved[0].Status)
	}

	// Verify no unfinished intents remain
	if len(journal.FindUnfinishedIntents()) != 0 {
		t.Fatalf("expected 0 unfinished intents after reconciliation")
	}
}

// Test 5: Dynamic Dispatch
// Parent activity dynamically proposes 5 subtasks. Governor authorizes them under parent budget and enqueues them.
func TestSpike_05_DynamicDispatch(t *testing.T) {
	dir := t.TempDir()
	store, _ := spike.NewDurableActivityStore(filepath.Join(dir, "activities"))
	gov := spike.NewPolicyGovernor([]string{"copilot_chat", "run_test"})

	parent := spike.ActivityRequest{
		ID:        "act-parent",
		AgentID:   "architect",
		Objective: "Decompose epic",
		Budget: spike.Budget{
			Allocated: 50000,
			Reserved:  0,
			Consumed:  5000,
		},
	}
	_ = store.Enqueue(parent)

	// Architect discovers 5 subtasks dynamically
	for i := 1; i <= 5; i++ {
		proposal := spike.ActivityProposal{
			ProposedAgentID: "coder",
			Objective:       fmt.Sprintf("Subtask %d", i),
			RequestedBudget: spike.Budget{Allocated: 5000},
		}

		childReq, err := gov.AuthorizeProposal(parent, proposal, fmt.Sprintf("subtask-%d", i))
		if err != nil {
			t.Fatalf("failed to authorize proposal %d: %v", i, err)
		}

		if err := store.Enqueue(*childReq); err != nil {
			t.Fatalf("failed to enqueue child %d: %v", i, err)
		}
	}

	// Verify parent + 5 children are in store
	claimedParent, _ := store.ClaimNext()
	_ = store.Complete(claimedParent.ID, spike.ActivityResult{ID: claimedParent.ID, Status: spike.StatusCompleted})

	childCount := 0
	for store.HasPending() {
		child, _ := store.ClaimNext()
		_ = store.Complete(child.ID, spike.ActivityResult{ID: child.ID, Status: spike.StatusCompleted})
		childCount++
	}

	if childCount != 5 {
		t.Fatalf("expected 5 dynamic children executed, got %d", childCount)
	}
}

// Test 6: Crash During Dispatch
// If process crashes mid-way through queuing proposals, reloaded store maintains exact consistency.
func TestSpike_06_CrashDuringDispatch(t *testing.T) {
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "activities")

	// Process 1: Enqueues subtask-1 and subtask-2, then crashes
	{
		store1, _ := spike.NewDurableActivityStore(storeDir)
		_ = store1.Enqueue(spike.ActivityRequest{ID: "subtask-1", AgentID: "coder"})
		_ = store1.Enqueue(spike.ActivityRequest{ID: "subtask-2", AgentID: "coder"})
		// Crash
	}

	// Process 2: Reloads, continues queuing subtask-3
	{
		store2, _ := spike.NewDurableActivityStore(storeDir)
		_ = store2.Enqueue(spike.ActivityRequest{ID: "subtask-3", AgentID: "coder"})

		// Attempting to duplicate subtask-1 must fail
		err := store2.Enqueue(spike.ActivityRequest{ID: "subtask-1", AgentID: "coder"})
		if err == nil {
			t.Fatalf("expected duplicate enqueue of subtask-1 to fail")
		}

		count := 0
		for store2.HasPending() {
			_, _ = store2.ClaimNext()
			count++
		}
		if count != 3 {
			t.Fatalf("expected 3 total unique subtasks, got %d", count)
		}
	}
}

// Test 7: Governance Bypass & Unauthorized Tools
// Verifies that agents cannot call unmediated shell or git tools directly.
func TestSpike_07_GovernanceBypass(t *testing.T) {
	gov := spike.NewPolicyGovernor([]string{"copilot_chat", "run_test"})

	// Allowed tool
	if err := gov.AuthorizeToolInvocation("coder", "copilot_chat"); err != nil {
		t.Fatalf("copilot_chat should be allowed: %v", err)
	}

	// Unauthorized direct shell bypass attempt
	if err := gov.AuthorizeToolInvocation("coder", "direct_bash_exec"); err == nil {
		t.Fatalf("expected direct_bash_exec to be rejected")
	}

	// Unauthorized git push bypass attempt
	if err := gov.AuthorizeToolInvocation("coder", "git_push"); err == nil {
		t.Fatalf("expected git_push to be rejected")
	}
}

// Test 8: Workspace Evidence Double-Hashing & Invalidation
// Tests that:
// 1. Double-hash pre/post validation confirms zero concurrent modification.
// 2. Modifying a file after test execution invalidates the evidence at the review gate.
func TestSpike_08_WorkspaceEvidenceIntegrity(t *testing.T) {
	wsDir := t.TempDir()
	filePath := filepath.Join(wsDir, "main.go")
	_ = os.WriteFile(filePath, []byte("package main\n\nfunc main() {}\n"), 0644)

	snapshotter := spike.NewWorkspaceSnapshotter()

	// Pre-test hash
	beforeHash, err := snapshotter.ComputeSnapshot(wsDir)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate test execution (zero workspace mutation)
	time.Sleep(10 * time.Millisecond)

	// Post-test hash
	afterHash, err := snapshotter.ComputeSnapshot(wsDir)
	if err != nil {
		t.Fatal(err)
	}

	if err := snapshotter.ValidateIntegrity(beforeHash, afterHash); err != nil {
		t.Fatalf("integrity check should pass: %v", err)
	}

	evidence := spike.ValidationEvidence{
		EvidenceID:          "evi-101",
		WorkspaceSnapshotID: afterHash,
		Command:             []string{"go", "test", "./..."},
		ExitCode:            0,
		Valid:               true,
	}

	// Review gate check against clean workspace -> Passes!
	if err := snapshotter.ValidateAgainstEvidence(wsDir, evidence); err != nil {
		t.Fatalf("evidence should validate against untouched workspace: %v", err)
	}

	// NOW: Coder mutates a file after the tests ran!
	_ = os.WriteFile(filePath, []byte("package main\n\nfunc main() { panic(\"untested change\") }\n"), 0644)

	// Review gate check against mutated workspace -> MUST FAIL!
	if err := snapshotter.ValidateAgainstEvidence(wsDir, evidence); err == nil {
		t.Fatalf("expected validation gate to FAIL because workspace was modified post-test")
	}
}

// Test 9: Hierarchical Budget Rejection
// Verifies that a child activity cannot request more tokens than the parent has available.
func TestSpike_09_HierarchicalBudgetRejection(t *testing.T) {
	gov := spike.NewPolicyGovernor([]string{"copilot_chat"})

	parent := spike.ActivityRequest{
		ID: "act-parent",
		Budget: spike.Budget{
			Allocated: 10000,
			Reserved:  8000, // Only 2000 available
			Consumed:  0,
		},
	}

	greedyProposal := spike.ActivityProposal{
		ProposedAgentID: "coder",
		RequestedBudget: spike.Budget{Allocated: 5000}, // Requests 5000 when only 2000 available
	}

	_, err := gov.AuthorizeProposal(parent, greedyProposal, "child-1")
	if err == nil {
		t.Fatalf("expected budget governor to reject proposal exceeding parent available budget")
	}
}
