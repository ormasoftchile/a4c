package a4c_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	spike "github.com/ormasoftchile/a4c/pkg/a4c"
)

// TestBenchmark_01_StrategyDispatch verifies that the three execution strategies
// (direct, reviewed, team) schedule the correct initial agents and DAG dependencies.
func TestBenchmark_01_StrategyDispatch(t *testing.T) {
	dir := t.TempDir()
	store, _ := spike.NewDurableActivityStore(filepath.Join(dir, "activities"))
	memory, _ := spike.NewDurableMemoryStore(filepath.Join(dir, "memory"))
	roster, _ := spike.DefaultTeamRoster(filepath.Join(dir, "roster"))
	gov := spike.NewPolicyGovernor([]string{"governed_read_file", "governed_apply_patch"})
	journal, _ := spike.NewDurableIntentJournal(filepath.Join(dir, "journal"))
	fakeProvider := spike.NewFakeProvider()
	runner := &spike.NativeGoTestRunner{}

	coord := spike.NewTeamCoordinator(dir, store, roster, memory, gov, journal, runner, fakeProvider)

	// 1. StrategyDirect -> enqueues single coder activity
	reqDirect, err := coord.SubmitTaskWithStrategy("Quick fix", spike.Budget{Allocated: 5000}, nil, spike.StrategyDirect)
	if err != nil {
		t.Fatalf("failed direct submit: %v", err)
	}
	if reqDirect.AgentID != "coder" || reqDirect.Strategy != spike.StrategyDirect {
		t.Fatalf("expected direct strategy assigned to coder, got agent=%s strategy=%s", reqDirect.AgentID, reqDirect.Strategy)
	}

	// 2. StrategyReviewed -> enqueues coder + reviewer with DAG dependency
	reqReviewed, err := coord.SubmitTaskWithStrategy("Reviewed fix", spike.Budget{Allocated: 10000}, nil, spike.StrategyReviewed)
	if err != nil {
		t.Fatalf("failed reviewed submit: %v", err)
	}
	if reqReviewed.AgentID != "coder" || reqReviewed.Strategy != spike.StrategyReviewed {
		t.Fatalf("expected reviewed strategy assigned to coder, got %s", reqReviewed.AgentID)
	}
	tree := store.GetTaskTree(reqReviewed.ID)
	if len(tree) != 2 {
		t.Fatalf("expected 2 activities (coder + reviewer) in reviewed task tree, got %d", len(tree))
	}
	if tree[1].Request.AgentID != "reviewer" || len(tree[1].Request.DependsOn) == 0 {
		t.Fatalf("expected reviewer with dependency on coder in reviewed strategy: %+v", tree[1])
	}

	// 3. StrategyTeam -> enqueues lead for dynamic multi-agent planning
	reqTeam, err := coord.SubmitTaskWithStrategy("Complex architecture", spike.Budget{Allocated: 20000}, nil, spike.StrategyTeam)
	if err != nil {
		t.Fatalf("failed team submit: %v", err)
	}
	if reqTeam.AgentID != "lead" || reqTeam.Strategy != spike.StrategyTeam {
		t.Fatalf("expected team strategy assigned to lead, got %s", reqTeam.AgentID)
	}
}

// TestBenchmark_02_QueryActivities verifies activity filtering by status, agent, and error presence.
func TestBenchmark_02_QueryActivities(t *testing.T) {
	dir := t.TempDir()
	store, _ := spike.NewDurableActivityStore(filepath.Join(dir, "activities"))

	// Seed 3 activities
	_ = store.Enqueue(spike.ActivityRequest{
		ID:        "act-1",
		AgentID:   "lead",
		Objective: "Plan feature",
		Budget:    spike.Budget{Allocated: 1000},
	})
	_ = store.Enqueue(spike.ActivityRequest{
		ID:        "act-2",
		AgentID:   "coder",
		Objective: "Implement feature",
		Budget:    spike.Budget{Allocated: 2000},
	})
	_ = store.Enqueue(spike.ActivityRequest{
		ID:        "act-3",
		AgentID:   "reviewer",
		Objective: "Review feature",
		Budget:    spike.Budget{Allocated: 1500},
	})

	// Complete act-1, fail act-2
	_ = store.Complete("act-1", spike.ActivityResult{ID: "act-1", Status: spike.StatusCompleted})
	_ = store.Fail("act-2", fmt.Errorf("compile error in module"))

	// Filter by status=completed
	completed := store.QueryActivities(spike.ActivityFilter{Status: spike.StatusCompleted})
	if len(completed) != 1 || completed[0].Request.ID != "act-1" {
		t.Fatalf("expected 1 completed activity (act-1), got %d", len(completed))
	}

	// Filter by agent=coder
	coderActs := store.QueryActivities(spike.ActivityFilter{AgentID: "coder"})
	if len(coderActs) != 1 || coderActs[0].Request.ID != "act-2" {
		t.Fatalf("expected 1 coder activity (act-2), got %d", len(coderActs))
	}

	// Filter by has_errors=true
	hasErr := true
	errorActs := store.QueryActivities(spike.ActivityFilter{HasErrors: &hasErr})
	if len(errorActs) != 1 || errorActs[0].Request.ID != "act-2" {
		t.Fatalf("expected 1 failed activity with errors, got %d", len(errorActs))
	}
}

