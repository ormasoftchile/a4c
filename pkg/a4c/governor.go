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
	if requested > budget.Available() {
		return nil, fmt.Errorf("%w: requested %.2f exceeds parent available %.2f",
			ErrBudgetExceeded, requested, budget.Available())
	}

	// Deduct requested from parent reservation
	budget.Reserved += requested

	childReq := &ActivityRequest{
		ID:        newID,
		ParentID:  parent.ID,
		AgentID:   proposal.ProposedAgentID,
		Objective: proposal.Objective,
		Inputs:    proposal.Inputs,
		DependsOn: proposal.DependsOn,
		Budget: Budget{
			Allocated: requested,
			Reserved:  0,
			Consumed:  0,
		},
		CreatedAt: time.Now(),
	}

	g.authorizedProposals[dedupKey] = childReq
	return childReq, nil
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
