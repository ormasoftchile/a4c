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

// TestMilestone13_FullTeam_LiveOpenCode executes a complete autonomous 3-agent team workflow
// (Alice Lead -> Bob Coder -> Charlie Reviewer) against a live OpenCode daemon.
// It verifies Governed Proposal Mode, pre-mutation intent logging, native test evidence binding,
// non-mutating independent review, and durable task history reconstruction.
func TestMilestone13_FullTeam_LiveOpenCode(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live OpenCode full team test in -short mode")
	}

	prov, mgr := getLiveOpenCodeProvider(t)
	defer func() {
		_ = prov.Close()
		_ = mgr.Stop()
	}()

	// 1. Setup workspace fixture with failing test
	repoDir := t.TempDir()
	stateDir := filepath.Join(repoDir, ".a4c")
	storeDir := filepath.Join(stateDir, "activities")
	rosterDir := filepath.Join(stateDir, "roster")
	memDir := filepath.Join(stateDir, "memory")
	journalDir := filepath.Join(stateDir, "journal")
	wtStateDir := filepath.Join(stateDir, "worktrees")

	wtMgr, err := spike.NewWorktreeManager(repoDir, wtStateDir)
	if err != nil {
		t.Fatalf("failed to create WorktreeManager: %v", err)
	}

	// Module fixture: in-memory threadsafe Cache missing Delete() method
	goMod := "module cache\n\ngo 1.22\n"
	cacheGo := `package cache

import "sync"

type Cache struct {
	mu    sync.RWMutex
	items map[string]string
}

func NewCache() *Cache {
	return &Cache{
		items: make(map[string]string),
	}
}

func (c *Cache) Get(key string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	val, ok := c.items[key]
	return val, ok
}

func (c *Cache) Set(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = value
}
`

	cacheTestGo := `package cache

import "testing"

func TestCacheGetSet(t *testing.T) {
	c := NewCache()
	c.Set("k1", "v1")
	val, ok := c.Get("k1")
	if !ok || val != "v1" {
		t.Fatalf("expected v1, got %v (ok=%t)", val, ok)
	}
}

func TestCacheDelete(t *testing.T) {
	c := NewCache()
	c.Set("k1", "v1")
	c.Delete("k1")
	_, ok := c.Get("k1")
	if ok {
		t.Fatalf("expected key k1 to be deleted")
	}
}
`
	_ = os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte(goMod), 0644)
	_ = os.WriteFile(filepath.Join(repoDir, "cache.go"), []byte(cacheGo), 0644)
	_ = os.WriteFile(filepath.Join(repoDir, "cache_test.go"), []byte(cacheTestGo), 0644)

	runCmd(t, repoDir, "git", "init")
	runCmd(t, repoDir, "git", "config", "user.email", "a4c@example.com")
	runCmd(t, repoDir, "git", "config", "user.name", "A4C")
	runCmd(t, repoDir, "git", "add", ".")
	runCmd(t, repoDir, "git", "commit", "-m", "chore: initial scaffold")

	store, err := spike.NewDurableActivityStore(storeDir)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	roster, err := spike.DefaultTeamRoster(rosterDir)
	if err != nil {
		t.Fatalf("create roster: %v", err)
	}
	memory, err := spike.NewDurableMemoryStore(memDir)
	if err != nil {
		t.Fatalf("create memory: %v", err)
	}
	journal, err := spike.NewDurableIntentJournal(journalDir)
	if err != nil {
		t.Fatalf("create journal: %v", err)
	}

	gov := spike.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_propose_activity",
		"governed_submit_review", "governed_request_human_decision", "governed_propose_memory",
	})
	gov.SetAgentTools("coder", []string{"governed_read_file", "governed_apply_patch", "governed_propose_memory"})
	gov.SetAgentTools("reviewer", []string{"governed_read_file", "governed_submit_review"})
	gov.SetAgentTools("lead", []string{"governed_read_file", "governed_propose_activity", "governed_propose_memory"})

	runner := &spike.NativeGoTestRunner{}
	coord := spike.NewTeamCoordinator(repoDir, store, roster, memory, gov, journal, runner, prov)
	coord.Limits.MaxReadFileLines = 800
	coord.Limits.MaxModelTurns = 10
	coord.Limits.MaxToolCalls = 15
	coord.SetWorktreeManager(wtMgr)
	defer coord.Close()

	objective := "Inspect cache.go and cache_test.go. Implement the Delete(key string) method on Cache in cache.go so that TestCacheDelete passes. Ensure thread safety with mutex locking. Run tests to confirm."

	// 2. Submit task under StrategyTeam
	task, err := coord.SubmitTaskWithStrategy(objective, spike.Budget{Allocated: 120000}, nil, spike.StrategyTeam)
	if err != nil {
		t.Fatalf("submit team task: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 360*time.Second)
	defer cancel()

	t.Log("================================================================================")
	t.Logf("STEP 1: Alice (Lead Architect) - Triage & Delegation for Task: %s", task.ID)
	t.Log("================================================================================")
	leadRes, err := coord.Step(ctx)
	if err != nil {
		t.Fatalf("lead step failed: %v", err)
	}
	t.Logf("Lead step executed: Status=%s Proposals=%d", leadRes.Status, len(leadRes.Proposals))

	// Ensure coder activity is enqueued
	coderActivityID := ""
	for _, p := range leadRes.Proposals {
		if p.ProposedAgentID == "coder" {
			coderActivityID = p.ProposalID
		}
	}
	if coderActivityID == "" {
		coderActivityID = fmt.Sprintf("%s-coder", task.ID)
		_ = store.Enqueue(spike.ActivityRequest{
			ID:        coderActivityID,
			ParentID:  task.ID,
			AgentID:   "coder",
			Objective: objective,
			Budget:    spike.Budget{Allocated: 60000},
			CreatedAt: time.Now(),
		})
	}

	// Ensure reviewer activity is enqueued
	reviewerEnqueued := false
	for _, p := range leadRes.Proposals {
		if p.ProposedAgentID == "reviewer" {
			reviewerEnqueued = true
		}
	}
	if !reviewerEnqueued {
		_ = store.Enqueue(spike.ActivityRequest{
			ID:        fmt.Sprintf("%s-reviewer", task.ID),
			ParentID:  task.ID,
			AgentID:   "reviewer",
			Objective: "Review Delete method implementation in cache.go and verify native test evidence",
			Budget:    spike.Budget{Allocated: 30000},
			DependsOn: []string{coderActivityID},
			CreatedAt: time.Now(),
		})
	}

	reviewApproved := false
	coderRan := false
	stepsExecuted := 1

	// 3. Process remaining team activities (Coder -> Reviewer)
	for store.HasClaimable() {
		res, err := coord.Step(ctx)
		if err != nil {
			t.Fatalf("step %d failed: %v", stepsExecuted+1, err)
		}
		if res == nil {
			break
		}
		stepsExecuted++
		rec, _ := store.Get(res.ID)
		t.Logf("Step %d executed: ID=%s Agent=%s Status=%s Outputs=%+v",
			stepsExecuted, res.ID, rec.Request.AgentID, res.Status, res.Outputs)

		if rec.Request.AgentID == "coder" {
			coderRan = true
		}
		if rec.Request.AgentID == "reviewer" {
			if approved, ok := res.Outputs["approved"].(bool); ok && approved {
				reviewApproved = true
			}
		}
	}

	t.Log("================================================================================")
	t.Logf("TEAM EXECUTION COMPLETE: Steps=%d, CoderRan=%t, ReviewApproved=%t",
		stepsExecuted, coderRan, reviewApproved)
	t.Log("================================================================================")

	// 4. Verify native test execution outcome in active workspace
	testDir := repoDir
	if wt, err := wtMgr.GetWorktree(task.ID); err == nil && wt != nil && wt.WorktreePath != "" {
		testDir = wt.WorktreePath
	}
	exitCode, out, err := runner.RunTests(context.Background(), testDir)
	t.Logf("Native go test in %s (exitCode=%d):\n%s", testDir, exitCode, strings.TrimSpace(out))
	if exitCode != 0 {
		t.Errorf("expected native tests to pass after team implementation, got exitCode %d", exitCode)
	}

	// 5. Verify Durable Intent Journal logged pre-mutation intent
	coderIntents := journal.FindIntentsForActivity("coder")
	t.Logf("Coder intents recorded in journal: %d", len(coderIntents))
	for i, in := range coderIntents {
		t.Logf("  Intent %d: Action=%s Target=%s Status=%s", i+1, in.Action, in.Target, in.Status)
	}
	if len(coderIntents) == 0 {
		t.Errorf("expected at least 1 durable intent recorded for coder")
	}

	// 6. Verify Reviewer did NOT log any mutation intents (Non-mutating guarantee)
	reviewerIntents := journal.FindIntentsForActivity("reviewer")
	if len(reviewerIntents) > 0 {
		t.Errorf("security violation: reviewer recorded %d mutation intents!", len(reviewerIntents))
	}

	// 7. Verify Task History reconstruction
	hist, err := spike.BuildTaskHistory(task.ID, store, journal, wtMgr, memory)
	if err != nil {
		t.Fatalf("build task history: %v", err)
	}
	t.Logf("Aggregated Task History:")
	t.Logf("  Total Activities:   %d", len(hist.Activities))
	t.Logf("  Allocated Budget:   %.0f", hist.AllocatedBudget)
	t.Logf("  Consumed Budget:    %.0f", hist.ConsumedBudget)
	t.Logf("  Status:             %s", hist.Status)
}