// TestBenchmark_03_CascadingDependencyFailure verifies that blocked dependent activities
// fail fast rather than hanging indefinitely when an upstream dependency fails.
func TestBenchmark_03_CascadingDependencyFailure(t *testing.T) {
	dir := t.TempDir()
	store, _ := spike.NewDurableActivityStore(filepath.Join(dir, "activities"))

	_ = store.Enqueue(spike.ActivityRequest{
		ID:        "parent-task",
		AgentID:   "coder",
		Objective: "Base step",
		Budget:    spike.Budget{Allocated: 1000},
	})
	_ = store.Enqueue(spike.ActivityRequest{
		ID:        "child-task",
		AgentID:   "reviewer",
		Objective: "Dependent step",
		Budget:    spike.Budget{Allocated: 1000},
		DependsOn: []string{"parent-task"},
	})

	// Fail the parent task
	_ = store.Fail("parent-task", fmt.Errorf("out of disk space"))

	// HasClaimable must detect cascading failure and return false without hanging
	if store.HasClaimable() {
		t.Fatalf("HasClaimable should return false when only pending activity has failed dependency")
	}

	// Child task must now be marked StatusFailed with cascading error
	childRec, err := store.Get("child-task")
	if err != nil {
		t.Fatalf("failed to get child task: %v", err)
	}
	if childRec.Status != spike.StatusFailed || !strings.Contains(childRec.Error, "dependency \"parent-task\" failed") {
		t.Fatalf("expected child task to cascade fail, got status=%s err=%s", childRec.Status, childRec.Error)
	}
}

