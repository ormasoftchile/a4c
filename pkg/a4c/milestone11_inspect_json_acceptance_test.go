package a4c_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	spike "github.com/ormasoftchile/a4c/pkg/a4c"
)

// TestMilestone11_Acceptance_TaskInspectJSON verifies the independent acceptance criteria
// for the autonomously implemented feature: 'a4c task inspect <task-id> --json'.
func TestMilestone11_Acceptance_TaskInspectJSON(t *testing.T) {
	absBin := filepath.Join(t.TempDir(), "a4c")
	cmdBuild := exec.Command("go", "build", "-o", absBin, "./cmd/a4c")
	cmdBuild.Dir = filepath.Join("..", "..")
	if out, err := cmdBuild.CombinedOutput(); err != nil {
		t.Fatalf("build a4c CLI binary failed: %v\nOutput: %s", err, string(out))
	}

	dir := t.TempDir()
	storeDir := filepath.Join(dir, ".a4c", "activities")
	rosterDir := filepath.Join(dir, ".a4c", "roster")
	memDir := filepath.Join(dir, ".a4c", "memory")
	journalDir := filepath.Join(dir, ".a4c", "journal")

	_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fixture\n\ngo 1.22\n"), 0644)
	_ = os.WriteFile(filepath.Join(dir, "fixture.go"), []byte("package fixture\nfunc Value() int { return 42 }\n"), 0644)
	_ = os.WriteFile(filepath.Join(dir, "fixture_test.go"), []byte("package fixture\nimport \"testing\"\nfunc TestValue(t *testing.T) { if Value() != 42 { t.Fail() } }\n"), 0644)

	store, _ := spike.NewDurableActivityStore(storeDir)
	roster, _ := spike.DefaultTeamRoster(rosterDir)
	memory, _ := spike.NewDurableMemoryStore(memDir)
	gov := spike.NewPolicyGovernor([]string{"governed_read_file", "governed_apply_patch", "governed_submit_review"})
	journal, _ := spike.NewDurableIntentJournal(journalDir)

	fake := spike.NewFakeProvider()
	fake.SetSessionResponseFunc(func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		session.SetDurations(80*time.Millisecond, 20*time.Millisecond)
		return &spike.AgentMessage{
			Role:    "assistant",
			Content: "Implemented feature fixture",
		}, nil
	})

	coord := spike.NewTeamCoordinator(dir, store, roster, memory, gov, journal, &spike.NativeGoTestRunner{}, fake)
	defer coord.Close()

	taskReq, err := coord.SubmitTaskWithStrategy("Generate inspect fixture task", spike.Budget{Allocated: 60000}, nil, spike.StrategyDirect)
	if err != nil {
		t.Fatalf("submit task: %v", err)
	}

	res, err := coord.Step(context.Background())
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if res.Status != spike.StatusCompleted {
		t.Fatalf("expected completed activity")
	}

	_ = coord.Close()

	// 2. Lock workspace with coordinator lock to test concurrent inspection safety
	lock, err := spike.AcquireWorkspaceLock(dir, "active-coord-holder", taskReq.ID)
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	defer lock.Release()

	// 3. Execute: a4c task inspect <task-id> --json
	cmdInspect := exec.Command(absBin, "task", "inspect", taskReq.ID, "--json")
	cmdInspect.Dir = dir
	out, err := cmdInspect.CombinedOutput()
	if err != nil {
		t.Fatalf("a4c task inspect %s --json failed: %v\nOutput: %s", taskReq.ID, err, string(out))
	}

	rawJSON := strings.TrimSpace(string(out))
	if !strings.HasPrefix(rawJSON, "{") || !strings.HasSuffix(rawJSON, "}") {
		t.Fatalf("expected valid JSON object starting with '{' and ending with '}', got:\n%s", rawJSON)
	}

	// 4. Validate JSON Schema conforms to TaskHistoryReport
	var parsed struct {
		TaskID            string `json:"task_id"`
		OriginalObjective string `json:"original_objective"`
		Status            string `json:"status"`
		AllocatedBudget   float64 `json:"allocated_budget"`
		ConsumedBudget    float64 `json:"consumed_budget"`
		Activities        []struct {
			ActivityID string `json:"activity_id"`
			AgentID    string `json:"agent_id"`
			Status     string `json:"status"`
			BudgetUsed float64 `json:"budget_used"`
		} `json:"activities"`
		MetricsSummary *struct {
			WallClockDuration    int64 `json:"wall_clock_duration"`
			ProviderWaitDuration int64 `json:"provider_wait_duration"`
			ToolDuration         int64 `json:"tool_duration"`
			TotalTokens          int   `json:"total_tokens"`
		} `json:"metrics_summary"`
	}

	if err := json.Unmarshal([]byte(rawJSON), &parsed); err != nil {
		t.Fatalf("failed to unmarshal JSON output into schema: %v\nOutput:\n%s", err, rawJSON)
	}

	if parsed.TaskID != taskReq.ID {
		t.Fatalf("expected task_id %s, got %s", taskReq.ID, parsed.TaskID)
	}
	if parsed.OriginalObjective != taskReq.Objective {
		t.Fatalf("expected original_objective %s, got %s", taskReq.Objective, parsed.OriginalObjective)
	}
	if len(parsed.Activities) == 0 {
		t.Fatalf("expected at least 1 activity in JSON output")
	}

	// 5. Verify standard human-readable format without --json remains working
	cmdHuman := exec.Command(absBin, "task", "inspect", taskReq.ID)
	cmdHuman.Dir = dir
	outHuman, err := cmdHuman.CombinedOutput()
	if err != nil {
		t.Fatalf("human inspect failed: %v\nOutput: %s", err, string(outHuman))
	}
	if strings.HasPrefix(strings.TrimSpace(string(outHuman)), "{") {
		t.Fatalf("standard task inspect should not output JSON when --json flag is omitted")
	}
	if !strings.Contains(string(outHuman), "A4C Task Activity History") && !strings.Contains(string(outHuman), taskReq.ID) {
		t.Fatalf("expected human readable task inspect output")
	}
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}
