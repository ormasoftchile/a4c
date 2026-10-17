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
	// The lead proposed 1 coder; mandatory-review policy enforcement then
	// schedules an independent reviewer for the final candidate, so the result
	// carries both the lead's coder and the coordinator-scheduled reviewer.
	if len(result.Proposals) != 2 {
		t.Errorf("expected 2 proposals (coder + enforced reviewer), got %d", len(result.Proposals))
	}
	hasCoder, hasReviewer := false, false
	for _, p := range result.Proposals {
		if p.ProposedAgentID == "coder" {
			hasCoder = true
		}
		if p.ProposedAgentID == "reviewer" {
			hasReviewer = true
		}
	}
	if !hasCoder || !hasReviewer {
		t.Errorf("expected both a coder and an enforced reviewer proposal, got %+v", result.Proposals)
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

// TestBudgetSalvage_CompletesWhenTestsPassAfterOverrun verifies that a
// specialist whose session overran its allocation is COMPLETED (with an
// explicit budget_overrun marker) when native tests pass on the resulting
// workspace — instead of failing the activity and discarding verified work.
//
// Regression context (large-repo team re-run): coder allocated 180k, consumed
// 217k after producing a correct fix; the full-suite validation cost pushed
// it over; activity failed and reviewer never ran, even though the worktree
// held an acceptance-passing implementation.
func TestBudgetSalvage_CompletesWhenTestsPassAfterOverrun(t *testing.T) {
	fake := spike.NewFakeProvider()
	runner := &noDelegationTestRunner{} // returns exit 0 / "ok"
	coord, store, _ := newNoDelegationCoordinator(t, fake)
	// Use a runner that always passes (already the stub)
	_ = runner

	fake.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		switch session.Config().AgentID {
		case "lead":
			if session.Config().OnProposeActivity != nil {
				session.Config().OnProposeActivity(spike.ActivityProposal{
					ProposalID:      "coder-1",
					ProposedAgentID: "coder",
					Objective:       "Implement the cypher fix",
					RequestedBudget: spike.Budget{Allocated: 40000},
				})
			}
			return &spike.AgentMessage{Content: "Delegated to coder."}, nil
		case "coder":
			// Blow the allocation: FakeSession errors when TotalTokens > Budget.Allocated.
			// Lead proposes 40k; floor is 25k, so 40k stands. Use 50k total.
			session.SetUsage(spike.UsageInfo{
				PromptTokens: 42000, CompletionTokens: 8000,
				TotalTokens: 50000, BilledTokens: 50000,
			})
			return &spike.AgentMessage{Content: "Implemented the fix; native tests pass."}, nil
		}
		return &spike.AgentMessage{Content: "ok"}, nil
	}

	_, err := coord.SubmitTask("Fix cypher WHERE", spike.Budget{Allocated: 500000}, nil)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	ctx := context.Background()
	var coderRes *spike.ActivityResult
	for i := 0; i < 10; i++ {
		if !store.HasClaimable() {
			break
		}
		res, stepErr := coord.Step(ctx)
		if stepErr != nil {
			t.Fatalf("step %d: %v", i, stepErr)
		}
		if res == nil {
			break
		}
		if res.ID == "coder-1" || (res.Consumed.Consumed > 0 && coderRes == nil && res.Status == spike.StatusCompleted) {
			if rec, _ := store.Get(res.ID); rec != nil && rec.Request.AgentID == "coder" {
				coderRes = res
			}
		}
	}

	// Find coder record
	var coderRec *spike.ActivityRecord
	for _, rec := range store.ListAllActivities() {
		if rec.Request.AgentID == "coder" {
			coderRec = rec
			break
		}
	}
	if coderRec == nil {
		t.Fatal("coder activity not found")
	}
	if coderRec.Status != spike.StatusCompleted {
		t.Fatalf("expected coder completed via salvage, got %s (err: %s)", coderRec.Status, coderRec.Error)
	}
	if coderRec.Result == nil {
		t.Fatal("expected coder result recorded")
	}
	if ov, _ := coderRec.Result.Outputs["budget_overrun"].(bool); !ov {
		t.Errorf("expected budget_overrun=true in outputs, got %v", coderRec.Result.Outputs)
	}
	if coderRec.Result.Outputs["budget_overrun_note"] == nil {
		t.Error("expected budget_overrun_note in outputs")
	}
}

