package a4c_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	spike "github.com/ormasoftchile/a4c/pkg/a4c"
)

func setupTestGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	// Initialize git repo with initial commit
	runCmd(t, dir, "git", "init")
	runCmd(t, dir, "git", "config", "user.name", "Test User")
	runCmd(t, dir, "git", "config", "user.email", "test@example.com")
	
	dummyFile := filepath.Join(dir, "README.md")
	if err := os.WriteFile(dummyFile, []byte("# Test Repo\n"), 0644); err != nil {
		t.Fatalf("failed to write dummy file: %v", err)
	}
	runCmd(t, dir, "git", "add", "README.md")
	runCmd(t, dir, "git", "commit", "-m", "initial commit")

	return dir
}

func runCmd(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cmd %s %v failed in %s: %s (%v)", name, args, dir, string(out), err)
	}
}

// TestWorktree_01_CreationAndBranchIsolation verifies isolated branch creation and worktree setup.
func TestWorktree_01_CreationAndBranchIsolation(t *testing.T) {
	repoDir := setupTestGitRepo(t)
	stateDir := filepath.Join(repoDir, ".a4c", "worktrees")

	mgr, err := spike.NewWorktreeManager(repoDir, stateDir)
	if err != nil {
		t.Fatalf("failed to create WorktreeManager: %v", err)
	}

	ctx := context.Background()
	taskID := "task-iso-101"
	rec, err := mgr.AcquireWorktree(ctx, taskID, "coder")
	if err != nil {
		t.Fatalf("failed to acquire worktree: %v", err)
	}

	if rec.Branch != "a4c/task-iso-101" {
		t.Fatalf("expected branch 'a4c/task-iso-101', got %q", rec.Branch)
	}

	// Verify worktree directory exists on disk
	if _, err := os.Stat(rec.WorktreePath); err != nil {
		t.Fatalf("worktree path %s does not exist: %v", rec.WorktreePath, err)
	}

	// Make a change in the worktree
	worktreeFile := filepath.Join(rec.WorktreePath, "feature.txt")
	if err := os.WriteFile(worktreeFile, []byte("feature content"), 0644); err != nil {
		t.Fatalf("failed to write in worktree: %v", err)
	}

	// Verify user base repository is UNTOUCHED
	rootFeatureFile := filepath.Join(repoDir, "feature.txt")
	if _, err := os.Stat(rootFeatureFile); !os.IsNotExist(err) {
		t.Fatalf("root repository was mutated! File should not exist in root: %s", rootFeatureFile)
	}
}

// TestWorktree_02_LeaseCollisionPrevention verifies that conflicting agents/tasks cannot acquire a held worktree.
func TestWorktree_02_LeaseCollisionPrevention(t *testing.T) {
	repoDir := setupTestGitRepo(t)
	stateDir := filepath.Join(repoDir, ".a4c", "worktrees")

	mgr, err := spike.NewWorktreeManager(repoDir, stateDir)
	if err != nil {
		t.Fatalf("failed to create WorktreeManager: %v", err)
	}

	ctx := context.Background()
	taskID := "task-lease-202"

	_, err = mgr.AcquireWorktree(ctx, taskID, "coder-1")
	if err != nil {
		t.Fatalf("coder-1 failed to acquire: %v", err)
	}

	// Second acquisition by different agent must fail
	_, err2 := mgr.AcquireWorktree(ctx, taskID, "coder-2")
	if err2 == nil {
		t.Fatalf("expected lease collision error, but acquisition succeeded")
	}
	if !errors.Is(err2, spike.ErrWorktreeConflict) {
		t.Fatalf("expected ErrWorktreeConflict, got: %v", err2)
	}
}

// TestWorktree_03_DirtyProtectionOnRemoval verifies uncommitted changes prevent accidental deletion.
func TestWorktree_03_DirtyProtectionOnRemoval(t *testing.T) {
	repoDir := setupTestGitRepo(t)
	stateDir := filepath.Join(repoDir, ".a4c", "worktrees")

	mgr, err := spike.NewWorktreeManager(repoDir, stateDir)
	if err != nil {
		t.Fatalf("failed to create WorktreeManager: %v", err)
	}

	ctx := context.Background()
	taskID := "task-dirty-303"

	rec, err := mgr.AcquireWorktree(ctx, taskID, "coder")
	if err != nil {
		t.Fatalf("failed to acquire worktree: %v", err)
	}

	// Create uncommitted dirty file in worktree
	dirtyFile := filepath.Join(rec.WorktreePath, "uncommitted_work.go")
	_ = os.WriteFile(dirtyFile, []byte("package dirty\n"), 0644)

	// Attempt removal without force must return ErrWorktreeDirty
	errDirty := mgr.RemoveWorktree(ctx, taskID, false)
	if errDirty == nil {
		t.Fatalf("expected ErrWorktreeDirty on dirty worktree removal, but succeeded")
	}
	if !errors.Is(errDirty, spike.ErrWorktreeDirty) {
		t.Fatalf("expected ErrWorktreeDirty, got: %v", errDirty)
	}

	// Confirm file still exists
	if _, err := os.Stat(dirtyFile); err != nil {
		t.Fatalf("uncommitted file was lost despite dirty protection: %v", err)
	}

	// Force removal should succeed
	if err := mgr.RemoveWorktree(ctx, taskID, true); err != nil {
		t.Fatalf("force removal failed: %v", err)
	}
}

// TestWorktree_04_ReleaseAndPersistenceRecovery verifies disk persistence and restart recovery.
func TestWorktree_04_ReleaseAndPersistenceRecovery(t *testing.T) {
	repoDir := setupTestGitRepo(t)
	stateDir := filepath.Join(repoDir, ".a4c", "worktrees")

	mgr1, err := spike.NewWorktreeManager(repoDir, stateDir)
	if err != nil {
		t.Fatalf("failed to create WorktreeManager: %v", err)
	}

	ctx := context.Background()
	taskID := "task-persist-404"

	rec, err := mgr1.AcquireWorktree(ctx, taskID, "coder")
	if err != nil {
		t.Fatalf("failed to acquire worktree: %v", err)
	}
	if !rec.LeaseHeld {
		t.Fatalf("expected lease held")
	}

	// Release worktree
	if err := mgr1.ReleaseWorktree(taskID, true); err != nil {
		t.Fatalf("failed to release worktree: %v", err)
	}

	// Simulate process restart
	mgr2, err := spike.NewWorktreeManager(repoDir, stateDir)
	if err != nil {
		t.Fatalf("failed to reload WorktreeManager: %v", err)
	}

	rec2, err := mgr2.GetWorktree(taskID)
	if err != nil {
		t.Fatalf("worktree not found after restart: %v", err)
	}
	if rec2.Status != spike.WorktreeStatusReleased {
		t.Fatalf("expected status released, got %s", rec2.Status)
	}
	if rec2.LeaseHeld {
		t.Fatalf("lease should not be held after release")
	}
}
