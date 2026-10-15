package a4c_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	spike "github.com/ormasoftchile/a4c/pkg/a4c"
)

// TestMilestone8_01_CapabilityRegistryAndProfileResolution tests declarative capability profiles.
func TestMilestone8_01_CapabilityRegistryAndProfileResolution(t *testing.T) {
	reg := spike.NewCapabilityRegistry()
	gov := spike.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_propose_activity",
		"governed_submit_review", "governed_request_human_decision", "governed_propose_memory",
	})

	// Test Architecture Investigation profile
	leadProf := reg.ResolveProfile("lead", "Lead Software Architect", "Investigate dependency graph", nil, gov)
	if leadProf.ID != spike.ProfileArchitectureInvestigation {
		t.Fatalf("expected profile %q, got %q", spike.ProfileArchitectureInvestigation, leadProf.ID)
	}
	hasTool := func(tools []string, target string) bool {
		for _, tool := range tools {
			if tool == target {
				return true
			}
		}
		return false
	}
	if !hasTool(leadProf.AllowedTools, "governed_propose_activity") {
		t.Errorf("lead profile missing governed_propose_activity")
	}
	if hasTool(leadProf.AllowedTools, "governed_apply_patch") {
		t.Errorf("lead profile should NOT allow governed_apply_patch")
	}

	// Test Code Implementation profile
	coderProf := reg.ResolveProfile("coder", "Software Engineer", "Implement feature fix", nil, gov)
	if coderProf.ID != spike.ProfileCodeImplementation {
		t.Fatalf("expected profile %q, got %q", spike.ProfileCodeImplementation, coderProf.ID)
	}
	if !hasTool(coderProf.AllowedTools, "governed_apply_patch") {
		t.Errorf("coder profile missing governed_apply_patch")
	}
	if hasTool(coderProf.AllowedTools, "governed_submit_review") {
		t.Errorf("coder profile should NOT allow governed_submit_review")
	}

	// Test Independent Review profile
	revProf := reg.ResolveProfile("reviewer", "Independent Reviewer", "Verify implementation diff", nil, gov)
	if revProf.ID != spike.ProfileIndependentReview {
		t.Fatalf("expected profile %q, got %q", spike.ProfileIndependentReview, revProf.ID)
	}
	if !hasTool(revProf.AllowedTools, "governed_submit_review") {
		t.Errorf("reviewer profile missing governed_submit_review")
	}
	if hasTool(revProf.AllowedTools, "governed_apply_patch") {
		t.Errorf("reviewer profile must strictly prohibit governed_apply_patch")
	}

	// Test Governor restriction overrides
	strictGov := spike.NewPolicyGovernor([]string{"governed_read_file"})
	restrictedProf := reg.ResolveProfile("lead", "Lead", "Investigate", nil, strictGov)
	if len(restrictedProf.AllowedTools) != 1 || restrictedProf.AllowedTools[0] != "governed_read_file" {
		t.Errorf("expected strictly filtered tools under restrictive governor, got %v", restrictedProf.AllowedTools)
	}
}

type mockTestRunner struct{}

func (m *mockTestRunner) RunTests(ctx context.Context, dir string) (int, string, error) {
	return 0, "PASS\nok\n", nil
}

