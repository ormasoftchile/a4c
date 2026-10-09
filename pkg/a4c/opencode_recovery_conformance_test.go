package a4c_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	spike "github.com/ormasoftchile/a4c/pkg/a4c"
)

// Scenario 1: Interruption during model invocation (unrecorded intent, session cancellation).
func TestGate3_Scenario1_InterruptedModelInvocation(t *testing.T) {
	mock := newMockOpenCodeServer()
	defer mock.Close()

	prov := spike.NewOpenCodeProvider(mock.server.URL, "opencode-go/mimo-v2.5", nil)
	defer prov.Close()

	ctx, cancel := context.WithCancel(context.Background())
	sess, err := prov.CreateSession(ctx, spike.AgentConfig{AgentID: "coder"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer sess.Close()

	// Cancel context immediately
	cancel()
	_, err = sess.Send(ctx, "Hello")
	if err == nil {
		t.Fatalf("expected error on cancelled context, got nil")
	}

	// Verify session can be aborted via Cancel()
	if err := sess.Cancel(); err != nil {
		t.Errorf("Cancel() failed: %v", err)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.abortedSessions) != 1 {
		t.Errorf("expected 1 aborted session, got %d", len(mock.abortedSessions))
	}
}

// Scenario 2: Crash after IntentPersisted but before mutation committed.
// On restart, DurableIntentJournal detects unfinished intent. Reconciler decides whether to commit or abandon.
type mockFileReconciler struct {
	workspaceDir string
}

func (r *mockFileReconciler) Reconcile(ctx context.Context, intent spike.IntentRecord) (bool, string, error) {
	target := filepath.Join(r.workspaceDir, intent.Target)
	if _, err := os.Stat(target); err == nil {
		return true, "committed-reconciled-" + intent.Target, nil
	}
	return false, "", nil
}

func TestGate3_Scenario2_CrashAfterIntentPersistedBeforeCommit(t *testing.T) {
	dir := t.TempDir()
	journalDir := filepath.Join(dir, "journal")
	journal, err := spike.NewDurableIntentJournal(journalDir)
	if err != nil {
		t.Fatalf("create journal: %v", err)
	}

	// Simulate pre-mutation intent logged before a crash
	intentID := "intent-crash-1"
	targetPath := "calc.go"
	intent := spike.IntentRecord{
		IntentID:   intentID,
		ActivityID: "coder",
		Action:     "apply_patch",
		Target:     targetPath,
	}
	if err := journal.RecordIntent(intent); err != nil {
		t.Fatalf("record intent: %v", err)
	}

	// Before restart, file was NOT written (crash happened before disk write)
	unfinished := journal.FindUnfinishedIntents()
	if len(unfinished) != 1 {
		t.Fatalf("expected 1 unfinished intent, got %d", len(unfinished))
	}

	// Restart: Reconcile with mock reconciler
	reconciler := &mockFileReconciler{workspaceDir: dir}
	resolved, err := journal.ReconcileUnfinished(context.Background(), reconciler)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(resolved) != 0 {
		t.Errorf("file didn't exist, should not have resolved/committed intent")
	}

	// Now simulate file was actually partially written before crash
	_ = os.WriteFile(filepath.Join(dir, targetPath), []byte("package calc\n"), 0644)
	resolved, err = journal.ReconcileUnfinished(context.Background(), reconciler)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(resolved) != 1 {
		t.Fatalf("file existed, should have reconciled and committed intent")
	}
	if resolved[0].Status != spike.IntentCommitted {
		t.Errorf("expected Status committed, got %v", resolved[0].Status)
	}
}

// Scenario 3: Crash after mutation committed, before activity marked completed.
func TestGate3_Scenario3_CrashAfterCommitBeforeActivityComplete(t *testing.T) {
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	journalDir := filepath.Join(dir, "journal")

	store, _ := spike.NewDurableActivityStore(storeDir)
	journal, _ := spike.NewDurableIntentJournal(journalDir)

	req := spike.ActivityRequest{
		ID:        "act-crashed-1",
		AgentID:   "coder",
		Objective: "Implement Add",
		CreatedAt: time.Now(),
	}
	if err := store.Enqueue(req); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Intent was already committed
	intentID := "intent-committed-1"
	_ = journal.RecordIntent(spike.IntentRecord{
		IntentID:   intentID,
		ActivityID: req.ID,
		Action:     "apply_patch",
		Target:     "calc.go",
	})
	_ = journal.RecordOutcome(intentID, "committed-calc.go")

	// On restart, activity store and journal are reloaded
	reloadedStore, _ := spike.NewDurableActivityStore(storeDir)
	reloadedJournal, _ := spike.NewDurableIntentJournal(journalDir)

	loadedAct, err := reloadedStore.Get(req.ID)
	if err != nil {
		t.Fatalf("get activity: %v", err)
	}
	if loadedAct.Status != spike.StatusPending {
		t.Errorf("expected StatusPending, got %v", loadedAct.Status)
	}

	intents := reloadedJournal.FindIntentsForActivity(req.ID)
	if len(intents) != 1 || intents[0].Status != spike.IntentCommitted {
		t.Errorf("expected 1 committed intent, got %v", intents)
	}
}

// Scenario 4: Mid-turn budget exhaustion triggers fail-closed ErrBudgetExceeded.
func TestGate3_Scenario4_MidTurnBudgetExhaustion(t *testing.T) {
	mock := newMockOpenCodeServer()
	defer mock.Close()

	// Return token consumption that exceeds allocated budget
	mock.customMessageResp = map[string]any{
		"info": map[string]any{
			"id": "msg-expensive",
			"tokens": map[string]any{
				"total":  10000,
				"input":  6000,
				"output": 4000,
			},
		},
		"parts": []map[string]any{
			{
				"type": "text",
				"text": "Completed task.",
			},
		},
	}

	prov := spike.NewOpenCodeProvider(mock.server.URL, "opencode-go/mimo-v2.5", nil)
	defer prov.Close()

	ctx := context.Background()
	config := spike.AgentConfig{
		AgentID: "coder",
		Budget:  spike.Budget{Allocated: 5000}, // Allocated 5k tokens
	}

	sess, err := prov.CreateSession(ctx, config)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer sess.Close()

	// Invocating consumes 10000 tokens, exceeding 5000 budget
	_, err = sess.Send(ctx, "Run expensive task")
	if err == nil {
		t.Fatalf("expected ErrBudgetExceeded, got nil")
	}
	if !errors.Is(err, spike.ErrBudgetExceeded) {
		t.Fatalf("expected ErrBudgetExceeded, got: %v", err)
	}

	// Immediate next invocation must also fail admission check closed
	_, err = sess.Send(ctx, "Follow-up")
	if !errors.Is(err, spike.ErrBudgetExceeded) {
		t.Fatalf("expected admission check to fail with ErrBudgetExceeded, got: %v", err)
	}
}

// Scenario 5: Daemon restart / session resumption preserves continuous session ID.
func TestGate3_Scenario5_SessionResumptionPreservesID(t *testing.T) {
	mock := newMockOpenCodeServer()
	defer mock.Close()

	prov := spike.NewOpenCodeProvider(mock.server.URL, "opencode-go/mimo-v2.5", nil)
	defer prov.Close()

	ctx := context.Background()
	sess1, err := prov.CreateSession(ctx, spike.AgentConfig{AgentID: "lead"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	origID := sess1.SessionID()
	sess1.Close()

	// Resume session with existing ID
	sess2, err := prov.ResumeSession(ctx, origID, spike.AgentConfig{AgentID: "lead"})
	if err != nil {
		t.Fatalf("resume session: %v", err)
	}
	defer sess2.Close()

	if sess2.SessionID() != origID {
		t.Errorf("expected resumed session to preserve ID %s, got %s", origID, sess2.SessionID())
	}
}

// Scenario 6: Malformed patch proposal fails closed without workspace corruption.
func TestGate3_Scenario6_MalformedPatchFailsClosed(t *testing.T) {
	mock := newMockOpenCodeServer()
	defer mock.Close()

	dir := t.TempDir()
	canaryPath := filepath.Join(dir, "canary.txt")
	_ = os.WriteFile(canaryPath, []byte("original canary data"), 0644)

	gov := spike.NewPolicyGovernor([]string{"governed_apply_patch"})
	gov.SetAgentTools("coder", []string{"governed_apply_patch"})

	prov := spike.NewOpenCodeProvider(mock.server.URL, "opencode-go/mimo-v2.5", nil)
	defer prov.Close()

	// Output proposal targeting path traversal
	mock.customMessageResp = map[string]any{
		"info": map[string]any{
			"tokens": map[string]any{"total": 50},
		},
		"parts": []map[string]any{
			{
				"type": "text",
				"text": "[PROPOSE_PATCH: path=\"../../../canary.txt\"]\n```\ncorrupted canary\n```\n[END_PATCH]",
			},
		},
	}

	ctx := context.Background()
	sess, _ := prov.CreateSession(ctx, spike.AgentConfig{
		AgentID:      "coder",
		WorkspaceDir: dir,
		Governor:     gov,
	})
	defer sess.Close()

	_, err := sess.Send(ctx, "Overwrite canary")
	if err == nil {
		t.Fatalf("expected error on malformed/traversal patch, got nil")
	}

	// Verify canary was unaffected
	canaryBytes, _ := os.ReadFile(canaryPath)
	if string(canaryBytes) != "original canary data" {
		t.Errorf("canary was corrupted! got %q", string(canaryBytes))
	}
}

// Scenario 7: Concurrent step / single-writer lock contention prevents conflicting mutation.
func TestGate3_Scenario7_SingleWriterLockContention(t *testing.T) {
	dir := t.TempDir()

	lock1, err := spike.AcquireWorkspaceLock(dir, "coord-1", "task-1")
	if err != nil {
		t.Fatalf("acquire first lock: %v", err)
	}
	defer lock1.Release()

	// Second writer attempting lock must fail
	lock2, err := spike.AcquireWorkspaceLock(dir, "coord-2", "task-2")
	if err == nil {
		lock2.Release()
		t.Fatalf("expected lock collision, but acquired second lock")
	}
	if !errors.Is(err, spike.ErrWorkspaceLocked) {
		t.Errorf("expected ErrWorkspaceLocked, got: %v", err)
	}
}

// Scenario 8: Token economics normalization across parts and info.
func TestGate3_Scenario8_TokenEconomicsNormalization(t *testing.T) {
	mock := newMockOpenCodeServer()
	defer mock.Close()

	// Response with explicit split tokens and cost
	mock.customMessageResp = map[string]any{
		"info": map[string]any{
			"id":   "msg-norm",
			"cost": 0.0042,
			"tokens": map[string]any{
				"input":     1200,
				"output":    350,
				"reasoning": 150,
				"cache": map[string]any{
					"read":  400,
					"write": 50,
				},
				"total": 1700,
			},
		},
		"parts": []map[string]any{
			{
				"type": "text",
				"text": "Normalized response.",
			},
		},
	}

	prov := spike.NewOpenCodeProvider(mock.server.URL, "opencode-go/mimo-v2.5", nil)
	defer prov.Close()

	ctx := context.Background()
	sess, _ := prov.CreateSession(ctx, spike.AgentConfig{AgentID: "coder"})
	defer sess.Close()

	_, err := sess.Send(ctx, "Normalize metrics")
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	usage := sess.Usage()
	if usage.PromptTokens != 1200 {
		t.Errorf("expected PromptTokens 1200, got %d", usage.PromptTokens)
	}
	if usage.CompletionTokens != 350 {
		t.Errorf("expected CompletionTokens 350, got %d", usage.CompletionTokens)
	}
	if usage.ReasoningTokens != 150 {
		t.Errorf("expected ReasoningTokens 150, got %d", usage.ReasoningTokens)
	}
	if usage.CachedTokens != 400 {
		t.Errorf("expected CachedTokens 400, got %d", usage.CachedTokens)
	}
	if usage.TotalTokens != 1700 {
		t.Errorf("expected TotalTokens 1700, got %d", usage.TotalTokens)
	}
	if usage.ReportedCostUSD != 0.0042 {
		t.Errorf("expected ReportedCostUSD 0.0042, got %f", usage.ReportedCostUSD)
	}
}
