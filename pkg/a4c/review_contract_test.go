package a4c_test

import (
	"context"
	"testing"

	spike "github.com/ormasoftchile/a4c/pkg/a4c"
)

// TestReviewContract_CoderOnlyLeadStillGetsMandatoryReviewer reproduces the
// Antigravity/Gemini defect: the lead delegated only a coder, omitting
// independent review. Policy enforcement must schedule a reviewer anyway.
func TestReviewContract_CoderOnlyLeadStillGetsMandatoryReviewer(t *testing.T) {
	fake := spike.NewFakeProvider()
	coord, store, _ := newNoDelegationCoordinator(t, fake)

	fake.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		if session.Config().AgentID == "lead" {
			// Deliberately propose ONLY a coder (omit the reviewer).
			if session.Config().OnProposeActivity != nil {
				session.Config().OnProposeActivity(spike.ActivityProposal{
					ProposalID:      "impl-1",
					ProposedAgentID: "coder",
					Objective:       "Implement the cypher fix",
					RequestedBudget: spike.Budget{Allocated: 120000},
				})
			}
			return &spike.AgentMessage{Content: "Delegated to coder only."}, nil
		}
		return &spike.AgentMessage{Content: "ok"}, nil
	}

	_, err := coord.SubmitTask("Implement the cypher property comparison", spike.Budget{Allocated: 500000}, nil)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	res, err := coord.Step(context.Background())
	if err != nil {
		t.Fatalf("lead step: %v", err)
	}
	if res.Status != spike.StatusCompleted {
		t.Fatalf("lead should complete, got %v", res.Status)
	}

	// A reviewer child must have been enqueued by policy enforcement.
	reviewerEnqueued := false
	for _, rec := range store.QueryActivities(spike.ActivityFilter{}) {
		if rec != nil && (rec.Request.AgentID == "reviewer") && rec.Request.ID != "" {
			reviewerEnqueued = true
		}
	}
	if !reviewerEnqueued {
		t.Fatalf("mandatory independent review was omitted: no reviewer activity enqueued")
	}
	// And it must be marked as scheduled on the lead result.
	if scheduled, _ := res.Outputs["mandatory_review_scheduled"].(bool); !scheduled {
		t.Fatalf("expected mandatory_review_scheduled on the lead result")
	}
}

// TestReviewContract_ReviewAlreadyPresentNotDuplicated: when the lead already
// proposed a reviewer, enforcement must not add a duplicate.
func TestReviewContract_ReviewAlreadyPresentNotDuplicated(t *testing.T) {
	fake := spike.NewFakeProvider()
	coord, store, _ := newNoDelegationCoordinator(t, fake)

	fake.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		if session.Config().AgentID == "lead" {
			if session.Config().OnProposeActivity != nil {
				session.Config().OnProposeActivity(spike.ActivityProposal{
					ProposalID: "impl-1", ProposedAgentID: "coder",
					Objective: "Implement", RequestedBudget: spike.Budget{Allocated: 100000},
				})
				session.Config().OnProposeActivity(spike.ActivityProposal{
					ProposalID: "rev-1", ProposedAgentID: "reviewer",
					Objective: "Review", RequestedBudget: spike.Budget{Allocated: 40000},
					DependsOn: []string{"impl-1"},
				})
			}
			return &spike.AgentMessage{Content: "Delegated coder+reviewer."}, nil
		}
		return &spike.AgentMessage{Content: "ok"}, nil
	}

	_, _ = coord.SubmitTask("Implement and review", spike.Budget{Allocated: 500000}, nil)
	res, err := coord.Step(context.Background())
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	reviewerCount := 0
	for _, rec := range store.QueryActivities(spike.ActivityFilter{}) {
		if rec != nil && rec.Request.AgentID == "reviewer" {
			reviewerCount++
		}
	}
	if reviewerCount != 1 {
		t.Fatalf("expected exactly 1 reviewer (no duplicate), got %d", reviewerCount)
	}
	if scheduled, _ := res.Outputs["mandatory_review_scheduled"].(bool); scheduled {
		t.Fatalf("should not mark mandatory_review_scheduled when the lead already proposed a reviewer")
	}
}

// TestReviewContract_ZeroProposalBoundedRecovery reproduces the OpenCode/Gemini
// defect: the lead ended with zero proposals. The coordinator performs one
// bounded corrective re-prompt; if the model then delegates, the task proceeds
// instead of failing.
func TestReviewContract_ZeroProposalBoundedRecovery(t *testing.T) {
	fake := spike.NewFakeProvider()
	coord, store, _ := newNoDelegationCoordinator(t, fake)

	leadCalls := 0
	fake.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		if session.Config().AgentID == "lead" {
			leadCalls++
			// First turn: propose nothing (the observed defect).
			if leadCalls == 1 {
				return &spike.AgentMessage{Content: "I analyzed the task but did not delegate."}, nil
			}
			// Corrective turn: delegate.
			if session.Config().OnProposeActivity != nil {
				session.Config().OnProposeActivity(spike.ActivityProposal{
					ProposalID: "impl-1", ProposedAgentID: "coder",
					Objective: "Implement", RequestedBudget: spike.Budget{Allocated: 120000},
				})
			}
			return &spike.AgentMessage{Content: "Delegating now."}, nil
		}
		return &spike.AgentMessage{Content: "ok"}, nil
	}

	_, err := coord.SubmitTask("Implement the fix", spike.Budget{Allocated: 500000}, nil)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	res, err := coord.Step(context.Background())
	if err != nil {
		t.Fatalf("lead step should recover via bounded retry, got: %v", err)
	}
	if res.Status == spike.StatusFailed {
		t.Fatalf("zero-proposal lead must not hard-fail after bounded recovery, got failed")
	}
	coderEnqueued := false
	for _, rec := range store.QueryActivities(spike.ActivityFilter{}) {
		if rec != nil && rec.Request.AgentID == "coder" {
			coderEnqueued = true
		}
	}
	if !coderEnqueued {
		t.Fatalf("bounded recovery did not result in a coder assignment")
	}
	if leadCalls < 2 {
		t.Fatalf("expected a corrective re-prompt (>=2 lead turns), got %d", leadCalls)
	}}
