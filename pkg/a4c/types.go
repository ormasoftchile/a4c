package a4c

import (
	"errors"
	"fmt"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/evidence"
)

var (
	ErrBudgetExceeded           = errors.New("governance: requested budget exceeds parent allocation")
	ErrUnauthorizedTool         = errors.New("governance: unauthorized tool execution channel")
	ErrUnfinishedIntentDetected = errors.New("reconciliation: unfinished external intent detected on restart")
	ErrWorkspaceConcurrentEdit  = errors.New("evidence: workspace content modified concurrently during validation")
	ErrActivityNotFound         = errors.New("store: activity not found")
)

type ActivityStatus string

const (
	StatusPending       ActivityStatus = "pending"
	StatusClaimed       ActivityStatus = "claimed"
	StatusCompleted     ActivityStatus = "completed"
	StatusFailed        ActivityStatus = "failed"
	StatusIndeterminate ActivityStatus = "indeterminate"
	StatusAwaitingHuman ActivityStatus = "awaiting_human"
)

// Budget models consumable tokens or compute cost with reserved vs consumed tracking.
type Budget struct {
	Allocated float64 `json:"allocated"`
	Reserved  float64 `json:"reserved"`
	Consumed  float64 `json:"consumed"`
}

func (b Budget) Available() float64 {
	return b.Allocated - (b.Reserved + b.Consumed)
}

// BudgetRecord provides durable persistence for hierarchical budget allocations and reservations.
type BudgetRecord struct {
	ID        string    `json:"id"`
	ParentID  string    `json:"parent_id,omitempty"`
	Allocated float64   `json:"allocated"`
	Reserved  float64   `json:"reserved"`
	Consumed  float64   `json:"consumed"`
	Status    string    `json:"status"` // "active", "settled", "released"
	UpdatedAt time.Time `json:"updated_at"`
}

// ExecutionStrategy defines how a submitted task is scheduled and executed.
type ExecutionStrategy string

const (
	StrategyDirect   ExecutionStrategy = "direct"   // Single programmer handles task directly
	StrategyReviewed ExecutionStrategy = "reviewed" // Programmer implements + Independent Reviewer verifies
	StrategyTeam     ExecutionStrategy = "team"     // Lead coordinates, delegates to Coder & Reviewer
)

// ExecutionMetrics tracks detailed per-activity model, tool, and latency measurements.
type ExecutionMetrics struct {
	WallClockDuration      time.Duration `json:"wall_clock_duration"`
	ModelDuration          time.Duration `json:"model_duration"`
	ToolDuration           time.Duration `json:"tool_duration"`
	ValidationDuration     time.Duration `json:"validation_duration,omitempty"`
	StructuralDuration     time.Duration `json:"structural_duration,omitempty"`
	PromptTokens           int           `json:"prompt_tokens"`
	CompletionTokens       int           `json:"completion_tokens"`
	TotalTokens            int           `json:"total_tokens"`
	PeakContextTokens      int           `json:"peak_context_tokens,omitempty"`
	OverrunTokens          int           `json:"overrun_tokens,omitempty"`
	ModelInvocations       int           `json:"model_invocations"`
	ToolCalls              int           `json:"tool_calls"`
	StructuralQueriesCount int           `json:"structural_queries_count,omitempty"`
	FileReadsCount         int           `json:"file_reads_count,omitempty"`
}

// ActivityFilter specifies criteria for querying historical or active activities.
type ActivityFilter struct {
	Status    ActivityStatus `json:"status,omitempty"`
	AgentID   string         `json:"agent_id,omitempty"`
	ParentID  string         `json:"parent_id,omitempty"`
	RootID    string         `json:"root_id,omitempty"`
	HasErrors *bool          `json:"has_errors,omitempty"`
}

// CapabilityProfile models a declarative, activity-scoped permission set and budget.
type CapabilityProfile struct {
	ID           string   `json:"id"`
	Name         string   `json:"name,omitempty"`
	Description  string   `json:"description,omitempty"`
	AllowedTools []string `json:"allowed_tools"`
	Budget       Budget   `json:"budget,omitempty"`
}

// Clone returns an isolated copy of the capability profile.
func (p CapabilityProfile) Clone() CapabilityProfile {
	tools := make([]string, len(p.AllowedTools))
	copy(tools, p.AllowedTools)
	return CapabilityProfile{
		ID:           p.ID,
		Name:         p.Name,
		Description:  p.Description,
		AllowedTools: tools,
		Budget:       p.Budget,
	}
}

// ActivityRequest is a unit of work scheduled for an agent or tool.
type ActivityRequest struct {
	ID                   string             `json:"id"`
	ParentID             string             `json:"parent_id,omitempty"`
	AgentID              string             `json:"agent_id"`
	Objective            string             `json:"objective"`
	Inputs               map[string]any     `json:"inputs,omitempty"`
	WorkspaceSnapshotID  string             `json:"workspace_snapshot_id,omitempty"`
	Budget               Budget             `json:"budget"`
	Strategy             ExecutionStrategy  `json:"strategy,omitempty"`
	DependsOn            []string           `json:"depends_on,omitempty"`
	CapabilityProfile    *CapabilityProfile `json:"capability_profile,omitempty"`
	StructuralArtifactID string             `json:"structural_artifact_id,omitempty"`
	CreatedAt            time.Time          `json:"created_at"`
}

