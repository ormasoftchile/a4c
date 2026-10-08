package a4c

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

type PolicyGovernor struct {
	mu                  sync.Mutex
	allowedTools        map[string]bool
	agentTools          map[string]map[string]bool
	activeBudgets       map[string]*Budget
	authorizedProposals map[string]*ActivityRequest
}

func NewPolicyGovernor(allowedTools []string) *PolicyGovernor {
	tools := make(map[string]bool)
	for _, t := range allowedTools {
		tools[t] = true
	}
	return &PolicyGovernor{
		allowedTools:        tools,
		agentTools:          make(map[string]map[string]bool),
		activeBudgets:       make(map[string]*Budget),
		authorizedProposals: make(map[string]*ActivityRequest),
	}
}

// SetAgentTools configures role-based tool capabilities for a specific agent identity.
func (g *PolicyGovernor) SetAgentTools(agentID string, tools []string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	tm := make(map[string]bool)
	for _, t := range tools {
		tm[t] = true
	}
	g.agentTools[agentID] = tm
}

// AuthorizeProposal evaluates a child proposal against parent's budget and idempotency rules.
func (g *PolicyGovernor) AuthorizeProposal(parent ActivityRequest, proposal ActivityProposal, newID string) (*ActivityRequest, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	// 1. Idempotency check: prevent duplicate delegation after restart/interruption
	key := proposal.ProposalID
	if key == "" {
		h := sha256.New()
		h.Write([]byte(parent.ID))
		h.Write([]byte(proposal.ProposedAgentID))
		h.Write([]byte(proposal.Objective))
		key = hex.EncodeToString(h.Sum(nil))
	}
	dedupKey := fmt.Sprintf("%s:%s", parent.ID, key)
	if existing, found := g.authorizedProposals[dedupKey]; found {
		return existing, nil
	}

	// 2. Budget verification
	budget, ok := g.activeBudgets[parent.ID]
	if !ok {
		budgetCopy := parent.Budget
		budget = &budgetCopy
		g.activeBudgets[parent.ID] = budget
	}

	requested := proposal.RequestedBudget.Allocated
	if budget.Allocated > 0 && requested > budget.Available() {
		return nil, fmt.Errorf("%w: requested %.2f exceeds parent available %.2f",
			ErrBudgetExceeded, requested, budget.Available())
	}

	// Deduct requested from parent reservation
	budget.Reserved += requested

	childBudget := Budget{
		Allocated: requested,
		Reserved:  0,
		Consumed:  0,
	}

	childReq := &ActivityRequest{
		ID:        newID,
		ParentID:  parent.ID,
		AgentID:   proposal.ProposedAgentID,
		Objective: proposal.Objective,
		Inputs:    proposal.Inputs,
		DependsOn: proposal.DependsOn,
		Budget:    childBudget,
		CreatedAt: time.Now(),
	}

	childBudgetCopy := childBudget
	g.activeBudgets[newID] = &childBudgetCopy
	g.authorizedProposals[dedupKey] = childReq
	return childReq, nil
}

// SetBudget sets or updates the budget record for an activity or root task.
func (g *PolicyGovernor) SetBudget(activityID string, b Budget) {
	g.mu.Lock()
	defer g.mu.Unlock()

	bCopy := b
	g.activeBudgets[activityID] = &bCopy
}

// RecordConsumption updates the consumed tokens for an activity and settles reservations on parent.
func (g *PolicyGovernor) RecordConsumption(activityID, parentID string, tokens float64) {
	g.mu.Lock()
	defer g.mu.Unlock()

	var allocated float64
	if b, ok := g.activeBudgets[activityID]; ok {
		b.Consumed += tokens
		allocated = b.Allocated
		b.Reserved = 0
	} else {
		g.activeBudgets[activityID] = &Budget{Consumed: tokens}
	}

	if parentID != "" && parentID != activityID {
		if p, ok := g.activeBudgets[parentID]; ok {
			p.Consumed += tokens
			if allocated > 0 {
				if p.Reserved >= allocated {
					p.Reserved -= allocated
				} else {
					p.Reserved = 0
				}
			} else if p.Reserved >= tokens {
				p.Reserved -= tokens
			} else {
				p.Reserved = 0
			}
		}
	}
}

// CheckBudget verifies whether an activity has sufficient budget to proceed.
func (g *PolicyGovernor) CheckBudget(activityID string, additionalTokens float64) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	b, ok := g.activeBudgets[activityID]
	if !ok || b.Allocated <= 0 {
		return nil // Unconstrained or unrecorded
	}
	if b.Consumed+additionalTokens > b.Allocated {
		return fmt.Errorf("%w: consumption %.0f exceeds allocation %.0f for activity %s",
			ErrBudgetExceeded, b.Consumed+additionalTokens, b.Allocated, activityID)
	}
	return nil
}

// AuthorizeToolInvocation gates direct tool calls from agent sessions, rejecting ambient unmediated bypasses.
func (g *PolicyGovernor) AuthorizeToolInvocation(agentID string, toolName string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	// Check agent-specific role permissions first if defined
	if specific, ok := g.agentTools[agentID]; ok {
		if !specific[toolName] {
			return fmt.Errorf("%w: agent %q not authorized for tool %q", ErrUnauthorizedTool, agentID, toolName)
		}
		return nil
	}

	// Fallback to global tool allowlist
	if !g.allowedTools[toolName] {
		return fmt.Errorf("%w: agent %q attempted unmediated tool %q", ErrUnauthorizedTool, agentID, toolName)
	}
	return nil
}

// GetBudget retrieves active budget metrics tracked for an activity ID.
func (g *PolicyGovernor) GetBudget(activityID string) (Budget, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	b, ok := g.activeBudgets[activityID]
	if !ok {
		return Budget{}, false
	}
	return *b, true
}
