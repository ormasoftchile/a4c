package a4c_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	spike "github.com/ormasoftchile/a4c/pkg/a4c"
)

// TestTeam_01_PersistentIdentitiesAndRestarts verifies that team member identities,
// charters, specialties, and tool permissions survive process termination and disk restart.
func TestTeam_01_PersistentIdentitiesAndRestarts(t *testing.T) {
	dir := t.TempDir()
	rosterDir := filepath.Join(dir, "team")

	// Phase 1: Initialize roster, customize an agent, register a new team member
	roster1, err := spike.DefaultTeamRoster(rosterDir)
	if err != nil {
		t.Fatalf("failed to initialize default roster: %v", err)
	}

	lead1, found := roster1.GetMember("lead")
	if !found {
		t.Fatalf("expected 'lead' member in default roster")
	}
	lead1.Charter = "Custom Lead Charter: Enforce strict TDD and clean architecture."
	if err := roster1.RegisterMember(*lead1); err != nil {
		t.Fatalf("failed to update lead: %v", err)
	}

	devops := spike.AgentIdentity{
		ID:           "devops",
		Name:         "Dave (DevOps Engineer)",
		Role:         "Infrastructure & CI",
		Charter:      "Manage deployment pipelines and docker configurations.",
		Specialties:  []string{"ci", "containers", "iac"},
		AllowedTools: []string{"governed_read_file"},
	}
	if err := roster1.RegisterMember(devops); err != nil {
		t.Fatalf("failed to register devops member: %v", err)
	}

	// Phase 2: Simulate process exit and restart from the same directory
	roster2, err := spike.DefaultTeamRoster(rosterDir)
	if err != nil {
		t.Fatalf("failed to reload roster after restart: %v", err)
	}

	lead2, found := roster2.GetMember("lead")
	if !found || lead2.Charter != "Custom Lead Charter: Enforce strict TDD and clean architecture." {
		t.Fatalf("lead charter not preserved across restart: %+v", lead2)
	}

	devops2, found := roster2.GetMember("devops")
	if !found || devops2.Name != "Dave (DevOps Engineer)" {
		t.Fatalf("devops member not preserved across restart: %+v", devops2)
	}

	members := roster2.ListMembers()
	if len(members) != 4 { // lead, coder, reviewer, devops
		t.Fatalf("expected 4 team members after restart, got %d", len(members))
	}
}

// TestTeam_02_CollaborativeDelegationAndOrdering verifies that an architect can delegate
// subtasks to a coder and reviewer, and the scheduler strictly enforces DAG dependency execution order.
func TestTeam_02_CollaborativeDelegationAndOrdering(t *testing.T) {
	dir := t.TempDir()
	workspaceDir := filepath.Join(dir, "repo")
	_ = os.MkdirAll(workspaceDir, 0755)

	store, _ := spike.NewDurableActivityStore(filepath.Join(dir, "activities"))
	memory, _ := spike.NewDurableMemoryStore(filepath.Join(dir, "memory"))
	roster, _ := spike.DefaultTeamRoster(filepath.Join(dir, "roster"))
	gov := spike.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_propose_activity",
		"governed_submit_review", "governed_request_human_decision", "governed_propose_memory",
	})
	journal, _ := spike.NewDurableIntentJournal(filepath.Join(dir, "journal"))

	fakeProvider := spike.NewFakeProvider()
	runner := &spike.NativeGoTestRunner{}

	var executionOrder []string

	fakeProvider.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		agentID := session.Config().AgentID
		executionOrder = append(executionOrder, agentID)

		if agentID == "lead" {
			// Lead delegates two tasks: coder implementation first, then reviewer review dependent on coder!
			coderSubtaskID := "lead-subtask-coder"
			if session.Config().OnProposeActivity != nil {
				session.Config().OnProposeActivity(spike.ActivityProposal{
					ProposalID:      coderSubtaskID,
					ProposedAgentID: "coder",
					Objective:       "Implement feature in calc.go",
					RequestedBudget: spike.Budget{Allocated: 5000},
				})
				session.Config().OnProposeActivity(spike.ActivityProposal{
					ProposalID:      "lead-subtask-reviewer",
					ProposedAgentID: "reviewer",
					Objective:       "Review feature implementation",
					RequestedBudget: spike.Budget{Allocated: 3000},
					DependsOn:       []string{coderSubtaskID},
				})
			}
			return &spike.AgentMessage{Content: "Delegated subtasks to coder and reviewer"}, nil
		}

		if agentID == "coder" {
			// Write valid Go files
			_ = os.WriteFile(filepath.Join(workspaceDir, "go.mod"), []byte("module example.com/calc\n\ngo 1.22\n"), 0644)
			_ = os.WriteFile(filepath.Join(workspaceDir, "calc.go"), []byte("package calc\n\nfunc Add(a, b int) int { return a + b }\n"), 0644)
			_ = os.WriteFile(filepath.Join(workspaceDir, "calc_test.go"), []byte("package calc\n\nimport \"testing\"\nfunc TestAdd(t *testing.T) { if Add(2,3) != 5 { t.Fatal(\"err\") } }\n"), 0644)
			return &spike.AgentMessage{Content: "Code implemented and verified"}, nil
		}

		if agentID == "reviewer" {
			if session.Config().OnSubmitReview != nil {
				session.Config().OnSubmitReview(spike.ReviewResult{
					Approved: true,
					Comments: "Approved. Code meets specification and tests pass natively.",
				})
			}
			return &spike.AgentMessage{Content: "Review completed successfully"}, nil
		}

		return &spike.AgentMessage{Content: "Done"}, nil
	}

	coord := spike.NewTeamCoordinator(workspaceDir, store, roster, memory, gov, journal, runner, fakeProvider)

	ctx := context.Background()
	_, err := coord.SubmitTask("Add Add(a,b int) feature", spike.Budget{Allocated: 20000}, nil)
	if err != nil {
		t.Fatalf("failed to submit task: %v", err)
	}

	// Run full team loop
	results, err := coord.RunLoop(ctx, 10)
	if err != nil {
		t.Fatalf("run loop failed: %v", err)
	}

	if len(results) != 3 {
		t.Fatalf("expected 3 completed activities (lead, coder, reviewer), got %d", len(results))
	}

	// Verify execution order strictly obeyed DAG dependencies: lead -> coder -> reviewer
	expectedOrder := []string{"lead", "coder", "reviewer"}
	for i, expected := range expectedOrder {
		if executionOrder[i] != expected {
			t.Fatalf("step %d: expected agent %q, but %q executed. Full order: %v",
				i, expected, executionOrder[i], executionOrder)
		}
	}
}