// ActivityProposal allows an agent to request dynamic subtasks without directly mutating the execution graph.
type ActivityProposal struct {
	ProposalID       string             `json:"proposal_id,omitempty"`
	ProposedAgentID  string             `json:"proposed_agent_id"`
	Objective        string             `json:"objective"`
	Inputs           map[string]any     `json:"inputs,omitempty"`
	RequestedBudget  Budget             `json:"requested_budget"`
	RequestedProfile *CapabilityProfile `json:"requested_profile,omitempty"`
	DependsOn        []string           `json:"depends_on,omitempty"`
}

// ActivityResult is the structured output produced by an activity.
type ActivityResult struct {
	ID                   string             `json:"id"`
	Status               ActivityStatus     `json:"status"`
	Outputs              map[string]any     `json:"outputs,omitempty"`
	EvidenceIDs          []string           `json:"evidence_ids,omitempty"`
	Proposals            []ActivityProposal `json:"proposals,omitempty"`
	Consumed             Budget             `json:"consumed"`
	Metrics              *ExecutionMetrics  `json:"metrics,omitempty"`
	CapabilityProfile    *CapabilityProfile `json:"capability_profile,omitempty"`
	StructuralArtifactID string             `json:"structural_artifact_id,omitempty"`
}


// AgentIdentity models a persistent team member identity.
type AgentIdentity struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Role         string   `json:"role"`
	Charter      string   `json:"charter"`
	Specialties  []string `json:"specialties"`
	AllowedTools []string `json:"allowed_tools"`
}

type HumanDecisionStatus string

const (
	HumanDecisionPending    HumanDecisionStatus = "pending"
	HumanDecisionApproved   HumanDecisionStatus = "approved"
	HumanDecisionRejected   HumanDecisionStatus = "rejected"
	HumanDecisionReassigned HumanDecisionStatus = "reassigned"
)

// HumanDecisionRequest models an escalation to the human engineer.
type HumanDecisionRequest struct {
	DecisionID   string              `json:"decision_id"`
	ActivityID   string              `json:"activity_id"`
	ProposedBy   string              `json:"proposed_by"`
	Question     string              `json:"question"`
	Context      map[string]any      `json:"context,omitempty"`
	Status       HumanDecisionStatus `json:"status"`
	Resolution   string              `json:"resolution,omitempty"`
	ReassignedTo string              `json:"reassigned_to,omitempty"`
	CreatedAt    time.Time           `json:"created_at"`
	ResolvedAt   *time.Time          `json:"resolved_at,omitempty"`
}

// ReviewResult encapsulates deterministic peer review artifacts.
type ReviewResult struct {
	ActivityID string    `json:"activity_id"`
	ReviewerID string    `json:"reviewer_id"`
	Approved   bool      `json:"approved"`
	Comments   string    `json:"comments"`
	Blockers   []string  `json:"blockers,omitempty"`
	EvidenceID string    `json:"evidence_id,omitempty"`
	Timestamp  time.Time `json:"timestamp"`
}

type IntentStatus string

const (
	IntentPersisted IntentStatus = "persisted"
	IntentCommitted IntentStatus = "committed"
	IntentUncertain IntentStatus = "uncertain"
)

// IntentRecord logs the durable pre-mutation intent before touching external systems.
type IntentRecord struct {
	IntentID    string         `json:"intent_id"`
	ActivityID  string         `json:"activity_id"`
	Action      string         `json:"action"` // e.g. "create_pull_request"
	Target      string         `json:"target"` // e.g. "branch/feature-1"
	PayloadHash string         `json:"payload_hash"`
	Status      IntentStatus   `json:"status"`
	ExternalID  string         `json:"external_id,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
}

// ValidationEvidence represents verifiable proof that tests ran against a specific snapshot.
type ValidationEvidence struct {
	EvidenceID          string        `json:"evidence_id"`
	WorkspaceSnapshotID string        `json:"workspace_snapshot_id"`
	Command             []string      `json:"command"`
	ExitCode            int           `json:"exit_code"`
	OutputSummary       string        `json:"output_summary,omitempty"`
	Duration            time.Duration `json:"duration,omitempty"`
	LogPath             string        `json:"log_path,omitempty"`
	StartedAt           time.Time     `json:"started_at"`
	CompletedAt         time.Time     `json:"completed_at"`
	Valid               bool          `json:"valid"`
}

// ToYAWREvidence bridges a4c validation evidence into YAWR runtime's native EvidenceRecord.
func (v ValidationEvidence) ToYAWREvidence() evidence.EvidenceRecord {
	return evidence.EvidenceRecord{
		Name:       "workspace_validation",
		Kind:       evidence.EvidenceKindSnapshot,
		SHA256:     v.WorkspaceSnapshotID,
		CapturedAt: v.CompletedAt,
		Value:      fmt.Sprintf("exit_code=%d", v.ExitCode),
	}
}
