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

const defaultCopilotCLI = "/Users/cristianormazabal/Library/Application Support/Code - Insiders/User/globalStorage/github.copilot-chat/copilotCli/copilot"

func getCopilotProvider(t *testing.T) *spike.CopilotProvider {
	cliPath := os.Getenv("COPILOT_CLI_PATH")
	if cliPath == "" {
		cliPath = defaultCopilotCLI
	}
	if _, err := os.Stat(cliPath); err != nil {
		t.Skipf("Copilot CLI not available at %s, skipping live test: %v", cliPath, err)
	}
	_ = os.Setenv("COPILOT_CLI_PATH", cliPath)

	prov, err := spike.NewCopilotProvider("")
	if err != nil {
		t.Skipf("Failed to initialize Copilot provider: %v", err)
	}
	return prov
}

// TestMilestone11_Live_ExperimentA_BoundedSuccess executes a real GitHub Copilot session
// within a comfortable budget and verifies that token consumption, provider wait duration,
// and completion status are accurately governed.
func TestMilestone11_Live_ExperimentA_BoundedSuccess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live Copilot test in -short mode")
	}
	prov := getCopilotProvider(t)
	defer prov.Close()

	dir := t.TempDir()
	storeDir := filepath.Join(dir, ".a4c", "activities")
	rosterDir := filepath.Join(dir, ".a4c", "roster")
	memDir := filepath.Join(dir, ".a4c", "memory")
	journalDir := filepath.Join(dir, ".a4c", "journal")

	_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fixture\n\ngo 1.22\n"), 0644)
	_ = os.WriteFile(filepath.Join(dir, "calc.go"), []byte("package fixture\nfunc Add(a, b int) int { return a + b }\n"), 0644)
	_ = os.WriteFile(filepath.Join(dir, "calc_test.go"), []byte("package fixture\nimport \"testing\"\nfunc TestAdd(t *testing.T) { if Add(2, 3) != 5 { t.Fail() } }\n"), 0644)

	store, _ := spike.NewDurableActivityStore(storeDir)
	roster, _ := spike.DefaultTeamRoster(rosterDir)
	memory, _ := spike.NewDurableMemoryStore(memDir)
	gov := spike.NewPolicyGovernor([]string{"governed_read_file", "governed_apply_patch"})
	journal, _ := spike.NewDurableIntentJournal(journalDir)

	coord := spike.NewTeamCoordinator(dir, store, roster, memory, gov, journal, &spike.NativeGoTestRunner{}, prov)
	defer coord.Close()

	// 25,000 token budget: generous enough to succeed without exhaustion
	taskReq, err := coord.SubmitTaskWithStrategy("Explain the calc package and verify tests pass", spike.Budget{Allocated: 25000}, nil, spike.StrategyDirect)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	res, err := coord.Step(ctx)
	if err != nil {
		t.Fatalf("step failed: %v", err)
	}

	if res.Status != spike.StatusCompleted {
		t.Fatalf("expected StatusCompleted, got %v", res.Status)
	}

	if res.Metrics == nil {
		t.Fatalf("expected ExecutionMetrics to be populated")
	}

	t.Logf("Experiment A Results:")
	t.Logf("  Total Tokens:          %d (Prompt: %d, Completion: %d)",
		res.Metrics.TotalTokens, res.Metrics.PromptTokens, res.Metrics.CompletionTokens)
	t.Logf("  Provider Wait Duration:%v", res.Metrics.ProviderWaitDuration)
	t.Logf("  Wall Clock Duration:   %v", res.Metrics.WallClockDuration)
	t.Logf("  Tool Duration:         %v", res.Metrics.ToolDuration)

	if res.Metrics.TotalTokens == 0 {
		t.Fatalf("expected non-zero token consumption from real Copilot")
	}
	if res.Metrics.ProviderWaitDuration == 0 {
		t.Fatalf("expected non-zero ProviderWaitDuration from real Copilot")
	}
	if float64(res.Metrics.TotalTokens) > 25000 {
		t.Fatalf("expected tokens <= budget, got %d", res.Metrics.TotalTokens)
	}

	// Verify governor recorded the consumption accurately
	bRoot, found := gov.GetBudget(taskReq.ID)
	if !found {
		t.Fatalf("expected root budget in governor")
	}
	if bRoot.Consumed != float64(res.Metrics.TotalTokens) {
		t.Fatalf("governor consumed mismatch: got %.0f, expected %d", bRoot.Consumed, res.Metrics.TotalTokens)
	}
}

