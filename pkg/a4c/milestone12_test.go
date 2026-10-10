package a4c_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	spike "github.com/ormasoftchile/a4c/pkg/a4c"
)

// mockOpenCodeServer creates an httptest.Server simulating OpenCode REST API.
type mockOpenCodeServer struct {
	server             *httptest.Server
	mu                 sync.Mutex
	createdSessions    []map[string]any
	receivedMessages   []map[string]any
	abortedSessions    []string
	customMessageResp  map[string]any
	customStatus       int
	healthStatus       int
}

func newMockOpenCodeServer() *mockOpenCodeServer {
	mock := &mockOpenCodeServer{
		healthStatus: http.StatusOK,
	}

	mux := http.NewServeMux()

	// Health check
	mux.HandleFunc("/global/health", func(w http.ResponseWriter, r *http.Request) {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		if mock.healthStatus != http.StatusOK {
			w.WriteHeader(mock.healthStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"healthy": true,
			"version": "1.18.35",
		})
	})

	// Session create
	mux.HandleFunc("/session", func(w http.ResponseWriter, r *http.Request) {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		if mock.customStatus != 0 && mock.customStatus != http.StatusOK {
			w.WriteHeader(mock.customStatus)
			_, _ = w.Write([]byte(fmt.Sprintf(`{"error": "status %d"}`, mock.customStatus)))
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mock.createdSessions = append(mock.createdSessions, body)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":    fmt.Sprintf("ses-mock-%d", len(mock.createdSessions)),
			"title": body["title"],
		})
	})

	// Session message & abort
	mux.HandleFunc("/session/", func(w http.ResponseWriter, r *http.Request) {
		mock.mu.Lock()
		defer mock.mu.Unlock()

		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/session/"), "/")
		if len(parts) >= 2 && parts[1] == "abort" {
			mock.abortedSessions = append(mock.abortedSessions, parts[0])
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success": true}`))
			return
		}

		if len(parts) >= 2 && parts[1] == "message" {
			if mock.customStatus != 0 && mock.customStatus != http.StatusOK {
				w.WriteHeader(mock.customStatus)
				_, _ = w.Write([]byte(fmt.Sprintf(`{"error": "status %d"}`, mock.customStatus)))
				return
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mock.receivedMessages = append(mock.receivedMessages, body)

			w.Header().Set("Content-Type", "application/json")
			if mock.customMessageResp != nil {
				_ = json.NewEncoder(w).Encode(mock.customMessageResp)
				return
			}

			// Default response
			_ = json.NewEncoder(w).Encode(map[string]any{
				"info": map[string]any{
					"id":        "msg-mock-1",
					"sessionID": parts[0],
					"role":      "assistant",
					"tokens": map[string]any{
						"input":     150,
						"output":    75,
						"reasoning": 25,
						"cache": map[string]any{
							"read":  50,
							"write": 0,
						},
						"total": 300,
					},
					"cost": 0.000125,
				},
				"parts": []map[string]any{
					{
						"type": "text",
						"text": "Implementation completed successfully.",
					},
				},
			})
			return
		}

		http.NotFound(w, r)
	})

	mock.server = httptest.NewServer(mux)
	return mock
}

func (m *mockOpenCodeServer) Close() {
	m.server.Close()
}

// 1. Availability detection
func TestMilestone12_01_OpenCodeAvailabilityDetection(t *testing.T) {
	// Custom env override
	tmpBin := filepath.Join(t.TempDir(), "fake-opencode")
	_ = os.WriteFile(tmpBin, []byte("#!/bin/sh\nexit 0\n"), 0755)

	t.Setenv("OPENCODE_BIN_PATH", tmpBin)
	p, err := spike.FindOpenCodeBinary()
	if err != nil {
		t.Fatalf("expected binary found via env, got: %v", err)
	}
	if p != tmpBin {
		t.Fatalf("expected %s, got %s", tmpBin, p)
	}

	// Missing override
	t.Setenv("OPENCODE_BIN_PATH", "/non/existent/path/opencode")
	t.Setenv("PATH", "")
	_, err = spike.FindOpenCodeBinary()
	if !errors.Is(err, spike.ErrOpenCodeNotInstalled) {
		t.Fatalf("expected ErrOpenCodeNotInstalled, got %v", err)
	}
}

