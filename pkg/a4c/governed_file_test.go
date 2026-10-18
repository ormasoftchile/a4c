package a4c_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ormasoftchile/a4c/pkg/a4c"
)

// governedFileHarness builds a TeamServiceImpl whose "worktree" is a temp dir.
// With no WorktreeManager, GetWorktreePath falls back to workspaceDir, which is
// exactly the containment surface under test. A real intent journal is wired so
// journaling can be asserted.
func governedFileHarness(t *testing.T) (*a4c.TeamServiceImpl, string) {
	t.Helper()
	dir := t.TempDir()
	journal, err := a4c.NewDurableIntentJournal(filepath.Join(dir, ".a4c", "journal"))
	if err != nil {
		t.Fatalf("create journal: %v", err)
	}
	svc := a4c.NewTeamService(dir, nil, nil, nil, nil, nil, journal, nil)
	return svc, dir
}

// TestGovernedFile_WritesInsideWorktree: the happy path journals an intent and
// lands the exact bytes at the requested relative path.
func TestGovernedFile_WritesInsideWorktree(t *testing.T) {
	svc, dir := governedFileHarness(t)

	res, err := svc.ApplyWorkspaceFile(context.Background(), a4c.ApplyWorkspaceFileRequest{
		TaskID:  "task-1",
		AgentID: "coder",
		Path:    "pkg/new/file.go",
		Content: "package new\n",
	})
	if err != nil {
		t.Fatalf("ApplyWorkspaceFile: %v", err)
	}
	// Compare on a canonical basis: on macOS t.TempDir sits under the /var
	// symlink, which the containment check resolves to /private/var.
	wantSuffix := filepath.Join("pkg", "new", "file.go")
	if !strings.HasSuffix(res.AbsolutePath, wantSuffix) {
		t.Errorf("unexpected absolute path: %s", res.AbsolutePath)
	}
	if res.Bytes != len("package new\n") {
		t.Errorf("expected 12 bytes, got %d", res.Bytes)
	}
	if res.Sha256 == "" {
		t.Errorf("expected a content hash")
	}
	if !res.Journaled || res.IntentID == "" {
		t.Errorf("expected the intent to be journaled, got journaled=%t id=%q", res.Journaled, res.IntentID)
	}
	got, err := os.ReadFile(res.AbsolutePath)
	if err != nil {
		t.Fatalf("read back written file: %v", err)
	}
	if string(got) != "package new\n" {
		t.Errorf("content mismatch: %q", string(got))
	}
	_ = dir
}

// TestGovernedFile_RejectsEscapes: absolute paths, traversal, and symlink
// escapes must all fail closed without writing anything.
func TestGovernedFile_RejectsEscapes(t *testing.T) {
	svc, dir := governedFileHarness(t)

	// A symlink inside the worktree that points outside it.
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	cases := []struct {
		name string
		path string
	}{
		{"absolute path", "/etc/passwd"},
		{"parent traversal", "../../evil.txt"},
		{"embedded traversal", "pkg/../../evil.txt"},
		{"symlink escape", "escape/evil.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.ApplyWorkspaceFile(context.Background(), a4c.ApplyWorkspaceFileRequest{
				TaskID:  "task-1",
				AgentID: "coder",
				Path:    tc.path,
				Content: "evil",
			})
			if err == nil {
				t.Fatalf("expected rejection for %q", tc.path)
			}
			if !strings.Contains(err.Error(), "escape") && !strings.Contains(err.Error(), "absolute") {
				t.Errorf("unexpected error for %q: %v", tc.path, err)
			}
		})
	}

	// Nothing escaped: no evil.txt anywhere under the temp root.
	if _, err := os.Stat(filepath.Join(outside, "evil.txt")); err == nil {
		t.Errorf("SECURITY: traversal wrote outside the worktree")
	}
}

// TestGovernedFile_RejectsNonMutatingAgent: the mutating-role boundary holds
// even when the tool is reachable, so a reviewer cannot write through it.
func TestGovernedFile_RejectsNonMutatingAgent(t *testing.T) {
	svc, dir := governedFileHarness(t)

	for _, agent := range []string{"reviewer", "charlie", "lead", "alice"} {
		_, err := svc.ApplyWorkspaceFile(context.Background(), a4c.ApplyWorkspaceFileRequest{
			TaskID:  "task-1",
			AgentID: agent,
			Path:    "x.txt",
			Content: "nope",
		})
		if err == nil {
			t.Fatalf("expected rejection for non-mutating agent %q", agent)
		}
		if !strings.Contains(err.Error(), "non-mutating") {
			t.Errorf("unexpected error for %q: %v", agent, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "x.txt")); err == nil {
		t.Errorf("non-mutating agent wrote a file")
	}
}

// TestGovernedFile_RejectsInvalidParams: required fields are enforced and empty
// content never creates a zero-byte file.
func TestGovernedFile_RejectsInvalidParams(t *testing.T) {
	svc, _ := governedFileHarness(t)

	cases := []a4c.ApplyWorkspaceFileRequest{
		{AgentID: "coder", Path: "a.txt", Content: "x"},    // missing task_id
		{TaskID: "task-1", AgentID: "coder", Content: "x"}, // missing path
		{TaskID: "task-1", AgentID: "coder", Path: "   ", Content: "x"},
		{TaskID: "task-1", AgentID: "coder", Path: "a.txt"}, // empty content
		{TaskID: "task-1", AgentID: "coder", Path: "a.txt", Content: "   "},
	}
	for _, req := range cases {
		if _, err := svc.ApplyWorkspaceFile(context.Background(), req); err == nil {
			t.Errorf("expected rejection for %+v", req)
		}
	}
}

// TestGovernedFile_PolicyDeniesFailClosed: an agent the roster does not grant
// governed_apply_patch falls through to the governor's global allowlist. When
// that allowlist excludes the tool, the write must not land.
func TestGovernedFile_PolicyDeniesFailClosed(t *testing.T) {
	repoDir := t.TempDir()
	store, _ := a4c.NewDurableActivityStore(filepath.Join(repoDir, ".a4c", "activities"))
	roster, _ := a4c.DefaultTeamRoster(filepath.Join(repoDir, ".a4c", "roster"))
	mem, _ := a4c.NewDurableMemoryStore(filepath.Join(repoDir, ".a4c", "memory"))
	journal, _ := a4c.NewDurableIntentJournal(filepath.Join(repoDir, ".a4c", "journal"))

	// Global allowlist deliberately excludes governed_apply_patch. Note the
	// coordinator grants roster members their AllowedTools per-agent, so an
	// agent outside the roster is what exercises this global gate.
	gov := a4c.NewPolicyGovernor([]string{"governed_read_file"})
	coord := a4c.NewTeamCoordinator(repoDir, store, roster, mem, gov, journal, &mockTestRunner{}, a4c.NewFakeProvider())

	worktree := t.TempDir()
	svc := a4c.NewTeamService(worktree, store, nil, coord, roster, mem, journal, nil)

	_, err := svc.ApplyWorkspaceFile(context.Background(), a4c.ApplyWorkspaceFileRequest{
		TaskID:  "task-1",
		AgentID: "contractor",
		Path:    "denied.txt",
		Content: "should not land",
	})
	if err == nil {
		t.Fatalf("expected policy denial")
	}
	if !errors.Is(err, a4c.ErrUnauthorizedTool) {
		t.Errorf("expected ErrUnauthorizedTool, got: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(worktree, "denied.txt")); statErr == nil {
		t.Errorf("SECURITY: policy-denied write landed on disk")
	}
}
