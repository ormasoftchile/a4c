package a4c_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	spike "github.com/ormasoftchile/a4c/pkg/a4c"
)

// stubTestRunner avoids requiring a real Go module in the workspace while
// still satisfying the coordinator's native-validation contract.
type stubTestRunner struct{ calls int }

func (s *stubTestRunner) RunTests(ctx context.Context, workspaceDir string) (int, string, error) {
	s.calls++
	return 0, "ok", nil
}

func newBudgetTestCoordinator(t *testing.T, fake *spike.FakeProvider, runner spike.TestRunner) (
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
	coord := spike.NewTeamCoordinator(dir, store, roster, memory, gov, journal, runner, fake)
	return coord, store, gov
}

// TestBudgetEconomics_ImpossiblySmallProposalRaisedToViableFloor verifies that
// a coordinator's impossibly small child budget (the exact benchmark failure:
// 4,000 tokens for a coder) is not authoritative: the runtime raises it to
// the viable minimum before the child activity is ever created.
func TestBudgetEconomics_ImpossiblySmallProposalRaisedToViableFloor(t *testing.T) {
	gov := spike.NewPolicyGovernor(nil)
	gov.SetBudget("task-root", spike.Budget{Allocated: 500000})

	parent := spike.ActivityRequest{ID: "task-root", Budget: spike.Budget{Allocated: 500000}}
	proposal := spike.ActivityProposal{
		ProposedAgentID: "coder",
		Objective:       "Implement interval algebra",
		RequestedBudget: spike.Budget{Allocated: 4000}, // the benchmark's defective allocation
	}

	child, err := gov.AuthorizeProposal(parent, proposal, "task-root-sub-1")
	if err != nil {
		t.Fatalf("expected proposal to be raised to viable floor, got error: %v", err)
	}
	if child.Budget.Allocated < spike.DefaultMinChildBudgetTokens {
		t.Fatalf("child allocation %.0f below viable floor %.0f",
			child.Budget.Allocated, spike.DefaultMinChildBudgetTokens)
	}
	// The measured host baseline for a fresh gpt-5.5 session's first model
	// call is ~12.3k uncached input tokens; the floor must exceed it.
	if spike.DefaultMinChildBudgetTokens <= 12300 {
		t.Fatalf("viable floor %.0f must exceed measured first-call baseline 12300",
			spike.DefaultMinChildBudgetTokens)
	}
	if child.Inputs["budget_adjusted_from"] != float64(4000) {
		t.Fatalf("expected adjustment record, got %v", child.Inputs)
	}
	if got := gov.AvailableFor("task-root"); got > 500000-spike.DefaultMinChildBudgetTokens {
		t.Fatalf("parent reservation inconsistent: available %.0f", got)
	}
}

// TestBudgetEconomics_ParentCannotFundViableChild verifies that when the
// parent cannot fund a viable allocation, authorization fails with an
// explicit actionable error instead of creating a doomed child.
func TestBudgetEconomics_ParentCannotFundViableChild(t *testing.T) {
	gov := spike.NewPolicyGovernor(nil)
	gov.SetBudget("task-small", spike.Budget{Allocated: 40000, Reserved: 20000})

	parent := spike.ActivityRequest{ID: "task-small", Budget: spike.Budget{Allocated: 40000, Reserved: 20000}}
	proposal := spike.ActivityProposal{
		ProposedAgentID: "coder",
		Objective:       "Implement feature",
		RequestedBudget: spike.Budget{Allocated: 4000},
	}

	_, err := gov.AuthorizeProposal(parent, proposal, "task-small-sub-1")
	if !errors.Is(err, spike.ErrInsufficientBudget) {
		t.Fatalf("expected ErrInsufficientBudget, got: %v", err)
	}
}

// TestBudgetEconomics_ParentReservationAndExhaustion verifies that child
// allocations can never silently exceed the parent's available budget after
// retaining the coordination reserve, and that exhaustion is explicit.
func TestBudgetEconomics_ParentReservationAndExhaustion(t *testing.T) {
	gov := spike.NewPolicyGovernor(nil)
	gov.SetBudget("task-50k", spike.Budget{Allocated: 50000})
	parent := spike.ActivityRequest{ID: "task-50k", Budget: spike.Budget{Allocated: 50000}}

	// Over-request beyond parent headroom (available 50k - reserve 10k): rejected.
	_, err := gov.AuthorizeProposal(parent, spike.ActivityProposal{
		ProposedAgentID: "coder",
		Objective:       "too big",
		RequestedBudget: spike.Budget{Allocated: 45000},
	}, "sub-over")
	if !errors.Is(err, spike.ErrBudgetExceeded) {
		t.Fatalf("expected ErrBudgetExceeded for over-request, got: %v", err)
	}

	// A viable allocation is accepted and reserved.
	child1, err := gov.AuthorizeProposal(parent, spike.ActivityProposal{
		ProposedAgentID: "coder",
		Objective:       "viable",
		RequestedBudget: spike.Budget{Allocated: 30000},
	}, "sub-1")
	if err != nil {
		t.Fatalf("viable child rejected: %v", err)
	}
	if child1.Budget.Allocated != 30000 {
		t.Fatalf("expected 30000, got %.0f", child1.Budget.Allocated)
	}

	// Parent now has 20k available; a further below-floor proposal cannot be
	// funded while retaining the reserve: explicit insufficient error.
	_, err = gov.AuthorizeProposal(parent, spike.ActivityProposal{
		ProposedAgentID: "reviewer",
		Objective:       "review",
		RequestedBudget: spike.Budget{Allocated: 2500},
	}, "sub-2")
	if !errors.Is(err, spike.ErrInsufficientBudget) {
		t.Fatalf("expected ErrInsufficientBudget once parent lacks viable headroom, got: %v", err)
	}
}

