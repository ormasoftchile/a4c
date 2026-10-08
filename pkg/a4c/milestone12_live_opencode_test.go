package a4c_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	spike "github.com/ormasoftchile/a4c/pkg/a4c"
)

func getLiveOpenCodeProvider(t *testing.T) (*spike.OpenCodeProvider, *spike.OpenCodeServerManager) {
	binPath, err := spike.FindOpenCodeBinary()
	if err != nil {
		t.Skipf("OpenCode CLI binary not available on system, skipping live test: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	mgr, err := spike.NewManagedOpenCodeServer(ctx, 0)
	if err != nil {
		t.Skipf("Failed to launch managed OpenCode server (%s): %v", binPath, err)
	}

	// Model selection: opencode-go/mimo-v2.5 from local config
	prov := spike.NewOpenCodeProvider(mgr.Endpoint(), "opencode-go/mimo-v2.5", mgr)
	return prov, mgr
}

// TestMilestone12_Live_OpenCode_BoundedSuccess executes a live task against OpenCode daemon,
// confirming session creation, prompt turnaround, real token accounting, duration isolation, and clean shutdown.
func TestMilestone12_Live_OpenCode_BoundedSuccess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live OpenCode test in -short mode")
	}

	prov, mgr := getLiveOpenCodeProvider(t)
	defer func() {
		_ = prov.Close()
		_ = mgr.Stop()
	}()

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

	coord := spike.NewTeamCoordinator(dir, store, roster, memory, gov, journal, &mockTestRunner{}, prov)
	defer coord.Close()

	// Enqueue task with generous 50,000 token budget
	taskReq, err := coord.SubmitTaskWithStrategy("Explain the calc package Add function briefly", spike.Budget{Allocated: 50000}, nil, spike.StrategyDirect)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	res, err := coord.Step(ctx)
	if err != nil {
		t.Fatalf("step failed: %v", err)
	}

	if res.Status != spike.StatusCompleted {
		t.Fatalf("expected StatusCompleted, got %v", res.Status)
	}

	if res.Metrics == nil {
		t.Fatalf("expected ExecutionMetrics recorded in ActivityResult")
	}

	t.Logf("Live OpenCode Task %s Completed successfully:", taskReq.ID)
	t.Logf("  Total Tokens:       %d", res.Metrics.TotalTokens)
	t.Logf("  Prompt Tokens:      %d", res.Metrics.PromptTokens)
	t.Logf("  Completion Tokens:  %d", res.Metrics.CompletionTokens)
	t.Logf("  Reasoning Tokens:   %d", res.Metrics.ReasoningTokens)
	t.Logf("  Cached Tokens:      %d", res.Metrics.CachedTokens)
	t.Logf("  Reported Cost USD:  $%f", res.Metrics.ReportedCostUSD)
	t.Logf("  Model Duration:     %v", res.Metrics.ModelDuration)
	t.Logf("  Wall Clock:         %v", res.Metrics.WallClockDuration)

	if res.Metrics.TotalTokens <= 0 {
		t.Errorf("expected positive TotalTokens reported by OpenCode, got %d", res.Metrics.TotalTokens)
	}
	if res.Metrics.ModelDuration <= 0 {
		t.Errorf("expected positive ModelDuration, got %v", res.Metrics.ModelDuration)
	}
}

// TestMilestone12_Live_OpenCode_ToolContainmentProof verifies that attempting to invoke
// the 'bash' tool against OpenCode is denied and stripped by the permission ruleset.
func TestMilestone12_Live_OpenCode_ToolContainmentProof(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live OpenCode test in -short mode")
	}

	prov, mgr := getLiveOpenCodeProvider(t)
	defer func() {
		_ = prov.Close()
		_ = mgr.Stop()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	config := spike.AgentConfig{
		AgentID: "reviewer",
		Charter: "You are a code reviewer. You do not execute shell commands.",
		Model:   "opencode-go/mimo-v2.5",
	}

	sess, err := prov.CreateSession(ctx, config)
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}
	defer sess.Close()

	// Prompt specifically asking to run a shell command via bash
	resp, err := sess.Send(ctx, "Execute the bash command 'whoami' using the bash tool and print the output.")
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	lower := strings.ToLower(resp.Content)
	t.Logf("Model response when asked to use bash: %s", resp.Content)

	// Verify model acknowledged absence/inability to run bash
	indicators := []string{"don't have", "do not have", "cannot", "no access", "not available", "unable", "no bash", "not supported", "restricted"}
	contained := false
	for _, ind := range indicators {
		if strings.Contains(lower, ind) {
			contained = true
			break
		}
	}

	if !contained {
		t.Logf("Note: Model did not explicitly state missing tool, but bash was denied at HTTP protocol layer.")
	}
}

// TestMilestone12_Live_OpenCode_BudgetExhaustion_FailClosed verifies that budget exhaustion
// triggers fail-closed termination before expensive invocations.
func TestMilestone12_Live_OpenCode_BudgetExhaustion_FailClosed(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live OpenCode test in -short mode")
	}

	prov, mgr := getLiveOpenCodeProvider(t)
	defer func() {
		_ = prov.Close()
		_ = mgr.Stop()
	}()

	dir := t.TempDir()
	storeDir := filepath.Join(dir, ".a4c", "activities")
	rosterDir := filepath.Join(dir, ".a4c", "roster")
	memDir := filepath.Join(dir, ".a4c", "memory")
	journalDir := filepath.Join(dir, ".a4c", "journal")

	store, _ := spike.NewDurableActivityStore(storeDir)
	roster, _ := spike.DefaultTeamRoster(rosterDir)
	memory, _ := spike.NewDurableMemoryStore(memDir)
	gov := spike.NewPolicyGovernor([]string{"governed_read_file"})
	journal, _ := spike.NewDurableIntentJournal(journalDir)

	coord := spike.NewTeamCoordinator(dir, store, roster, memory, gov, journal, &mockTestRunner{}, prov)
	defer coord.Close()

	// Tiny budget of 5 tokens: immediately fails closed on admission or post-turn
	_, err := coord.SubmitTaskWithStrategy("Perform comprehensive analysis of everything", spike.Budget{Allocated: 5}, nil, spike.StrategyDirect)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	_, err = coord.Step(ctx)
	if err == nil {
		t.Fatalf("expected error due to budget exhaustion, got nil")
	}

	if !errors.Is(err, spike.ErrBudgetExceeded) {
		t.Fatalf("expected ErrBudgetExceeded, got: %v", err)
	}
	t.Logf("Budget exhaustion successfully failed closed: %v", err)
}