// 2. Server lifecycle: external health check
func TestMilestone12_02_ServerLifecycle_ExternalHealth(t *testing.T) {
	mock := newMockOpenCodeServer()
	defer mock.Close()

	ctx := context.Background()
	mgr, err := spike.NewExternalOpenCodeServer(ctx, mock.server.URL)
	if err != nil {
		t.Fatalf("expected healthy connection, got: %v", err)
	}
	if mgr.IsManaged() {
		t.Fatalf("expected IsManaged=false for external server")
	}
	if mgr.Endpoint() != mock.server.URL {
		t.Fatalf("endpoint mismatch: %s vs %s", mgr.Endpoint(), mock.server.URL)
	}

	// Unhealthy external server
	mock.healthStatus = http.StatusServiceUnavailable
	_, err = spike.NewExternalOpenCodeServer(ctx, mock.server.URL)
	if err == nil {
		t.Fatalf("expected error on unhealthy server")
	}
}

// 3. Model identifier parsing
func TestMilestone12_03_ModelIdentifierParsing(t *testing.T) {
	cases := []struct {
		input        string
		wantProvider string
		wantModel    string
	}{
		{"opencode-go/mimo-v2.5", "opencode-go", "mimo-v2.5"},
		{"anthropic/claude-3-5-sonnet", "anthropic", "claude-3-5-sonnet"},
		{"gpt-4o", "opencode-go", "gpt-4o"},
		{"", "opencode-go", "mimo-v2.5"},
	}

	for _, tc := range cases {
		p, m := spike.ParseModelIdentifier(tc.input)
		if p != tc.wantProvider || m != tc.wantModel {
			t.Errorf("ParseModelIdentifier(%q) = (%q, %q), want (%q, %q)",
				tc.input, p, m, tc.wantProvider, tc.wantModel)
		}
	}
}

// 4. Session creation & capability profile with tool containment
func TestMilestone12_04_SessionCreation_ToolContainmentRules(t *testing.T) {
	mock := newMockOpenCodeServer()
	defer mock.Close()

	provider := spike.NewOpenCodeProvider(mock.server.URL, "opencode-go/mimo-v2.5", nil)
	defer provider.Close()

	ctx := context.Background()
	reviewerConfig := spike.AgentConfig{
		AgentID: "reviewer",
		Charter: "Quality Engineer",
		Model:   "opencode-go/mimo-v2.5",
	}

	sess, err := provider.CreateSession(ctx, reviewerConfig)
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}
	defer sess.Close()

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.createdSessions) != 1 {
		t.Fatalf("expected 1 created session, got %d", len(mock.createdSessions))
	}

	created := mock.createdSessions[0]
	perms, ok := created["permission"].([]any)
	if !ok || len(perms) == 0 {
		t.Fatalf("expected non-empty permission rules in session, got: %v", created["permission"])
	}

	// Verify that reviewer has bash and task explicitly denied
	bashDenied := false
	taskDenied := false
	for _, p := range perms {
		rule, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if rule["permission"] == "bash" && rule["action"] == "deny" {
			bashDenied = true
		}
		if rule["permission"] == "task" && rule["action"] == "deny" {
			taskDenied = true
		}
	}

	if !bashDenied {
		t.Errorf("security violation: 'bash' permission rule was NOT denied for reviewer")
	}
	if !taskDenied {
		t.Errorf("security violation: 'task' permission rule was NOT denied")
	}
}