// TestBudgetEconomics_RecoveryFromExhaustion reproduces the benchmark failure
// shape end-to-end: the lead under-budgets the coder, the runtime raises the
// allocation to the viable floor, the first attempt still exhausts it, and
// the coordinator performs one explicit reallocation from the parent's
// remaining budget. The retry completes; nothing is granted beyond the parent.
func TestBudgetEconomics_RecoveryFromExhaustion(t *testing.T) {
	fake := spike.NewFakeProvider()
	// First test run fails so salvage (complete-on-overrun-when-tests-pass)
	// does not intercept; the retried attempt then passes.
	coord, store, gov := newBudgetTestCoordinator(t, fake, &flakyThenPassRunner{failCalls: 3})

	coderSends := 0
	fake.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		switch session.Config().AgentID {
		case "lead":
			session.Config().OnProposeActivity(spike.ActivityProposal{
				ProposalID:      "coder-1",
				ProposedAgentID: "coder",
				Objective:       "Implement the feature in feature.go",
				RequestedBudget: spike.Budget{Allocated: 4000}, // impossibly small
			})
			return &spike.AgentMessage{Content: "Delegated to coder."}, nil
		case "coder":
			// Count only the primary attempt prompt, not repair-loop resends.
			if !strings.Contains(prompt, "Tests failed with exit code") {
				coderSends++
			}
			if coderSends <= 1 {
				// First primary attempt blows the (floor-sized) allocation.
				session.SetUsage(spike.UsageInfo{
					PromptTokens: 28000, CompletionTokens: 2000,
					TotalTokens: 30000, BilledTokens: 30000,
				})
				return &spike.AgentMessage{Content: "Work attempted but budget ran out."}, nil
			}
			session.SetUsage(spike.UsageInfo{
				PromptTokens: 9000, CompletionTokens: 1000,
				TotalTokens: 10000, BilledTokens: 10000,
			})
			return &spike.AgentMessage{Content: "Implemented and verified."}, nil
		}
		return &spike.AgentMessage{Content: "ok"}, nil
	}

	ref, err := coord.SubmitTaskWithStrategy("Implement the feature", spike.Budget{Allocated: 200000}, nil, spike.StrategyTeam)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	ctx := context.Background()
	var sawRecovery bool
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
		if res.Outputs["recovery"] == "budget_reallocation" {
			sawRecovery = true
			if res.Status != spike.StatusPending {
				t.Fatalf("recovery result should be pending requeue, got %s", res.Status)
			}
			if res.Outputs["reallocated_to"] != float64(spike.DefaultMinChildBudgetTokens*2) {
				t.Fatalf("expected reallocation to %.0f, got %v",
					spike.DefaultMinChildBudgetTokens*2, res.Outputs["reallocated_to"])
			}
		}
	}

	if !sawRecovery {
		t.Fatalf("expected one explicit budget_reallocation recovery step")
	}
	if coderSends != 2 {
		t.Fatalf("expected exactly 2 coder attempts (bounded), got %d", coderSends)
	}

	coderRec, err := store.Get("coder-1")
	if err != nil {
		t.Fatalf("coder record: %v", err)
	}
	if coderRec.Status != spike.StatusCompleted {
		t.Fatalf("expected coder completed after recovery, got %s (err: %s)", coderRec.Status, coderRec.Error)
	}
	if coderRec.Request.Budget.Allocated != spike.DefaultMinChildBudgetTokens*2 {
		t.Fatalf("expected final allocation %.0f, got %.0f",
			spike.DefaultMinChildBudgetTokens*2, coderRec.Request.Budget.Allocated)
	}
	switch retries := coderRec.Request.Inputs["budget_retries"].(type) {
	case int:
		if retries != 1 {
			t.Fatalf("expected budget_retries=1, got %v", retries)
		}
	case float64:
		if retries != 1 {
			t.Fatalf("expected budget_retries=1, got %v", retries)
		}
	default:
		t.Fatalf("expected budget_retries=1, got %T %v", retries, retries)
	}
	if got := gov.AvailableFor(ref.ID); got < 0 {
		t.Fatalf("parent budget over-granted: available %.0f", got)
	}
}

