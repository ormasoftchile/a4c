package a4c_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	spike "github.com/ormasoftchile/a4c/pkg/a4c"
)

// Mock test runner for unit tests
type MockTestRunner struct {
	exitCode int
	output   string
}

func (m *MockTestRunner) RunTests(ctx context.Context, dir string) (int, string, error) {
	return m.exitCode, m.output, nil
}

// 1. Unauthorized Copilot Tool Invocation Rejected
func TestExtended_01_UnauthorizedToolInvocation(t *testing.T) {
	gov := spike.NewPolicyGovernor([]string{"read_file", "apply_patch"})

	// Allowed
	if err := gov.AuthorizeToolInvocation("programmer", "read_file"); err != nil {
		t.Fatalf("expected read_file to be allowed: %v", err)
	}

	// Direct shell bypass rejected
	if err := gov.AuthorizeToolInvocation("programmer", "bash_exec"); err == nil {
		t.Fatalf("expected direct bash_exec to be blocked")
	}

	// Direct git push rejected
	if err := gov.AuthorizeToolInvocation("programmer", "git_push"); err == nil {
		t.Fatalf("expected direct git_push to be blocked")
	}
}

// 2. Agent Session Cancellation
func TestExtended_02_SessionCancellation(t *testing.T) {
	provider := spike.NewFakeProvider()
	provider.ResponseFunc = func(ctx context.Context, prompt string) (*spike.AgentMessage, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	session, err := provider.CreateSession(context.Background(), spike.AgentConfig{AgentID: "programmer"})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err = session.Send(ctx, "Long running task")
	if err == nil {
		t.Fatalf("expected context deadline error on cancellation")
	}
}

// 3. Session Recovery After Process Restart
func TestExtended_03_SessionRecoveryAfterRestart(t *testing.T) {
	provider := spike.NewFakeProvider()

	// Initial session
	s1, err := provider.CreateSession(context.Background(), spike.AgentConfig{AgentID: "programmer"})
	if err != nil {
		t.Fatal(err)
	}
	sessionID := s1.SessionID()
	_ = s1.Close()

	// Simulated restart: resume using saved session ID
	s2, err := provider.ResumeSession(context.Background(), sessionID, spike.AgentConfig{AgentID: "programmer"})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	if s2.SessionID() != sessionID {
		t.Fatalf("expected resumed session ID %s, got %s", sessionID, s2.SessionID())
	}
}

// 4. Budget Exhaustion During an Active Coding Task
func TestExtended_04_BudgetExhaustion(t *testing.T) {
	dir := t.TempDir()
	provider := spike.NewFakeProvider()
	gov := spike.NewPolicyGovernor([]string{"read_file", "apply_patch"})
	agent := spike.NewProgrammerAgent(dir, provider, gov, nil)

	// Set prompt consumption higher than allocated budget
	provider.ResponseFunc = func(ctx context.Context, prompt string) (*spike.AgentMessage, error) {
		return &spike.AgentMessage{
			Role:    "assistant",
			Content: "Code patch",
		}, nil
	}

	req := spike.ActivityRequest{
		ID:        "act-budget-test",
		AgentID:   "programmer",
		Objective: "Expensive operation",
		Budget: spike.Budget{
			Allocated: 0, // Zero budget available
		},
	}

	res, err := agent.ExecuteActivity(context.Background(), req)
	if err != spike.ErrBudgetExceeded {
		t.Fatalf("expected ErrBudgetExceeded, got %v", err)
	}
	if res.Status != spike.StatusFailed {
		t.Fatalf("expected activity status to be failed, got %s", res.Status)
	}
}

// 5. Coding Activity Interrupted After Successful Tool Effect
func TestExtended_05_InterruptedAfterSuccessfulToolEffect(t *testing.T) {
	dir := t.TempDir()
	journalDir := filepath.Join(dir, "journal")
	journal, err := spike.NewDurableIntentJournal(journalDir)
	if err != nil {
		t.Fatal(err)
	}

	// Tool effect logs intent before modifying file
	targetFile := filepath.Join(dir, "calc.go")
	intent := spike.IntentRecord{
		IntentID:   "intent-patch-1",
		ActivityID: "act-patch",
		Action:     "apply_patch",
		Target:     "calc.go",
	}
	_ = journal.RecordIntent(intent)
	_ = os.WriteFile(targetFile, []byte("package main\n"), 0644)

	// Simulated crash: process dies before committing outcome
	// On restart:
	reloadedJournal, err := spike.NewDurableIntentJournal(journalDir)
	if err != nil {
		t.Fatal(err)
	}

	unfinished := reloadedJournal.FindUnfinishedIntents()
	if len(unfinished) != 1 {
		t.Fatalf("expected 1 unfinished intent detected, got %d", len(unfinished))
	}
	if unfinished[0].Target != "calc.go" {
		t.Fatalf("expected target calc.go, got %s", unfinished[0].Target)
	}
}

// 6. Workspace Modification After Validation Rejected
func TestExtended_06_WorkspaceModificationAfterValidation(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "handler.go")
	_ = os.WriteFile(filePath, []byte("package main\n\nfunc Handle() string { return \"ok\" }\n"), 0644)

	snapshotter := spike.NewWorkspaceSnapshotter()
	hash1, err := snapshotter.ComputeSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}

	evidence := spike.ValidationEvidence{
		EvidenceID:          "evi-valid",
		WorkspaceSnapshotID: hash1,
		ExitCode:            0,
		Valid:               true,
	}

	// Clean workspace check -> ok
	if err := snapshotter.ValidateAgainstEvidence(dir, evidence); err != nil {
		t.Fatalf("validation should pass: %v", err)
	}

	// Uncommitted file change
	_ = os.WriteFile(filePath, []byte("package main\n\nfunc Handle() string { return \"tampered\" }\n"), 0644)

	// Validation must fail
	if err := snapshotter.ValidateAgainstEvidence(dir, evidence); err == nil {
		t.Fatalf("validation must fail because workspace was modified post-validation")
	}
}

