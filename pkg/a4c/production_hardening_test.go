package a4c_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ormasoftchile/a4c/pkg/a4c"
)

type mockGenericProvider struct{}

func (m *mockGenericProvider) CreateSession(ctx context.Context, config a4c.AgentConfig) (a4c.AgentSession, error) {
	return nil, errors.New("not implemented")
}

func (m *mockGenericProvider) ResumeSession(ctx context.Context, sessionID string, config a4c.AgentConfig) (a4c.AgentSession, error) {
	return nil, errors.New("not implemented")
}

func (m *mockGenericProvider) Capabilities() a4c.RuntimeCapabilities {
	return a4c.RuntimeCapabilities{
		NativeToolRestrictions: false,
		SessionResumption:      false,
		SessionCancellation:    true,
		UsageReporting:         false,
	}
}

// 1. Truthful capability reporting and explicit rejection
func TestProductionHardening_TruthfulCapabilities(t *testing.T) {
	// A. Nil provider
	nilCaps := a4c.GetHostCapability(nil, "missing")
	if nilCaps.Available {
		t.Errorf("nil provider must report Available: false")
	}
	err := a4c.RequireExecutionSupport(nilCaps)
	if !errors.Is(err, a4c.ErrHostUnavailable) {
		t.Errorf("expected ErrHostUnavailable for nil provider, got: %v", err)
	}

	// B. Generic provider without HostProvider interface
	genCaps := a4c.GetHostCapability(&mockGenericProvider{}, "generic")
	if genCaps.Available {
		t.Errorf("generic provider must report Available: false")
	}
	if genCaps.SessionInvocation || genCaps.WorktreeBinding || genCaps.ContextInjection {
		t.Errorf("generic provider must report unverified mandatory capabilities as false")
	}
	err = a4c.RequireExecutionSupport(genCaps)
	if !errors.Is(err, a4c.ErrHostUnavailable) {
		t.Errorf("expected ErrHostUnavailable for generic provider, got: %v", err)
	}

	// C. Unreachable OpenCode server
	offlineOp := a4c.NewOpenCodeProvider("http://127.0.0.1:59999", "opencode-go/mimo-v2.5", nil)
	opCaps := offlineOp.HostCapabilities()
	if opCaps.Available {
		t.Errorf("unreachable OpenCode server must report Available: false")
	}
	err = a4c.RequireExecutionSupport(opCaps)
	if !errors.Is(err, a4c.ErrHostUnavailable) {
		t.Errorf("expected ErrHostUnavailable for unreachable OpenCode, got: %v", err)
	}

	// D. Offline Fake provider
	fake := a4c.NewFakeProvider()
	fake.Available = false
	fakeCaps := fake.HostCapabilities()
	if fakeCaps.Available {
		t.Errorf("fake provider with Available=false must report Available: false")
	}
	err = a4c.RequireExecutionSupport(fakeCaps)
	if !errors.Is(err, a4c.ErrHostUnavailable) {
		t.Errorf("expected ErrHostUnavailable for offline fake, got: %v", err)
	}

	// E. Capability validation: Antigravity lacks programmatic tool restrictions in print mode
	agy, err := a4c.NewAntigravityProvider("gemini-3.8-flash-low")
	if err == nil {
		agyCaps := agy.HostCapabilities()
		if agyCaps.ToolRestrictions {
			t.Errorf("Antigravity print mode must report ToolRestrictions: false")
		}
		err = a4c.RequireToolRestrictions(agyCaps)
		if !errors.Is(err, a4c.ErrUnsupportedCapability) {
			t.Errorf("expected ErrUnsupportedCapability for Antigravity tool restrictions, got: %v", err)
		}
	}
}

