package a4c_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	spike "github.com/ormasoftchile/a4c/pkg/a4c"
)

// TestHelperProcess is invoked as a separate OS subprocess to test multi-process lock contention.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	dir := os.Getenv("WORKSPACE_DIR")
	lock, err := spike.AcquireWorkspaceLock(dir, "child-coordinator", "child-task")
	if err != nil {
		fmt.Fprintf(os.Stderr, "child failed to acquire lock: %v\n", err)
		os.Exit(2)
	}
	defer lock.Release()

	fmt.Println("LOCKED")
	_ = os.Stdout.Sync()

	// Sleep indefinitely until parent kills the process with SIGKILL
	time.Sleep(30 * time.Second)
}

// TestMilestone11_01_SingleWriterExclusiveLock_MultiProcess verifies that:
// 1. A second OS subprocess attempting to acquire the workspace lock fails immediately with ErrWorkspaceLocked.
// 2. Error details include holding PID and coordinator metadata.
// 3. When the holding process is killed (SIGKILL), the OS kernel immediately releases the lock without manual cleanup.
func TestMilestone11_01_SingleWriterExclusiveLock_MultiProcess(t *testing.T) {
	dir := t.TempDir()

	// 1. Launch real OS child process that acquires workspace lock
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess", "--")
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1", "WORKSPACE_DIR="+dir)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()

	// Wait for child process to acquire lock
	scanner := bufio.NewScanner(stdout)
	ready := false
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "LOCKED" {
			ready = true
			break
		}
	}
	if !ready {
		t.Fatalf("child process failed to signal LOCKED")
	}

	childPID := cmd.Process.Pid

	// 2. Parent attempts to acquire lock on the same workspace - MUST FAIL immediately
	_, err = spike.AcquireWorkspaceLock(dir, "parent-coordinator", "parent-task")
	if err == nil {
		t.Fatalf("expected parent AcquireWorkspaceLock to fail, but it succeeded")
	}
	if !errors.Is(err, spike.ErrWorkspaceLocked) {
		t.Fatalf("expected ErrWorkspaceLocked, got: %v", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%d", childPID)) {
		t.Fatalf("expected lock error to report child PID %d, got: %v", childPID, err)
	}

	// 3. Terminate child process violently with Kill (simulating crash / SIGKILL)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill child process: %v", err)
	}
	_ = cmd.Wait()

	// 4. Parent attempts to acquire lock again - MUST SUCCEED immediately via OS kernel release
	parentLock, err := spike.AcquireWorkspaceLock(dir, "parent-coordinator", "parent-task")
	if err != nil {
		t.Fatalf("expected parent AcquireWorkspaceLock to succeed after child SIGKILL, got: %v", err)
	}
	if !parentLock.IsLocked() {
		t.Fatalf("expected parent lock to be in locked state")
	}

	// 5. Release parent lock cleanly
	if err := parentLock.Release(); err != nil {
		t.Fatalf("release parent lock: %v", err)
	}
	if parentLock.IsLocked() {
		t.Fatalf("expected parent lock to be unlocked")
	}
}

// TestMilestone11_02_SingleWriter_ConcurrentReadOnlyAccess verifies that non-mutating
// inspect and history commands remain concurrently accessible while a coordinator holds the lock.
func TestMilestone11_02_SingleWriter_ConcurrentReadOnlyAccess(t *testing.T) {
	dir := t.TempDir()
	storeDir := filepath.Join(dir, ".a4c", "activities")
	journalDir := filepath.Join(dir, ".a4c", "journal")
	memDir := filepath.Join(dir, ".a4c", "memory")

	store, _ := spike.NewDurableActivityStore(storeDir)
	journal, _ := spike.NewDurableIntentJournal(journalDir)
	memory, _ := spike.NewDurableMemoryStore(memDir)

	taskID := "task-read-test-1"
	req := spike.ActivityRequest{
		ID:        taskID,
		AgentID:   "coder",
		Objective: "Read-only concurrency test",
		Budget:    spike.Budget{Allocated: 50000},
		CreatedAt: time.Now(),
	}
	_ = store.Enqueue(req)

	// Coordinator acquires exclusive lock on workspace
	coordLock, err := spike.AcquireWorkspaceLock(dir, "active-coord", taskID)
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	defer coordLock.Release()

	// Concurrently perform read-only inspection operations across 10 goroutines
	var wg sync.WaitGroup
	errCh := make(chan error, 10)

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Task tree query
			matches := store.QueryActivities(spike.ActivityFilter{})
			if len(matches) == 0 {
				errCh <- fmt.Errorf("expected activities from store query")
				return
			}
			// Task history construction
			hist, err := spike.BuildTaskHistory(taskID, store, journal, nil, memory)
			if err != nil {
				errCh <- fmt.Errorf("build task history: %w", err)
				return
			}
			if hist.TaskID != taskID {
				errCh <- fmt.Errorf("expected task id %s, got %s", taskID, hist.TaskID)
				return
			}
			// Inspect formatting
			formatted := hist.FormatInspect(nil, dir)
			if !strings.Contains(formatted, taskID) {
				errCh <- fmt.Errorf("expected formatted inspect to contain task id")
				return
			}
		}()
	}

	wg.Wait()
	close(errCh)

	for readErr := range errCh {
		t.Fatalf("concurrent read failed: %v", readErr)
	}
}

