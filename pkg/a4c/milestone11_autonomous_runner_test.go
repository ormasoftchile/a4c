package a4c_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	spike "github.com/ormasoftchile/a4c/pkg/a4c"
)

// TestMilestone11_AutonomousExecution_ImplementTaskInspectJSON executes A4C's autonomous
// multi-agent pipeline (Coder + Reviewer + Native Tests) against the real codebase to implement
// the 'a4c task inspect <task-id> --json' feature.
func TestMilestone11_AutonomousExecution_ImplementTaskInspectJSON(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live autonomous pipeline execution in -short mode")
	}
	prov := getCopilotProvider(t)
	defer prov.Close()

	repoDir, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repo dir: %v", err)
	}

	dotDir := filepath.Join(repoDir, ".a4c")
	storeDir := filepath.Join(dotDir, "activities")
	rosterDir := filepath.Join(dotDir, "roster")
	memDir := filepath.Join(dotDir, "memory")
	journalDir := filepath.Join(dotDir, "journal")

	store, err := spike.NewDurableActivityStore(storeDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	roster, err := spike.DefaultTeamRoster(rosterDir)
	if err != nil {
		t.Fatalf("open roster: %v", err)
	}
	memory, err := spike.NewDurableMemoryStore(memDir)
	if err != nil {
		t.Fatalf("open memory: %v", err)
	}
	gov := spike.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_submit_review",
		"governed_propose_activity", "governed_propose_memory",
	})
	journal, err := spike.NewDurableIntentJournal(journalDir)
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}

	coord := spike.NewTeamCoordinator(repoDir, store, roster, memory, gov, journal, &spike.NativeGoTestRunner{}, prov)
	coord.Limits.MaxReadFileLines = 800
	coord.Limits.MaxModelTurns = 10
	coord.Limits.MaxToolCalls = 15
	defer coord.Close()

	objective := "Implement machine-readable JSON output for 'a4c task inspect <task-id> --json'. " +
		"In cmd/a4c/main.go under the 'inspect' command case: check if any argument in args[1:] is '--json'. " +
		"If '--json' is present, find the taskID (the argument that does not start with '--'), build the report with BuildTaskHistory, " +
		"and output the report marshaled as indented JSON (json.MarshalIndent(report, \"\", \"  \")) followed by a newline, then return. " +
		"If '--json' is NOT present, retain the existing human-readable output (fmt.Print(report.FormatInspect(structStore, \".\"))). " +
		"Ensure cmd/a4c/main.go compiles and all tests pass."

	taskReq, err := coord.SubmitTaskWithStrategy(objective, spike.Budget{Allocated: 140000}, nil, spike.StrategyReviewed)
	if err != nil {
		t.Fatalf("submit task: %v", err)
	}

	t.Logf("Submitted Autonomous Feature Task: %s", taskReq.ID)
	t.Logf("Objective: %s", taskReq.Objective)

	ctx, cancel := context.WithTimeout(context.Background(), 360*time.Second)
	defer cancel()

	// Run governed team loop
	results, err := coord.RunLoop(ctx, 5)
	if err != nil {
		t.Fatalf("A4C team loop failed: %v", err)
	}

	t.Logf("A4C completed %d pipeline activities:", len(results))
	for _, res := range results {
		t.Logf("  - Activity %s: Status=%s, Consumed=%.0f tokens",
			res.ID, res.Status, res.Consumed.Consumed)
		if res.Outputs != nil {
			if app, ok := res.Outputs["approved"]; ok {
				t.Logf("    Review Approved: %v (Comments: %v)", app, res.Outputs["review_comments"])
			}
			if errOut, ok := res.Outputs["error"]; ok {
				t.Logf("    Error: %v", errOut)
			}
		}
	}
}