// TestBudgetEconomics_RecoveryIsBounded verifies the coordinator gives up
// after MaxBudgetRetries exhaustions: repeated exhaustion terminates with the
// explicit budget-exhausted state instead of looping.
func TestBudgetEconomics_RecoveryIsBounded(t *testing.T) {
	fake := spike.NewFakeProvider()
	// Always fail tests so neither salvage nor a successful retry can
	// complete; both reallocation attempts exhaust and terminate.
	coord, store, _ := newBudgetTestCoordinator(t, fake, &failingTestRunner{})
	coord.MaxBudgetRetries = 1

	coderSends := 0
	fake.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		switch session.Config().AgentID {
		case "lead":
			session.Config().OnProposeActivity(spike.ActivityProposal{
				ProposalID:      "coder-1",
				ProposedAgentID: "coder",
				Objective:       "Implement the feature",
				RequestedBudget: spike.Budget{Allocated: 4000},
			})
			return &spike.AgentMessage{Content: "Delegated."}, nil
		case "coder":
			if !strings.Contains(prompt, "Tests failed with exit code") {
				coderSends++
			}
			// Exceed any reallocation the governor can grant from the 500k
			// parent while leaving headroom for one retry: first attempt
			// 400k > 25k floor; reallocation caps at ~parent-available;
			// retry still exceeds the reallocated cap → terminal.
			session.SetUsage(spike.UsageInfo{TotalTokens: 400000, BilledTokens: 400000})
			return &spike.AgentMessage{Content: "Budget keeps running out."}, nil
		}
		return &spike.AgentMessage{Content: "ok"}, nil
	}

	_, err := coord.SubmitTaskWithStrategy("Implement the feature", spike.Budget{Allocated: 500000}, nil, spike.StrategyTeam)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	ctx := context.Background()
	var stepErr error
	for i := 0; i < 10; i++ {
		if !store.HasClaimable() {
			break
		}
		_, stepErr = coord.Step(ctx)
		if stepErr != nil {
			break
		}
	}

	if stepErr == nil || !errors.Is(stepErr, spike.ErrBudgetExceeded) {
		t.Fatalf("expected terminal ErrBudgetExceeded after bounded retries, got: %v", stepErr)
	}
	if coderSends != 2 {
		t.Fatalf("expected exactly 2 coder attempts (initial + 1 retry), got %d", coderSends)
	}
	coderRec, _ := store.Get("coder-1")
	if coderRec == nil || coderRec.Status != spike.StatusFailed {
		t.Fatalf("expected coder terminally failed, got %+v", coderRec)
	}
	if coderRec.Result == nil || coderRec.Result.Outputs["failure_reason"] != "budget_exhausted" {
		t.Fatalf("expected explicit budget_exhausted failure state, got %+v", coderRec.Result)
	}
}

// TestBudgetEconomics_DirectModePreserved verifies direct mode is unaffected:
// the root coder activity keeps its submitted budget and completes normally.
func TestBudgetEconomics_DirectModePreserved(t *testing.T) {
	fake := spike.NewFakeProvider()
	runner := &stubTestRunner{}
	coord, store, _ := newBudgetTestCoordinator(t, fake, runner)

	fake.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		if session.Config().AgentID != "coder" {
			t.Fatalf("direct mode must not involve agent %q", session.Config().AgentID)
		}
		if session.Config().Budget.Allocated != 5000 {
			t.Fatalf("direct root budget must be preserved at 5000, got %.0f", session.Config().Budget.Allocated)
		}
		session.SetUsage(spike.UsageInfo{TotalTokens: 1200, BilledTokens: 1200})
		return &spike.AgentMessage{Content: "Implemented."}, nil
	}

	ref, err := coord.SubmitTaskWithStrategy("Quick fix", spike.Budget{Allocated: 5000}, nil, spike.StrategyDirect)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	ctx := context.Background()
	res, err := coord.Step(ctx)
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if res == nil || res.Status != spike.StatusCompleted {
		t.Fatalf("expected completed direct activity, got %+v", res)
	}
	rec, err := store.Get(ref.ID)
	if err != nil || rec.Status != spike.StatusCompleted {
		t.Fatalf("expected completed record, got %+v (err %v)", rec, err)
	}
}

// TestBudgetEconomics_BilledAccountingExcludesCacheReads documents the
// accounting semantics: budget consumption counts uncached input + output;
// cache re-serves do not consume budget.
func TestBudgetEconomics_BilledAccountingExcludesCacheReads(t *testing.T) {
	usage := spike.UsageInfo{
		PromptTokens:     91589, // total input incl. 75,776 cache reads
		CachedTokens:     75776,
		CompletionTokens: 4098,
		TotalTokens:      95687,
		BilledTokens:     15813 + 4098,
	}
	if got := usage.BilledConsumption(); got != 19911 {
		t.Fatalf("expected billed consumption 19911, got %.0f", got)
	}
	legacy := spike.UsageInfo{TotalTokens: 300}
	if got := legacy.BilledConsumption(); got != 300 {
		t.Fatalf("legacy fallback expected 300, got %.0f", got)
	}
}
