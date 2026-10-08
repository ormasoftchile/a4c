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

type RuntimeComparisonMetrics struct {
	Runtime              string
	Model                string
	Success              bool
	Status               spike.ActivityStatus
	TotalTokens          int
	PromptTokens         int
	CompletionTokens     int
	ReasoningTokens      int
	CachedTokens         int
	ReportedCostUSD      float64
	WallClockDuration    time.Duration
	ProviderWaitDuration time.Duration
	ToolDuration         time.Duration
	TestExitCode         int
	TestOutput           string
	ActivitiesCount      int
}

func runTrialTask(
	t *testing.T,
	runtimeName string,
	modelName string,
	prov spike.AgentProvider,
	objective string,
	budgetTokens float64,
) RuntimeComparisonMetrics {
	dir := t.TempDir()
	storeDir := filepath.Join(dir, ".a4c", "activities")
	rosterDir := filepath.Join(dir, ".a4c", "roster")
	memDir := filepath.Join(dir, ".a4c", "memory")
	journalDir := filepath.Join(dir, ".a4c", "journal")

	_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module ratelimit\n\ngo 1.22\n"), 0644)
	_ = os.WriteFile(filepath.Join(dir, "limiter.go"), []byte("package ratelimit\n\nimport \"sync\"\n\ntype Limiter struct {\n\tmu sync.Mutex\n}\n\nfunc NewLimiter() *Limiter {\n\treturn &Limiter{}\n}\n\nfunc (l *Limiter) Allow(key string) bool {\n\treturn true\n}\n"), 0644)
	_ = os.WriteFile(filepath.Join(dir, "limiter_test.go"), []byte("package ratelimit\n\nimport \"testing\"\n\nfunc TestLimiterAllow(t *testing.T) {\n\tl := NewLimiter()\n\tif !l.Allow(\"user1\") {\n\t\tt.Fail()\n\t}\n}\n"), 0644)

	store, err := spike.NewDurableActivityStore(storeDir)
	if err != nil {
		t.Fatalf("[%s] open store: %v", runtimeName, err)
	}
	roster, err := spike.DefaultTeamRoster(rosterDir)
	if err != nil {
		t.Fatalf("[%s] open roster: %v", runtimeName, err)
	}
	memory, err := spike.NewDurableMemoryStore(memDir)
	if err != nil {
		t.Fatalf("[%s] open memory: %v", runtimeName, err)
	}
	gov := spike.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_submit_review",
		"governed_propose_activity", "governed_propose_memory",
	})
	journal, err := spike.NewDurableIntentJournal(journalDir)
	if err != nil {
		t.Fatalf("[%s] open journal: %v", runtimeName, err)
	}

	runner := &spike.NativeGoTestRunner{}
	coord := spike.NewTeamCoordinator(dir, store, roster, memory, gov, journal, runner, prov)
	coord.Limits.MaxReadFileLines = 800
	coord.Limits.MaxModelTurns = 10
	coord.Limits.MaxToolCalls = 15
	defer coord.Close()

	inputs := map[string]any{
		"runtime": runtimeName,
		"model":   modelName,
	}

	taskReq, err := coord.SubmitTaskWithStrategy(objective, spike.Budget{Allocated: budgetTokens}, inputs, spike.StrategyDirect)
	if err != nil {
		t.Fatalf("[%s] submit task: %v", runtimeName, err)
	}

	t.Logf("[%s] Enqueued Task: %s", runtimeName, taskReq.ID)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	tStart := time.Now()
	results, err := coord.RunLoop(ctx, 5)
	wallClock := time.Since(tStart)

	trial := RuntimeComparisonMetrics{
		Runtime:           runtimeName,
		Model:             modelName,
		WallClockDuration: wallClock,
		ActivitiesCount:   len(results),
	}

	if err != nil {
		t.Logf("[%s] RunLoop completed with error/notice: %v", runtimeName, err)
	}

	if len(results) > 0 {
		lastRes := results[len(results)-1]
		trial.Status = lastRes.Status
		trial.Success = lastRes.Status == spike.StatusCompleted

		for _, r := range results {
			if r.Metrics != nil {
				trial.TotalTokens += r.Metrics.TotalTokens
				trial.PromptTokens += r.Metrics.PromptTokens
				trial.CompletionTokens += r.Metrics.CompletionTokens
				trial.ReasoningTokens += r.Metrics.ReasoningTokens
				trial.CachedTokens += r.Metrics.CachedTokens
				trial.ReportedCostUSD += r.Metrics.ReportedCostUSD
				trial.ProviderWaitDuration += r.Metrics.ProviderWaitDuration
				trial.ToolDuration += r.Metrics.ToolDuration
			}
		}
	}

	// Verify native test execution outcome in active workspace
	exitCode, out, _ := runner.RunTests(context.Background(), dir)
	trial.TestExitCode = exitCode
	trial.TestOutput = strings.TrimSpace(out)

	return trial
}

