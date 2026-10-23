package a4c

import (
	"errors"
	"time"
)

var (
	ErrTaskNotFound             = errors.New("service: task not found")
	ErrInvalidStateTransition   = errors.New("service: invalid lifecycle state transition")
	ErrSnapshotMismatch         = errors.New("service: snapshot identity modified after acceptance; re-review required")
	ErrWorkingTreeDirty         = errors.New("service: base working checkout contains uncommitted changes; integration aborted")
	ErrTaskAlreadyAccepted      = errors.New("service: task already accepted")
	ErrTaskAlreadyIntegrated    = errors.New("service: task already integrated")
	ErrTaskNotReadyForReview    = errors.New("service: task is not awaiting human review")
	ErrTaskNotAccepted          = errors.New("service: task must be accepted before integration")
	ErrActivityGraphIncomplete  = errors.New("service: task activity graph is not successfully complete")
	ErrReviewRequired           = errors.New("service: strategy requires an approved review before acceptance")
	ErrDelegationNotPermitted   = errors.New("service: direct mode does not delegate activities; complete the work in the root activity")
	ErrTaskFinalized            = errors.New("service: task is finalized; new activities would invalidate the accepted result")
)

// TaskLifecycleState describes the end-to-end status of a root task from assignment to integration.
// Derived from authoritative activity execution state + persisted human lifecycle facts.
type TaskLifecycleState string

const (
	TaskStateQueued              TaskLifecycleState = "queued"
	TaskStateRunning             TaskLifecycleState = "running"
	TaskStateAwaitingDecision    TaskLifecycleState = "awaiting_decision"
	TaskStateAwaitingHumanReview TaskLifecycleState = "awaiting_human_review"
	TaskStateRevisionRequested   TaskLifecycleState = "revision_requested"
	TaskStateAccepted            TaskLifecycleState = "accepted"
	TaskStateIntegrated          TaskLifecycleState = "integrated"
	TaskStateRejected            TaskLifecycleState = "rejected"
	TaskStateFailed              TaskLifecycleState = "failed"
	TaskStateInterrupted         TaskLifecycleState = "interrupted"
)

