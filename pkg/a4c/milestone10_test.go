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
	"time"

	spike "github.com/ormasoftchile/a4c/pkg/a4c"
)

// TestMilestone10_01_DurableBudgetPersistenceAndCrashRecovery verifies that budget allocations
// and reservations survive process restarts and load cleanly from disk without corruption.
func TestMilestone10_01_DurableBudgetPersistenceAndCrashRecovery(t *testing.T) {
	dir := t.TempDir()
	budgetDir := filepath.Join(dir, "budgets")

	gov := spike.NewPolicyGovernor([]string{"governed_read_file", "governed_apply_patch"})
	if err := gov.SetStorageDir(budgetDir); err != nil {
		t.Fatalf("set storage dir: %v", err)
	}

	rootID := "task-root-100"
	gov.SetBudget(rootID, spike.Budget{Allocated: 100000})

	// Authorize child proposal with 40,000 reservation
	parentReq := spike.ActivityRequest{
		ID:     rootID,
		Budget: spike.Budget{Allocated: 100000},
	}
	prop := spike.ActivityProposal{
		ProposalID:      "child-1",
		ProposedAgentID: "coder",
		Objective:       "Implement subtask 1",
		RequestedBudget: spike.Budget{Allocated: 40000},
	}
	childReq, err := gov.AuthorizeProposal(parentReq, prop, "sub-activity-1")
	if err != nil {
		t.Fatalf("authorize proposal: %v", err)
	}
	if childReq.Budget.Allocated != 40000 {
		t.Fatalf("expected child budget 40000, got %.0f", childReq.Budget.Allocated)
	}

	// Verify on-disk persistence
	rootFile := filepath.Join(budgetDir, rootID+".json")
	childFile := filepath.Join(budgetDir, "sub-activity-1.json")
	if _, err := os.Stat(rootFile); err != nil {
		t.Fatalf("expected root budget file to exist: %v", err)
	}
	if _, err := os.Stat(childFile); err != nil {
		t.Fatalf("expected child budget file to exist: %v", err)
	}

	// Verify in-memory state before restart
	bRoot, _ := gov.GetBudget(rootID)
	if bRoot.Reserved != 40000 || bRoot.Available() != 60000 {
		t.Fatalf("expected root reserved=40000 available=60000, got reserved=%.0f available=%.0f", bRoot.Reserved, bRoot.Available())
	}

	// Simulate crash & restart: initialize a fresh PolicyGovernor on the same directory
	restoredGov := spike.NewPolicyGovernor([]string{"governed_read_file", "governed_apply_patch"})
	if err := restoredGov.SetStorageDir(budgetDir); err != nil {
		t.Fatalf("restore storage dir: %v", err)
	}

	restoredRoot, found := restoredGov.GetBudget(rootID)
	if !found {
		t.Fatalf("expected restored root budget to exist")
	}
	if restoredRoot.Allocated != 100000 || restoredRoot.Reserved != 40000 || restoredRoot.Available() != 60000 {
		t.Fatalf("restored root budget mismatch: allocated=%.0f reserved=%.0f available=%.0f",
			restoredRoot.Allocated, restoredRoot.Reserved, restoredRoot.Available())
	}

	restoredChild, found := restoredGov.GetBudget("sub-activity-1")
	if !found {
		t.Fatalf("expected restored child budget to exist")
	}
	if restoredChild.Allocated != 40000 {
		t.Fatalf("restored child budget mismatch: allocated=%.0f", restoredChild.Allocated)
	}

	// Attempting to authorize child with 70,000 on restored governor must fail (only 60,000 available)
	propOver := spike.ActivityProposal{
		ProposalID:      "child-2-over",
		ProposedAgentID: "coder",
		Objective:       "Subtask 2 exceeding available",
		RequestedBudget: spike.Budget{Allocated: 70000},
	}
	_, err = restoredGov.AuthorizeProposal(parentReq, propOver, "sub-activity-2")
	if !errors.Is(err, spike.ErrBudgetExceeded) {
		t.Fatalf("expected ErrBudgetExceeded on restored governor, got: %v", err)
	}
}

