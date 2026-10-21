package a4c_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ormasoftchile/a4c/pkg/a4c"
)

func setupGoTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
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

func wiService(t *testing.T, repoDir string) *a4c.TeamServiceImpl {
	t.Helper()
	stateDir := filepath.Join(repoDir, ".a4c", "worktrees")
	wtMgr, err := a4c.NewWorktreeManager(repoDir, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store, _ := a4c.NewDurableActivityStore(filepath.Join(dir, "activities"))
	lifecycle, _ := a4c.NewDurableLifecycleStore(filepath.Join(dir, "lifecycle"))
	memory, _ := a4c.NewDurableMemoryStore(filepath.Join(dir, "memory"))
	journal, _ := a4c.NewDurableIntentJournal(filepath.Join(dir, "journal"))
	gov := a4c.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_propose_memory",
	})
	return a4c.NewTeamService(repoDir, store, lifecycle, gov, memory, journal, wtMgr)
}

// TestWorkspaceIsolation_EscapeRefusedIntegration: work that lands in the
// source checkout instead of the task worktree can never be integrated. The
// host owns execution, so A4C enforces this at its authoritative boundary -
// the integration gate - rather than by supervising the agent.
func TestWorkspaceIsolation_EscapeRefusedIntegration(t *testing.T) {
	repoDir := setupGoTestRepo(t)
	svc := wiService(t, repoDir)

	res, err := svc.BeginTask(context.Background(), a4c.BeginTaskRequest{Objective: "implement a feature"})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	// ESCAPE: the host writes into the SOURCE CHECKOUT, not the worktree.
	if err := os.WriteFile(filepath.Join(repoDir, "escaped.go"), []byte("package calc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CompleteActivity(context.Background(), a4c.CompleteActivityRequest{
		TaskID: res.TaskID, ActivityID: res.TaskID, Actor: "host",
		Summary: "modified the source checkout", Success: true,
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if err := acceptForTest(svc, res.TaskID); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if _, err := svc.IntegrateTask(context.Background(), a4c.IntegrateTaskRequest{TaskID: res.TaskID}); err == nil {
		t.Fatalf("integrate must fail while the base checkout carries escaped work")
	}
	if _, serr := os.Stat(filepath.Join(repoDir, "escaped.go")); serr != nil {
		t.Fatalf("test fixture should have written escaped.go: %v", serr)
	}
}

// TestWorkspaceIsolation_HoldsWhenAgentUsesWorktree is the positive control: a
// host that mutates its assigned worktree through the governed write path is
// accepted and integrated, and the source checkout stays clean.
func TestWorkspaceIsolation_HoldsWhenAgentUsesWorktree(t *testing.T) {
	repoDir := setupGoTestRepo(t)
	svc := wiService(t, repoDir)

	res, err := svc.BeginTask(context.Background(), a4c.BeginTaskRequest{Objective: "implement a feature"})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := svc.ApplyWorkspaceFile(context.Background(), a4c.ApplyWorkspaceFileRequest{
		TaskID:  res.TaskID,
		AgentID: "host",
		Path:    "impl.go",
		Content: "package calc\n",
	}); err != nil {
		t.Fatalf("governed write into worktree: %v", err)
	}
	if _, err := svc.CompleteActivity(context.Background(), a4c.CompleteActivityRequest{
		TaskID: res.TaskID, ActivityID: res.TaskID, Actor: "host",
		Summary: "implemented in the worktree", Success: true,
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if err := acceptForTest(svc, res.TaskID); err != nil {
		t.Fatalf("accept: %v", err)
	}
	// Before integration the source checkout must still be clean: the
	// deliverable lives in the worktree.
	if _, serr := os.Stat(filepath.Join(repoDir, "impl.go")); !os.IsNotExist(serr) {
		t.Fatalf("source checkout must not receive the agent's file before integration")
	}
	if _, err := svc.IntegrateTask(context.Background(), a4c.IntegrateTaskRequest{TaskID: res.TaskID}); err != nil {
		t.Fatalf("integrate must succeed for worktree-contained work: %v", err)
	}
	// Integration lands the verified deliverable in the repository.
	if _, serr := os.Stat(filepath.Join(repoDir, "impl.go")); serr != nil {
		t.Fatalf("integrated deliverable missing from the repository: %v", serr)
	}
	if _, serr := os.Stat(filepath.Join(repoDir, "escaped.go")); serr == nil {
		t.Fatalf("escaped.go leaked into the checkout")
	}
}

func acceptForTest(svc *a4c.TeamServiceImpl, taskID string) error {
	_, err := svc.AcceptTask(context.Background(), a4c.AcceptTaskRequest{
		TaskID:        taskID,
		AcceptedBy:    "human_cli",
		IsInteractive: true,
		Comments:      "test acceptance",
	})
	return err
}

var _ = strings.Contains
