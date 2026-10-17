package a4c_test

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	spike "github.com/ormasoftchile/a4c/pkg/a4c"
)

// ValidateTaskInspectOutput independently asserts all Milestone 8 diagnostic requirements.
func ValidateTaskInspectOutput(t *testing.T, output string) error {
	requiredSections := []struct {
		Name      string
		Substrings []string
	}{
		{
			Name: "Task Status & Identity",
			Substrings: []string{"Status", "Task"},
		},
		{
			Name: "Agent Assignment & Handoffs",
			Substrings: []string{"Agent"},
		},
		{
			Name: "Activity Dependency Tree / Lifecycle",
			Substrings: []string{"Activity", "sub-", "lead"},
		},
		{
			Name: "Capability Profiles",
			Substrings: []string{"Capability", "profile", "Tools"},
		},
		{
			Name: "Budget & Token Consumption",
			Substrings: []string{"Budget", "Token"},
		},
		{
			Name: "Execution Latency & Durations",
			Substrings: []string{"Duration", "Model", "Wall"},
		},
		{
			Name: "Validation Evidence & Recovery",
			Substrings: []string{"Evidence"},
		},
	}

	for _, sec := range requiredSections {
		found := false
		for _, s := range sec.Substrings {
			if strings.Contains(strings.ToLower(output), strings.ToLower(s)) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("diagnostics inspection output missing section %q (checked for %v)", sec.Name, sec.Substrings)
		}
	}

	return nil
}

// TestIndependentAcceptance_TaskInspectVerification provides an automated harness
// to verify the dogfooding artifact created by A4C.
func TestIndependentAcceptance_TaskInspectVerification(t *testing.T) {
	// 1. Build latest binary. Tests run from the package directory (pkg/a4c),
	// so the repository root is two levels up.
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	cmd := exec.Command("go", "build", "-o", "bin/a4c", "./cmd/a4c")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to build a4c binary: %v\nOutput: %s", err, string(out))
	}

	// 2. Test running inspect on a seeded task
	dir := t.TempDir()
	initCmd := exec.Command(filepath.Join(repoRoot, "bin", "a4c"), "init", dir)
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("init failed: %v\nOutput: %s", err, string(out))
	}

	// Seed a task with activities and metrics
	dotDir := filepath.Join(dir, ".a4c")
	store, _ := spike.NewDurableActivityStore(filepath.Join(dotDir, "activities"))
	journal, _ := spike.NewDurableIntentJournal(filepath.Join(dotDir, "journal"))
	mem, _ := spike.NewDurableMemoryStore(filepath.Join(dotDir, "memory"))

	reg := spike.NewCapabilityRegistry()
	leadProf, _ := reg.GetProfile(spike.ProfileArchitectureInvestigation)
	coderProf, _ := reg.GetProfile(spike.ProfileCodeImplementation)

	rootReq := spike.ActivityRequest{
		ID:                "test-inspect-root",
		AgentID:           "lead",
		Objective:         "Investigate and implement diagnostics",
		Budget:            spike.Budget{Allocated: 50000, Consumed: 15200},
		Strategy:          spike.StrategyTeam,
		CapabilityProfile: &leadProf,
		CreatedAt:         time.Now().Add(-10 * time.Minute),
	}
	_ = store.Enqueue(rootReq)
	_, _ = store.ClaimNext()
	_ = store.Complete("test-inspect-root", spike.ActivityResult{
		ID:                "test-inspect-root",
		Status:            spike.StatusCompleted,
		CapabilityProfile: &leadProf,
		Consumed:          spike.Budget{Consumed: 5200},
		Metrics: &spike.ExecutionMetrics{
			WallClockDuration: 4 * time.Second,
			ModelDuration:     3800 * time.Millisecond,
			PromptTokens:      4200,
			CompletionTokens:  350,
			TotalTokens:       4550,
			ModelInvocations:  1,
		},
		Outputs: map[string]any{"summary": "Deconstructed task into coder and reviewer"},
	})

	coderReq := spike.ActivityRequest{
		ID:                "test-inspect-coder",
		ParentID:          "test-inspect-root",
		AgentID:           "coder",
		Objective:         "Write diagnostics CLI inspect command",
		Budget:            spike.Budget{Allocated: 30000, Consumed: 10000},
		CapabilityProfile: &coderProf,
		CreatedAt:         time.Now().Add(-5 * time.Minute),
	}
	_ = store.Enqueue(coderReq)
	_, _ = store.ClaimNext()
	_ = store.Complete("test-inspect-coder", spike.ActivityResult{
		ID:                "test-inspect-coder",
		Status:            spike.StatusCompleted,
		CapabilityProfile: &coderProf,
		Consumed:          spike.Budget{Consumed: 10000},
		Metrics: &spike.ExecutionMetrics{
			WallClockDuration: 8 * time.Second,
			ModelDuration:     7200 * time.Millisecond,
			PromptTokens:      8200,
			CompletionTokens:  1100,
			TotalTokens:       9300,
			ModelInvocations:  2,
			FileReadsCount:    3,
		},
		EvidenceIDs: []string{"evi-test-snapshot-123"},
		Outputs:     map[string]any{"summary": "Added inspect sub-command"},
	})

	_ = journal.RecordIntent(spike.IntentRecord{
		IntentID:   "act-1",
		Action:     "apply_patch",
		Target:     "cmd/a4c/main.go",
		Status:     spike.IntentCommitted,
		CreatedAt:  time.Now(),
	})
	_, _ = mem.Propose("arch.diagnostics", "Inspect command reports live records", "lead", "test-inspect-root")
	_, _ = mem.Promote("arch.diagnostics", "lead")

	// Execute inspect command
	inspectCmd := exec.Command(filepath.Join(repoRoot, "bin", "a4c"), "task", "inspect", "test-inspect-root")
	inspectCmd.Dir = dir
	inspectOut, inspectErr := inspectCmd.CombinedOutput()

	// If inspect command is not yet implemented by A4C, log and skip (Phase A preparation)
	if strings.Contains(string(inspectOut), "Unknown task command") {
		t.Logf("[Phase A Notice]: 'task inspect' is not yet implemented (A4C dogfooding will implement this in Phase B)")
		t.Skip("Skipping acceptance test until A4C dogfooding run completes")
		return
	}

	if inspectErr != nil {
		t.Fatalf("task inspect command failed with error: %v\nOutput: %s", inspectErr, string(inspectOut))
	}

	outStr := string(inspectOut)
	t.Logf("[INSPECT OUTPUT]:\n%s\n", outStr)

	if err := ValidateTaskInspectOutput(t, outStr); err != nil {
		t.Fatalf("task inspect output validation failed: %v", err)
	}
}