// TestBudgetReallocation_CoversObservedOverrun verifies the retry allocation
// is sized from observed consumption (consumed*1.5), not merely 2× the
// original — so a 217k first attempt is not retried with only 360k when the
// suite will re-run.
func TestBudgetReallocation_CoversObservedOverrun(t *testing.T) {
	fake := spike.NewFakeProvider()
	runner := &noDelegationTestRunner{}
	coord, store, _ := newNoDelegationCoordinator(t, fake)
	_ = runner

	coderSends := 0
	fake.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		switch session.Config().AgentID {
		case "lead":
			if session.Config().OnProposeActivity != nil {
				session.Config().OnProposeActivity(spike.ActivityProposal{
					ProposalID:      "coder-1",
					ProposedAgentID: "coder",
					Objective:       "Implement the cypher fix",
					RequestedBudget: spike.Budget{Allocated: 40000},
				})
			}
			return &spike.AgentMessage{Content: "Delegated."}, nil
		case "coder":
			coderSends++
			if coderSends == 1 {
				// First attempt: overruns allocation (40k proposed → 40k used).
				// Failing runner means salvage is skipped; reallocation fires.
				session.SetUsage(spike.UsageInfo{
					PromptTokens: 42000, CompletionTokens: 8000,
					TotalTokens: 50000, BilledTokens: 50000,
				})
				return &spike.AgentMessage{Content: "Partial work; tests not green."}, nil
			}
			session.SetUsage(spike.UsageInfo{
				PromptTokens: 10000, CompletionTokens: 1000,
				TotalTokens: 11000, BilledTokens: 11000,
			})
			return &spike.AgentMessage{Content: "Fixed and verified."}, nil
		}
		return &spike.AgentMessage{Content: "ok"}, nil
	}

	// Failing-then-passing runner: first attempt fails tests so salvage is
	// skipped and reallocation fires; the retried attempt can complete.
	coord.TestRunner = &flakyThenPassRunner{failCalls: 3}

	_, err := coord.SubmitTask("Fix cypher WHERE", spike.Budget{Allocated: 500000}, nil)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	ctx := context.Background()
	var sawRecovery bool
	var recoveryAlloc float64
	for i := 0; i < 12; i++ {
		if !store.HasClaimable() {
			break
		}
		res, stepErr := coord.Step(ctx)
		if stepErr != nil {
			t.Fatalf("step %d: %v", i, stepErr)
		}
		if res == nil {
			break
		}
		if res.Outputs["recovery"] == "budget_reallocation" {
			sawRecovery = true
			if v, ok := res.Outputs["reallocated_to"].(float64); ok {
				recoveryAlloc = v
			}
		}
	}

	if !sawRecovery {
		t.Fatal("expected budget reallocation recovery")
	}
	// Floor is 25k; first alloc raised to 25k; consumed 32k → target 48k.
	// 2×25k=50k also applies; max should be at least consumed*1.5=48k.
	if recoveryAlloc < 50000*1.5 {
		t.Errorf("recovery allocation %.0f should cover observed overrun 50000*1.5=75000", recoveryAlloc)
	}
}

type failingTestRunner struct{}

func (s *failingTestRunner) RunTests(ctx context.Context, workspaceDir string) (int, string, error) {
	return 1, "FAIL: simulated", nil
}

// flakyThenPassRunner fails the first N calls (so budget salvage is skipped
// and reallocation is exercised), then passes (so the retried attempt can
// complete).
type flakyThenPassRunner struct {
	failCalls int
	calls     int
}

func (s *flakyThenPassRunner) RunTests(ctx context.Context, workspaceDir string) (int, string, error) {
	s.calls++
	if s.calls <= s.failCalls {
		return 1, "FAIL: simulated first attempt", nil
	}
	return 0, "ok", nil
}
