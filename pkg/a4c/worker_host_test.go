package a4c_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ormasoftchile/a4c/pkg/a4c"
)

type workerHostHarness struct {
	repoDir     string
	store       *a4c.DurableActivityStore
	journal     *a4c.DurableIntentJournal
	coordinator *a4c.TeamCoordinator
	host        *a4c.WorkerHost
}

type workerMockTestRunner struct{}

func (m *workerMockTestRunner) RunTests(ctx context.Context, dir string) (int, string, error) {
	return 0, "PASS", nil
}

func setupWorkerHostHarness(t *testing.T, maxSteps int) *workerHostHarness {
	t.Helper()
	repoDir := t.TempDir()

	runGitCmd(t, repoDir, "init")
	runGitCmd(t, repoDir, "config", "user.name", "Tester")
	runGitCmd(t, repoDir, "config", "user.email", "tester@example.com")
	_ = os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# Test\n"), 0644)
	runGitCmd(t, repoDir, "add", "README.md")
	runGitCmd(t, repoDir, "commit", "-m", "init")

	dotDir := filepath.Join(repoDir, ".a4c")
	storeDir := filepath.Join(dotDir, "activities")
	rosterDir := filepath.Join(dotDir, "roster")
	memDir := filepath.Join(dotDir, "memory")
	journalDir := filepath.Join(dotDir, "journal")
	wtDir := filepath.Join(dotDir, "worktrees")

	store, err := a4c.NewDurableActivityStore(storeDir)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	roster, err := a4c.DefaultTeamRoster(rosterDir)
	if err != nil {
		t.Fatalf("create roster: %v", err)
	}
	memory, err := a4c.NewDurableMemoryStore(memDir)
	if err != nil {
		t.Fatalf("create memory: %v", err)
	}
	journal, err := a4c.NewDurableIntentJournal(journalDir)
	if err != nil {
		t.Fatalf("create journal: %v", err)
	}
	wtMgr, err := a4c.NewWorktreeManager(repoDir, wtDir)
	if err != nil {
		t.Fatalf("create worktrees: %v", err)
	}

	gov := a4c.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_propose_activity",
		"governed_submit_review", "governed_request_human_decision", "governed_propose_memory",
	})
	fake := a4c.NewFakeProvider()
	fake.SetSessionResponseFunc(func(ctx context.Context, session *a4c.FakeSession, prompt string) (*a4c.AgentMessage, error) {
		return &a4c.AgentMessage{
			Role:    "assistant",
			Content: "Completed successfully",
		}, nil
	})
	coord := a4c.NewTeamCoordinator(repoDir, store, roster, memory, gov, journal, &workerMockTestRunner{}, fake)
	coord.SetWorktreeManager(wtMgr)

	cfg := a4c.WorkerHostConfig{
		WorkspaceDir: repoDir,
		PollInterval: 50 * time.Millisecond,
		MaxSteps:     maxSteps,
	}
	host := a4c.NewWorkerHost(cfg, coord, store, journal)

	return &workerHostHarness{
		repoDir:     repoDir,
		store:       store,
		journal:     journal,
		coordinator: coord,
		host:        host,
	}
}

func TestWorkerHost_SingleWriterWorkspaceLock(t *testing.T) {
	h := setupWorkerHostHarness(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		_ = h.host.Run(ctx)
	}()

	// Wait for host to start and hold lock
	time.Sleep(100 * time.Millisecond)

	// Second process attempts to acquire workspace lock -> Must fail with ErrWorkspaceLocked
	_, err := a4c.AcquireWorkspaceLock(h.repoDir, "competitor-process", "task-x")
	if !errors.Is(err, a4c.ErrWorkspaceLocked) {
		t.Fatalf("expected ErrWorkspaceLocked when host is running, got %v", err)
	}

	// Cancel context to trigger graceful shutdown
	cancel()
	wg.Wait()

	// After host exits, lock must be released cleanly
	secondLock, err := a4c.AcquireWorkspaceLock(h.repoDir, "competitor-process", "task-x")
	if err != nil {
		t.Fatalf("expected lock acquisition to succeed after host shutdown, got %v", err)
	}
	_ = secondLock.Release()
}

func TestWorkerHost_BoundedExecution(t *testing.T) {
	h := setupWorkerHostHarness(t, 5)

	// Enqueue a task with fake adapter
	_, err := h.coordinator.SubmitTaskWithStrategy("Quick test feature", a4c.Budget{Allocated: 10000}, nil, a4c.StrategyDirect)
	if err != nil {
		t.Fatalf("submit task: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = h.host.Run(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("host.Run failed: %v", err)
	}

	// Verify that claimable activities were executed
	completed := h.store.CompletedCount()
	if completed < 1 {
		t.Fatalf("expected at least 1 completed activity, got %d", completed)
	}
}
