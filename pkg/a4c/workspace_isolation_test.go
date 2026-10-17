package a4c_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	spike "github.com/ormasoftchile/a4c/pkg/a4c"
)

// setupGoTestRepo creates a committed Go module with a passing test so
// project-aware verification succeeds and the isolation guard is reached.
func setupGoTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run("init")
	_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module calc\n\ngo 1.22\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "calc_test.go"), []byte("package calc\n\nimport \"testing\"\n\nfunc TestOK(t *testing.T) {}\n"), 0o644)
	run("add", "-A")
	run("commit", "-m", "base")
	return dir
}

// TestWorkspaceIsolation_EscapeDetected reproduces the observed defect: an agent
// modifies the SOURCE CHECKOUT instead of its assigned isolated worktree. A4C
// must fail explicitly with workspace_escape rather than accept a successful
// isolated execution.
func TestWorkspaceIsolation_EscapeDetected(t *testing.T) {
	repoDir := setupGoTestRepo(t)
	stateDir := filepath.Join(repoDir, ".a4c", "worktrees")
	wtMgr, err := spike.NewWorktreeManager(repoDir, stateDir)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	store, _ := spike.NewDurableActivityStore(filepath.Join(dir, "activities"))
	memory, _ := spike.NewDurableMemoryStore(filepath.Join(dir, "memory"))
	roster, _ := spike.DefaultTeamRoster(filepath.Join(dir, "roster"))
	gov := spike.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_propose_activity",
		"governed_submit_review", "governed_request_human_decision", "governed_propose_memory",
	})
	journal, _ := spike.NewDurableIntentJournal(filepath.Join(dir, "journal"))
	fake := spike.NewFakeProvider()

	fake.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		// ESCAPE: write to the SOURCE CHECKOUT, not the assigned worktree.
		_ = os.WriteFile(filepath.Join(repoDir, "escaped.go"), []byte("package calc\n"), 0o644)
		return &spike.AgentMessage{Content: "modified the source checkout"}, nil
	}

	coord := spike.NewTeamCoordinator(repoDir, store, roster, memory, gov, journal, &spike.NativeGoTestRunner{}, fake)
	coord.SetWorktreeManager(wtMgr)

	if _, err := coord.SubmitTaskWithStrategy("Implement a feature", spike.Budget{Allocated: 100000}, nil, spike.StrategyDirect); err != nil {
		t.Fatalf("submit: %v", err)
	}

	res, err := coord.Step(context.Background())
	if err == nil {
		t.Fatalf("expected the isolated-escape to fail explicitly, got success (res=%+v)", res)
	}
	if res == nil {
		t.Fatalf("expected a failure result, got nil (err=%v)", err)
	}
	if fr, _ := res.Outputs["failure_reason"].(string); fr != "workspace_escape" {
		t.Fatalf("expected failure_reason=workspace_escape, got %q", fr)
	}
	if res.Status != spike.StatusFailed {
		t.Fatalf("expected StatusFailed, got %s", res.Status)
	}
	escaped, _ := res.Outputs["escaped_paths"].([]string)
	if len(escaped) == 0 || !strings.Contains(strings.Join(escaped, " "), "escaped.go") {
		t.Fatalf("expected escaped.go in escaped_paths, got %v", escaped)
	}
	if _, serr := os.Stat(filepath.Join(repoDir, "escaped.go")); serr != nil {
		t.Fatalf("test fixture should have written escaped.go: %v", serr)
	}
}

// TestWorkspaceIsolation_HoldsWhenAgentUsesWorktree is the positive control: an
// agent that mutates its assigned worktree is accepted, and the source checkout
// stays clean.
func TestWorkspaceIsolation_HoldsWhenAgentUsesWorktree(t *testing.T) {
	repoDir := setupGoTestRepo(t)
	stateDir := filepath.Join(repoDir, ".a4c", "worktrees")
	wtMgr, err := spike.NewWorktreeManager(repoDir, stateDir)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	store, _ := spike.NewDurableActivityStore(filepath.Join(dir, "activities"))
	memory, _ := spike.NewDurableMemoryStore(filepath.Join(dir, "memory"))
	roster, _ := spike.DefaultTeamRoster(filepath.Join(dir, "roster"))
	gov := spike.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_propose_activity",
		"governed_submit_review", "governed_request_human_decision", "governed_propose_memory",
	})
	journal, _ := spike.NewDurableIntentJournal(filepath.Join(dir, "journal"))
	fake := spike.NewFakeProvider()

	fake.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		// CORRECT: write inside the assigned worktree.
		_ = os.WriteFile(filepath.Join(session.Config().WorkspaceDir, "impl.go"), []byte("package calc\n"), 0o644)
		return &spike.AgentMessage{Content: "implemented in the worktree"}, nil
	}

	coord := spike.NewTeamCoordinator(repoDir, store, roster, memory, gov, journal, &spike.NativeGoTestRunner{}, fake)
	coord.SetWorktreeManager(wtMgr)

	if _, err := coord.SubmitTaskWithStrategy("Implement a feature", spike.Budget{Allocated: 100000}, nil, spike.StrategyDirect); err != nil {
		t.Fatalf("submit: %v", err)
	}
	res, err := coord.Step(context.Background())
	if err != nil {
		t.Fatalf("in-worktree mutation should succeed, got: %v", err)
	}
	if res.Status != spike.StatusCompleted {
		t.Fatalf("expected StatusCompleted, got %s", res.Status)
	}
	if fr, _ := res.Outputs["failure_reason"].(string); fr == "workspace_escape" {
		t.Fatalf("in-worktree mutation must not be flagged as escape")
	}
	if _, serr := os.Stat(filepath.Join(repoDir, "impl.go")); !os.IsNotExist(serr) {
		t.Fatalf("source checkout must not receive the agent's file")
	}
}