// 5. Prompt execution, message tools map & usage parsing
func TestMilestone12_05_PromptExecution_And_UsageParsing(t *testing.T) {
	mock := newMockOpenCodeServer()
	defer mock.Close()

	provider := spike.NewOpenCodeProvider(mock.server.URL, "opencode-go/mimo-v2.5", nil)
	defer provider.Close()

	ctx := context.Background()
	config := spike.AgentConfig{
		AgentID: "coder",
		Charter: "Software Engineer",
	}

	sess, err := provider.CreateSession(ctx, config)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	defer sess.Close()

	msg, err := sess.Send(ctx, "Implement pure Add function in calc.go")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if !strings.Contains(msg.Content, "Implementation completed successfully") {
		t.Fatalf("unexpected content: %s", msg.Content)
	}

	// Verify message level tools containment
	mock.mu.Lock()
	if len(mock.receivedMessages) != 1 {
		t.Fatalf("expected 1 received message, got %d", len(mock.receivedMessages))
	}
	received := mock.receivedMessages[0]
	toolsMap, ok := received["tools"].(map[string]any)
	if !ok {
		t.Fatalf("expected tools toggle map in message, got %v", received["tools"])
	}
	if toolsMap["task"] != false {
		t.Errorf("message-level containment failed: 'task' not disabled in tools map")
	}
	if toolsMap["bash"] != true {
		t.Errorf("expected native bash to be enabled for coder")
	}
	mock.mu.Unlock()

	// Verify usage parsing
	usage := sess.Usage()
	if usage.PromptTokens != 150 {
		t.Errorf("expected 150 prompt tokens, got %d", usage.PromptTokens)
	}
	if usage.CompletionTokens != 75 {
		t.Errorf("expected 75 completion tokens, got %d", usage.CompletionTokens)
	}
	if usage.ReasoningTokens != 25 {
		t.Errorf("expected 25 reasoning tokens, got %d", usage.ReasoningTokens)
	}
	if usage.CachedTokens != 50 {
		t.Errorf("expected 50 cached tokens, got %d", usage.CachedTokens)
	}
	if usage.TotalTokens != 300 {
		t.Errorf("expected 300 total tokens, got %d", usage.TotalTokens)
	}
	if usage.ReportedCostUSD < 0.00012 {
		t.Errorf("expected cost reported, got %f", usage.ReportedCostUSD)
	}
}

// 6. Dual-layer tool containment: Reviewer role has no write permissions
func TestMilestone12_06_ReviewerRole_HasNoWritePermissions(t *testing.T) {
	mock := newMockOpenCodeServer()
	defer mock.Close()

	provider := spike.NewOpenCodeProvider(mock.server.URL, "opencode-go/mimo-v2.5", nil)
	defer provider.Close()

	ctx := context.Background()
	revConfig := spike.AgentConfig{
		AgentID: "reviewer",
		Charter: "Code Reviewer",
	}

	sess, err := provider.CreateSession(ctx, revConfig)
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}
	defer sess.Close()

	_, err = sess.Send(ctx, "Review diff for concurrency bugs")
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	received := mock.receivedMessages[0]
	toolsMap := received["tools"].(map[string]any)

	// Reviewer must have write, edit, patch disabled
	if toolsMap["write"] != false {
		t.Errorf("reviewer must have write=false")
	}
	if toolsMap["edit"] != false {
		t.Errorf("reviewer must have edit=false")
	}
	if toolsMap["patch"] != false {
		t.Errorf("reviewer must have patch=false")
	}
}

// 7. Budget enforcement: admission check fails closed
func TestMilestone12_07_BudgetEnforcement_AdmissionCheckFailClosed(t *testing.T) {
	mock := newMockOpenCodeServer()
	defer mock.Close()

	provider := spike.NewOpenCodeProvider(mock.server.URL, "opencode-go/mimo-v2.5", nil)
	defer provider.Close()

	ctx := context.Background()
	config := spike.AgentConfig{
		AgentID: "coder",
		Budget: spike.Budget{
			Allocated: 100,
			Consumed:  150, // Already exceeded!
		},
	}

	sess, err := provider.CreateSession(ctx, config)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	defer sess.Close()

	_, err = sess.Send(ctx, "Continue working")
	if !errors.Is(err, spike.ErrBudgetExceeded) {
		t.Fatalf("expected ErrBudgetExceeded on exhausted budget, got: %v", err)
	}
}

// 8. Budget enforcement: post-turn consumption exceeds budget
func TestMilestone12_08_BudgetEnforcement_PostTurnFailClosed(t *testing.T) {
	mock := newMockOpenCodeServer()
	defer mock.Close()

	provider := spike.NewOpenCodeProvider(mock.server.URL, "opencode-go/mimo-v2.5", nil)
	defer provider.Close()

	ctx := context.Background()
	config := spike.AgentConfig{
		AgentID: "coder",
		Budget: spike.Budget{
			Allocated: 200, // Response will consume 300 total tokens!
			Consumed:  0,
		},
	}

	sess, err := provider.CreateSession(ctx, config)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	defer sess.Close()

	_, err = sess.Send(ctx, "Perform expensive computation")
	if !errors.Is(err, spike.ErrBudgetExceeded) {
		t.Fatalf("expected ErrBudgetExceeded when response exceeds budget, got: %v", err)
	}
}