// TestBenchmark_04_HandoffBuilder_TokenReduction verifies that HandoffBuilder
// generates concise prompts and limits large diffs to avoid token waste.
func TestBenchmark_04_HandoffBuilder_TokenReduction(t *testing.T) {
	builder := spike.NewHandoffBuilder()
	builder.MaxDiffLines = 10

	// Generate a 50-line diff
	var diffLines []string
	for i := 0; i < 50; i++ {
		diffLines = append(diffLines, fmt.Sprintf("+line %d: function Add()", i))
	}
	largeDiff := strings.Join(diffLines, "\n")

	artifact := spike.HandoffArtifact{
		Objective:          "Add math utility",
		Role:               "Charlie (Quality Reviewer)",
		AgentID:            "reviewer",
		Diff:               largeDiff,
		ValidationEvidence: "evi-101 (ExitCode: 0)",
	}

	prompt := builder.BuildPrompt(artifact)

	if !strings.Contains(prompt, "... [Diff truncated: 40 more lines]") {
		t.Fatalf("expected diff truncation in reviewer prompt, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Native Evidence: evi-101") {
		t.Fatalf("expected native evidence in prompt")
	}
}

// Benchmark Trial Result container for comparative reporting
type TrialMetrics struct {
	Mode               string
	TaskName           string
	AcceptancePassed   bool
	WallClockDuration  time.Duration
	ModelDuration      time.Duration
	PromptTokens       int
	CompletionTokens   int
	TotalTokens        int
	ModelInvocations   int
	ToolCalls          int
	ReviewCycles       int
	HumanInterventions int
	FinalOutcome       string
}

// TestBenchmark_05_ComparativeTrials_SingleVsTeam executes identical synthetic trials
// for Benchmark A, B, and C comparing Mode A (A4C Team) vs Mode B (Single Agent).
func TestBenchmark_05_ComparativeTrials_SingleVsTeam(t *testing.T) {
	tasks := []struct {
		name      string
		objective string
	}{
		{
			name:      "Benchmark A: Nontrivial Bug Repair (Cascading Deadlock)",
			objective: "Diagnose and fix cascading DAG failure when dependency fails in activity store",
		},
		{
			name:      "Benchmark B: Multi-File Feature (Strategy & Activity Query)",
			objective: "Implement ActivityFilter and QueryActivities across types and store with CLI support",
		},
		{
			name:      "Benchmark C: Behavior-Preserving Refactoring (Handoff Compression)",
			objective: "Refactor prompt construction into token-efficient HandoffBuilder",
		},
	}

	var results []TrialMetrics

	for _, task := range tasks {
		// Run Mode B: Single Agent (StrategyDirect)
		resSingle := runSimulatedBenchmark(t, task.name, task.objective, spike.StrategyDirect)
		results = append(results, resSingle)

		// Run Mode A: A4C Team (StrategyTeam)
		resTeam := runSimulatedBenchmark(t, task.name, task.objective, spike.StrategyTeam)
		results = append(results, resTeam)
	}

	// Print structured comparative benchmark table
	fmt.Println("\n=========================================================================================")
	fmt.Println("A4C MILESTONE 5: COMPARATIVE BENCHMARK TRIAL RESULTS")
	fmt.Println("=========================================================================================")
	fmt.Printf("%-32s | %-12s | %-6s | %-10s | %-7s | %-6s\n",
		"Task / Benchmark", "Mode", "Passed", "Tokens", "Duration", "Calls")
	fmt.Println("-----------------------------------------------------------------------------------------")
	for _, r := range results {
		fmt.Printf("%-32s | %-12s | %-6v | %-10d | %-7v | %-6d\n",
			r.TaskName[:min(32, len(r.TaskName))],
			r.Mode,
			r.AcceptancePassed,
			r.TotalTokens,
			r.WallClockDuration.Round(time.Millisecond),
			r.ModelInvocations,
		)
	}
	fmt.Println("=========================================================================================")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func runSimulatedBenchmark(t *testing.T, taskName, objective string, strategy spike.ExecutionStrategy) TrialMetrics {
	t.Helper()
	dir := t.TempDir()
	repoDir := filepath.Join(dir, "repo")
	_ = os.MkdirAll(repoDir, 0755)

	// Scaffolding clean go repo
	_ = os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte("module example.com/bench\n\ngo 1.22\n"), 0644)
	_ = os.WriteFile(filepath.Join(repoDir, "feature.go"), []byte("package bench\nfunc Run() string { return \"ok\" }\n"), 0644)
	_ = os.WriteFile(filepath.Join(repoDir, "feature_test.go"), []byte("package bench\nimport \"testing\"\nfunc TestRun(t *testing.T) { if Run() != \"ok\" { t.Fatal(\"err\") } }\n"), 0644)

	store, _ := spike.NewDurableActivityStore(filepath.Join(dir, "activities"))
	memory, _ := spike.NewDurableMemoryStore(filepath.Join(dir, "memory"))
	roster, _ := spike.DefaultTeamRoster(filepath.Join(dir, "roster"))
	gov := spike.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_propose_activity", "governed_submit_review",
	})
	journal, _ := spike.NewDurableIntentJournal(filepath.Join(dir, "journal"))
	fakeProvider := spike.NewFakeProvider()
	runner := &spike.NativeGoTestRunner{}

	modelCalls := 0
	tokensConsumed := 0
	reviewCycles := 0

	fakeProvider.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		modelCalls++
		agentID := session.Config().AgentID
		// Approximate token measurement based on prompt length
		promptTokens := len(strings.Fields(prompt)) * 4
		completionTokens := 150
		tokensConsumed += promptTokens + completionTokens

		session.SetUsage(spike.UsageInfo{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      promptTokens + completionTokens,
		})

		switch agentID {
		case "lead":
			coderID := "bench-coder-subtask"
			session.Config().OnProposeActivity(spike.ActivityProposal{
				ProposalID:      coderID,
				ProposedAgentID: "coder",
				Objective:       objective,
				RequestedBudget: spike.Budget{Allocated: 5000},
			})
			session.Config().OnProposeActivity(spike.ActivityProposal{
				ProposalID:      "bench-reviewer-subtask",
				ProposedAgentID: "reviewer",
				Objective:       "Verify " + objective,
				RequestedBudget: spike.Budget{Allocated: 3000},
				DependsOn:       []string{coderID},
			})
			return &spike.AgentMessage{Content: "Delegated subtasks to coder and reviewer"}, nil

		case "coder":
			return &spike.AgentMessage{Content: "Implemented changes"}, nil

		case "reviewer":
			reviewCycles++
			session.Config().OnSubmitReview(spike.ReviewResult{
				Approved: true,
				Comments: "Approved cleanly with passing native tests.",
			})
			return &spike.AgentMessage{Content: "Review approved"}, nil
		}
		return &spike.AgentMessage{Content: "Done"}, nil
	}

	coord := spike.NewTeamCoordinator(repoDir, store, roster, memory, gov, journal, runner, fakeProvider)
	start := time.Now()

	taskReq, err := coord.SubmitTaskWithStrategy(objective, spike.Budget{Allocated: 30000}, nil, strategy)
	if err != nil {
		t.Fatalf("failed submit: %v", err)
	}

	results, err := coord.RunLoop(context.Background(), 10)
	duration := time.Since(start)
	if err != nil {
		t.Fatalf("run loop failed: %v", err)
	}

	modeStr := "Single Agent"
	if strategy == spike.StrategyTeam {
		modeStr = "A4C Team"
	}

	history, err := spike.BuildTaskHistory(taskReq.ID, store, journal, nil, memory)
	if err != nil {
		t.Fatalf("failed build history: %v", err)
	}

	return TrialMetrics{
		Mode:              modeStr,
		TaskName:          taskName,
		AcceptancePassed:  len(results) > 0 && strings.Contains(history.FinalOutcome, "successfully implemented"),
		WallClockDuration: duration,
		ModelDuration:     duration,
		TotalTokens:       tokensConsumed,
		ModelInvocations:  modelCalls,
		ReviewCycles:      reviewCycles,
		FinalOutcome:      history.FinalOutcome,
	}
}
