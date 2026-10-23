package a4c_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/ormasoftchile/a4c/pkg/a4c"
)

// resolvingService builds a service over real durable stores and creates n
// tasks, returning it and their IDs in creation order.
func resolvingService(t *testing.T, n int) (*a4c.TeamServiceImpl, []string) {
	t.Helper()
	dir := t.TempDir()

	store, err := a4c.NewDurableActivityStore(filepath.Join(dir, ".a4c", "activities"))
	if err != nil {
		t.Fatalf("activity store: %v", err)
	}
	memory, err := a4c.NewDurableMemoryStore(filepath.Join(dir, ".a4c", "memory"))
	if err != nil {
		t.Fatalf("memory store: %v", err)
	}
	journal, err := a4c.NewDurableIntentJournal(filepath.Join(dir, ".a4c", "journal"))
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	lifecycle, err := a4c.NewDurableLifecycleStore(filepath.Join(dir, ".a4c", "lifecycle"))
	if err != nil {
		t.Fatalf("lifecycle store: %v", err)
	}
	gov := a4c.NewPolicyGovernor([]string{"governed_read_file", "governed_apply_patch", "governed_propose_memory"})
	svc := a4c.NewTeamService(dir, store, lifecycle, gov, memory, journal, nil)

	var ids []string
	for i := 0; i < n; i++ {
		res, err := svc.BeginTask(t.Context(), a4c.BeginTaskRequest{
			Objective: "objective-" + string(rune('a'+i)),
		})
		if err != nil {
			t.Fatalf("BeginTask %d: %v", i, err)
		}
		ids = append(ids, res.TaskID)
		// Distinct timestamps so "most recent" is well defined.
		time.Sleep(2 * time.Millisecond)
	}
	return svc, ids
}

// TestResolveTask_ExplicitID: an explicit identifier always wins and is
// validated.
func TestResolveTask_ExplicitID(t *testing.T) {
	svc, ids := resolvingService(t, 2)

	got, err := svc.ResolveTask(ids[0], nil)
	if err != nil {
		t.Fatalf("ResolveTask(explicit): %v", err)
	}
	if got.TaskID != ids[0] {
		t.Errorf("resolved %q, want %q", got.TaskID, ids[0])
	}

	if _, err := svc.ResolveTask("task-does-not-exist", nil); !errors.Is(err, a4c.ErrTaskNotFound) {
		t.Errorf("expected ErrTaskNotFound, got %v", err)
	}
}

// TestResolveTask_OmittedPicksMostRecent: with no ID, the newest task wins —
// that is what "resume whatever I was doing" means after an interruption.
func TestResolveTask_OmittedPicksMostRecent(t *testing.T) {
	svc, ids := resolvingService(t, 3)

	got, err := svc.ResolveTask("", nil)
	if err != nil {
		t.Fatalf("ResolveTask(omitted): %v", err)
	}
	if got.TaskID != ids[2] {
		t.Errorf("omitted ID resolved to %q, want most recent %q", got.TaskID, ids[2])
	}
}

// TestResolveTask_PreferFilters: a preferred state narrows the search, and a
// preference that matches nothing is an error rather than a silent fallback.
func TestResolveTask_PreferFilters(t *testing.T) {
	svc, ids := resolvingService(t, 1)
	_ = ids

	// BeginTask enqueues the task, so queued is the state it lands in.
	if _, err := svc.ResolveTask("", []a4c.TaskLifecycleState{a4c.TaskStateQueued}); err != nil {
		t.Fatalf("expected the queued task to match: %v", err)
	}

	// accepted does not match anything in this workspace.
	_, err := svc.ResolveTask("", []a4c.TaskLifecycleState{a4c.TaskStateAccepted})
	if err == nil {
		t.Fatalf("expected an error when no task matches the preferred state")
	}
	if !errors.Is(err, a4c.ErrTaskNotFound) && err.Error() == "" {
		t.Errorf("expected a descriptive error, got %v", err)
	}
}

// TestResolveTask_EmptyWorkspace: an omitted ID in an empty workspace must fail
// loudly rather than no-op.
func TestResolveTask_EmptyWorkspace(t *testing.T) {
	dir := t.TempDir()
	store, err := a4c.NewDurableActivityStore(filepath.Join(dir, ".a4c", "activities"))
	if err != nil {
		t.Fatalf("activity store: %v", err)
	}
	lifecycle, err := a4c.NewDurableLifecycleStore(filepath.Join(dir, ".a4c", "lifecycle"))
	if err != nil {
		t.Fatalf("lifecycle store: %v", err)
	}
	gov := a4c.NewPolicyGovernor([]string{"governed_read_file", "governed_apply_patch", "governed_propose_memory"})
	svc := a4c.NewTeamService(dir, store, lifecycle, gov, nil, nil, nil)
	if _, err := svc.ResolveTask("", nil); err == nil {
		t.Fatalf("expected an error for an empty workspace")
	}
	// An explicit ID in an empty workspace is a not-found, not a crash.
	if _, err := svc.ResolveTask("task-1", nil); !errors.Is(err, a4c.ErrTaskNotFound) {
		t.Errorf("expected ErrTaskNotFound, got %v", err)
	}
}