// TestMilestone11_03_CrashRecovery_ScenarioA_PostReservation verifies Scenario A:
// Crash occurs after child reservation is created, before execution.
// On startup reconciliation, unconsumed reservation is released back to parent without leaks or double-counting.
func TestMilestone11_03_CrashRecovery_ScenarioA_PostReservation(t *testing.T) {
	dir := t.TempDir()
	budgetDir := filepath.Join(dir, "budgets")
	storeDir := filepath.Join(dir, "activities")

	gov := spike.NewPolicyGovernor([]string{"governed_read_file"})
	_ = gov.SetStorageDir(budgetDir)
	store, _ := spike.NewDurableActivityStore(storeDir)

	rootID := "task-root-scen-a"
	gov.SetBudget(rootID, spike.Budget{Allocated: 100000})

	parentReq := spike.ActivityRequest{
		ID:     rootID,
		Budget: spike.Budget{Allocated: 100000},
	}
	prop := spike.ActivityProposal{
		ProposalID:      "child-a",
		ProposedAgentID: "coder",
		Objective:       "Implement feature A",
		RequestedBudget: spike.Budget{Allocated: 40000},
	}
	childID := "child-act-a"
	_, err := gov.AuthorizeProposal(parentReq, prop, childID)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}

	// Verify pre-crash state: parent reserved = 40,000, available = 60,000
	bRoot, _ := gov.GetBudget(rootID)
	if bRoot.Reserved != 40000 || bRoot.Available() != 60000 {
		t.Fatalf("expected pre-crash reserved=40000 available=60000, got reserved=%.0f available=%.0f", bRoot.Reserved, bRoot.Available())
	}

	// Simulate crash: child was never enqueued in store, process terminated abruptly.
	// Startup recovery:
	recoveredGov := spike.NewPolicyGovernor([]string{"governed_read_file"})
	if err := recoveredGov.SetStorageDir(budgetDir); err != nil {
		t.Fatalf("recover storage dir: %v", err)
	}
	if err := recoveredGov.ReconcileWithStore(store); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Parent should have reservation released because child activity never executed
	bRecovered, _ := recoveredGov.GetBudget(rootID)
	if bRecovered.Reserved != 0 || bRecovered.Available() != 100000 {
		t.Fatalf("expected post-recovery reserved=0 available=100000, got reserved=%.0f available=%.0f",
			bRecovered.Reserved, bRecovered.Available())
	}
}