// 2. Automatic multi-activity evidence resolution across task tree
func TestProductionHardening_AutomaticMultiActivityEvidenceResolution(t *testing.T) {
	h := setupInvisibleTeamHarness(t)
	ctx := context.Background()

	fake := a4c.NewFakeProvider()
	h.coordinator.RegisterProvider("fake", fake)
	h.coordinator.SetDefaultProvider(fake)

	// Coder implements code and passes tests
	fake.SessionResponseFunc = func(ctx context.Context, session *a4c.FakeSession, prompt string) (*a4c.AgentMessage, error) {
		agentID := session.Config().AgentID
		if agentID == "coder" {
			wtPath := session.Config().WorkspaceDir
			_ = os.WriteFile(filepath.Join(wtPath, "feature.go"), []byte("package main\nfunc Feature() string { return \"ok\" }\n"), 0644)
			return &a4c.AgentMessage{Content: "Feature implemented and verified"}, nil
		}
		if agentID == "reviewer" {
			if session.Config().OnSubmitReview != nil {
				session.Config().OnSubmitReview(a4c.ReviewResult{
					Approved:  true,
					Comments:  "Verified feature implementation",
					Timestamp: time.Now(),
				})
			}
			return &a4c.AgentMessage{Content: "Approved [REVIEW_APPROVED]"}, nil
		}
		return &a4c.AgentMessage{Content: "Completed"}, nil
	}

	// Enqueue root task (Lead)
	rootReq := a4c.ActivityRequest{
		ID:        "tree-task-root",
		AgentID:   "lead",
		Objective: "Build new feature",
		Budget:    a4c.Budget{Allocated: 50000},
		CreatedAt: time.Now(),
	}
	_ = h.store.Enqueue(rootReq)

	// Lead runs: completes with child proposals
	leadRes := a4c.ActivityResult{
		ID:     "tree-task-root",
		Status: a4c.StatusCompleted,
	}
	_ = h.store.Complete(rootReq.ID, leadRes)

	// Enqueue Coder subtask under root
	coderReq := a4c.ActivityRequest{
		ID:        "tree-task-coder",
		ParentID:  "tree-task-root",
		AgentID:   "coder",
		Objective: "Implement feature in feature.go",
		Budget:    a4c.Budget{Allocated: 30000},
		CreatedAt: time.Now(),
	}
	_ = h.store.Enqueue(coderReq)

	// Step 1: Coder executes, native test runner produces evidence
	coderResult, err := h.coordinator.Step(ctx)
	if err != nil {
		t.Fatalf("coder step failed: %v", err)
	}
	if coderResult == nil || len(coderResult.EvidenceIDs) == 0 {
		t.Fatalf("expected coder activity to produce test evidence")
	}

	// Now enqueue Reviewer subtask.
	// NOTE: In Team mode, Reviewer depends on root "tree-task-root", NOT directly on "tree-task-coder".
	reviewerReq := a4c.ActivityRequest{
		ID:        "tree-task-reviewer",
		ParentID:  "tree-task-root",
		AgentID:   "reviewer",
		Objective: "Review feature implementation",
		DependsOn: []string{"tree-task-root"}, // Points to parent, not direct sibling!
		Budget:    a4c.Budget{Allocated: 20000},
		CreatedAt: time.Now(),
	}
	_ = h.store.Enqueue(reviewerReq)

	// Reset in-memory lastEvidence to simulate fresh coordinator restart or memory clearing
	h.coordinator.ClearValidationEvidenceForTest()

	// Step 2: Reviewer executes. It must automatically discover evidence from child coder subtask in tree!
	revResult, err := h.coordinator.Step(ctx)
	if err != nil {
		t.Fatalf("reviewer step failed: %v", err)
	}
	if revResult == nil {
		t.Fatalf("reviewer step returned nil")
	}

	approved, _ := revResult.Outputs["approved"].(bool)
	comments, _ := revResult.Outputs["review_comments"].(string)

	if !approved {
		t.Fatalf("expected reviewer to approve via automatic tree evidence discovery, got rejected: %s", comments)
	}
}