// TestMilestone11_Live_ExperimentB_IntentionalBudgetExhaustion executes a real GitHub Copilot
// session with an intentionally insufficient budget (10 tokens), proving that:
// 1. Admission control & post-flight checks cut off execution fail-closed.
// 2. ErrBudgetExceeded is returned and failure_reason="budget_exhausted" is durably recorded.
// 3. Overrun tokens (liability) are accurately tracked without runaway execution.
func TestMilestone11_Live_ExperimentB_IntentionalBudgetExhaustion(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live Copilot test in -short mode")
	}
	prov := getCopilotProvider(t)
	defer prov.Close()

	dir := t.TempDir()
	storeDir := filepath.Join(dir, ".a4c", "activities")
	rosterDir := filepath.Join(dir, ".a4c", "roster")
	memDir := filepath.Join(dir, ".a4c", "memory")
	journalDir := filepath.Join(dir, ".a4c", "journal")

	_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fixture\n\ngo 1.22\n"), 0644)
	_ = os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main() {}\n"), 0644)

	store, _ := spike.NewDurableActivityStore(storeDir)
	roster, _ := spike.DefaultTeamRoster(rosterDir)
	memory, _ := spike.NewDurableMemoryStore(memDir)
	gov := spike.NewPolicyGovernor([]string{"governed_read_file"})
	journal, _ := spike.NewDurableIntentJournal(journalDir)

	coord := spike.NewTeamCoordinator(dir, store, roster, memory, gov, journal, &spike.NativeGoTestRunner{}, prov)
	defer coord.Close()

	// 10 token budget: guaranteed to be exceeded by any prompt + response
	taskReq, err := coord.SubmitTaskWithStrategy("Write a comprehensive guide to distributed consensus algorithms", spike.Budget{Allocated: 10}, nil, spike.StrategyDirect)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	_, err = coord.Step(ctx)
	if err == nil {
		t.Fatalf("expected Step to return ErrBudgetExceeded, but got nil error")
	}
	if !errors.Is(err, spike.ErrBudgetExceeded) {
		t.Fatalf("expected ErrBudgetExceeded, got: %v", err)
	}

	actRec, getErr := store.Get(taskReq.ID)
	if getErr != nil {
		t.Fatalf("get activity record: %v", getErr)
	}
	if actRec.Status != spike.StatusFailed {
		t.Fatalf("expected StatusFailed, got: %v", actRec.Status)
	}

	t.Logf("Experiment B Results:")
	t.Logf("  Allocated Budget: %d tokens", 10)
	t.Logf("  Consumed Tokens:  %.0f", actRec.Result.Consumed.Consumed)
	t.Logf("  Overrun Tokens:   %d", actRec.Result.Metrics.OverrunTokens)
	t.Logf("  Failure Reason:   %v", actRec.Result.Outputs["failure_reason"])

	if actRec.Result.Outputs["failure_reason"] != "budget_exhausted" {
		t.Fatalf("expected failure_reason=budget_exhausted, got: %v", actRec.Result.Outputs["failure_reason"])
	}
	if actRec.Result.Metrics.OverrunTokens <= 0 {
		t.Fatalf("expected OverrunTokens > 0, got %d", actRec.Result.Metrics.OverrunTokens)
	}
}
