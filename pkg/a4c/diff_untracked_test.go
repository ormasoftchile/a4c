package a4c_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ormasoftchile/a4c/pkg/a4c"
)

// TestGetTaskDiff_IncludesUntrackedFiles: new work is exactly what a task
// usually produces, and 'git diff' is blind to it. A task that created a file
// must never report an empty diff, or the operator is invited to approve
// nothing.
func TestGetTaskDiff_IncludesUntrackedFiles(t *testing.T) {
	dir := t.TempDir()

	store, err := a4c.NewDurableActivityStore(filepath.Join(dir, ".a4c", "activities"))
	if err != nil {
		t.Fatalf("activity store: %v", err)
	}
	lifecycle, err := a4c.NewDurableLifecycleStore(filepath.Join(dir, ".a4c", "lifecycle"))
	if err != nil {
		t.Fatalf("lifecycle store: %v", err)
	}
	roster, err := a4c.DefaultTeamRoster(filepath.Join(dir, ".a4c", "roster"))
	if err != nil {
		t.Fatalf("roster: %v", err)
	}
	memory, err := a4c.NewDurableMemoryStore(filepath.Join(dir, ".a4c", "memory"))
	if err != nil {
		t.Fatalf("memory store: %v", err)
	}
	journal, err := a4c.NewDurableIntentJournal(filepath.Join(dir, ".a4c", "journal"))
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	svc := a4c.NewTeamService(dir, store, lifecycle, nil, roster, memory, journal, nil)

	// An initial commit, so the workspace is a real repository.
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.name", "a4c")
	runGit(t, dir, "config", "user.email", "a4c@example.com")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# seed\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "seed")

	begun, err := svc.BeginTask(context.Background(), a4c.BeginTaskRequest{Objective: "add a file"})
	if err != nil {
		t.Fatalf("BeginTask: %v", err)
	}

	// The work is a NEW file, untracked.
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "calc.go"), []byte("package src\n\nconst Answer = 42\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	diff, err := svc.GetTaskDiff(context.Background(), begun.TaskID)
	if err != nil {
		t.Fatalf("GetTaskDiff: %v", err)
	}

	if diff.TotalFiles == 0 {
		t.Fatalf("new untracked work reported an EMPTY diff: %+v", diff)
	}
	found := false
	for _, f := range diff.Files {
		if filepath.ToSlash(f.Path) == "src/calc.go" {
			found = true
			if f.Additions == 0 {
				t.Errorf("expected additions for the new file, got %+v", f)
			}
		}
	}
	if !found {
		t.Errorf("src/calc.go missing from the diff; got %+v", diff.Files)
	}
	if diff.RawDiff == "" {
		t.Errorf("expected a raw diff for the new file")
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, string(out))
	}
}