// TestMilestone12_ComparativeTrials_CopilotVsOpenCode executes the same bounded engineering
// assignment across both GitHub Copilot and OpenCode in independent workspaces, capturing
// comparative metrics for token economics, durations, and validation outcomes.
func TestMilestone12_ComparativeTrials_CopilotVsOpenCode(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live comparative trials in -short mode")
	}

	objective := "Inspect limiter.go and limiter_test.go. " +
		"Refine Limiter to include a method Reset(key string) that clears any recorded state for that key. " +
		"Add a unit test TestLimiterReset in limiter_test.go confirming Reset functionality. " +
		"Verify that all tests pass."

	budgetTokens := 60000.0

	// 1. Run Trial with OpenCode
	t.Log("================================================================================")
	t.Log("STARTING TRIAL 1: OpenCode Runtime (opencode-go/mimo-v2.5)")
	t.Log("================================================================================")
	opProv, opMgr := getLiveOpenCodeProvider(t)
	defer func() {
		_ = opProv.Close()
		_ = opMgr.Stop()
	}()

	opMetrics := runTrialTask(t, "opencode", "opencode-go/mimo-v2.5", opProv, objective, budgetTokens)

	// 2. Run Trial with GitHub Copilot
	t.Log("================================================================================")
	t.Log("STARTING TRIAL 2: GitHub Copilot Runtime (Default Model)")
	t.Log("================================================================================")
	cpProv := getCopilotProvider(t)
	defer cpProv.Close()

	cpMetrics := runTrialTask(t, "copilot", "default", cpProv, objective, budgetTokens)

	// 3. Print Comparative Matrix
	report := formatComparativeMatrix(opMetrics, cpMetrics)
	t.Log(report)
}

func formatComparativeMatrix(op, cp RuntimeComparisonMetrics) string {
	var sb strings.Builder
	sb.WriteString("\n=========================================================================================\n")
	sb.WriteString("A4C MILESTONE 12: COMPARATIVE RUNTIME TRIAL RESULTS (OPENCODE VS COPILOT)\n")
	sb.WriteString("=========================================================================================\n")
	sb.WriteString(fmt.Sprintf("%-24s | %-24s | %-24s\n", "Metric", "OpenCode (mimo-v2.5)", "GitHub Copilot"))
	sb.WriteString("-----------------------------------------------------------------------------------------\n")
	sb.WriteString(fmt.Sprintf("%-24s | %-24s | %-24s\n", "Status", string(op.Status), string(cp.Status)))
	sb.WriteString(fmt.Sprintf("%-24s | %-24t | %-24t\n", "Native Tests Pass", op.TestExitCode == 0, cp.TestExitCode == 0))
	sb.WriteString(fmt.Sprintf("%-24s | %-24d | %-24d\n", "Total Tokens Consumed", op.TotalTokens, cp.TotalTokens))
	sb.WriteString(fmt.Sprintf("%-24s | %-24d | %-24d\n", "Prompt Tokens", op.PromptTokens, cp.PromptTokens))
	sb.WriteString(fmt.Sprintf("%-24s | %-24d | %-24d\n", "Completion Tokens", op.CompletionTokens, cp.CompletionTokens))
	sb.WriteString(fmt.Sprintf("%-24s | %-24d | %-24d\n", "Reasoning Tokens", op.ReasoningTokens, cp.ReasoningTokens))
	sb.WriteString(fmt.Sprintf("%-24s | %-24d | %-24d\n", "Cached Tokens", op.CachedTokens, cp.CachedTokens))
	sb.WriteString(fmt.Sprintf("%-24s | $%-23.6f | $%-23.6f\n", "Reported Cost USD", op.ReportedCostUSD, cp.ReportedCostUSD))
	sb.WriteString(fmt.Sprintf("%-24s | %-24v | %-24v\n", "Wall Clock Duration", op.WallClockDuration.Round(time.Millisecond), cp.WallClockDuration.Round(time.Millisecond)))
	sb.WriteString(fmt.Sprintf("%-24s | %-24v | %-24v\n", "Model / Wait Duration", op.ProviderWaitDuration.Round(time.Millisecond), cp.ProviderWaitDuration.Round(time.Millisecond)))
	sb.WriteString(fmt.Sprintf("%-24s | %-24v | %-24v\n", "Tool Duration", op.ToolDuration.Round(time.Millisecond), cp.ToolDuration.Round(time.Millisecond)))
	sb.WriteString(fmt.Sprintf("%-24s | %-24d | %-24d\n", "Activities Executed", op.ActivitiesCount, cp.ActivitiesCount))
	sb.WriteString("=========================================================================================\n")
	return sb.String()
}