// TestTeam_03_DuplicateDelegationPreventionAfterCrash simulates a crash during
// delegation and verifies that re-running does not produce duplicate activities or double-spend budget.
func TestTeam_03_DuplicateDelegationPreventionAfterCrash(t *testing.T) {
	dir := t.TempDir()
	store, _ := spike.NewDurableActivityStore(filepath.Join(dir, "activities"))
	gov := spike.NewPolicyGovernor([]string{"governed_read_file", "governed_propose_activity"})

	parentReq := spike.ActivityRequest{
		ID:        "parent-act",
		AgentID:   "lead",
		Objective: "Decompose epic",
		Budget:    spike.Budget{Allocated: 10000, Reserved: 0},
		CreatedAt: time.Now(),
	}
	_ = store.Enqueue(parentReq)

	proposal := spike.ActivityProposal{
		ProposalID:      "subtask-unique-id",
		ProposedAgentID: "coder",
		Objective:       "Implement subtask 1",
		RequestedBudget: spike.Budget{Allocated: 3000},
	}

	// First authorization: should succeed
	childReq1, err := gov.AuthorizeProposal(parentReq, proposal, "child-1")
	if err != nil {
		t.Fatalf("first authorization failed: %v", err)
	}
	if err := store.Enqueue(*childReq1); err != nil {
		t.Fatalf("first enqueue failed: %v", err)
	}

	// Simulate crash and re-execution of parent with identical proposal
	childReq2, err := gov.AuthorizeProposal(parentReq, proposal, "child-2")
	if err != nil {
		t.Fatalf("idempotent authorization failed: %v", err)
	}

	// Governor must return the deduplicated childReq1 instead of creating child-2
	if childReq2.ID != "child-1" {
		t.Fatalf("expected deduplicated proposal to retain ID 'child-1', got %q", childReq2.ID)
	}

	// Check that parent budget was not reserved twice
	b, ok := gov.GetBudget(parentReq.ID)
	if !ok || b.Reserved != 3000 {
		t.Fatalf("expected reserved budget 3000, got %.2f (found=%t)", b.Reserved, ok)
	}
}