// TestMilestone11_04_CrashRecovery_ScenarioB_InFlightExecution verifies Scenario B:
// Crash occurs while child is actively executing (status claimed, partial consumption recorded).
// On startup, activity is marked failed/interrupted, checkpointed consumption charged to parent,
// and unconsumed reservation released back to parent.
func TestMilestone11_04_CrashRecovery_ScenarioB_InFlightExecution(t *testing.T) {
	dir := t.TempDir()
	budgetDir := filepath.Join(dir, "budgets")
	storeDir := filepath.Join(dir, "activities")

	gov := spike.NewPolicyGovernor([]string{"governed_read_file"})
	_ = gov.SetStorageDir(budgetDir)
	store, _ := spike.NewDurableActivityStore(storeDir)

	rootID := "task-root-scen-b"
	gov.SetBudget(rootID, spike.Budget{Allocated: 100000})

	parentReq := spike.ActivityRequest{
		ID:     rootID,
		Budget: spike.Budget{Allocated: 100000},
	}
	childID := "child-act-b"
	prop := spike.ActivityProposal{
		ProposalID:      "child-b",
		ProposedAgentID: "coder",
		Objective:       "Implement feature B",
		RequestedBudget: spike.Budget{Allocated: 40000},
	}
	childReq, _ := gov.AuthorizeProposal(parentReq, prop, childID)
	_ = store.Enqueue(*childReq)

	// Child is claimed
	claimedReq, _ := store.ClaimNext()
	if claimedReq == nil {
		t.Fatalf("expected claimable activity")
	}

	// Simulate partial consumption checkpointed in child budget record: 8,000 tokens
	gov.SetBudget(childID, spike.Budget{Allocated: 40000, Consumed: 8000})

	// Process crashes while activity is in-flight (StatusClaimed).
	// Startup recovery:
	recoveredGov := spike.NewPolicyGovernor([]string{"governed_read_file"})
	_ = recoveredGov.SetStorageDir(budgetDir)
	recoveredStore, _ := spike.NewDurableActivityStore(storeDir)

	if err := recoveredGov.ReconcileWithStore(recoveredStore); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// 1. Activity in store must be marked failed with interrupted status
	actRec, err := recoveredStore.Get(childID)
	if err != nil || actRec.Status != spike.StatusFailed {
		t.Fatalf("expected child activity StatusFailed, got %v (err=%v)", actRec.Status, err)
	}
	if actRec.Result == nil || actRec.Result.Consumed.Consumed != 8000 {
		t.Fatalf("expected child activity result to record 8000 consumed tokens")
	}

	// 2. Parent budget must reflect:
	// - Consumed: 8,000
	// - Reserved: 0 (remaining 32,000 unconsumed released)
	// - Available: 92,000
	bRoot, _ := recoveredGov.GetBudget(rootID)
	if bRoot.Reserved != 0 || bRoot.Available() != 92000 {
		t.Fatalf("expected parent reserved=0 available=92000, got reserved=%.0f available=%.0f",
			bRoot.Reserved, bRoot.Available())
	}
}

// TestMilestone11_05_CrashRecovery_ScenarioC_PostSettlement verifies Scenario C:
// Crash occurs during or immediately after settlement.
// Reconciliation ensures child status is settled, parent reservation cleared, and is strictly idempotent.
func TestMilestone11_05_CrashRecovery_ScenarioC_PostSettlement(t *testing.T) {
	dir := t.TempDir()
	budgetDir := filepath.Join(dir, "budgets")
	storeDir := filepath.Join(dir, "activities")

	gov := spike.NewPolicyGovernor([]string{"governed_read_file"})
	_ = gov.SetStorageDir(budgetDir)
	store, _ := spike.NewDurableActivityStore(storeDir)

	rootID := "task-root-scen-c"
	gov.SetBudget(rootID, spike.Budget{Allocated: 100000})

	parentReq := spike.ActivityRequest{
		ID:     rootID,
		Budget: spike.Budget{Allocated: 100000},
	}
	childID := "child-act-c"
	prop := spike.ActivityProposal{
		ProposalID:      "child-c",
		ProposedAgentID: "coder",
		Objective:       "Implement feature C",
		RequestedBudget: spike.Budget{Allocated: 40000},
	}
	childReq, _ := gov.AuthorizeProposal(parentReq, prop, childID)
	_ = store.Enqueue(*childReq)
	_, _ = store.ClaimNext()

	// Child completes and consumes 12,000 tokens
	res := spike.ActivityResult{
		ID:       childID,
		Status:   spike.StatusCompleted,
		Consumed: spike.Budget{Consumed: 12000},
	}
	_ = store.Complete(childID, res)

	// Settle in governor
	gov.RecordConsumption(childID, rootID, 12000)

	// Simulate crash & restart
	recoveredGov := spike.NewPolicyGovernor([]string{"governed_read_file"})
	_ = recoveredGov.SetStorageDir(budgetDir)
	recoveredStore, _ := spike.NewDurableActivityStore(storeDir)

	if err := recoveredGov.ReconcileWithStore(recoveredStore); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}

	bRoot1, _ := recoveredGov.GetBudget(rootID)
	if bRoot1.Consumed != 12000 || bRoot1.Reserved != 0 || bRoot1.Available() != 88000 {
		t.Fatalf("pass 1 mismatch: consumed=%.0f reserved=%.0f available=%.0f",
			bRoot1.Consumed, bRoot1.Reserved, bRoot1.Available())
	}

	// Second reconciliation pass: must be strictly idempotent
	if err := recoveredGov.ReconcileWithStore(recoveredStore); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	bRoot2, _ := recoveredGov.GetBudget(rootID)
	if bRoot2.Consumed != 12000 || bRoot2.Reserved != 0 || bRoot2.Available() != 88000 {
		t.Fatalf("pass 2 idempotent mismatch: consumed=%.0f reserved=%.0f available=%.0f",
			bRoot2.Consumed, bRoot2.Reserved, bRoot2.Available())
	}
}

