package a4c

import (
	"context"
	"time"
)

// AgentMessage represents an inbound or outbound message in an agent session.
type AgentMessage struct {
	Role       string    `json:"role"`
	Content    string    `json:"content"`
	ToolCalls  []string  `json:"tool_calls,omitempty"`
	Timestamp  time.Time `json:"timestamp"`
}

// UsageInfo reports token and cost metrics for a session or invocation.
type UsageInfo struct {
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	EstimatedCostUSD float64 `json:"estimated_cost_usd"`
}

// AgentConfig configures a governed session for an agent.
type AgentConfig struct {
	AgentID                string                      `json:"agent_id"`
	Charter                string                      `json:"charter"`
	WorkspaceDir           string                      `json:"workspace_dir"`
	Budget                 Budget                      `json:"budget"`
	Governor               *PolicyGovernor             `json:"-"`
	IntentJournal          *DurableIntentJournal       `json:"-"`
	OnProposeActivity      func(ActivityProposal)      `json:"-"`
	OnSubmitReview         func(ReviewResult)          `json:"-"`
	OnRequestHumanDecision func(HumanDecisionRequest)  `json:"-"`
	OnProposeMemory        func(key, content string)   `json:"-"`
	Structural             StructuralProvider          `json:"-"`
	OnRecordMetric         func(metric string, delta int) `json:"-"`
}

// AgentSession manages conversational continuity for an active agent.
type AgentSession interface {
	SessionID() string
	Send(ctx context.Context, prompt string) (*AgentMessage, error)
	Cancel() error
	Usage() UsageInfo
	Close() error
}

// AgentProvider is the pluggable interface for LLM backends (Copilot Go SDK, FakeProvider).
type AgentProvider interface {
	CreateSession(ctx context.Context, config AgentConfig) (AgentSession, error)
	ResumeSession(ctx context.Context, sessionID string, config AgentConfig) (AgentSession, error)
}