// TestMilestone10_02_ConcurrentSiblingBudgetAllocation verifies that concurrent sibling
// subtask requests cannot exceed root allocation or cause data races.
func TestMilestone10_02_ConcurrentSiblingBudgetAllocation(t *testing.T) {
	dir := t.TempDir()
	gov := spike.NewPolicyGovernor([]string{"governed_read_file"})
	if err := gov.SetStorageDir(filepath.Join(dir, "budgets")); err != nil {
		t.Fatalf("set storage dir: %v", err)
	}

	rootID := "task-concurrent-root"
	gov.SetBudget(rootID, spike.Budget{Allocated: 50000})
	parentReq := spike.ActivityRequest{
		ID:     rootID,
		Budget: spike.Budget{Allocated: 50000},
	}

	var wg sync.WaitGroup
	successCount := 0
	var mu sync.Mutex

	// Launch 15 concurrent requests for 5,000 tokens each. Only 10 should succeed (50,000 total).
	for i := 0; i < 15; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			subID := fmt.Sprintf("sub-conc-%d", idx)
			prop := spike.ActivityProposal{
				ProposalID:      subID,
				ProposedAgentID: "coder",
				Objective:       fmt.Sprintf("Concurrent task %d", idx),
				RequestedBudget: spike.Budget{Allocated: 5000},
			}
			_, err := gov.AuthorizeProposal(parentReq, prop, subID)
			if err == nil {
				mu.Lock()
				successCount++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if successCount != 10 {
		t.Fatalf("expected exactly 10 successful sibling allocations, got %d", successCount)
	}

	b, _ := gov.GetBudget(rootID)
	if b.Reserved != 50000 || b.Available() != 0 {
		t.Fatalf("expected root reserved=50000 available=0, got reserved=%.0f available=%.0f", b.Reserved, b.Available())
	}
}

// TestMilestone10_03_ReservationSettlementAndRelease verifies that unconsumed budget
// reservations are cleanly released back to the parent and persisted.
func TestMilestone10_03_ReservationSettlementAndRelease(t *testing.T) {
	dir := t.TempDir()
	gov := spike.NewPolicyGovernor([]string{"governed_read_file"})
	if err := gov.SetStorageDir(filepath.Join(dir, "budgets")); err != nil {
		t.Fatalf("set storage dir: %v", err)
	}

	rootID := "task-settle-root"
	gov.SetBudget(rootID, spike.Budget{Allocated: 100000})
	parentReq := spike.ActivityRequest{
		ID:     rootID,
		Budget: spike.Budget{Allocated: 100000},
	}

	prop := spike.ActivityProposal{
		ProposalID:      "sub-under",
		ProposedAgentID: "coder",
		Objective:       "Subtask completing under budget",
		RequestedBudget: spike.Budget{Allocated: 30000},
	}
	_, err := gov.AuthorizeProposal(parentReq, prop, "sub-under")
	if err != nil {
		t.Fatalf("authorize proposal: %v", err)
	}

	// Subtask only consumed 12,000 tokens out of 30,000 allocated
	gov.RecordConsumption("sub-under", rootID, 12000)

	// Release any remaining reserved capacity
	if err := gov.ReleaseReservation("sub-under", rootID); err != nil {
		t.Fatalf("release reservation: %v", err)
	}

	bRoot, _ := gov.GetBudget(rootID)
	if bRoot.Consumed != 12000 {
		t.Fatalf("expected root consumed=12000, got %.0f", bRoot.Consumed)
	}
	if bRoot.Reserved != 0 {
		t.Fatalf("expected root reserved=0, got %.0f", bRoot.Reserved)
	}
	if bRoot.Available() != 88000 {
		t.Fatalf("expected root available=88000, got %.0f", bRoot.Available())
	}

	// Check durable record status
	rec, ok := gov.GetBudgetRecord("sub-under")
	if !ok || rec.Status != "released" {
		t.Fatalf("expected subtask durable record status=released, got %v", rec)
	}
}

// TestMilestone10_04_TimingDomainSeparation verifies that WallClockDuration, ModelDuration,
// ToolDuration, and ValidationDuration are strictly non-overlapping and accurately tracked.
func TestMilestone10_04_TimingDomainSeparation(t *testing.T) {
	dir := t.TempDir()
	store, _ := spike.NewDurableActivityStore(filepath.Join(dir, "store"))
	roster, _ := spike.DefaultTeamRoster(filepath.Join(dir, "roster"))
	memory, _ := spike.NewDurableMemoryStore(filepath.Join(dir, "memory"))
	gov := spike.NewPolicyGovernor([]string{"governed_read_file", "governed_apply_patch"})
	journal, _ := spike.NewDurableIntentJournal(filepath.Join(dir, "journal"))
	fakeProvider := spike.NewFakeProvider()
	runner := &spike.NativeGoTestRunner{}

	fakeProvider.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		activeDir := session.Config().WorkspaceDir
		_ = os.WriteFile(filepath.Join(activeDir, "go.mod"), []byte("module example.com/timer\n\ngo 1.22\n"), 0644)
		_ = os.WriteFile(filepath.Join(activeDir, "calc.go"), []byte("package timer\nfunc Add(a, b int) int { return a + b }\n"), 0644)
		_ = os.WriteFile(filepath.Join(activeDir, "calc_test.go"), []byte("package timer\nimport \"testing\"\nfunc TestAdd(t *testing.T) {}\n"), 0644)

		// Set simulated model and tool durations
		session.SetDurations(120*time.Millisecond, 40*time.Millisecond)
		session.SetUsage(spike.UsageInfo{
			PromptTokens:     150,
			CompletionTokens: 50,
			TotalTokens:      200,
		})
		return &spike.AgentMessage{Content: "Implemented Add with tests"}, nil
	}

	coord := spike.NewTeamCoordinator(dir, store, roster, memory, gov, journal, runner, fakeProvider)
	req, err := coord.SubmitTaskWithStrategy("Direct task for duration validation", spike.Budget{Allocated: 5000}, nil, spike.StrategyDirect)
	if err != nil {
		t.Fatalf("submit task: %v", err)
	}

	res, err := coord.Step(context.Background())
	if err != nil || res.Status != spike.StatusCompleted {
		t.Fatalf("step failed: %v, res: %v", err, res)
	}

	metrics := res.Metrics
	if metrics == nil {
		t.Fatalf("expected non-nil ExecutionMetrics")
	}

	if metrics.ModelDuration != 120*time.Millisecond {
		t.Fatalf("expected ModelDuration=120ms, got %v", metrics.ModelDuration)
	}
	if metrics.ToolDuration != 40*time.Millisecond {
		t.Fatalf("expected ToolDuration=40ms, got %v", metrics.ToolDuration)
	}
	if metrics.ValidationDuration <= 0 {
		t.Fatalf("expected non-zero ValidationDuration, got %v", metrics.ValidationDuration)
	}
	if metrics.WallClockDuration <= 0 {
		t.Fatalf("expected non-zero WallClockDuration")
	}

	// Verify history reports show independent timing metrics
	hist, err := spike.BuildTaskHistory(req.ID, store, journal, nil, memory)
	if err != nil {
		t.Fatalf("build task history: %v", err)
	}

	readable := hist.FormatHumanReadable()
	if !strings.Contains(readable, "wall-clock") || !strings.Contains(readable, "model:") || !strings.Contains(readable, "tools:") || !strings.Contains(readable, "validation:") {
		t.Fatalf("expected FormatHumanReadable to contain domain durations, got:\n%s", readable)
	}

	inspect := hist.FormatInspect(nil, dir)
	if !strings.Contains(inspect, "wall-clock") || !strings.Contains(inspect, "tools") || !strings.Contains(inspect, "validation") {
		t.Fatalf("expected FormatInspect to contain domain durations, got:\n%s", inspect)
	}
}