// 3. Stale evidence rejection when files change after tests
func TestProductionHardening_StaleEvidenceRejection(t *testing.T) {
	h := setupInvisibleTeamHarness(t)
	ctx := context.Background()

	fake := a4c.NewFakeProvider()
	h.coordinator.RegisterProvider("fake", fake)
	h.coordinator.SetDefaultProvider(fake)

	fake.SessionResponseFunc = func(ctx context.Context, session *a4c.FakeSession, prompt string) (*a4c.AgentMessage, error) {
		agentID := session.Config().AgentID
		if agentID == "coder" {
			wtPath := session.Config().WorkspaceDir
			_ = os.WriteFile(filepath.Join(wtPath, "v1.go"), []byte("package main\nfunc V1() {}\n"), 0644)
			return &a4c.AgentMessage{Content: "Implemented v1"}, nil
		}
		if agentID == "reviewer" {
			if session.Config().OnSubmitReview != nil {
				session.Config().OnSubmitReview(a4c.ReviewResult{
					Approved:  true,
					Comments:  "LGTM",
					Timestamp: time.Now(),
				})
			}
			return &a4c.AgentMessage{Content: "Approved"}, nil
		}
		return &a4c.AgentMessage{Content: "Done"}, nil
	}

	coderReq := a4c.ActivityRequest{
		ID:        "stale-task-coder",
		AgentID:   "coder",
		Objective: "Implement V1",
		Budget:    a4c.Budget{Allocated: 30000},
		CreatedAt: time.Now(),
	}
	_ = h.store.Enqueue(coderReq)

	// Coder executes and generates test evidence
	_, err := h.coordinator.Step(ctx)
	if err != nil {
		t.Fatalf("coder step failed: %v", err)
	}

	// Mutate workspace after evidence collection!
	_ = os.WriteFile(filepath.Join(h.repoDir, "untested_mutation.go"), []byte("package main\n// unverified changes\n"), 0644)

	// Reviewer executes
	reviewerReq := a4c.ActivityRequest{
		ID:        "stale-task-reviewer",
		AgentID:   "reviewer",
		Objective: "Review changes",
		DependsOn: []string{"stale-task-coder"},
		Budget:    a4c.Budget{Allocated: 20000},
		CreatedAt: time.Now(),
	}
	_ = h.store.Enqueue(reviewerReq)

	revResult, err := h.coordinator.Step(ctx)
	if err != nil {
		t.Fatalf("reviewer step failed: %v", err)
	}

	approved, _ := revResult.Outputs["approved"].(bool)
	comments, _ := revResult.Outputs["review_comments"].(string)

	if approved {
		t.Fatalf("expected reviewer to reject stale evidence due to post-validation workspace modification")
	}
	if !strings.Contains(comments, "STALE") && !strings.Contains(comments, "modified") {
		t.Errorf("expected comments to indicate stale evidence, got: %s", comments)
	}
}

// 4. Reviewer mutation detection and automatic rollback
func TestProductionHardening_ReviewerMutationDetectionAndRollback(t *testing.T) {
	h := setupInvisibleTeamHarness(t)
	ctx := context.Background()

	fake := a4c.NewFakeProvider()
	h.coordinator.RegisterProvider("fake", fake)
	h.coordinator.SetDefaultProvider(fake)

	fake.SessionResponseFunc = func(ctx context.Context, session *a4c.FakeSession, prompt string) (*a4c.AgentMessage, error) {
		agentID := session.Config().AgentID
		if agentID == "coder" {
			wtPath := session.Config().WorkspaceDir
			_ = os.WriteFile(filepath.Join(wtPath, "valid.go"), []byte("package main\nfunc Valid() {}\n"), 0644)
			return &a4c.AgentMessage{Content: "Implemented valid.go"}, nil
		}
		if agentID == "reviewer" {
			// Reviewer improperly mutates a file in the workspace!
			wtPath := session.Config().WorkspaceDir
			if wtPath == "" {
				wtPath = h.repoDir
			}
			_ = os.WriteFile(filepath.Join(wtPath, "rogue_reviewer_file.go"), []byte("package main\n// unauthorized\n"), 0644)

			if session.Config().OnSubmitReview != nil {
				session.Config().OnSubmitReview(a4c.ReviewResult{
					Approved:  true,
					Comments:  "Approved with rogue changes",
					Timestamp: time.Now(),
				})
			}
			return &a4c.AgentMessage{Content: "Approved [REVIEW_APPROVED]"}, nil
		}
		return &a4c.AgentMessage{Content: "Done"}, nil
	}

	coderReq := a4c.ActivityRequest{
		ID:        "mut-task-coder",
		AgentID:   "coder",
		Objective: "Implement valid.go",
		Budget:    a4c.Budget{Allocated: 30000},
		CreatedAt: time.Now(),
	}
	_ = h.store.Enqueue(coderReq)

	_, err := h.coordinator.Step(ctx)
	if err != nil {
		t.Fatalf("coder step failed: %v", err)
	}

	reviewerReq := a4c.ActivityRequest{
		ID:        "mut-task-reviewer",
		AgentID:   "reviewer",
		Objective: "Review valid.go",
		DependsOn: []string{"mut-task-coder"},
		Budget:    a4c.Budget{Allocated: 20000},
		CreatedAt: time.Now(),
	}
	_ = h.store.Enqueue(reviewerReq)

	revResult, err := h.coordinator.Step(ctx)
	if err != nil {
		t.Fatalf("reviewer step failed: %v", err)
	}

	approved, _ := revResult.Outputs["approved"].(bool)
	mutDetected, _ := revResult.Outputs["reviewer_mutation_detected"].(bool)
	comments, _ := revResult.Outputs["review_comments"].(string)

	if approved {
		t.Fatalf("reviewer must NOT be approved when mutation is detected")
	}
	if !mutDetected {
		t.Fatalf("expected reviewer_mutation_detected to be true")
	}
	if !strings.Contains(comments, "rolled back") && !strings.Contains(comments, "mutation") {
		t.Errorf("expected comments to indicate rollback, got: %s", comments)
	}

	// Verify the rogue file was rolled back and deleted!
	rogueFile := filepath.Join(h.repoDir, "rogue_reviewer_file.go")
	if _, err := os.Stat(rogueFile); !os.IsNotExist(err) {
		t.Errorf("expected rogue file %s to be rolled back/deleted, but it exists!", rogueFile)
	}
}