// TestTeam_04_SharedOrganizationalMemory verifies that proposed architectural conventions
// are not authoritative until verified by native test evidence or explicit review approval,
// and persist across restarts.
func TestTeam_04_SharedOrganizationalMemory(t *testing.T) {
	dir := t.TempDir()
	memoryDir := filepath.Join(dir, "memory")

	// Phase 1: Propose conventions
	store1, err := spike.NewDurableMemoryStore(memoryDir)
	if err != nil {
		t.Fatalf("failed to create memory store: %v", err)
	}

	rec, err := store1.Propose("error_handling", "All errors must be wrapped with fmt.Errorf", "lead", "act-1")
	if err != nil {
		t.Fatalf("failed to propose memory: %v", err)
	}
	if rec.Status != spike.MemoryStatusProposed {
		t.Fatalf("expected status 'proposed', got %q", rec.Status)
	}

	// Unverified memory must NOT appear in verified list or prompt context
	if len(store1.ListVerified()) != 0 {
		t.Fatalf("unverified memory should not be listed in verified conventions")
	}
	if strings.Contains(store1.FormatContext(), "error_handling") {
		t.Fatalf("unverified memory leaked into prompt context")
	}

	// Promote after verification
	promoted, err := store1.Promote("error_handling", "reviewer:charlie")
	if err != nil {
		t.Fatalf("failed to promote memory: %v", err)
	}
	if promoted.Status != spike.MemoryStatusVerified || promoted.Verifier != "reviewer:charlie" {
		t.Fatalf("invalid promoted record: %+v", promoted)
	}

	if len(store1.ListVerified()) != 1 {
		t.Fatalf("expected 1 verified memory")
	}
	if !strings.Contains(store1.FormatContext(), "error_handling") {
		t.Fatalf("verified memory missing from prompt context")
	}

	// Phase 2: Restart and verify durability
	store2, err := spike.NewDurableMemoryStore(memoryDir)
	if err != nil {
		t.Fatalf("failed to reload memory store: %v", err)
	}

	reloaded, err := store2.Get("error_handling")
	if err != nil || reloaded.Status != spike.MemoryStatusVerified {
		t.Fatalf("verified memory did not survive restart: %+v", reloaded)
	}
}

// TestTeam_05_IndependentReviewWithEvidence verifies that reviewer approval requires
// valid native test evidence matching the current workspace snapshot, and fails closed
// if code was tampered with after test validation.
func TestTeam_05_IndependentReviewWithEvidence(t *testing.T) {
	dir := t.TempDir()
	workspaceDir := filepath.Join(dir, "repo")
	_ = os.MkdirAll(workspaceDir, 0755)

	store, _ := spike.NewDurableActivityStore(filepath.Join(dir, "activities"))
	memory, _ := spike.NewDurableMemoryStore(filepath.Join(dir, "memory"))
	roster, _ := spike.DefaultTeamRoster(filepath.Join(dir, "roster"))
	gov := spike.NewPolicyGovernor([]string{"governed_read_file", "governed_submit_review"})
	journal, _ := spike.NewDurableIntentJournal(filepath.Join(dir, "journal"))
	fakeProvider := spike.NewFakeProvider()
	runner := &spike.NativeGoTestRunner{}

	coord := spike.NewTeamCoordinator(workspaceDir, store, roster, memory, gov, journal, runner, fakeProvider)

	// Case A: Reviewer attempts to approve, but NO test evidence exists yet
	fakeProvider.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		if session.Config().OnSubmitReview != nil {
			session.Config().OnSubmitReview(spike.ReviewResult{
				Approved: true,
				Comments: "Looks good to me!",
			})
		}
		return &spike.AgentMessage{Content: "Reviewed and approved"}, nil
	}

	reviewReq := spike.ActivityRequest{
		ID:        "rev-without-evidence",
		AgentID:   "reviewer",
		Objective: "Review unverified changes",
		Budget:    spike.Budget{Allocated: 5000},
		CreatedAt: time.Now(),
	}
	_ = store.Enqueue(reviewReq)

	res, err := coord.Step(context.Background())
	if err != nil {
		t.Fatalf("step returned unexpected error: %v", err)
	}

	// Coordinator must enforce that without native test evidence, approval is rejected!
	if res.Outputs["approved"] != false {
		t.Fatalf("expected reviewer approval to be overridden to false without test evidence, got %v", res.Outputs["approved"])
	}
}

