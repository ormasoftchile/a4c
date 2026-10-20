package a4c

import (
	"errors"
	"time"
)

var (
	ErrBudgetExceeded           = errors.New("governance: requested budget exceeds parent allocation")
	ErrInsufficientBudget       = errors.New("governance: parent budget cannot fund a viable child allocation")
	ErrUnauthorizedTool         = errors.New("governance: unauthorized tool execution channel")
	ErrUnfinishedIntentDetected = errors.New("reconciliation: unfinished external intent detected on restart")
	ErrWorkspaceConcurrentEdit  = errors.New("evidence: workspace content modified concurrently during validation")
	ErrActivityNotFound         = errors.New("store: activity not found")
	ErrActivityTerminal         = errors.New("store: activity is already terminal; conflicting outcome rejected")
	ErrWorkspaceLocked          = errors.New("workspace: exclusive lock held by another coordinator process")
	ErrTurnLimitExceeded        = errors.New("limits: max model turns exceeded for activity")
	ErrToolCallLimitExceeded    = errors.New("limits: max tool calls exceeded for activity")
	ErrRevisionLimitExceeded    = errors.New("limits: max revisions exceeded for activity")
	ErrStaleValidationEvidence  = errors.New("evidence: validation evidence does not match workspace state")
)

type ActivityStatus string

const (
	StatusPending       ActivityStatus = "pending"
	StatusClaimed       ActivityStatus = "claimed"
	StatusCompleted     ActivityStatus = "completed"
	StatusFailed        ActivityStatus = "failed"
	StatusIndeterminate ActivityStatus = "indeterminate"
	StatusAwaitingHuman ActivityStatus = "awaiting_human"
	StatusInterrupted   ActivityStatus = "interrupted"
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
	ProviderWaitDuration   time.Duration `json:"provider_wait_duration"`
	ModelDuration          time.Duration `json:"model_duration,omitempty"` // legacy alias for backwards compatibility
	ToolDuration           time.Duration `json:"tool_duration"`
	ValidationDuration     time.Duration `json:"validation_duration,omitempty"`
	StructuralDuration     time.Duration `json:"structural_duration,omitempty"`
	PromptTokens           int           `json:"prompt_tokens"`
	CompletionTokens       int           `json:"completion_tokens"`
	TotalTokens            int           `json:"total_tokens"`
	BilledTokens           int           `json:"billed_tokens,omitempty"` // uncached input + output (budget-relevant consumption)
	PeakContextTokens      int           `json:"peak_context_tokens,omitempty"`
	OverrunTokens          int           `json:"overrun_tokens,omitempty"`
	ReasoningTokens        int           `json:"reasoning_tokens,omitempty"`
	CachedTokens           int           `json:"cached_tokens,omitempty"`
	ReportedCostUSD        float64       `json:"reported_cost_usd,omitempty"`
	ModelInvocations       int           `json:"model_invocations"`
	ToolCalls              int           `json:"tool_calls"`
	StructuralQueriesCount int           `json:"structural_queries_count,omitempty"`
	FileReadsCount         int           `json:"file_reads_count,omitempty"`
	// Coordination observability: how many governed_propose_activity tool
	// invocations were accepted by the session adapter (via OnProposeActivity).
	ProposalsRecorded int `json:"proposals_recorded,omitempty"`
	// How many governed_* tool calls were rejected by in-session limits
	// (budget, tool-call cap) — surfaces "delegation tools are failing".
	ToolLimitHits int `json:"tool_limit_hits,omitempty"`
}

// BilledConsumption returns the budget-relevant token consumption for a
// usage snapshot: billed tokens when reported, else the legacy total.
func (u UsageInfo) BilledConsumption() float64 {
	if u.BilledTokens > 0 {
		return float64(u.BilledTokens)
	}
	return float64(u.TotalTokens)
}

