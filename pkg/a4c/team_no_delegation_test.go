package a4c_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	spike "github.com/ormasoftchile/a4c/pkg/a4c"
)

type noDelegationTestRunner struct{}

func (s *noDelegationTestRunner) RunTests(ctx context.Context, workspaceDir string) (int, string, error) {
	return 0, "ok", nil
}

func newNoDelegationCoordinator(t *testing.T, fake *spike.FakeProvider) (
	*spike.TeamCoordinator, *spike.DurableActivityStore, *spike.PolicyGovernor) {
	t.Helper()
	dir := t.TempDir()
	store, err := spike.NewDurableActivityStore(filepath.Join(dir, "activities"))
	if err != nil {
		t.Fatal(err)
	}
	memory, err := spike.NewDurableMemoryStore(filepath.Join(dir, "memory"))
	if err != nil {
		t.Fatal(err)
	}
	roster, err := spike.DefaultTeamRoster(filepath.Join(dir, "roster"))
	if err != nil {
		t.Fatal(err)
	}
	gov := spike.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_propose_activity",
		"governed_submit_review", "governed_request_human_decision", "governed_propose_memory",
	})
	journal, err := spike.NewDurableIntentJournal(filepath.Join(dir, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	coord := spike.NewTeamCoordinator(dir, store, roster, memory, gov, journal, &noDelegationTestRunner{}, fake)
	return coord, store, gov
}

// TestTeam_ZeroProposalGuard verifies that a lead which ends its turn without
// proposing any child activities is FAILED with failure_reason=no_delegation,
// not silently marked completed.
//
// Regression context (large-repo benchmark 2026-10-15): the lead burned its
// 10-call tool budget exploring a 1,400-file codebase, governed_propose_activity
// was then rejected by the tool-call cap, the model wrote "delegation tools are
// failing" and ended the turn successfully, and the coordinator marked the lead
// completed with zero children — task landed in awaiting_human_review with no
// implementation.
func TestTeam_ZeroProposalGuard(t *testing.T) {
	fake := spike.NewFakeProvider()
	coord, store, _ := newNoDelegationCoordinator(t, fake)

	fake.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		// Lead ends the turn claiming tools failed, never calling propose.
		return &spike.AgentMessage{
			Content: "**Blocked:** I inspected the surfaces, but the required delegation tools are failing, so I can't hand this to the coder.",
		}, nil
	}

	_, err := coord.SubmitTask("Fix the cypher WHERE parser", spike.Budget{Allocated: 500000}, nil)
	if err != nil {
		t.Fatalf("submit task: %v", err)
	}

	result, stepErr := coord.Step(context.Background())
	if stepErr == nil {
		t.Fatalf("expected error when lead proposes zero children, got result status=%v", result.Status)
	}
	if !errors.Is(stepErr, errNoDelegationSentinel()) && !strings.Contains(stepErr.Error(), "without proposing") {
		t.Errorf("expected no-delegation error, got: %v", stepErr)
	}
	if result == nil {
		t.Fatalf("expected a failed activity result with diagnostics, got nil")
	}
	if result.Status != spike.StatusFailed {
		t.Errorf("expected status failed, got %v", result.Status)
	}
	if reason, _ := result.Outputs["failure_reason"].(string); reason != "no_delegation" {
		t.Errorf("expected failure_reason=no_delegation, got %q", reason)
	}
	if summary, _ := result.Outputs["lead_summary"].(string); !strings.Contains(summary, "delegation tools") {
		t.Errorf("expected lead_summary preserved in outputs, got %q", summary)
	}
	if guidance, _ := result.Outputs["guidance"].(string); guidance == "" {
		t.Error("expected actionable guidance in outputs")
	}

	// Store must reflect the failed terminal state, not completed.
	rec, getErr := store.Get(result.ID)
	if getErr != nil || rec == nil || rec.Status != spike.StatusFailed {
		t.Errorf("store record should be failed, got %+v (err=%v)", rec, getErr)
	}
}

// errNoDelegationSentinel is a helper so the test can assert on the error
// text without exporting a new sentinel from the package.
func errNoDelegationSentinel() error { return errors.New("no_delegation") }

// TestTeam_LeadWithProposalCompletes is the positive control: a lead that
// proposes a coder child completes and the child is enqueued.
func TestTeam_LeadWithProposalCompletes(t *testing.T) {
	fake := spike.NewFakeProvider()
	coord, _, _ := newNoDelegationCoordinator(t, fake)

	fake.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		agentID := session.Config().AgentID
		if agentID == "lead" {
			if session.Config().OnProposeActivity != nil {
				session.Config().OnProposeActivity(spike.ActivityProposal{
					ProposalID:      "lead-sub-1",
					ProposedAgentID: "coder",
					Objective:       "Implement the RHS property comparison in cypher.c",
					RequestedBudget: spike.Budget{Allocated: 80000},
				})
			}
			return &spike.AgentMessage{Content: "Delegated to coder with a precise handoff."}, nil
		}
		return &spike.AgentMessage{Content: "Done"}, nil
	}

	_, err := coord.SubmitTask("Fix the cypher WHERE parser", spike.Budget{Allocated: 500000}, nil)
	if err != nil {
		t.Fatalf("submit task: %v", err)
	}

	result, stepErr := coord.Step(context.Background())
	if stepErr != nil {
		t.Fatalf("lead with proposal should complete: %v", stepErr)
	}
	if result.Status != spike.StatusCompleted {
		t.Errorf("expected lead completed, got %v", result.Status)
	}
	if n, ok := result.Outputs["proposals_recorded"].(int); !ok || n != 1 {
		t.Errorf("expected proposals_recorded=1, got %v", result.Outputs["proposals_recorded"])
	}
	if len(result.Proposals) != 1 {
		t.Errorf("expected 1 proposal on result, got %d", len(result.Proposals))
	}
}

// TestTeam_LeadToolAssert verifies that a lead session whose effective
// capability profile lacks governed_propose_activity is rejected before the
// session starts, rather than letting the model discover the missing tool
// after burning exploration turns.
func TestTeam_LeadToolAssert(t *testing.T) {
	fake := spike.NewFakeProvider()
	coord, _, _ := newNoDelegationCoordinator(t, fake)

	// Replace the architecture-investigation profile with one that cannot
	// delegate — simulates a misconfigured lead.
	coord.Capabilities.RegisterProfile(spike.CapabilityProfile{
		ID:          spike.ProfileArchitectureInvestigation,
		Name:        "Architecture Investigation (broken)",
		AllowedTools: []string{
			"governed_read_file", // no propose_activity
		},
	})

	fake.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		t.Error("session should not be created for a lead without propose_activity")
		return &spike.AgentMessage{Content: "should not run"}, nil
	}

	_, err := coord.SubmitTask("Fix the cypher WHERE parser", spike.Budget{Allocated: 500000}, nil)
	if err != nil {
		t.Fatalf("submit task: %v", err)
	}

	result, stepErr := coord.Step(context.Background())
	if stepErr == nil {
		t.Fatalf("expected error when lead lacks propose_activity, got status=%v", result.Status)
	}
	if !strings.Contains(stepErr.Error(), "missing governed_propose_activity") {
		t.Errorf("expected tool-availability error, got: %v", stepErr)
	}
}