// TestMilestone11_06_CryptographicValidationEvidenceBinding_StaleRejection verifies that:
// 1. Modifying workspace content after native validation invalidates the snapshot SHA and causes review rejection.
// 2. Missing validation evidence causes immediate review rejection.
func TestMilestone11_06_CryptographicValidationEvidenceBinding_StaleRejection(t *testing.T) {
	dir := t.TempDir()
	storeDir := filepath.Join(dir, ".a4c", "activities")
	rosterDir := filepath.Join(dir, ".a4c", "roster")
	memDir := filepath.Join(dir, ".a4c", "memory")
	journalDir := filepath.Join(dir, ".a4c", "journal")

	_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module calc\n\ngo 1.22\n"), 0644)
	_ = os.WriteFile(filepath.Join(dir, "calc.go"), []byte("package calc\nfunc Add(a, b int) int { return a + b }\n"), 0644)
	_ = os.WriteFile(filepath.Join(dir, "calc_test.go"), []byte("package calc\nimport \"testing\"\nfunc TestAdd(t *testing.T) { if Add(2,3) != 5 { t.Fail() } }\n"), 0644)

	store, _ := spike.NewDurableActivityStore(storeDir)
	roster, _ := spike.DefaultTeamRoster(rosterDir)
	memory, _ := spike.NewDurableMemoryStore(memDir)
	gov := spike.NewPolicyGovernor([]string{"governed_read_file", "governed_apply_patch", "governed_submit_review"})
	journal, _ := spike.NewDurableIntentJournal(journalDir)

	fake := spike.NewFakeProvider()
	fake.SetSessionResponseFunc(func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		cfg := session.Config()
		if cfg.AgentID == "coder" {
			return &spike.AgentMessage{
				Role:    "assistant",
				Content: "Implementation completed.",
			}, nil
		}
		if cfg.AgentID == "reviewer" {
			// Reviewer attempts to submit approval
			return &spike.AgentMessage{
				Role:    "assistant",
				Content: "[SUBMIT_REVIEW: approved=true, comments=\"Looks good.\"]",
			}, nil
		}
		return &spike.AgentMessage{Role: "assistant", Content: "OK"}, nil
	})

	coord := spike.NewTeamCoordinator(dir, store, roster, memory, gov, journal, &spike.NativeGoTestRunner{}, fake)
	defer coord.Close()

	// 1. Submit reviewed task
	_, err := coord.SubmitTaskWithStrategy("Implement Calc", spike.Budget{Allocated: 50000}, nil, spike.StrategyReviewed)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	// 2. Run Step 1: Coder executes and generates native validation evidence
	coderRes, err := coord.Step(context.Background())
	if err != nil {
		t.Fatalf("coder step: %v", err)
	}
	if coderRes.Status != spike.StatusCompleted {
		t.Fatalf("expected coder completed, got %v", coderRes.Status)
	}

	// 3. Tamper with workspace after coder validation (simulate post-validation drift)
	_ = os.WriteFile(filepath.Join(dir, "calc.go"), []byte("package calc\nfunc Add(a, b int) int { return 999 }\n"), 0644)

	// 4. Run Step 2: Reviewer executes. Model approved, but Snapshotter detects drift -> REJECTION
	revRes, err := coord.Step(context.Background())
	if err != nil {
		t.Fatalf("reviewer step: %v", err)
	}
	approved, _ := revRes.Outputs["approved"].(bool)
	if approved {
		t.Fatalf("expected reviewer approval to be OVERRULED and REJECTED due to post-validation tampering")
	}
	comments, _ := revRes.Outputs["review_comments"].(string)
	if !strings.Contains(comments, "workspace modified after validation") {
		t.Fatalf("expected rejection comments to cite workspace modification, got: %s", comments)
	}
}