// ActivityLimits sets bounded economic and turn constraints for an activity.
type ActivityLimits struct {
	MaxModelTurns       int `json:"max_model_turns,omitempty"`
	MaxToolCalls        int `json:"max_tool_calls,omitempty"`
	MaxReadFileLines    int `json:"max_read_file_lines,omitempty"`
	MaxDiffLines        int `json:"max_diff_lines,omitempty"`
	MaxValidationOutput int `json:"max_validation_output,omitempty"`
	MaxRevisions        int `json:"max_revisions,omitempty"`
	// MaxVerificationAttempts bounds how many times the coordinator will
	// collect evidence and retry a review that ends verification_blocked.
	// It is independent of MaxRevisions, which governs coding revisions only.
	MaxVerificationAttempts int `json:"max_verification_attempts,omitempty"`
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
	ID           string          `json:"id"`
	Name         string          `json:"name,omitempty"`
	Description  string          `json:"description,omitempty"`
	AllowedTools []string        `json:"allowed_tools"`
	Budget       Budget          `json:"budget,omitempty"`
	Limits       *ActivityLimits `json:"limits,omitempty"`
}

// Clone returns an isolated copy of the capability profile.
func (p CapabilityProfile) Clone() CapabilityProfile {
	tools := make([]string, len(p.AllowedTools))
	copy(tools, p.AllowedTools)
	var limCopy *ActivityLimits
	if p.Limits != nil {
		lim := *p.Limits
		limCopy = &lim
	}
	return CapabilityProfile{
		ID:           p.ID,
		Name:         p.Name,
		Description:  p.Description,
		AllowedTools: tools,
		Budget:       p.Budget,
		Limits:       limCopy,
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
	Limits               *ActivityLimits    `json:"limits,omitempty"`
	Runtime              string             `json:"runtime,omitempty"`
	Model                string             `json:"model,omitempty"`
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

// ReviewVerdict represents an explicit, typed peer review outcome.
type ReviewVerdict string

const (
	ReviewVerdictApproved   ReviewVerdict = "approved"
	ReviewVerdictRejected   ReviewVerdict = "rejected"
	ReviewVerdictIncomplete ReviewVerdict = "incomplete"
	// ReviewVerdictVerificationBlocked records that the reviewer could not
	// establish correctness because evidence or capability was unavailable,
	// WITHOUT asserting any implementation defect. It must never consume a
	// coding revision.
	ReviewVerdictVerificationBlocked ReviewVerdict = "verification_blocked"
)

// ReviewResult encapsulates deterministic peer review artifacts.
type ReviewResult struct {
	ActivityID       string        `json:"activity_id"`
	ReviewerID       string        `json:"reviewer_id"`
	Verdict          ReviewVerdict `json:"verdict"` // "approved", "rejected", "incomplete"
	Approved         bool          `json:"approved"`
	Comments         string        `json:"comments"`
	Blockers         []string      `json:"blockers,omitempty"`
	EvidenceID       string        `json:"evidence_id,omitempty"`
	ReviewedSnapshot string        `json:"reviewed_snapshot,omitempty"`
	HostID           string        `json:"host_id,omitempty"`
	Model            string        `json:"model,omitempty"`
	Timestamp        time.Time     `json:"timestamp"`
}

type IntentStatus string

const (
	IntentPersisted IntentStatus = "persisted"
	IntentCommitted IntentStatus = "committed"
	IntentUncertain IntentStatus = "uncertain"
)

// IntentRecord logs the durable pre-mutation intent before touching external systems.
type IntentRecord struct {
	IntentID    string       `json:"intent_id"`
	ActivityID  string       `json:"activity_id"`
	Action      string       `json:"action"` // e.g. "create_pull_request"
	Target      string       `json:"target"` // e.g. "branch/feature-1"
	PayloadHash string       `json:"payload_hash"`
	Status      IntentStatus `json:"status"`
	ExternalID  string       `json:"external_id,omitempty"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
}

// ValidationEvidence represents verifiable proof that tests ran against a specific snapshot.
type ValidationEvidence struct {
	EvidenceID          string        `json:"evidence_id"`
	ActivityID          string        `json:"activity_id,omitempty"`
	TaskID              string        `json:"task_id,omitempty"`
	WorkspaceDir        string        `json:"workspace_dir,omitempty"`
	WorkspaceSnapshotID string        `json:"workspace_snapshot_id"`
	Command             []string      `json:"command"`
	ExitCode            int           `json:"exit_code"`
	OutputSummary       string        `json:"output_summary,omitempty"`
	Duration            time.Duration `json:"duration,omitempty"`
	LogPath             string        `json:"log_path,omitempty"`
	HostID              string        `json:"host_id,omitempty"`
	AgentID             string        `json:"agent_id,omitempty"`
	StartedAt           time.Time     `json:"started_at"`
	CompletedAt         time.Time     `json:"completed_at"`
	Valid               bool          `json:"valid"`
	// TestsExecuted is the number of test cases the command actually ran.
	// Zero means the command was vacuous (e.g. `go test` on a repository
	// with no Go test files) and therefore proves nothing about the change.
	TestsExecuted int `json:"tests_executed,omitempty"`
	// Vacuous marks evidence whose command ran but exercised no tests.
	// Vacuous evidence is never treated as VALID: it is a verification gap,
	// not proof of correctness.
	Vacuous bool `json:"vacuous,omitempty"`
	// SourceRevision is the git HEAD the verification validated, so evidence
	// is tied to the code revision under review.
	SourceRevision string `json:"source_revision,omitempty"`
}

// CoderHandoff is the structured completion artifact a coder publishes for a
// reviewer. It exists so a reviewer can verify without re-deriving what the
// coder already established. Every field is optional; missing fields are
// reported as verification gaps rather than defects.
type CoderHandoff struct {
	FilesChanged         []string `json:"files_changed,omitempty"`
	FilesAdded           []string `json:"files_added,omitempty"`
	BehaviorSummary      string   `json:"behavior_summary,omitempty"`
	TestsAdded           []string `json:"tests_added,omitempty"`
	TestsUpdated         []string `json:"tests_updated,omitempty"`
	VerificationCmds     []string `json:"verification_commands,omitempty"`
	TestExitCodes        []int    `json:"test_exit_codes,omitempty"`
	TestLogPaths         []string `json:"test_log_paths,omitempty"`
	KnownLimitations     []string `json:"known_limitations,omitempty"`
	ValidationEvidenceID string   `json:"validation_evidence_id,omitempty"`
}

// ReviewFinding is one item surfaced to the reviewer, either a concrete defect
// candidate or a diff-hygiene warning the reviewer must adjudicate.
type ReviewFinding struct {
	Kind    string `json:"kind"` // "defect" | "diff_hygiene" | "verification_gap"
	Path    string `json:"path,omitempty"`
	Message string `json:"message"`
}

// TaskCheckpoint captures authoritative task and engineering state for cross-host continuation.
type TaskCheckpoint struct {
	TaskID                   string              `json:"task_id"`
	Objective                string              `json:"objective"`
	ActiveActivityID         string              `json:"active_activity_id,omitempty"`
	SpecialistID             string              `json:"specialist_id"`
	SpecialistRole           string              `json:"specialist_role,omitempty"`
	WorktreePath             string              `json:"worktree_path"`
	WorktreeBranch           string              `json:"worktree_branch"`
	CurrentSnapshotSHA       string              `json:"current_snapshot_sha"`
	GitStatusSummary         string              `json:"git_status_summary,omitempty"`
	ModifiedFiles            []string            `json:"modified_files,omitempty"`
	NewFiles                 []string            `json:"new_files,omitempty"`
	AcceptedDecisions        []string            `json:"accepted_decisions,omitempty"`
	CompletedWork            []string            `json:"completed_work,omitempty"`
	RemainingObligations     []string            `json:"remaining_obligations,omitempty"`
	ValidationEvidence       *ValidationEvidence `json:"validation_evidence,omitempty"`
	Strategy                 ExecutionStrategy   `json:"strategy"`
	SourceRuntime            string              `json:"source_runtime,omitempty"`
	TargetRuntime            string              `json:"target_runtime,omitempty"`
	Model                    string              `json:"model,omitempty"`
	FailureReason            string              `json:"failure_reason,omitempty"`
	InterruptionKind         ProviderFailureKind `json:"interruption_kind,omitempty"`
	InterruptionMessage      string              `json:"interruption_message,omitempty"`
	ContinuationInstructions string              `json:"continuation_instructions,omitempty"`
	UpdatedAt                time.Time           `json:"updated_at"`
}

type ResumeTaskRequest struct {
	TaskID       string `json:"task_id"`
	Runtime      string `json:"runtime,omitempty"`
	Model        string `json:"model,omitempty"`
	Specialist   string `json:"specialist,omitempty"`
	Instructions string `json:"instructions,omitempty"`
}

type EscalateTaskRequest struct {
	TaskID      string            `json:"task_id"`
	NewStrategy ExecutionStrategy `json:"new_strategy"` // "reviewed" or "team"
	Reviewer    string            `json:"reviewer,omitempty"`
	Reason      string            `json:"reason,omitempty"`
}