// RevisionRecord captures a human-requested revision and its feedback.
type RevisionRecord struct {
	RevisionID         string    `json:"revision_id"`
	Feedback           string    `json:"feedback"`
	TargetAgent        string    `json:"target_agent"` // e.g. "coder"
	RevisionActivityID string    `json:"revision_activity_id,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
}

// HumanLifecycleRecord persists non-derivable human decisions for a task.
type HumanLifecycleRecord struct {
	TaskID                 string           `json:"task_id"`
	HumanConfirmationToken string           `json:"human_confirmation_token,omitempty"` // Secret token required for human approval via MCP
	Accepted               bool             `json:"accepted"`
	AcceptedAt             time.Time        `json:"accepted_at,omitempty"`
	AcceptedBy             string           `json:"accepted_by,omitempty"`
	AcceptedSnapshot       string           `json:"accepted_snapshot,omitempty"`
	AcceptComments         string           `json:"accept_comments,omitempty"`
	Integrated             bool             `json:"integrated"`
	IntegratedAt           time.Time        `json:"integrated_at,omitempty"`
	IntegratedBy           string           `json:"integrated_by,omitempty"`
	IntegratedCommit       string           `json:"integrated_commit,omitempty"`
	// VersionBump records the patch-level release rule attached to an
	// acceptance: the version the workspace carried when the change was
	// approved and the successor that integration must land. It is decided
	// at acceptance (when the human signs off) and applied at integration,
	// so a rejected or abandoned task never advances the release.
	VersionBumpFrom string           `json:"version_bump_from,omitempty"`
	VersionBumpTo   string           `json:"version_bump_to,omitempty"`
	Rejected        bool             `json:"rejected"`
	RejectedAt      time.Time        `json:"rejected_at,omitempty"`
	RejectionReason string           `json:"rejection_reason,omitempty"`
	Revisions       []RevisionRecord `json:"revisions,omitempty"`
	UpdatedAt       time.Time        `json:"updated_at"`
}

// SubmitTaskRequest parameters for task delegation.
type SubmitTaskRequest struct {
	Objective    string            `json:"objective"`
	BudgetTokens float64           `json:"budget_tokens,omitempty"`
	Strategy     ExecutionStrategy `json:"strategy,omitempty"` // direct (default), reviewed, team
	Specialist   string            `json:"specialist,omitempty"`
	Runtime      string            `json:"runtime,omitempty"`  // copilot, opencode, fake
	Model        string            `json:"model,omitempty"`
	Inputs       map[string]any    `json:"inputs,omitempty"`
}

// TaskRef is a lightweight identifier returned immediately on submission.
type TaskRef struct {
	TaskID    string             `json:"task_id"`
	State     TaskLifecycleState `json:"state"`
	CreatedAt time.Time          `json:"created_at"`
}

// TaskSummary provides a high-level view of a task for listings.
type TaskSummary struct {
	TaskID            string             `json:"task_id"`
	Objective         string             `json:"objective"`
	State             TaskLifecycleState `json:"state"`
	Strategy          ExecutionStrategy  `json:"strategy"`
	ActiveAgent       string             `json:"active_agent,omitempty"`
	TotalTokens       int                `json:"total_tokens"`
	AllocatedBudget   float64            `json:"allocated_budget"`
	SubtaskCount      int                `json:"subtask_count"`
	CreatedAt         time.Time          `json:"created_at"`
	UpdatedAt         time.Time          `json:"updated_at"`
}

// TaskDetails provides comprehensive inspection data for a task.
type TaskDetails struct {
	TaskID            string                 `json:"task_id"`
	Objective         string                 `json:"objective"`
	State             TaskLifecycleState     `json:"state"`
	Strategy          ExecutionStrategy      `json:"strategy"`
	Runtime           string                 `json:"runtime,omitempty"`
	Model             string                 `json:"model,omitempty"`
	WorktreePath      string                 `json:"worktree_path,omitempty"`
	WorktreeBranch    string                 `json:"worktree_branch,omitempty"`
	CurrentSnapshotID string                 `json:"current_snapshot_id,omitempty"`
	AllocatedBudget   float64                `json:"allocated_budget"`
	ConsumedBudget    float64                `json:"consumed_budget"`
	Metrics           *ExecutionMetrics      `json:"metrics,omitempty"`
	Activities        []ActivityHistoryEntry `json:"activities"`
	LifecycleFacts    *HumanLifecycleRecord  `json:"lifecycle_facts,omitempty"`
	EvidenceIDs       []string               `json:"evidence_ids,omitempty"`
	ReviewVerdict     string                 `json:"review_verdict,omitempty"`
	ReviewComments    string                 `json:"review_comments,omitempty"`
	KnowledgeCoverage *TaskKnowledgeCoverage `json:"knowledge_coverage,omitempty"`
	CreatedAt         time.Time              `json:"created_at"`
	UpdatedAt         time.Time              `json:"updated_at"`
}

// TaskFilter defines criteria for querying tasks.
type TaskFilter struct {
	State   TaskLifecycleState `json:"state,omitempty"`
	Runtime string             `json:"runtime,omitempty"`
	Limit   int                `json:"limit,omitempty"`
}

// FileDiff describes modifications to a single file.
type FileDiff struct {
	Path      string `json:"path"`
	OldPath   string `json:"old_path,omitempty"`
	Status    string `json:"status"` // modified, added, deleted
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Patch     string `json:"patch,omitempty"`
}

// DiffResult provides full review diffs for human evaluation.
type DiffResult struct {
	TaskID       string     `json:"task_id"`
	SnapshotID   string     `json:"snapshot_id"`
	BaseBranch   string     `json:"base_branch"`
	Files        []FileDiff `json:"files"`
	RawDiff      string     `json:"raw_diff"`
	TotalAdds    int        `json:"total_additions"`
	TotalDels    int        `json:"total_deletions"`
	TotalFiles   int        `json:"total_files"`
}

// FeedbackRequest submits human review guidance and requests revisions.
type FeedbackRequest struct {
	TaskID      string `json:"task_id"`
	Feedback    string `json:"feedback"`
	TargetAgent string `json:"target_agent,omitempty"` // defaults to "coder"
}

// AcceptTaskRequest parameters for recording human approval.
type AcceptTaskRequest struct {
	TaskID            string `json:"task_id"`
	Comments          string `json:"comments,omitempty"`
	AcceptedBy        string `json:"accepted_by,omitempty"`
	ConfirmationToken string `json:"confirmation_token,omitempty"`
	IsInteractive     bool   `json:"is_interactive,omitempty"`
}

// AcceptResult confirms human acceptance without modifying the working tree.
type AcceptResult struct {
	TaskID              string    `json:"task_id"`
	AcceptedSnapshot    string    `json:"accepted_snapshot"`
	AcceptedAt          time.Time `json:"accepted_at"`
	Comments            string    `json:"comments,omitempty"`
	AuthorizationMethod string    `json:"authorization_method,omitempty"`
	// VersionBumpTo is the patch-level successor recorded with this
	// approval; empty when the workspace carries no release version file.
	VersionBumpTo string   `json:"version_bump_to,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
}