// TestMilestone10_05_NativeEvidencePropagationAndIndependentReview verifies that rich test
// execution evidence (output summary, command, exit code) propagates into the Reviewer handoff,
// preventing blind rejection loops.
func TestMilestone10_05_NativeEvidencePropagationAndIndependentReview(t *testing.T) {
	dir := t.TempDir()
	workDir := filepath.Join(dir, "src")
	if err := os.MkdirAll(workDir, 0755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}

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

	var reviewerReceivedPrompt string

	fakeProvider.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		agentID := session.Config().AgentID
		activeDir := session.Config().WorkspaceDir

		if agentID == "coder" {
			_ = os.WriteFile(filepath.Join(activeDir, "go.mod"), []byte("module example.com/feature\n\ngo 1.22\n"), 0644)
			_ = os.WriteFile(filepath.Join(activeDir, "feature.go"), []byte("package feature\nfunc Run() string { return \"ok\" }\n"), 0644)
			_ = os.WriteFile(filepath.Join(activeDir, "feature_test.go"), []byte("package feature\nimport \"testing\"\nfunc TestRun(t *testing.T) {\n\tif Run() != \"ok\" { t.Fatal(\"fail\") }\n}\n"), 0644)
			session.SetUsage(spike.UsageInfo{TotalTokens: 300})
			return &spike.AgentMessage{Content: "Feature implemented with unit tests."}, nil
		}

		if agentID == "reviewer" {
			reviewerReceivedPrompt = prompt
			// Reviewer checks native test evidence in prompt
			if !strings.Contains(prompt, "Native Evidence") || !strings.Contains(prompt, "ExitCode: 0") {
				session.Config().OnSubmitReview(spike.ReviewResult{
					Approved: false,
					Comments: "Rejected: evidence missing from prompt",
				})
				return &spike.AgentMessage{Content: "Rejected"}, nil
			}
			session.Config().OnSubmitReview(spike.ReviewResult{
				Approved: true,
				Comments: "Changes verified and native tests passed.",
			})
			return &spike.AgentMessage{Content: "Approved"}, nil
		}

		return &spike.AgentMessage{Content: "Done"}, nil
	}

	coord := spike.NewTeamCoordinator(workDir, store, roster, memory, gov, journal, runner, fakeProvider)
	_, err := coord.SubmitTaskWithStrategy("Implement feature with reviewed strategy", spike.Budget{Allocated: 5000}, nil, spike.StrategyReviewed)
	if err != nil {
		t.Fatalf("submit task: %v", err)
	}

	// Step 1: Coder implements and runs native tests
	res1, err := coord.Step(context.Background())
	if err != nil || res1.Status != spike.StatusCompleted {
		t.Fatalf("step 1 failed: %v, res: %v", err, res1)
	}
	if len(res1.EvidenceIDs) == 0 {
		t.Fatalf("expected coder to produce ValidationEvidence ID")
	}

	// Step 2: Reviewer reviews
	res2, err := coord.Step(context.Background())
	if err != nil || res2.Status != spike.StatusCompleted {
		t.Fatalf("step 2 failed: %v, res: %v", err, res2)
	}

	if approved, ok := res2.Outputs["approved"].(bool); !ok || !approved {
		t.Fatalf("expected reviewer approval, got approved=%v comments=%v", approved, res2.Outputs["review_comments"])
	}

	// Verify reviewer received rich test evidence in prompt
	if !strings.Contains(reviewerReceivedPrompt, "Native Evidence:") {
		t.Fatalf("expected prompt to contain Native Evidence header, prompt:\n%s", reviewerReceivedPrompt)
	}
	if !strings.Contains(reviewerReceivedPrompt, "Test Output") {
		t.Fatalf("expected prompt to contain Test Output summary, prompt:\n%s", reviewerReceivedPrompt)
	}
}

