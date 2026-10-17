package a4c_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	spike "github.com/ormasoftchile/a4c/pkg/a4c"
)

// TestContinuation_CheckpointSurvivesRestart is the critical fix: a checkpoint
// injected into an activity's Inputs must survive a JSON persistence round-trip
// and still produce a continuation prompt after a coordinator restart. Before
// the fix, Inputs["checkpoint"] round-tripped as map[string]any and the type
// assertion silently dropped it, rebuilding the task from its initial prompt.
func TestContinuation_CheckpointSurvivesRestart(t *testing.T) {
	chk := spike.TaskCheckpoint{
		TaskID:               "task-1",
		Objective:            "Implement the feature",
		WorktreePath:         "/tmp/wt",
		ModifiedFiles:        []string{"src/a.c"},
		CompletedWork:        []string{"investigated parser"},
		RemainingObligations: []string{"add tests"},
		SourceRuntime:        "copilot",
		TargetRuntime:        "opencode",
	}
	// Simulate the durable round-trip: *TaskCheckpoint -> JSON -> map[string]any.
	inputs := map[string]any{"checkpoint": chk}
	b, err := json.Marshal(inputs)
	if err != nil {
		t.Fatal(err)
	}
	var restored map[string]any
	if err := json.Unmarshal(b, &restored); err != nil {
		t.Fatal(err)
	}
	// The coordinator's rehydration must recover the typed checkpoint.
	recovered := spike.CheckpointFromInput(restored["checkpoint"])
	if recovered == nil {
		t.Fatalf("checkpoint must be rehydrated from JSON round-trip, got nil")
	}
	if recovered.TaskID != "task-1" || recovered.SourceRuntime != "copilot" || recovered.TargetRuntime != "opencode" {
		t.Fatalf("rehydrated checkpoint lost identity: %+v", recovered)
	}
	if len(recovered.CompletedWork) != 1 || recovered.CompletedWork[0] != "investigated parser" {
		t.Fatalf("rehydrated checkpoint lost completed work: %+v", recovered.CompletedWork)
	}
	// The continuation prompt must reflect durable state, not the initial prompt.
	prompt := spike.BuildContinuationPrompt(*recovered)
	if !strings.Contains(prompt, "investigated parser") || !strings.Contains(prompt, "add tests") {
		t.Fatalf("continuation prompt must include completed work and remaining obligations:\n%s", prompt)
	}
	if !strings.Contains(prompt, "copilot -> opencode") {
		t.Fatalf("continuation prompt must show host transition:\n%s", prompt)
	}
	// A typed value passes through unchanged.
	if got := spike.CheckpointFromInput(&chk); got == nil || got.TaskID != "task-1" {
		t.Fatalf("typed checkpoint must pass through, got %+v", got)
	}
	// nil / empty is nil.
	if got := spike.CheckpointFromInput(nil); got != nil {
		t.Fatalf("nil input must yield nil checkpoint")
	}
}

// TestContinuation_SameHostSessionResume: an interrupted activity with a saved
// session reference is resumed on the SAME host via ResumeSession (preserving
// session identity), not recreated with a fresh session.
func TestContinuation_SameHostSessionResume(t *testing.T) {
	fake := spike.NewFakeProvider()
	dir := t.TempDir()
	ws := filepath.Join(dir, "repo")
	_ = os.MkdirAll(ws, 0o755)
	_ = os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module x\n\ngo 1.22\n"), 0o644)
	_ = os.WriteFile(filepath.Join(ws, "x_test.go"), []byte("package x\n\nimport \"testing\"\n\nfunc TestOK(t *testing.T) {}\n"), 0o644)
	store, _ := spike.NewDurableActivityStore(filepath.Join(dir, "activities"))
	memory, _ := spike.NewDurableMemoryStore(filepath.Join(dir, "memory"))
	roster, _ := spike.DefaultTeamRoster(filepath.Join(dir, "roster"))
	gov := spike.NewPolicyGovernor([]string{"governed_read_file", "governed_propose_activity", "governed_submit_review"})
	journal, _ := spike.NewDurableIntentJournal(filepath.Join(dir, "journal"))
	coord := spike.NewTeamCoordinator(ws, store, roster, memory, gov, journal, &spike.NativeGoTestRunner{}, fake)

	resumedIDs := []string{}
	fake.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		return &spike.AgentMessage{Content: "done"}, nil
	}
	// Wrap ResumeSession to observe resumption.
	observed := &resumeObservingProvider{inner: fake, resumed: &resumedIDs}
	coord.RegisterProvider("fake", observed)
	coord.SetDefaultProvider(observed)

	// Enqueue + run an activity once (creates + saves a session ref).
	_ = store.Enqueue(spike.ActivityRequest{ID: "act-1", AgentID: "coder", Objective: "impl", Runtime: "fake", CreatedAt: time.Now()})
	if _, err := coord.Step(context.Background()); err != nil {
		t.Fatalf("first step: %v", err)
	}
	coord.ReleaseWorkspaceLock() // simulate process exit before "restart"
	// Simulate an interruption: reset to claimed and record the session ref.
	ref := coord.LoadSessionRef("act-1")
	if ref == nil || ref.SessionID == "" {
		t.Fatalf("a session reference must be saved after execution, got %+v", ref)
	}
	// Restart: a new coordinator over the same durable state.
	coord2 := spike.NewTeamCoordinator(ws, store, roster, memory, gov, journal, &spike.NativeGoTestRunner{}, observed)
	coord2.RecoverInterruptedActivities(context.Background())
	// Re-claim and run again; it should resume the recorded session.
	if rec, err := store.Get("act-1"); err == nil {
		rec.Status = spike.StatusPending
		_ = store.Update(*rec)
	}
	if _, err := coord2.Step(context.Background()); err != nil {
		t.Fatalf("resumed step: %v", err)
	}
	if len(resumedIDs) == 0 {
		t.Fatalf("expected ResumeSession to be invoked on same-host resume")
	}
	if resumedIDs[0] != ref.SessionID {
		t.Fatalf("expected resume of session %s, got %s", ref.SessionID, resumedIDs[0])
	}
}