// IntegrateTaskRequest applies an accepted task to the working tree.
type IntegrateTaskRequest struct {
	TaskID            string `json:"task_id"`
	ConfirmationToken string `json:"confirmation_token,omitempty"`
	IntegratedBy      string `json:"integrated_by,omitempty"`
}

// IntegrateResult confirms integration of approved changes.
type IntegrateResult struct {
	TaskID           string    `json:"task_id"`
	IntegratedCommit string    `json:"integrated_commit"`
	IntegratedAt     time.Time `json:"integrated_at"`
	FilesChanged     int       `json:"files_changed"`
	// Version is the release version the repository carries after
	// integration; empty when the workspace has no release version file.
	Version string `json:"version,omitempty"`
}

// RejectTaskRequest parameters for human rejection.
type RejectTaskRequest struct {
	TaskID string `json:"task_id"`
	Reason string `json:"reason"`
}

// RejectResult confirms task rejection.
type RejectResult struct {
	TaskID     string    `json:"task_id"`
	RejectedAt time.Time `json:"rejected_at"`
	Reason     string    `json:"reason"`
}

// TeamMemberInfo summarizes a persistent team member.
type TeamMemberInfo struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Role         string   `json:"role"`
	Charter      string   `json:"charter"`
	AllowedTools []string `json:"allowed_tools"`
}

// TeamContextResult provides environmental context for host-native coordinators.
type TeamContextResult struct {
	WorkspaceRoot  string           `json:"workspace_root"`
	CurrentBranch  string           `json:"current_branch"`
	HeadCommit     string           `json:"head_commit"`
	TeamMembers    []TeamMemberInfo `json:"team_members"`
	ActiveTasks    []TaskSummary    `json:"active_tasks"`
	RecentMemories []string         `json:"recent_memories"`
	KnowledgeDir   string           `json:"knowledge_dir,omitempty"`
}

// KnowledgeClaim is the knowledge disposition an agent reports with activity
// completion. An absent claim is NOT equivalent to Significant=false: the
// assessment is expected for A4C-managed tasks, while actual document creation
// remains optional.
type KnowledgeClaim struct {
	// Significant reports whether reusable engineering knowledge emerged.
	Significant bool `json:"significant"`
	// Reason explains a non-significant disposition (e.g. "routine fix").
	Reason string `json:"reason,omitempty"`
	// Paths are knowledge-dir-relative documents the agent already wrote.
	Paths []string `json:"paths,omitempty"`
	// Content is an inline document used when the coding host cannot write
	// outside its workspace. FileName (or Paths[0]) names the destination.
	Content  string `json:"content,omitempty"`
	FileName string `json:"file_name,omitempty"`
}

// KnowledgeOutcome is A4C's verified record of a knowledge claim. Status is
// one of: "assessed" (declared not significant), "verified" (documents exist),
// "pending" (significant but not preserved), or "unavailable" (KB could not
// be resolved). TaskLevel marks the root (task-level) assessment.
type KnowledgeOutcome struct {
	Significant bool                 `json:"significant"`
	Status      string               `json:"status"`
	Reason      string               `json:"reason,omitempty"`
	Paths       []string             `json:"paths,omitempty"`
	Verified    []KnowledgePathStatus `json:"verified,omitempty"`
	Actor       string               `json:"actor,omitempty"`
	TaskLevel   bool                 `json:"task_level,omitempty"`
}