// 5. JSON-deserialized agent_runtimes routing
func TestProductionHardening_JSONDeserializedAgentRuntimes(t *testing.T) {
	h := setupInvisibleTeamHarness(t)
	ctx := context.Background()

	hostAntigravity := a4c.NewFakeProvider()
	hostOpencode := a4c.NewFakeProvider()
	hostCopilot := a4c.NewFakeProvider()

	h.coordinator.RegisterProvider("antigravity", hostAntigravity)
	h.coordinator.RegisterProvider("opencode", hostOpencode)
	h.coordinator.RegisterProvider("copilot", hostCopilot)

	var coderRoutedHost, reviewerRoutedHost string

	hostOpencode.SessionResponseFunc = func(ctx context.Context, session *a4c.FakeSession, prompt string) (*a4c.AgentMessage, error) {
		coderRoutedHost = "opencode"
		wtPath := session.Config().WorkspaceDir
		_ = os.WriteFile(filepath.Join(wtPath, "routed.go"), []byte("package main\nfunc Routed() {}\n"), 0644)
		return &a4c.AgentMessage{Content: "Code written via opencode"}, nil
	}

	hostCopilot.SessionResponseFunc = func(ctx context.Context, session *a4c.FakeSession, prompt string) (*a4c.AgentMessage, error) {
		reviewerRoutedHost = "copilot"
		if session.Config().OnSubmitReview != nil {
			session.Config().OnSubmitReview(a4c.ReviewResult{
				Approved:  true,
				Comments:  "Verified via copilot",
				Timestamp: time.Now(),
			})
		}
		return &a4c.AgentMessage{Content: "Review approved via copilot [REVIEW_APPROVED]"}, nil
	}

	// Simulate Lead activity that has JSON-deserialized inputs (map[string]any instead of map[string]string)
	jsonDeserializedInputs := map[string]any{
		"agent_runtimes": map[string]any{
			"coder":    "opencode",
			"reviewer": "copilot",
		},
		"reviewer_runtime": "copilot",
	}

	// Root activity (Lead)
	rootReq := a4c.ActivityRequest{
		ID:        "json-team-root",
		AgentID:   "lead",
		Objective: "Develop component",
		Inputs:    jsonDeserializedInputs,
		Runtime:   "antigravity", // Lead runs on antigravity
		Strategy:  a4c.StrategyTeam,
		Budget:    a4c.Budget{Allocated: 50000},
		CreatedAt: time.Now(),
	}
	_ = h.store.Enqueue(rootReq)

	// Lead response proposing coder and reviewer
	leadSessionFunc := func(ctx context.Context, session *a4c.FakeSession, prompt string) (*a4c.AgentMessage, error) {
		return &a4c.AgentMessage{
			Content: `{"proposals": [
				{"proposal_id": "json-sub-coder", "proposed_agent_id": "coder", "objective": "Write code", "budget_tokens": 20000},
				{"proposal_id": "json-sub-reviewer", "proposed_agent_id": "reviewer", "objective": "Review code", "budget_tokens": 15000, "depends_on": ["json-sub-coder"]}
			]}`,
		}, nil
	}
	hostAntigravity.SessionResponseFunc = leadSessionFunc

	// Step 1: Lead runs on default fake provider
	_, err := h.coordinator.Step(ctx)
	if err != nil {
		t.Fatalf("Lead step failed: %v", err)
	}

	// Step 2: Coder runs. Must be routed to hostOpencode!
	_, err = h.coordinator.Step(ctx)
	if err != nil {
		t.Fatalf("Coder step failed: %v", err)
	}

	if coderRoutedHost != "opencode" {
		t.Errorf("expected coder to be routed to opencode, got: %q", coderRoutedHost)
	}

	// Step 3: Reviewer runs. Must be routed to hostCopilot!
	_, err = h.coordinator.Step(ctx)
	if err != nil {
		t.Fatalf("Reviewer step failed: %v", err)
	}

	if reviewerRoutedHost != "copilot" {
		t.Errorf("expected reviewer to be routed to copilot, got: %q", reviewerRoutedHost)
	}
}