// 7. Missing or Malformed Model Output Handled Cleanly
func TestExtended_07_MalformedModelOutput(t *testing.T) {
	dir := t.TempDir()
	provider := spike.NewFakeProvider()
	gov := spike.NewPolicyGovernor([]string{"read_file", "apply_patch"})
	agent := spike.NewProgrammerAgent(dir, provider, gov, nil)

	provider.ResponseFunc = func(ctx context.Context, prompt string) (*spike.AgentMessage, error) {
		return nil, spike.ErrMalformedOutput
	}

	req := spike.ActivityRequest{
		ID:        "act-malformed",
		AgentID:   "programmer",
		Objective: "Generate something",
		Budget:    spike.Budget{Allocated: 5000},
	}

	res, err := agent.ExecuteActivity(context.Background(), req)
	if err != spike.ErrMalformedOutput {
		t.Fatalf("expected ErrMalformedOutput, got %v", err)
	}
	if res.Status != spike.StatusFailed {
		t.Fatalf("expected failed status, got %s", res.Status)
	}
}

// 8. Provider Failure and Clean Error Propagation
func TestExtended_08_ProviderFailure(t *testing.T) {
	dir := t.TempDir()
	provider := spike.NewFakeProvider()
	provider.FailNext = true // Simulate 503 unavailable
	gov := spike.NewPolicyGovernor([]string{"read_file", "apply_patch"})
	agent := spike.NewProgrammerAgent(dir, provider, gov, nil)

	req := spike.ActivityRequest{
		ID:        "act-fail",
		AgentID:   "programmer",
		Objective: "Generate",
		Budget:    spike.Budget{Allocated: 5000},
	}

	_, err := agent.ExecuteActivity(context.Background(), req)
	if err == nil {
		t.Fatalf("expected provider error")
	}
}

// 9. Real Go Repository Coding Demonstration (End-to-End Governed Activity Loop)
// Creates an actual Go module on disk with a failing test.
// Programmer agent inspects, applies fix, native go test runs, passes, and evidence commits.
// Then terminates and restarts, verifying no duplicate execution.
func TestExtended_09_RealGoRepoCodingDemonstration(t *testing.T) {
	dir := t.TempDir()
	repoDir := filepath.Join(dir, "repo")
	storeDir := filepath.Join(dir, "activities")
	_ = os.MkdirAll(repoDir, 0755)

	// Create real Go module
	_ = os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte("module example.com/calc\n\ngo 1.22\n"), 0644)
	_ = os.WriteFile(filepath.Join(repoDir, "calc.go"), []byte("package calc\n\nfunc Add(a, b int) int {\n\treturn 0 // Bug: returns 0\n}\n"), 0644)
	_ = os.WriteFile(filepath.Join(repoDir, "calc_test.go"), []byte("package calc\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatalf(\"Add(2, 3) expected 5, got %d\", Add(2, 3))\n\t}\n}\n"), 0644)

	provider := spike.NewFakeProvider()
	gov := spike.NewPolicyGovernor([]string{"read_file", "apply_patch"})
	journal, _ := spike.NewDurableIntentJournal(filepath.Join(dir, "journal"))
	agent := spike.NewProgrammerAgent(repoDir, provider, gov, journal)
	store, _ := spike.NewDurableActivityStore(storeDir)

	// Programmer applies the correct fix when invoked
	provider.ResponseFunc = func(ctx context.Context, prompt string) (*spike.AgentMessage, error) {
		fixCode := "package calc\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n"
		err := os.WriteFile(filepath.Join(repoDir, "calc.go"), []byte(fixCode), 0644)
		if err != nil {
			return nil, err
		}
		return &spike.AgentMessage{
			Role:    "assistant",
			Content: "Fixed Add() to return a + b",
		}, nil
	}

	// Enqueue coding activity
	_ = store.Enqueue(spike.ActivityRequest{
		ID:        "act-fix-add",
		AgentID:   "programmer",
		Objective: "Fix failing TestAdd in calc.go",
		Budget:    spike.Budget{Allocated: 10000},
	})

	// Coordinator with real NativeGoTestRunner!
	runner := &spike.NativeGoTestRunner{}
	coordinator := spike.NewGovernedCoordinator(store, agent, runner)

	// Execute activity through governed lifecycle
	res, err := coordinator.ExecuteNext(context.Background())
	if err != nil {
		t.Fatalf("governed execution failed: %v", err)
	}

	if res.Status != spike.StatusCompleted {
		t.Fatalf("expected completed status, got %s", res.Status)
	}
	if len(res.EvidenceIDs) == 0 {
		t.Fatalf("expected validation evidence to be attached")
	}

	// Verify calc.go now has the fix
	data, _ := os.ReadFile(filepath.Join(repoDir, "calc.go"))
	if string(data) != "package calc\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n" {
		t.Fatalf("unexpected content in calc.go: %s", string(data))
	}

	// ──► SIMULATED PROCESS RESTART ◄──
	reloadedStore, err := spike.NewDurableActivityStore(storeDir)
	if err != nil {
		t.Fatal(err)
	}

	// Must have zero pending activities (completed activity is NOT repeated)
	if reloadedStore.HasPending() {
		t.Fatalf("expected zero pending activities after restart")
	}
	rec, err := reloadedStore.Get("act-fix-add")
	if err != nil || rec.Status != spike.StatusCompleted {
		t.Fatalf("activity should be marked completed in reloaded store")
	}
}