// TaskKnowledgeCoverage is derived knowledge-accountability state for a task.
type TaskKnowledgeCoverage struct {
	// AssessmentMissing: no completion on the task carried any knowledge claim.
	AssessmentMissing bool `json:"assessment_missing"`
	// AssessmentRecorded: the root (task-level) completion carries a claim.
	AssessmentRecorded bool `json:"assessment_recorded"`
	// AssessmentSignificant / AssessmentStatus / AssessmentPaths describe the
	// task-level assessment when recorded.
	AssessmentSignificant bool     `json:"assessment_significant"`
	AssessmentStatus      string   `json:"assessment_status,omitempty"`
	AssessmentPaths       []string `json:"assessment_paths,omitempty"`
	// ActivityCandidates counts activity-level significant claims below the root.
	ActivityCandidates int `json:"activity_candidates"`
	// Pending / Unverified list human-readable obligation reasons.
	Pending   []string `json:"pending,omitempty"`
	Unverified []string `json:"unverified,omitempty"`
}

// BeginTaskRequest begins an engineering task without launching a standalone inference loop.
type BeginTaskRequest struct {
	Objective    string            `json:"objective"`
	BudgetTokens float64           `json:"budget_tokens,omitempty"`
	Strategy     ExecutionStrategy `json:"strategy,omitempty"`
	Coordinator  string            `json:"coordinator,omitempty"` // default "alice"
	Runtime      string            `json:"runtime,omitempty"`     // "copilot", "opencode", "antigravity"
}

// BeginTaskResult returns details of the newly created task and its isolated worktree.
type BeginTaskResult struct {
	TaskID         string             `json:"task_id"`
	State          TaskLifecycleState `json:"state"`
	// Strategy is the EFFECTIVE mode after resolving any request value against
	// the workspace default, so a caller can report what actually ran.
	Strategy       ExecutionStrategy  `json:"strategy"`
	Coordinator    string             `json:"coordinator"`
	WorktreePath   string             `json:"worktree_path"`
	WorktreeBranch string             `json:"worktree_branch"`
	CreatedAt      time.Time          `json:"created_at"`
}

// AssignActivityRequest records that the coordinator dispatched work to a specialist.
type AssignActivityRequest struct {
	TaskID       string   `json:"task_id"`
	Agent        string   `json:"agent"` // e.g. "bob"
	Role         string   `json:"role"`  // e.g. "coder"
	Objective    string   `json:"objective"`
	Input        string   `json:"input,omitempty"`
	AllowedTools []string `json:"allowed_tools,omitempty"`
}

// ActivityRef identifies an active specialist assignment.
type ActivityRef struct {
	ActivityID   string    `json:"activity_id"`
	TaskID       string    `json:"task_id"`
	Agent        string    `json:"agent"`
	Role         string    `json:"role"`
	WorktreePath string    `json:"worktree_path"`
	AssignedAt   time.Time `json:"assigned_at"`
}

// CheckpointRequest records intermediate progress, discoveries, and state.
type CheckpointRequest struct {
	TaskID         string   `json:"task_id"`
	ActivityID     string   `json:"activity_id,omitempty"`
	Actor          string   `json:"actor"` // e.g. "alice", "bob"
	Summary        string   `json:"summary"`
	Assumptions    []string `json:"assumptions,omitempty"`
	Findings       []string `json:"findings,omitempty"`
	FilesInspected []string `json:"files_inspected,omitempty"`
	NextSteps      []string `json:"next_steps,omitempty"`
}

// CheckpointResult confirms durable checkpoint persistence.
type CheckpointResult struct {
	CheckpointID string    `json:"checkpoint_id"`
	TaskID       string    `json:"task_id"`
	Actor        string    `json:"actor"`
	RecordedAt   time.Time `json:"recorded_at"`
	SnapshotSHA  string    `json:"snapshot_sha,omitempty"`
}