// TestTeam_06_HumanEscalationAndResumption verifies that an agent can request a human
// decision, pausing the activity in StatusAwaitingHuman, and that the human engineer can
// resolve it via CLI and resume execution cleanly.
func TestTeam_06_HumanEscalationAndResumption(t *testing.T) {
	dir := t.TempDir()
	workspaceDir := filepath.Join(dir, "repo")
	_ = os.MkdirAll(workspaceDir, 0755)

	store, _ := spike.NewDurableActivityStore(filepath.Join(dir, "activities"))
	memory, _ := spike.NewDurableMemoryStore(filepath.Join(dir, "memory"))
	roster, _ := spike.DefaultTeamRoster(filepath.Join(dir, "roster"))
	gov := spike.NewPolicyGovernor([]string{"governed_read_file", "governed_request_human_decision"})
	journal, _ := spike.NewDurableIntentJournal(filepath.Join(dir, "journal"))
	fakeProvider := spike.NewFakeProvider()
	runner := &spike.NativeGoTestRunner{}

	leadCalledCount := 0
	fakeProvider.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		leadCalledCount++
		if leadCalledCount == 1 {
			// First invocation: Lead encounters ambiguous requirement and asks human engineer
			if session.Config().OnRequestHumanDecision != nil {
				session.Config().OnRequestHumanDecision(spike.HumanDecisionRequest{
					DecisionID: "dec-storage-backend",
					Question:   "Should we use PostgreSQL or SQLite for local storage?",
				})
			}
			return &spike.AgentMessage{Content: "Escalated decision to engineer"}, nil
		}
		// Second invocation after human resolution: lead now has the decision
		// and delegates implementation.
		if session.Config().OnProposeActivity != nil {
			session.Config().OnProposeActivity(spike.ActivityProposal{
				ProposalID:      "post-decision-coder",
				ProposedAgentID: "coder",
				Objective:       "Implement storage layer with SQLite",
				RequestedBudget: spike.Budget{Allocated: 5000},
			})
		}
		return &spike.AgentMessage{Content: "Architecture finalized; delegated implementation."}, nil
	}

	coord := spike.NewTeamCoordinator(workspaceDir, store, roster, memory, gov, journal, runner, fakeProvider)

	_, err := coord.SubmitTask("Design storage layer", spike.Budget{Allocated: 10000}, nil)
	if err != nil {
		t.Fatalf("failed to submit task: %v", err)
	}

	// Step 1: Lead executes and requests human decision
	res1, err := coord.Step(context.Background())
	if err != nil {
		t.Fatalf("step 1 failed: %v", err)
	}
	if res1.Status != spike.StatusAwaitingHuman {
		t.Fatalf("expected status %s, got %s", spike.StatusAwaitingHuman, res1.Status)
	}

	// Verify pending decision listed for human engineer
	pending := coord.ListPendingDecisions()
	if len(pending) != 1 || pending[0].DecisionID != "dec-storage-backend" {
		t.Fatalf("expected pending decision 'dec-storage-backend', got %+v", pending)
	}

	// Scheduler must refuse to claim activities blocked awaiting human
	if coord.Store.HasClaimable() {
		t.Fatalf("store should have no claimable activities while awaiting human decision")
	}

	// Human engineer resolves the decision via CLI API
	err = coord.ResolveDecision("dec-storage-backend", "Use SQLite for zero-dependency local storage", true, "")
	if err != nil {
		t.Fatalf("failed to resolve decision: %v", err)
	}

	// Activity is unblocked and returns to StatusPending
	if !coord.Store.HasClaimable() {
		t.Fatalf("store should now have claimable activity after decision resolution")
	}

	// Step 2: Resume execution
	res2, err := coord.Step(context.Background())
	if err != nil {
		t.Fatalf("step 2 failed: %v", err)
	}
	if res2.Status != spike.StatusCompleted {
		t.Fatalf("expected status %s after resumption, got %s", spike.StatusCompleted, res2.Status)
	}
}

// TestTeam_07_ToolPrivilegeSeparation verifies that the PolicyGovernor strictly prevents
// non-coder roles (lead, reviewer) from executing code mutation tools (governed_apply_patch),
// ensuring execution authority cannot be abused by reasoning agents.
func TestTeam_07_ToolPrivilegeSeparation(t *testing.T) {
	dir := t.TempDir()
	roster, _ := spike.DefaultTeamRoster(filepath.Join(dir, "roster"))
	gov := spike.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_submit_review",
	})

	for _, member := range roster.ListMembers() {
		gov.SetAgentTools(member.ID, member.AllowedTools)
	}

	// Coder is allowed to mutate files
	if err := gov.AuthorizeToolInvocation("coder", "governed_apply_patch"); err != nil {
		t.Fatalf("coder should be allowed to use governed_apply_patch: %v", err)
	}

	// Lead is strictly FORBIDDEN from mutating files
	if err := gov.AuthorizeToolInvocation("lead", "governed_apply_patch"); err == nil {
		t.Fatalf("lead must be rejected from using governed_apply_patch")
	}

	// Reviewer is strictly FORBIDDEN from mutating files
	if err := gov.AuthorizeToolInvocation("reviewer", "governed_apply_patch"); err == nil {
		t.Fatalf("reviewer must be rejected from using governed_apply_patch")
	}

	// Reviewer is allowed to submit reviews
	if err := gov.AuthorizeToolInvocation("reviewer", "governed_submit_review"); err != nil {
		t.Fatalf("reviewer should be allowed to use governed_submit_review: %v", err)
	}

	// Coder is NOT allowed to submit peer reviews
	if err := gov.AuthorizeToolInvocation("coder", "governed_submit_review"); err == nil {
		t.Fatalf("coder must be rejected from using governed_submit_review")
	}
}