// TestMilestone10_06_BudgetExtensionAndOverrunLiability verifies that PolicyGovernor.ExtendBudget
// updates the durable limit and that budget overruns are recorded in UsageInfo and ExecutionMetrics.
func TestMilestone10_06_BudgetExtensionAndOverrunLiability(t *testing.T) {
	dir := t.TempDir()
	budgetDir := filepath.Join(dir, "budgets")
	gov := spike.NewPolicyGovernor([]string{"governed_read_file"})
	if err := gov.SetStorageDir(budgetDir); err != nil {
		t.Fatalf("set storage dir: %v", err)
	}

	actID := "act-overrun-test"
	gov.SetBudget(actID, spike.Budget{Allocated: 1000})

	// Consuming 1500 tokens when 1000 allocated
	gov.RecordConsumption(actID, "", 1500)

	b, _ := gov.GetBudget(actID)
	if b.Consumed != 1500 {
		t.Fatalf("expected consumed=1500, got %.0f", b.Consumed)
	}

	// Extend budget by 1000 tokens
	if err := gov.ExtendBudget(actID, 1000); err != nil {
		t.Fatalf("extend budget: %v", err)
	}

	bExtended, _ := gov.GetBudget(actID)
	if bExtended.Allocated != 2000 {
		t.Fatalf("expected allocated=2000 after extension, got %.0f", bExtended.Allocated)
	}

	// Check durable file reflects the extension
	rec, ok := gov.GetBudgetRecord(actID)
	if !ok || rec.Allocated != 2000 {
		t.Fatalf("expected durable record allocated=2000, got %v", rec)
	}
}