// CompleteActivityRequest records completion of a specialist's assignment.
// HostUsage carries provider-reported consumption for one activity. Every
// field is optional: hosts report only what their own telemetry actually
// provides. Unreported values are absent (nil), never presented as measured
// zeros, and provider accounting units are preserved as reported (Copilot AIU
// credits, OpenCode USD, Antigravity unpriced quota) — never converted or
// conflated.
type HostUsage struct {
	InputTokens      *int     `json:"input_tokens,omitempty"`       // uncached input as the host reports it
	CacheReadTokens  *int     `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens *int     `json:"cache_write_tokens,omitempty"`
	OutputTokens     *int     `json:"output_tokens,omitempty"`
	ReasoningTokens  *int     `json:"reasoning_tokens,omitempty"` // host semantics: subset of output or separate
	ModelInvocations *int     `json:"model_invocations,omitempty"`
	CostReported     *float64 `json:"cost_reported,omitempty"`
	CostUnit         string   `json:"cost_unit,omitempty"` // "usd", "ai_credits", "unpriced"
	Model            string   `json:"model,omitempty"`
	Source           string   `json:"source,omitempty"` // host id: "copilot", "opencode", "antigravity"
	// Scope is "activity" (default) or "session_including_subagents" when the
	// reported totals already include subagent consumption.
	Scope string `json:"scope,omitempty"`
}

type CompleteActivityRequest struct {
	TaskID             string         `json:"task_id"`
	ActivityID         string         `json:"activity_id"`
	Actor              string         `json:"actor"`
	Summary            string         `json:"summary"`
	FilesModified      []string       `json:"files_modified,omitempty"`
	ValidationEvidence string         `json:"validation_evidence,omitempty"`
	Success            bool           `json:"success"`
	Knowledge          *KnowledgeClaim `json:"knowledge,omitempty"`
	// Usage is optional host telemetry; completion never depends on it.
	Usage *HostUsage `json:"usage,omitempty"`
}

// HostActivityResult represents the result of completing a specialist activity.
type HostActivityResult struct {
	ActivityID    string             `json:"activity_id"`
	TaskID        string             `json:"task_id"`
	Actor         string             `json:"actor"`
	Role          string             `json:"role"`
	Status        string             `json:"status"`
	Summary       string             `json:"summary"`
	FilesModified []string           `json:"files_modified,omitempty"`
	Evidence      []string           `json:"evidence,omitempty"`
	Knowledge     *KnowledgeOutcome  `json:"knowledge,omitempty"`
	CompletedAt   time.Time          `json:"completed_at"`
}

// RecordReviewRequest records an independent reviewer verdict.
type RecordReviewRequest struct {
	TaskID      string   `json:"task_id"`
	Reviewer    string   `json:"reviewer"` // e.g. "charlie"
	Verdict     string   `json:"verdict"`  // "approved", "rejected", "revisions_requested"
	Comments    string   `json:"comments"`
	Findings    []string `json:"findings,omitempty"`
	SnapshotSHA string   `json:"snapshot_sha,omitempty"`
}

// HostReviewResult confirms review verdict persistence.
type HostReviewResult struct {
	TaskID         string             `json:"task_id"`
	Reviewer       string             `json:"reviewer"`
	Verdict        string             `json:"verdict"`
	ReviewedAt     time.Time          `json:"reviewed_at"`
	LifecycleState TaskLifecycleState `json:"lifecycle_state"`
	SnapshotSHA    string             `json:"snapshot_sha"`
}

// AdoptWorkRequest encapsulates parameters to adopt external ongoing development into A4C.
type AdoptWorkRequest struct {
	Objective    string            `json:"objective"`
	HandoffNotes string            `json:"handoff_notes,omitempty"`
	SourceHost   string            `json:"source_host,omitempty"` // e.g. "antigravity"
	Runtime      string            `json:"runtime,omitempty"`     // Target host to continue with e.g. "copilot", "opencode"
	Model        string            `json:"model,omitempty"`
	Specialist   string            `json:"specialist,omitempty"`
	Strategy     ExecutionStrategy `json:"strategy,omitempty"`
	BudgetTokens float64           `json:"budget_tokens,omitempty"`
	InPlace      bool              `json:"in_place,omitempty"` // If true, binds current workspace without creating separate worktree
}

// AdoptWorkResult returns details of the newly adopted A4C task.
type AdoptWorkResult struct {
	TaskID             string          `json:"task_id"`
	WorkspacePath      string          `json:"workspace_path"`
	Branch             string          `json:"branch"`
	ModifiedFiles      []string        `json:"modified_files"`
	NewFiles           []string        `json:"new_files"`
	TotalChanges       int             `json:"total_changes"`
	Checkpoint         *TaskCheckpoint `json:"checkpoint"`
	ContinuationPrompt string          `json:"continuation_prompt,omitempty"`
}

