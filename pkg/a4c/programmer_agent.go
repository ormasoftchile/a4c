package a4c

import (
	"context"
	"fmt"
)

const DefaultProgrammerCharter = `You are a disciplined software engineer. You inspect repository code using read_file, apply minimal changes using apply_patch, and verify correctness. You never execute unverified assumptions.`

type ProgrammerAgent struct {
	ID            string
	Charter       string
	WorkspaceDir  string
	Provider      AgentProvider
	Governor      *PolicyGovernor
	IntentJournal *DurableIntentJournal
	ActiveSession AgentSession
}

func NewProgrammerAgent(workspaceDir string, provider AgentProvider, governor *PolicyGovernor, journal *DurableIntentJournal) *ProgrammerAgent {
	return &ProgrammerAgent{
		ID:            "programmer",
		Charter:       DefaultProgrammerCharter,
		WorkspaceDir:  workspaceDir,
		Provider:      provider,
		Governor:      governor,
		IntentJournal: journal,
	}
}

// ExecuteActivity executes a single governed coding activity within the assigned budget.
func (a *ProgrammerAgent) ExecuteActivity(ctx context.Context, req ActivityRequest) (*ActivityResult, error) {
	// 1. Pre-flight budget verification
	if req.Budget.Available() <= 0 {
		return &ActivityResult{
			ID:     req.ID,
			Status: StatusFailed,
			Outputs: map[string]any{
				"error": "activity budget exhausted before start",
			},
		}, ErrBudgetExceeded
	}

	// 2. Initialize or resume session
	sessionConfig := AgentConfig{
		AgentID:       a.ID,
		Charter:       a.Charter,
		WorkspaceDir:  a.WorkspaceDir,
		Budget:        req.Budget,
		Governor:      a.Governor,
		IntentJournal: a.IntentJournal,
	}

	session, err := a.Provider.CreateSession(ctx, sessionConfig)
	if err != nil {
		return nil, fmt.Errorf("agent: failed to initialize session: %w", err)
	}
	defer session.Close()
	a.ActiveSession = session

	// 3. Prompt the agent with the objective
	prompt := fmt.Sprintf("Objective: %s\nWorkspace: %s\nInputs: %v",
		req.Objective, a.WorkspaceDir, req.Inputs)

	resp, err := session.Send(ctx, prompt)
	if err != nil {
		return &ActivityResult{
			ID:     req.ID,
			Status: StatusFailed,
			Outputs: map[string]any{
				"error": err.Error(),
			},
		}, err
	}

	usage := session.Usage()
	consumedBudget := Budget{
		Consumed: float64(usage.TotalTokens),
	}

	// Check if budget exceeded during generation
	if consumedBudget.Consumed > req.Budget.Allocated {
		return &ActivityResult{
			ID:       req.ID,
			Status:   StatusFailed,
			Consumed: consumedBudget,
			Outputs: map[string]any{
				"error": "consumed tokens exceeded allocation",
			},
		}, ErrBudgetExceeded
	}

	return &ActivityResult{
		ID:     req.ID,
		Status: StatusCompleted,
		Outputs: map[string]any{
			"summary":    resp.Content,
			"session_id": session.SessionID(),
		},
		Consumed: consumedBudget,
	}, nil
}