// TestContinuation_CrashRecoverySurfacesInterrupted: a claimed activity after a
// crash (no resumable same-host session) is surfaced as interrupted/recovery
// needed — never falsely completed and never blindly replayed.
func TestContinuation_CrashRecoverySurfacesInterrupted(t *testing.T) {
	fake := spike.NewFakeProvider()
	dir := t.TempDir()
	ws := filepath.Join(dir, "repo")
	_ = os.MkdirAll(ws, 0o755)
	store, _ := spike.NewDurableActivityStore(filepath.Join(dir, "activities"))
	memory, _ := spike.NewDurableMemoryStore(filepath.Join(dir, "memory"))
	roster, _ := spike.DefaultTeamRoster(filepath.Join(dir, "roster"))
	gov := spike.NewPolicyGovernor([]string{"governed_read_file"})
	journal, _ := spike.NewDurableIntentJournal(filepath.Join(dir, "journal"))
	coord := spike.NewTeamCoordinator(ws, store, roster, memory, gov, journal, &spike.NativeGoTestRunner{}, fake)

	// Simulate a crash: an activity left in claimed with no session ref.
	_ = store.Enqueue(spike.ActivityRequest{ID: "act-crash", AgentID: "coder", Objective: "impl", Runtime: "fake", CreatedAt: time.Now()})
	if rec, err := store.Get("act-crash"); err == nil {
		rec.Status = spike.StatusClaimed
		_ = store.Update(*rec)
	}

	recovered := coord.RecoverInterruptedActivities(context.Background())
	if recovered != 1 {
		t.Fatalf("expected 1 recovered activity, got %d", recovered)
	}
	rec, err := store.Get("act-crash")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != spike.StatusInterrupted {
		t.Fatalf("a crashed claimed activity must become interrupted (recovery-needed), got %s", rec.Status)
	}
	if rec.Result == nil || rec.Result.Outputs["recovery"] != "recovery_needed" {
		t.Fatalf("interrupted activity must carry a recovery_needed marker, got %+v", rec.Result)
	}
	// It must NOT be falsely completed.
	if rec.Status == spike.StatusCompleted {
		t.Fatalf("crash recovery must never falsely claim completion")
	}
}

// resumeObservingProvider wraps a provider and records ResumeSession calls.
type resumeObservingProvider struct {
	inner   *spike.FakeProvider
	resumed *[]string
}

func (p *resumeObservingProvider) Capabilities() spike.RuntimeCapabilities {
	return p.inner.Capabilities()
}
func (p *resumeObservingProvider) CreateSession(ctx context.Context, config spike.AgentConfig) (spike.AgentSession, error) {
	return p.inner.CreateSession(ctx, config)
}
func (p *resumeObservingProvider) ResumeSession(ctx context.Context, sessionID string, config spike.AgentConfig) (spike.AgentSession, error) {
	*p.resumed = append(*p.resumed, sessionID)
	return p.inner.ResumeSession(ctx, sessionID, config)
}