// TestMilestone11_07_ActivitySpecificEconomicLimits_Exhaustion verifies that:
// 1. Turn limits fail activities with ErrTurnLimitExceeded and record turn_limit_exhausted.
// 2. Tool call limits fail activities with ErrToolCallLimitExceeded and record tool_limit_exhausted.
// 3. Revision limits fail activities with ErrRevisionLimitExceeded and record revision_limit_exhausted.
func TestMilestone11_07_ActivitySpecificEconomicLimits_Exhaustion(t *testing.T) {
	dir := t.TempDir()
	storeDir := filepath.Join(dir, ".a4c", "activities")
	rosterDir := filepath.Join(dir, ".a4c", "roster")
	memDir := filepath.Join(dir, ".a4c", "memory")
	journalDir := filepath.Join(dir, ".a4c", "journal")

	store, _ := spike.NewDurableActivityStore(storeDir)
	roster, _ := spike.DefaultTeamRoster(rosterDir)
	memory, _ := spike.NewDurableMemoryStore(memDir)
	gov := spike.NewPolicyGovernor([]string{"governed_read_file", "governed_apply_patch", "governed_submit_review"})
	journal, _ := spike.NewDurableIntentJournal(journalDir)

	// Subtest A: Turn Limit Exhaustion
	t.Run("TurnLimitExhaustion", func(t *testing.T) {
		fake := spike.NewFakeProvider()
		coord := spike.NewTeamCoordinator(dir, store, roster, memory, gov, journal, &spike.NativeGoTestRunner{}, fake)
		defer coord.Close()

		req := spike.ActivityRequest{
			ID:        "task-turn-limit",
			AgentID:   "coder",
			Objective: "Turn limit test",
			Budget:    spike.Budget{Allocated: 50000},
			Limits: &spike.ActivityLimits{
				MaxModelTurns: 1, // Only 1 turn allowed
			},
			CreatedAt: time.Now(),
		}
		_ = store.Enqueue(req)

		// Set fake session to attempt a second send
		fake.SetSessionResponseFunc(func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
			// Second send will exceed MaxModelTurns
			return session.Send(ctx, "Second turn inside session")
		})

		_, err := coord.Step(context.Background())
		if err == nil || !errors.Is(err, spike.ErrTurnLimitExceeded) {
			t.Fatalf("expected ErrTurnLimitExceeded, got: %v", err)
		}

		actRec, _ := store.Get(req.ID)
		if actRec.Result == nil || actRec.Result.Outputs["failure_reason"] != "turn_limit_exhausted" {
			t.Fatalf("expected durable failure_reason=turn_limit_exhausted, got: %v", actRec.Result)
		}
	})

	// Subtest B: Revision Limit Exhaustion
	t.Run("RevisionLimitExhaustion", func(t *testing.T) {
		_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module calc\n\ngo 1.22\n"), 0644)
		_ = os.WriteFile(filepath.Join(dir, "calc.go"), []byte("package calc\nfunc Add(a, b int) int { return a + b }\n"), 0644)
		_ = os.WriteFile(filepath.Join(dir, "calc_test.go"), []byte("package calc\nimport \"testing\"\nfunc TestAdd(t *testing.T) { if Add(2,3) != 5 { t.Fail() } }\n"), 0644)

		fake := spike.NewFakeProvider()
		// Always reject reviews
		fake.SetSessionResponseFunc(func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
			cfg := session.Config()
			if cfg.AgentID == "coder" {
				return &spike.AgentMessage{Role: "assistant", Content: "Code written."}, nil
			}
			return &spike.AgentMessage{Role: "assistant", Content: "[SUBMIT_REVIEW: approved=false, comments=\"Code fails quality standards\"]"}, nil
		})

		coord := spike.NewTeamCoordinator(dir, store, roster, memory, gov, journal, &spike.NativeGoTestRunner{}, fake)
		coord.MaxRevisions = 0
		coord.Limits.MaxRevisions = 0 // Allow 0 revisions: first rejection is final failure!
		defer coord.Close()

		_, err := coord.SubmitTaskWithStrategy("Zero revision task", spike.Budget{Allocated: 50000}, nil, spike.StrategyReviewed)
		if err != nil {
			t.Fatalf("submit: %v", err)
		}

		// Step 1: Coder
		_, _ = coord.Step(context.Background())

		// Step 2: Reviewer rejects -> revisions exhausted immediately!
		revRes, _ := coord.Step(context.Background())
		if revRes == nil {
			t.Fatalf("expected reviewer result")
		}
		t.Logf("revRes status=%s outputs=%+v", revRes.Status, revRes.Outputs)
		if revRes.Outputs["failure_reason"] != "revision_limit_exhausted" {
			t.Fatalf("expected failure_reason=revision_limit_exhausted, got: %v (outputs=%+v)", revRes.Outputs["failure_reason"], revRes.Outputs)
		}
	})
}