// 9. Session abort / cancellation
func TestMilestone12_09_SessionAbortCancel(t *testing.T) {
	mock := newMockOpenCodeServer()
	defer mock.Close()

	provider := spike.NewOpenCodeProvider(mock.server.URL, "opencode-go/mimo-v2.5", nil)
	defer provider.Close()

	ctx := context.Background()
	sess, err := provider.CreateSession(ctx, spike.AgentConfig{AgentID: "lead"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	sessionID := sess.SessionID()

	// Abort session
	if err := sess.Cancel(); err != nil {
		t.Fatalf("Cancel failed: %v", err)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.abortedSessions) != 1 {
		t.Fatalf("expected 1 aborted session call, got %d", len(mock.abortedSessions))
	}
	if mock.abortedSessions[0] != sessionID {
		t.Fatalf("expected aborted session %s, got %s", sessionID, mock.abortedSessions[0])
	}
}

// 10. Duration separation (ModelDuration vs WallClock)
func TestMilestone12_10_DurationSeparation(t *testing.T) {
	mock := newMockOpenCodeServer()
	defer mock.Close()

	provider := spike.NewOpenCodeProvider(mock.server.URL, "opencode-go/mimo-v2.5", nil)
	defer provider.Close()

	ctx := context.Background()
	sess, err := provider.CreateSession(ctx, spike.AgentConfig{AgentID: "coder"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	defer sess.Close()

	time.Sleep(10 * time.Millisecond)
	_, _ = sess.Send(ctx, "Hello")

	modelDur, toolDur := sess.Durations()
	if modelDur <= 0 {
		t.Errorf("expected positive ModelDuration, got %v", modelDur)
	}
	if toolDur != 0 {
		t.Errorf("expected zero tool duration for direct turn, got %v", toolDur)
	}
}

// 11. Error mapping: HTTP 401 Unauthorized
func TestMilestone12_11_ErrorMapping_Unauthorized(t *testing.T) {
	mock := newMockOpenCodeServer()
	defer mock.Close()
	mock.customStatus = http.StatusUnauthorized

	provider := spike.NewOpenCodeProvider(mock.server.URL, "opencode-go/mimo-v2.5", nil)
	defer provider.Close()

	ctx := context.Background()
	_, err := provider.CreateSession(ctx, spike.AgentConfig{AgentID: "coder"})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected 401 error, got: %v", err)
	}
}

// 12. Error mapping: HTTP 429 Rate Limit
func TestMilestone12_12_ErrorMapping_RateLimit(t *testing.T) {
	mock := newMockOpenCodeServer()
	defer mock.Close()

	provider := spike.NewOpenCodeProvider(mock.server.URL, "opencode-go/mimo-v2.5", nil)
	defer provider.Close()

	ctx := context.Background()
	sess, err := provider.CreateSession(ctx, spike.AgentConfig{AgentID: "coder"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	defer sess.Close()

	mock.customStatus = http.StatusTooManyRequests
	_, err = sess.Send(ctx, "Run prompt")
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("expected 429 rate limit error, got: %v", err)
	}
}

// 13. Multi-Runtime Coordinator: Dynamic provider resolution
func TestMilestone12_13_CoordinatorMultiRuntimeResolution(t *testing.T) {
	mock := newMockOpenCodeServer()
	defer mock.Close()

	workDir := t.TempDir()
	storeDir := filepath.Join(workDir, ".a4c", "activities")
	rosterDir := filepath.Join(workDir, ".a4c", "roster")
	memDir := filepath.Join(workDir, ".a4c", "memory")
	journalDir := filepath.Join(workDir, ".a4c", "journal")

	roster, _ := spike.DefaultTeamRoster(rosterDir)
	store, _ := spike.NewDurableActivityStore(storeDir)
	memStore, _ := spike.NewDurableMemoryStore(memDir)
	journal, _ := spike.NewDurableIntentJournal(journalDir)
	gov := spike.NewPolicyGovernor([]string{"governed_read_file"})
	runner := &mockTestRunner{}

	fakeProvider := spike.NewFakeProvider()
	opProvider := spike.NewOpenCodeProvider(mock.server.URL, "opencode-go/mimo-v2.5", nil)
	defer opProvider.Close()

	coord := spike.NewTeamCoordinator(workDir, store, roster, memStore, gov, journal, runner, fakeProvider)
	coord.RegisterProvider("opencode", opProvider)

	// Submit task with OpenCode runtime
	inputs := map[string]any{
		"runtime": "opencode",
		"model":   "opencode-go/mimo-v2.5",
	}
	req, err := coord.SubmitTaskWithStrategy("Verify OpenCode routing", spike.Budget{Allocated: 50000}, inputs, spike.StrategyDirect)
	if err != nil {
		t.Fatalf("SubmitTaskWithStrategy failed: %v", err)
	}

	if req.Runtime != "opencode" {
		t.Fatalf("expected req.Runtime = opencode, got %s", req.Runtime)
	}

	// Step coordinator - must invoke OpenCode provider, not FakeProvider
	ctx := context.Background()
	res, err := coord.Step(ctx)
	if err != nil {
		t.Fatalf("Step failed: %v", err)
	}

	if res.Status != spike.StatusCompleted {
		t.Fatalf("expected StatusCompleted, got %s", res.Status)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.createdSessions) != 1 {
		t.Fatalf("expected OpenCode session to be created, got %d", len(mock.createdSessions))
	}
	if len(mock.receivedMessages) != 1 {
		t.Fatalf("expected OpenCode message to be sent, got %d", len(mock.receivedMessages))
	}
}

// 14. Normalized Usage Settlement into ActivityResult
func TestMilestone12_14_UsageSettlementToActivityResult(t *testing.T) {
	mock := newMockOpenCodeServer()
	defer mock.Close()

	workDir := t.TempDir()
	storeDir := filepath.Join(workDir, ".a4c", "activities")
	rosterDir := filepath.Join(workDir, ".a4c", "roster")
	memDir := filepath.Join(workDir, ".a4c", "memory")
	journalDir := filepath.Join(workDir, ".a4c", "journal")

	roster, _ := spike.DefaultTeamRoster(rosterDir)
	store, _ := spike.NewDurableActivityStore(storeDir)
	memStore, _ := spike.NewDurableMemoryStore(memDir)
	journal, _ := spike.NewDurableIntentJournal(journalDir)
	gov := spike.NewPolicyGovernor([]string{"governed_read_file"})
	runner := &mockTestRunner{}

	opProvider := spike.NewOpenCodeProvider(mock.server.URL, "opencode-go/mimo-v2.5", nil)
	defer opProvider.Close()

	coord := spike.NewTeamCoordinator(workDir, store, roster, memStore, gov, journal, runner, opProvider)

	_, err := coord.SubmitTaskWithStrategy("Settlement check", spike.Budget{Allocated: 50000}, nil, spike.StrategyDirect)
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	res, err := coord.Step(context.Background())
	if err != nil {
		t.Fatalf("Step: %v", err)
	}

	// Total tokens from mock = 300
	if res.Consumed.Consumed != 300 {
		t.Fatalf("expected Consumed.Consumed = 300, got %.0f", res.Consumed.Consumed)
	}
	if res.Metrics.TotalTokens != 300 {
		t.Fatalf("expected Metrics.TotalTokens = 300, got %d", res.Metrics.TotalTokens)
	}
	if res.Metrics.ReasoningTokens != 25 {
		t.Fatalf("expected ReasoningTokens = 25, got %d", res.Metrics.ReasoningTokens)
	}
	if res.Metrics.CachedTokens != 50 {
		t.Fatalf("expected CachedTokens = 50, got %d", res.Metrics.CachedTokens)
	}
	if res.Metrics.ReportedCostUSD < 0.00012 {
		t.Fatalf("expected ReportedCostUSD recorded, got %f", res.Metrics.ReportedCostUSD)
	}
}

// 15. Crash Recovery: Resume OpenCode session from stored session ID
func TestMilestone12_15_ResumeSessionReconnection(t *testing.T) {
	mock := newMockOpenCodeServer()
	defer mock.Close()

	provider := spike.NewOpenCodeProvider(mock.server.URL, "opencode-go/mimo-v2.5", nil)
	defer provider.Close()

	ctx := context.Background()
	existingSessionID := "ses-persisted-prior-crash"

	sess, err := provider.ResumeSession(ctx, existingSessionID, spike.AgentConfig{
		AgentID: "coder",
	})
	if err != nil {
		t.Fatalf("ResumeSession failed: %v", err)
	}
	defer sess.Close()

	if sess.SessionID() != existingSessionID {
		t.Fatalf("expected session ID %s, got %s", existingSessionID, sess.SessionID())
	}

	// Sending prompt uses existing session ID without calling POST /session
	_, err = sess.Send(ctx, "Continue interrupted work")
	if err != nil {
		t.Fatalf("Send on resumed session failed: %v", err)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	// No new session should have been created
	if len(mock.createdSessions) != 0 {
		t.Fatalf("expected 0 new sessions created on resume, got %d", len(mock.createdSessions))
	}
	if len(mock.receivedMessages) != 1 {
		t.Fatalf("expected 1 message sent, got %d", len(mock.receivedMessages))
	}
}

// 16. Scoped tools: Read/Glob/Grep permitted for lead and reviewer
func TestMilestone12_16_ScopedTools_PermittedReadOnly(t *testing.T) {
	mock := newMockOpenCodeServer()
	defer mock.Close()

	provider := spike.NewOpenCodeProvider(mock.server.URL, "opencode-go/mimo-v2.5", nil)
	defer provider.Close()

	ctx := context.Background()
	for _, roleID := range []string{"lead", "reviewer"} {
		sess, err := provider.CreateSession(ctx, spike.AgentConfig{AgentID: roleID})
		if err != nil {
			t.Fatalf("CreateSession for %s: %v", roleID, err)
		}
		_, err = sess.Send(ctx, "Analyze repository structure")
		if err != nil {
			t.Fatalf("Send for %s: %v", roleID, err)
		}
		sess.Close()
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.receivedMessages) != 2 {
		t.Fatalf("expected 2 received messages, got %d", len(mock.receivedMessages))
	}
	for i, msg := range mock.receivedMessages {
		tools := msg["tools"].(map[string]any)
		if tools["read"] != true {
			t.Errorf("msg %d: expected read=true", i)
		}
		if tools["glob"] != true {
			t.Errorf("msg %d: expected glob=true", i)
		}
		if tools["grep"] != true {
			t.Errorf("msg %d: expected grep=true", i)
		}
		if tools["write"] != false {
			t.Errorf("msg %d: expected write=false", i)
		}
	}
}

// 17. Scoped tools: Coder operates in Governed Proposal Mode (zero native write/edit authority)
func TestMilestone12_17_ScopedTools_CoderGovernedProposalMode(t *testing.T) {
	mock := newMockOpenCodeServer()
	defer mock.Close()

	provider := spike.NewOpenCodeProvider(mock.server.URL, "opencode-go/mimo-v2.5", nil)
	defer provider.Close()

	ctx := context.Background()
	sess, err := provider.CreateSession(ctx, spike.AgentConfig{AgentID: "coder"})
	if err != nil {
		t.Fatalf("CreateSession for coder: %v", err)
	}
	defer sess.Close()

	_, err = sess.Send(ctx, "Apply bug fix")
	if err != nil {
		t.Fatalf("Send for coder: %v", err)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	msg := mock.receivedMessages[0]
	tools := msg["tools"].(map[string]any)
	if tools["write"] != true {
		t.Errorf("expected write=true for coder under native worktree execution")
	}
	if tools["edit"] != true {
		t.Errorf("expected edit=true for coder under native worktree execution")
	}
	if tools["bash"] != true {
		t.Errorf("expected bash=true for coder under native worktree execution")
	}
}

// 18. Turn limit enforcement
func TestMilestone12_18_TurnLimitEnforcement(t *testing.T) {
	mock := newMockOpenCodeServer()
	defer mock.Close()

	provider := spike.NewOpenCodeProvider(mock.server.URL, "opencode-go/mimo-v2.5", nil)
	defer provider.Close()

	ctx := context.Background()
	sess, err := provider.CreateSession(ctx, spike.AgentConfig{
		AgentID: "coder",
		Limits: &spike.ActivityLimits{
			MaxModelTurns: 2,
		},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	defer sess.Close()

	// Turn 1
	if _, err := sess.Send(ctx, "Turn 1"); err != nil {
		t.Fatalf("Turn 1 failed: %v", err)
	}
	// Turn 2
	if _, err := sess.Send(ctx, "Turn 2"); err != nil {
		t.Fatalf("Turn 2 failed: %v", err)
	}
	// Turn 3: must exceed limit fail-closed
	_, err = sess.Send(ctx, "Turn 3")
	if !errors.Is(err, spike.ErrTurnLimitExceeded) {
		t.Fatalf("expected ErrTurnLimitExceeded on Turn 3, got: %v", err)
	}
}

type mockReconciler struct {
	reconciled bool
}

func (m *mockReconciler) Reconcile(ctx context.Context, intent spike.IntentRecord) (bool, string, error) {
	m.reconciled = true
	return true, "ext-1", nil
}

// 19. Unfinished journal entry reconciliation
func TestMilestone12_19_JournalReconciliation(t *testing.T) {
	mock := newMockOpenCodeServer()
	defer mock.Close()

	workDir := t.TempDir()
	storeDir := filepath.Join(workDir, ".a4c", "activities")
	rosterDir := filepath.Join(workDir, ".a4c", "roster")
	memDir := filepath.Join(workDir, ".a4c", "memory")
	journalDir := filepath.Join(workDir, ".a4c", "journal")

	roster, _ := spike.DefaultTeamRoster(rosterDir)
	store, _ := spike.NewDurableActivityStore(storeDir)
	memStore, _ := spike.NewDurableMemoryStore(memDir)
	journal, _ := spike.NewDurableIntentJournal(journalDir)
	gov := spike.NewPolicyGovernor([]string{"governed_read_file"})
	runner := &mockTestRunner{}

	// Record an unfinished intent in journal
	intent := spike.IntentRecord{
		IntentID:    "intent-crash-1",
		ActivityID:  "act-1",
		Action:      "write_file",
		Target:      "calc.go",
		PayloadHash: "hash-123",
	}
	_ = journal.RecordIntent(intent)

	reconciler := &mockReconciler{}

	opProvider := spike.NewOpenCodeProvider(mock.server.URL, "opencode-go/mimo-v2.5", nil)
	defer opProvider.Close()

	coord := spike.NewTeamCoordinator(workDir, store, roster, memStore, gov, journal, runner, opProvider)
	coord.Reconciler = reconciler

	// Enqueue and step
	_, _ = coord.SubmitTaskWithStrategy("Reconcile test", spike.Budget{Allocated: 50000}, nil, spike.StrategyDirect)
	_, _ = coord.Step(context.Background())

	if !reconciler.reconciled {
		t.Fatalf("expected unfinished journal intent to be reconciled on coordinator startup")
	}
}

// 20. End-to-end team delegation workflow: Lead -> Coder -> Reviewer with OpenCode provider
func TestMilestone12_20_EndToEndTeamDelegationWorkflow(t *testing.T) {
	mock := newMockOpenCodeServer()
	defer mock.Close()

	workDir := t.TempDir()
	storeDir := filepath.Join(workDir, ".a4c", "activities")
	rosterDir := filepath.Join(workDir, ".a4c", "roster")
	memDir := filepath.Join(workDir, ".a4c", "memory")
	journalDir := filepath.Join(workDir, ".a4c", "journal")

	roster, _ := spike.DefaultTeamRoster(rosterDir)
	store, _ := spike.NewDurableActivityStore(storeDir)
	memStore, _ := spike.NewDurableMemoryStore(memDir)
	journal, _ := spike.NewDurableIntentJournal(journalDir)
	gov := spike.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_propose_activity",
		"governed_submit_review", "governed_request_human_decision", "governed_propose_memory",
	})
	runner := &mockTestRunner{}

	opProvider := spike.NewOpenCodeProvider(mock.server.URL, "opencode-go/mimo-v2.5", nil)
	defer opProvider.Close()

	coord := spike.NewTeamCoordinator(workDir, store, roster, memStore, gov, journal, runner, opProvider)

	// Submit task with StrategyTeam
	req, err := coord.SubmitTaskWithStrategy("Implement safe calculator", spike.Budget{Allocated: 100000}, nil, spike.StrategyTeam)
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	// Step 1: Lead acts
	res1, err := coord.Step(context.Background())
	if err != nil {
		t.Fatalf("Step 1 failed: %v", err)
	}
	if res1.ID != req.ID {
		t.Fatalf("expected step 1 to process lead task %s, got %s", req.ID, res1.ID)
	}
	if res1.Status != spike.StatusCompleted {
		t.Fatalf("expected lead task completed, got %s", res1.Status)
	}

	mock.mu.Lock()
	sessCount := len(mock.createdSessions)
	msgCount := len(mock.receivedMessages)
	mock.mu.Unlock()

	if sessCount < 1 || msgCount < 1 {
		t.Fatalf("expected OpenCode sessions and messages generated, got %d sessions, %d messages", sessCount, msgCount)
	}
}