// TestMilestone8_03_CoordinatorCapabilityEnforcementAndHandoff verifies that TeamCoordinator
// injects capability profiles into the agent loop.
func TestMilestone8_03_CoordinatorCapabilityEnforcementAndHandoff(t *testing.T) {
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "activities")
	rosterDir := filepath.Join(dir, "roster")
	memDir := filepath.Join(dir, "memory")
	journalDir := filepath.Join(dir, "journal")

	store, _ := spike.NewDurableActivityStore(storeDir)
	roster, _ := spike.DefaultTeamRoster(rosterDir)
	memory, _ := spike.NewDurableMemoryStore(memDir)
	journal, _ := spike.NewDurableIntentJournal(journalDir)

	gov := spike.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_propose_activity",
		"governed_submit_review", "governed_request_human_decision", "governed_propose_memory",
	})

	fakeProv := spike.NewFakeProvider()
	var capturedCoderTools []string
	var capturedReviewerTools []string

	fakeProv.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		agentID := session.Config().AgentID
		switch agentID {
		case "lead":
			// Lead publishes an activity proposal to coder and reviewer
			// (budgets at viable scale for a 100k parent task).
			session.Config().OnProposeActivity(spike.ActivityProposal{
				ProposalID:      "sub-coder-1",
				ProposedAgentID: "coder",
				Objective:       "Implement diagnostic inspector",
				RequestedBudget: spike.Budget{Allocated: 40000},
			})
			session.Config().OnProposeActivity(spike.ActivityProposal{
				ProposalID:      "sub-reviewer-1",
				ProposedAgentID: "reviewer",
				Objective:       "Verify diagnostics output natively",
				RequestedBudget: spike.Budget{Allocated: 30000},
				DependsOn:       []string{"sub-coder-1"},
			})
		case "coder":
			capturedCoderTools = session.Config().AllowedTools
		case "reviewer":
			capturedReviewerTools = session.Config().AllowedTools
			session.Config().OnSubmitReview(spike.ReviewResult{
				Approved:  true,
				Comments:  "Diagnostics pass all verification checks",
				Timestamp: time.Now(),
			})
		}
		return &spike.AgentMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("[%s] done", agentID),
		}, nil
	}

	runner := &mockTestRunner{}
	coord := spike.NewTeamCoordinator(dir, store, roster, memory, gov, journal, runner, fakeProv)

	taskReq, err := coord.SubmitTask("Build diagnostics feature", spike.Budget{Allocated: 100000}, nil)
	if err != nil {
		t.Fatalf("task submission failed: %v", err)
	}

	ctx := context.Background()
	results, err := coord.RunLoop(ctx, 5)
	if err != nil {
		t.Fatalf("RunLoop failed: %v", err)
	}

	if len(results) < 3 {
		t.Fatalf("expected at least 3 completed activities (lead, coder, reviewer), got %d", len(results))
	}

	// 1. Verify capability scoping on Coder
	hasTool := func(tools []string, target string) bool {
		for _, tool := range tools {
			if tool == target {
				return true
			}
		}
		return false
	}
	if !hasTool(capturedCoderTools, "governed_apply_patch") {
		t.Errorf("coder was missing governed_apply_patch in allowed tools: %v", capturedCoderTools)
	}
	if hasTool(capturedCoderTools, "governed_submit_review") {
		t.Errorf("coder received governed_submit_review: %v", capturedCoderTools)
	}

	// 2. Verify capability scoping on Reviewer
	if !hasTool(capturedReviewerTools, "governed_submit_review") {
		t.Errorf("reviewer was missing governed_submit_review: %v", capturedReviewerTools)
	}
	if hasTool(capturedReviewerTools, "governed_apply_patch") {
		t.Errorf("reviewer received governed_apply_patch: %v", capturedReviewerTools)
	}

	// 3. Verify durable history record includes capability profile
	report, err := spike.BuildTaskHistory(taskReq.ID, store, journal, nil, memory)
	if err != nil {
		t.Fatalf("failed to build task history: %v", err)
	}
	for _, act := range report.Activities {
		if act.CapabilityProfile == nil {
			t.Errorf("activity %s missing recorded CapabilityProfile in history", act.ActivityID)
		}
	}
}

// TestMilestone8_04_CapabilityIsolationAndFailClosed verifies that capability limits
// cannot be bypassed by session reuse or ambient calls.
func TestMilestone8_04_CapabilityIsolationAndFailClosed(t *testing.T) {
	reg := spike.NewCapabilityRegistry()
	gov := spike.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_submit_review",
	})
	gov.SetAgentTools("coder", []string{"governed_read_file", "governed_apply_patch"})
	gov.SetAgentTools("reviewer", []string{"governed_read_file", "governed_submit_review"})

	// 1. Coder profile cannot invoke governed_submit_review
	coderProf := reg.ResolveProfile("coder", "Software Engineer", "Write code", nil, gov)
	for _, tool := range coderProf.AllowedTools {
		if tool == "governed_submit_review" {
			t.Fatalf("coder profile illegally contains governed_submit_review")
		}
	}
	if err := gov.AuthorizeToolInvocation("coder", "governed_submit_review"); err == nil {
		t.Fatalf("expected governor to reject coder invoking governed_submit_review")
	}

	// 2. Reviewer profile cannot invoke governed_apply_patch
	revProf := reg.ResolveProfile("reviewer", "Reviewer", "Audit diff", nil, gov)
	for _, tool := range revProf.AllowedTools {
		if tool == "governed_apply_patch" {
			t.Fatalf("reviewer profile illegally contains governed_apply_patch")
		}
	}
	if err := gov.AuthorizeToolInvocation("reviewer", "governed_apply_patch"); err == nil {
		t.Fatalf("expected governor to reject reviewer invoking governed_apply_patch")
	}

	// 3. Unauthorized ambient tool invocation fails closed
	if err := gov.AuthorizeToolInvocation("coder", "unregistered_dangerous_tool"); err == nil {
		t.Fatalf("expected governor to reject unregistered tool")
	}
}

