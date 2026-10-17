package a4c

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func kbTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runKBGit(t, dir, "init")
	runKBGit(t, dir, "config", "user.name", "KB Tester")
	runKBGit(t, dir, "config", "user.email", "kb@example.com")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# repo\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runKBGit(t, dir, "add", "README.md")
	runKBGit(t, dir, "commit", "-m", "init")
	return dir
}

func runKBGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %s (%v)", args, string(out), err)
	}
}

func setKBRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	prev := knowledgeRootOverride
	knowledgeRootOverride = root
	t.Cleanup(func() { knowledgeRootOverride = prev })
	return root
}

func TestKnowledgeDir_ResolveFromMainAndWorktree(t *testing.T) {
	root := setKBRoot(t)
	repo := kbTestRepo(t)
	repoName := filepath.Base(repo)

	fromMain, err := ResolveKnowledgeDir(repo)
	if err != nil {
		t.Fatalf("resolve from main: %v", err)
	}
	want := filepath.Join(root, repoName, "knowledge")
	if fromMain != want {
		t.Fatalf("main resolution = %q, want %q", fromMain, want)
	}

	// Linked worktree: directory is named "tree" — must NOT become the repo name.
	wtPath := filepath.Join(repo, ".a4c", "worktrees", "task-1", "tree")
	runKBGit(t, repo, "worktree", "add", "-b", "a4c/task-1", wtPath, "HEAD")
	fromWT, err := ResolveKnowledgeDir(wtPath)
	if err != nil {
		t.Fatalf("resolve from worktree: %v", err)
	}
	if fromWT != want {
		t.Fatalf("worktree resolution = %q, want %q (must not use 'tree' as repo name)", fromWT, want)
	}
	if strings.HasSuffix(fromWT, string(filepath.Separator)+"tree"+string(filepath.Separator)+"knowledge") {
		t.Fatalf("worktree resolution incorrectly used the worktree directory name: %q", fromWT)
	}
}

func TestKnowledgeDir_ResolveRejectsInvalidIdentity(t *testing.T) {
	setKBRoot(t)
	if _, err := ResolveKnowledgeDir(t.TempDir()); err == nil {
		t.Fatal("expected error resolving a non-git directory")
	}
}

func TestKnowledgeDir_PathContainment(t *testing.T) {
	kb := t.TempDir()
	absKB, _ := filepath.Abs(kb)

	if _, err := KnowledgePath(kb, "../escape.md"); !errors.Is(err, ErrKnowledgeOutsideDir) {
		t.Fatalf("traversal: err = %v, want ErrKnowledgeOutsideDir", err)
	}
	if _, err := KnowledgePath(kb, "/etc/passwd"); !errors.Is(err, ErrKnowledgeOutsideDir) {
		t.Fatalf("absolute escape: err = %v, want ErrKnowledgeOutsideDir", err)
	}
	if _, err := KnowledgePath(kb, ""); !errors.Is(err, ErrKnowledgeOutsideDir) {
		t.Fatalf("empty: err = %v, want ErrKnowledgeOutsideDir", err)
	}
	// Absolute path INSIDE the knowledge dir is allowed.
	if got, err := KnowledgePath(kb, filepath.Join(absKB, "findings", "ok.md")); err != nil || got == "" {
		t.Fatalf("absolute inside kb: got %q err %v", got, err)
	}
	// Normal relative path.
	if got, err := KnowledgePath(kb, "findings/ok.md"); err != nil || filepath.Base(got) != "ok.md" {
		t.Fatalf("relative path: got %q err %v", got, err)
	}
}

func TestKnowledgeDir_SymlinkEscapeRejected(t *testing.T) {
	kb := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(kb, "findings")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := KnowledgePath(kb, "findings/doc.md"); !errors.Is(err, ErrKnowledgeOutsideDir) {
		t.Fatalf("symlink escape: err = %v, want ErrKnowledgeOutsideDir", err)
	}
}

func TestKnowledgeDir_WriteAtomicNoOverwrite(t *testing.T) {
	kb := t.TempDir()
	content := "# Finding\n\nSomething learned.\n"

	abs, err := WriteKnowledgeFile(kb, "findings/2026-10-17-x.md", content)
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	got, err := os.ReadFile(abs)
	if err != nil || string(got) != content {
		t.Fatalf("content mismatch: %q (%v)", got, err)
	}

	// Replacement is refused; original preserved byte-for-byte.
	if _, err := WriteKnowledgeFile(kb, "findings/2026-10-17-x.md", "# Overwritten\n"); !errors.Is(err, ErrKnowledgeExists) {
		t.Fatalf("overwrite: err = %v, want ErrKnowledgeExists", err)
	}
	got2, _ := os.ReadFile(abs)
	if string(got2) != content {
		t.Fatalf("original was modified by refused overwrite: %q", got2)
	}

	// Empty content refused.
	if _, err := WriteKnowledgeFile(kb, "findings/empty.md", "   \n"); err == nil {
		t.Fatal("empty content should be refused")
	}
	// Escape refused.
	if _, err := WriteKnowledgeFile(kb, "../outside.md", "x"); err == nil {
		t.Fatal("traversal write should be refused")
	}
}

func TestKnowledgeDir_VerifyPaths(t *testing.T) {
	kb := t.TempDir()
	if _, err := WriteKnowledgeFile(kb, "findings/good.md", "# Good\n"); err != nil {
		t.Fatal(err)
	}
	emptyPath := filepath.Join(kb, "findings-empty.md")
	if err := os.WriteFile(emptyPath, nil, 0644); err != nil {
		t.Fatal(err)
	}

	statuses := VerifyKnowledgePaths(kb, []string{
		"findings/good.md",
		"findings/missing.md",
		"findings-empty.md",
		"../escape.md",
	})
	if len(statuses) != 4 {
		t.Fatalf("expected 4 statuses, got %d", len(statuses))
	}
	if !statuses[0].OK {
		t.Fatalf("good.md should verify: %+v", statuses[0])
	}
	if statuses[1].OK || !strings.Contains(statuses[1].Reason, "does not exist") {
		t.Fatalf("missing.md: %+v", statuses[1])
	}
	if statuses[2].OK || !strings.Contains(statuses[2].Reason, "empty") {
		t.Fatalf("empty file: %+v", statuses[2])
	}
	if statuses[3].OK {
		t.Fatalf("escape path must not verify: %+v", statuses[3])
	}
}