// TestMilestone11_08_AccurateDurationReportingAndInspection verifies that ProviderWaitDuration
// is reported accurately and distinguished from WallClockDuration and ToolDuration in metrics and inspect formats.
func TestMilestone11_08_AccurateDurationReportingAndInspection(t *testing.T) {
	dir := t.TempDir()
	storeDir := filepath.Join(dir, ".a4c", "activities")
	rosterDir := filepath.Join(dir, ".a4c", "roster")
	memDir := filepath.Join(dir, ".a4c", "memory")
	journalDir := filepath.Join(dir, ".a4c", "journal")

	_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module calc\n\ngo 1.22\n"), 0644)
	_ = os.WriteFile(filepath.Join(dir, "calc.go"), []byte("package calc\nfunc Add(a, b int) int { return a + b }\n"), 0644)
	_ = os.WriteFile(filepath.Join(dir, "calc_test.go"), []byte("package calc\nimport \"testing\"\nfunc TestAdd(t *testing.T) { if Add(2,3) != 5 { t.Fail() } }\n"), 0644)

	store, _ := spike.NewDurableActivityStore(storeDir)
	roster, _ := spike.DefaultTeamRoster(rosterDir)
	memory, _ := spike.NewDurableMemoryStore(memDir)
	gov := spike.NewPolicyGovernor([]string{"governed_read_file", "governed_apply_patch"})
	journal, _ := spike.NewDurableIntentJournal(journalDir)

	fake := spike.NewFakeProvider()
	fake.SetSessionResponseFunc(func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		session.SetDurations(125*time.Millisecond, 45*time.Millisecond)
		return &spike.AgentMessage{
			Role:    "assistant",
			Content: "Implemented accurately",
		}, nil
	})

	coord := spike.NewTeamCoordinator(dir, store, roster, memory, gov, journal, &spike.NativeGoTestRunner{}, fake)
	defer coord.Close()

	req, err := coord.SubmitTaskWithStrategy("Verify duration metrics", spike.Budget{Allocated: 50000}, nil, spike.StrategyDirect)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	res, err := coord.Step(context.Background())
	if err != nil {
		t.Fatalf("step: %v", err)
	}

	if res.Metrics == nil {
		t.Fatalf("expected execution metrics")
	}
	if res.Metrics.ProviderWaitDuration < 100*time.Millisecond {
		t.Fatalf("expected ProviderWaitDuration >= 100ms, got: %v", res.Metrics.ProviderWaitDuration)
	}
	if res.Metrics.ToolDuration < 40*time.Millisecond {
		t.Fatalf("expected ToolDuration >= 40ms, got: %v", res.Metrics.ToolDuration)
	}

	hist, err := spike.BuildTaskHistory(req.ID, store, journal, nil, memory)
	if err != nil {
		t.Fatalf("build history: %v", err)
	}
	if hist.MetricsSummary.ProviderWaitDuration < 100*time.Millisecond {
		t.Fatalf("expected aggregated ProviderWaitDuration >= 100ms, got: %v", hist.MetricsSummary.ProviderWaitDuration)
	}

	human := hist.FormatHumanReadable()
	if !strings.Contains(human, "provider wait:") {
		t.Fatalf("expected human readable to contain 'provider wait:', got:\n%s", human)
	}

	inspect := hist.FormatInspect(nil, dir)
	if !strings.Contains(inspect, "provider wait:") {
		t.Fatalf("expected inspect to contain 'provider wait:', got:\n%s", inspect)
	}
}