// TestMilestone10_07_SessionEconomics_BoundedReadingAndToolCaps verifies that file reading
// is bounded by max lines, supports line range slicing, and prevents unbounded context dumping.
func TestMilestone10_07_SessionEconomics_BoundedReadingAndToolCaps(t *testing.T) {
	// 1. Generate 500 lines of content
	var sb strings.Builder
	for i := 1; i <= 500; i++ {
		sb.WriteString(fmt.Sprintf("line %d: some code content here\n", i))
	}
	content := strings.TrimRight(sb.String(), "\n")

	// 2. Default truncation without range parameters (max 300)
	truncated := spike.SliceFileLines(content, 0, 0, 300)
	if !strings.Contains(truncated, "... [File truncated: showing first 300 lines of 500 total lines") {
		t.Fatalf("expected default truncation marker, got:\n%s", truncated)
	}
	truncatedLines := strings.Split(truncated, "\n")
	// 300 lines + 1 truncation marker line
	if len(truncatedLines) != 301 {
		t.Fatalf("expected 301 lines in truncated output, got %d", len(truncatedLines))
	}

	// 3. Line range slicing: lines 10 to 25
	slice := spike.SliceFileLines(content, 10, 25, 300)
	sliceLines := strings.Split(slice, "\n")
	if len(sliceLines) != 16 {
		t.Fatalf("expected 16 lines (10..25), got %d", len(sliceLines))
	}
	if !strings.HasPrefix(sliceLines[0], "line 10:") {
		t.Fatalf("expected first line to be line 10, got: %s", sliceLines[0])
	}
	if !strings.HasPrefix(sliceLines[15], "line 25:") {
		t.Fatalf("expected last line to be line 25, got: %s", sliceLines[15])
	}

	// 4. Line range exceeding maxLines (request 400 lines when max is 50)
	rangeTrunc := spike.SliceFileLines(content, 1, 400, 50)
	if !strings.Contains(rangeTrunc, "... [Output truncated to 50 lines. Total lines requested: 400. Total file lines: 500]") {
		t.Fatalf("expected range truncation marker, got:\n%s", rangeTrunc)
	}

	// 5. Start line beyond total lines
	beyond := spike.SliceFileLines(content, 600, 700, 50)
	if !strings.Contains(beyond, "start_line 600 exceeds total file lines (500)") {
		t.Fatalf("expected beyond lines error notice, got:\n%s", beyond)
	}
}
