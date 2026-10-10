package a4c

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// TeamService defines the canonical application interface consumed by CLI, MCP, and workers.
type TeamService interface {
	SubmitTask(ctx context.Context, req SubmitTaskRequest) (*TaskRef, error)
	GetTask(ctx context.Context, taskID string) (*TaskDetails, error)
	ListTasks(ctx context.Context, filter TaskFilter) ([]TaskSummary, error)
	GetTaskDiff(ctx context.Context, taskID string) (*DiffResult, error)
	SubmitFeedback(ctx context.Context, req FeedbackRequest) (*TaskRef, error)
	AcceptTask(ctx context.Context, req AcceptTaskRequest) (*AcceptResult, error)
	IntegrateTask(ctx context.Context, req IntegrateTaskRequest) (*IntegrateResult, error)
	RejectTask(ctx context.Context, req RejectTaskRequest) (*RejectResult, error)
	ResumeTask(ctx context.Context, taskID string) error
	ExecuteTask(ctx context.Context, taskID string, maxSteps int) ([]*ActivityResult, error)

	// Host-Native Coordination Interface (Mode A)
	GetTeamContext(ctx context.Context) (*TeamContextResult, error)
	BeginTask(ctx context.Context, req BeginTaskRequest) (*BeginTaskResult, error)
	AssignActivity(ctx context.Context, req AssignActivityRequest) (*ActivityRef, error)
	RecordCheckpoint(ctx context.Context, req CheckpointRequest) (*CheckpointResult, error)
	CompleteActivity(ctx context.Context, req CompleteActivityRequest) (*HostActivityResult, error)
	RecordReview(ctx context.Context, req RecordReviewRequest) (*HostReviewResult, error)
	GetWorktreePath(taskID string) (string, error)
	WorkspaceDir() string
}


// TeamServiceImpl is the default production implementation of TeamService.
type TeamServiceImpl struct {
	mu             sync.Mutex
	workspaceDir   string
	store          *DurableActivityStore
	lifecycleStore *DurableLifecycleStore
	coordinator    *TeamCoordinator
	roster         *TeamRoster
	memory         *DurableMemoryStore
	journal        *DurableIntentJournal
	wtMgr          *WorktreeManager
	structStore    *DurableStructuralStore
}

// NewTeamService initializes a TeamServiceImpl over existing A4C durable subsystems.
func NewTeamService(
	workspaceDir string,
	store *DurableActivityStore,
	lifecycleStore *DurableLifecycleStore,
	coordinator *TeamCoordinator,
	roster *TeamRoster,
	memory *DurableMemoryStore,
	journal *DurableIntentJournal,
	wtMgr *WorktreeManager,
	structStore *DurableStructuralStore,
) *TeamServiceImpl {
	if workspaceDir == "" {
		workspaceDir = "."
	}
	return &TeamServiceImpl{
		workspaceDir:   workspaceDir,
		store:          store,
		lifecycleStore: lifecycleStore,
		coordinator:    coordinator,
		roster:         roster,
		memory:         memory,
		journal:        journal,
		wtMgr:          wtMgr,
		structStore:    structStore,
	}
}

func (s *TeamServiceImpl) Coordinator() *TeamCoordinator {
	return s.coordinator
}

func (s *TeamServiceImpl) Store() *DurableActivityStore {
	return s.store
}

func (s *TeamServiceImpl) Journal() *DurableIntentJournal {
	return s.journal
}

func (s *TeamServiceImpl) LifecycleStore() *DurableLifecycleStore {
	return s.lifecycleStore
}

func (s *TeamServiceImpl) WorktreeManager() *WorktreeManager {
	return s.wtMgr
}

func (s *TeamServiceImpl) WorkspaceDir() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.workspaceDir
}

func (s *TeamServiceImpl) GetWorktreePath(taskID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wtMgr == nil {
		return s.workspaceDir, nil
	}
	rec, err := s.wtMgr.GetWorktree(taskID)
	if err != nil {
		return "", err
	}
	if rec == nil || rec.WorktreePath == "" {
		return s.workspaceDir, nil
	}
	return s.absPath(rec.WorktreePath), nil
}

// SubmitTask enqueues a new assignment under the configured execution strategy.
func (s *TeamServiceImpl) SubmitTask(ctx context.Context, req SubmitTaskRequest) (*TaskRef, error) {
	if strings.TrimSpace(req.Objective) == "" {
		return nil, fmt.Errorf("service: objective cannot be empty")
	}

	strategy := req.Strategy
	if strategy == "" {
		strategy = StrategyTeam
	}

	budgetTokens := req.BudgetTokens
	if budgetTokens <= 0 {
		budgetTokens = 50000.0
	}

	inputs := make(map[string]any)
	if req.Inputs != nil {
		for k, v := range req.Inputs {
			inputs[k] = v
		}
	}
	if req.Runtime != "" {
		inputs["runtime"] = req.Runtime
	}
	if req.Model != "" {
		inputs["model"] = req.Model
	}

	actReq, err := s.coordinator.SubmitTaskWithStrategy(
		req.Objective,
		Budget{Allocated: budgetTokens},
		inputs,
		strategy,
	)
	if err != nil {
		return nil, fmt.Errorf("service: submit task: %w", err)
	}

	taskRef := &TaskRef{
		TaskID:    actReq.ID,
		State:     TaskStateQueued,
		CreatedAt: actReq.CreatedAt,
	}
	return taskRef, nil
}

// GetTask reconstructs the full task audit trail and derives its authoritative lifecycle state.
func (s *TeamServiceImpl) GetTask(ctx context.Context, taskID string) (*TaskDetails, error) {
	state, lifecycle, err := s.deriveTaskState(taskID)
	if err != nil {
		return nil, err
	}

	report, err := BuildTaskHistory(taskID, s.store, s.journal, s.wtMgr, s.memory)
	if err != nil {
		return nil, fmt.Errorf("service: build history for %s: %w", taskID, err)
	}

	details := &TaskDetails{
		TaskID:          report.TaskID,
		Objective:       report.OriginalObjective,
		State:           state,
		Strategy:        report.Strategy,
		WorktreeBranch:  report.Branch,
		AllocatedBudget: report.AllocatedBudget,
		ConsumedBudget:  report.ConsumedBudget,
		Metrics:         report.MetricsSummary,
		Activities:      report.Activities,
		LifecycleFacts:  lifecycle,
		EvidenceIDs:     report.EvidenceIDs,
		CreatedAt:       report.CreatedAt,
		UpdatedAt:       time.Now(),
	}

	if s.wtMgr != nil {
		if wt, err := s.wtMgr.GetWorktree(taskID); err == nil && wt != nil {
			details.WorktreePath = wt.WorktreePath
		}
	}
	if details.WorktreePath == "" {
		details.WorktreePath = s.workspaceDir
	}

	details.CurrentSnapshotID = s.resolveCurrentSnapshot(taskID)

	// Resolve review outcome
	for _, act := range report.Activities {
		if act.AgentID == "reviewer" && act.Outputs != nil {
			if approved, ok := act.Outputs["approved"].(bool); ok {
				if approved {
					details.ReviewVerdict = "approved"
				} else {
					details.ReviewVerdict = "rejected"
				}
			}
			if comments, ok := act.Outputs["review_comments"].(string); ok {
				details.ReviewComments = comments
			}
		}
	}

	return details, nil
}

// ListTasks returns summaries of all root tasks matching filter criteria.
func (s *TeamServiceImpl) ListTasks(ctx context.Context, filter TaskFilter) ([]TaskSummary, error) {
	allRecs := s.store.ListAllActivities()
	var rootRecs []*ActivityRecord
	for _, rec := range allRecs {
		if rec.Request.ParentID == "" {
			rootRecs = append(rootRecs, rec)
		}
	}

	var summaries []TaskSummary
	for _, rec := range rootRecs {
		taskID := rec.Request.ID
		state, _, err := s.deriveTaskState(taskID)
		if err != nil {
			continue
		}

		if filter.State != "" && state != filter.State {
			continue
		}
		if filter.Runtime != "" && rec.Request.Runtime != filter.Runtime {
			continue
		}

		subtasks := s.store.GetTaskTree(taskID)
		totalTok := 0
		activeAgent := rec.Request.AgentID
		for _, sub := range subtasks {
			if sub.Result != nil && sub.Result.Metrics != nil {
				totalTok += sub.Result.Metrics.TotalTokens
			}
			if sub.Status == StatusClaimed || sub.Status == StatusPending {
				activeAgent = sub.Request.AgentID
			}
		}

		summary := TaskSummary{
			TaskID:          taskID,
			Objective:       rec.Request.Objective,
			State:           state,
			Strategy:        rec.Request.Strategy,
			ActiveAgent:     activeAgent,
			TotalTokens:     totalTok,
			AllocatedBudget: rec.Request.Budget.Allocated,
			SubtaskCount:    len(subtasks),
			CreatedAt:       rec.Request.CreatedAt,
			UpdatedAt:       time.Now(),
		}
		summaries = append(summaries, summary)
	}

	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].CreatedAt.After(summaries[j].CreatedAt)
	})

	if filter.Limit > 0 && len(summaries) > filter.Limit {
		summaries = summaries[:filter.Limit]
	}

	return summaries, nil
}

// GetTaskDiff computes file modifications between the task worktree and the base repository.
func (s *TeamServiceImpl) GetTaskDiff(ctx context.Context, taskID string) (*DiffResult, error) {
	_, err := s.store.Get(taskID)
	if err != nil {
		return nil, ErrTaskNotFound
	}

	workDir := s.workspaceDir
	baseBranch := "HEAD"
	if s.wtMgr != nil {
		if wt, err := s.wtMgr.GetWorktree(taskID); err == nil && wt != nil && wt.WorktreePath != "" {
			workDir = wt.WorktreePath
			if wt.Branch != "" {
				baseBranch = wt.Branch
			}
		}
	}

	snapshotID := s.resolveCurrentSnapshot(taskID)

	// 1. Try uncommitted git diff in workDir
	cmdDiff := exec.CommandContext(ctx, "git", "diff", "HEAD")
	cmdDiff.Dir = workDir
	rawDiffBytes, err := cmdDiff.CombinedOutput()
	rawDiff := string(rawDiffBytes)

	// 2. If uncommitted diff empty, try diff against base branch (committed worktree changes)
	if strings.TrimSpace(rawDiff) == "" {
		cmdBranchDiff := exec.CommandContext(ctx, "git", "diff", "HEAD~1..HEAD")
		cmdBranchDiff.Dir = workDir
		if bOut, bErr := cmdBranchDiff.CombinedOutput(); bErr == nil && len(strings.TrimSpace(string(bOut))) > 0 {
			rawDiff = string(bOut)
		}
	}

	files, totalAdds, totalDels := parseGitDiff(rawDiff)

	// 3. Fallback to IntentJournal if git diff was empty
	if len(files) == 0 && s.journal != nil {
		subtasks := s.store.GetTaskTree(taskID)
		seenFiles := make(map[string]bool)
		for _, sub := range subtasks {
			intents := s.journal.FindIntentsForActivity(sub.Request.ID)
			for _, in := range intents {
				if in.Action == "apply_patch" && in.Target != "" && !seenFiles[in.Target] {
					seenFiles[in.Target] = true
					targetPath := filepath.Join(workDir, in.Target)
					lines := 0
					if data, err := os.ReadFile(targetPath); err == nil {
						lines = len(strings.Split(string(data), "\n"))
					}
					files = append(files, FileDiff{
						Path:      in.Target,
						Status:    "modified",
						Additions: lines,
						Deletions: 0,
					})
					totalAdds += lines
				}
			}
		}
	}

	res := &DiffResult{
		TaskID:     taskID,
		SnapshotID: snapshotID,
		BaseBranch: baseBranch,
		Files:      files,
		RawDiff:    rawDiff,
		TotalAdds:  totalAdds,
		TotalDels:  totalDels,
		TotalFiles: len(files),
	}
	return res, nil
}

// SubmitFeedback records human review feedback and reopens the task for governed revision.
func (s *TeamServiceImpl) SubmitFeedback(ctx context.Context, req FeedbackRequest) (*TaskRef, error) {
	if strings.TrimSpace(req.Feedback) == "" {
		return nil, fmt.Errorf("service: feedback text cannot be empty")
	}

	state, lifecycle, err := s.deriveTaskState(req.TaskID)
	if err != nil {
		return nil, err
	}

	// Feedback is only permitted on tasks awaiting review or rejected
	if state != TaskStateAwaitingHumanReview && state != TaskStateRejected {
		return nil, fmt.Errorf("%w: task in state %s cannot accept review feedback (must be %s)",
			ErrInvalidStateTransition, state, TaskStateAwaitingHumanReview)
	}

	targetAgent := req.TargetAgent
	if targetAgent == "" {
		targetAgent = "coder"
	}

	rootRec, _ := s.store.Get(req.TaskID)
	revActivityID := fmt.Sprintf("%s-rev-%d", req.TaskID, len(lifecycle.Revisions)+1)

	// Record revision fact
	revRecord := RevisionRecord{
		RevisionID:         fmt.Sprintf("rev-%d", len(lifecycle.Revisions)+1),
		Feedback:           req.Feedback,
		TargetAgent:        targetAgent,
		RevisionActivityID: revActivityID,
		CreatedAt:          time.Now(),
	}
	lifecycle.Revisions = append(lifecycle.Revisions, revRecord)
	lifecycle.Accepted = false // Invalidate any prior acceptance on revision
	lifecycle.AcceptedSnapshot = ""
	lifecycle.AcceptedAt = time.Time{}
	lifecycle.AcceptComments = ""
	lifecycle.Rejected = false

	if err := s.lifecycleStore.Save(lifecycle); err != nil {
		return nil, fmt.Errorf("service: save lifecycle feedback: %w", err)
	}

	// Enqueue Coder revision activity
	coderRevReq := ActivityRequest{
		ID:        revActivityID,
		ParentID:  req.TaskID,
		AgentID:   targetAgent,
		Objective: fmt.Sprintf("Address human feedback on %s:\n%s", rootRec.Request.Objective, req.Feedback),
		Budget:    Budget{Allocated: 40000},
		CreatedAt: time.Now(),
	}
	if err := s.store.Enqueue(coderRevReq); err != nil {
		return nil, fmt.Errorf("service: enqueue revision activity: %w", err)
	}

	// Enqueue Reviewer re-review activity
	reviewerRevReq := ActivityRequest{
		ID:        fmt.Sprintf("%s-review", revActivityID),
		ParentID:  req.TaskID,
		AgentID:   "reviewer",
		Objective: fmt.Sprintf("Re-review implementation of revision %s", revActivityID),
		Budget:    Budget{Allocated: 20000},
		DependsOn: []string{revActivityID},
		CreatedAt: time.Now(),
	}
	if err := s.store.Enqueue(reviewerRevReq); err != nil {
		return nil, fmt.Errorf("service: enqueue reviewer revision activity: %w", err)
	}

	return &TaskRef{
		TaskID:    req.TaskID,
		State:     TaskStateRevisionRequested,
		CreatedAt: time.Now(),
	}, nil
}

// AcceptTask records explicit human approval of an exact reviewed snapshot without touching working checkout.
func (s *TeamServiceImpl) AcceptTask(ctx context.Context, req AcceptTaskRequest) (*AcceptResult, error) {
	state, lifecycle, err := s.deriveTaskState(req.TaskID)
	if err != nil {
		return nil, err
	}

	if state == TaskStateAccepted {
		return nil, ErrTaskAlreadyAccepted
	}
	if state == TaskStateIntegrated {
		return nil, ErrTaskAlreadyIntegrated
	}
	if state != TaskStateAwaitingHumanReview {
		return nil, fmt.Errorf("%w: task state is %s (must be %s)",
			ErrTaskNotReadyForReview, state, TaskStateAwaitingHumanReview)
	}

	// Authenticate human approval boundary (Milestone 16 Phase 1)
	// Only explicit human CLI execution (AcceptedBy == "human_cli" or "human") is authenticated.
	// Agents and MCP callers are strictly unauthorized.
	if req.AcceptedBy != "human_cli" && req.AcceptedBy != "human" {
		_ = AppendAudit(s.workspaceDir, "approvals", map[string]any{
			"op": "accept", "task_id": req.TaskID, "outcome": "denied",
			"reason": "unauthorized_caller", "caller": req.AcceptedBy,
		})
		return nil, fmt.Errorf("unauthorized: AI agents cannot approve their own work; manual execution via CLI ('a4c task accept %s') is required", req.TaskID)
	}

	snapshotID := s.resolveCurrentSnapshot(req.TaskID)
	if reviewed := s.reviewedSnapshot(req.TaskID); reviewed != "" && snapshotID != "" && snapshotID != reviewed {
		_ = AppendAudit(s.workspaceDir, "approvals", map[string]any{
			"op": "accept", "task_id": req.TaskID, "outcome": "denied",
			"reason": "stale_or_unreviewed_snapshot", "reviewed_snapshot": reviewed, "current_snapshot": snapshotID,
		})
		return nil, fmt.Errorf("%w: current worktree snapshot %s does not match reviewed snapshot %s", ErrSnapshotMismatch, snapshotID, reviewed)
	}

	lifecycle.Accepted = true
	lifecycle.AcceptedAt = time.Now()
	lifecycle.AcceptedBy = req.AcceptedBy
	if lifecycle.AcceptedBy == "" {
		lifecycle.AcceptedBy = "human"
	}
	lifecycle.AcceptedSnapshot = snapshotID
	lifecycle.AcceptComments = req.Comments
	lifecycle.Rejected = false

	if err := s.lifecycleStore.Save(lifecycle); err != nil {
		return nil, fmt.Errorf("service: save acceptance: %w", err)
	}

	_ = AppendAudit(s.workspaceDir, "approvals", map[string]any{
		"op": "accept", "task_id": req.TaskID, "outcome": "approved",
		"snapshot": snapshotID, "actor": req.AcceptedBy, "comments": req.Comments,
	})

	res := &AcceptResult{
		TaskID:           req.TaskID,
		AcceptedSnapshot: snapshotID,
		AcceptedAt:       lifecycle.AcceptedAt,
		Comments:         req.Comments,
	}
	return res, nil
}

// IntegrateTask applies changes from an accepted task to the base repository working checkout.
func (s *TeamServiceImpl) IntegrateTask(ctx context.Context, req IntegrateTaskRequest) (*IntegrateResult, error) {
	state, lifecycle, err := s.deriveTaskState(req.TaskID)
	if err != nil {
		return nil, err
	}

	if state == TaskStateIntegrated {
		return nil, ErrTaskAlreadyIntegrated
	}
	if lifecycle.Accepted {
		currentSnapshot := s.resolveCurrentSnapshot(req.TaskID)
		if lifecycle.AcceptedSnapshot != "" && currentSnapshot != "" && currentSnapshot != lifecycle.AcceptedSnapshot {
			_ = AppendAudit(s.workspaceDir, "approvals", map[string]any{
				"op": "integrate", "task_id": req.TaskID, "outcome": "denied",
				"reason": "snapshot_mismatch", "accepted_snapshot": lifecycle.AcceptedSnapshot, "current_snapshot": currentSnapshot,
			})
			return nil, fmt.Errorf("%w: accepted %s != current %s", ErrSnapshotMismatch, lifecycle.AcceptedSnapshot, currentSnapshot)
		}
	}
	if state != TaskStateAccepted {
		_ = AppendAudit(s.workspaceDir, "approvals", map[string]any{
			"op": "integrate", "task_id": req.TaskID, "outcome": "denied",
			"reason": "task_not_accepted", "state": state,
		})
		return nil, fmt.Errorf("%w: task state is %s", ErrTaskNotAccepted, state)
	}

	currentSnapshot := s.resolveCurrentSnapshot(req.TaskID)

	// 2. Verify base working checkout is clean (excluding internal .a4c state directory)
	cmdStatus := exec.CommandContext(ctx, "git", "status", "--porcelain")
	cmdStatus.Dir = s.workspaceDir
	if statOut, err := cmdStatus.CombinedOutput(); err == nil {
		lines := strings.Split(strings.TrimSpace(string(statOut)), "\n")
		var dirtyLines []string
		for _, l := range lines {
			l = strings.TrimSpace(l)
			if l == "" || strings.HasPrefix(l, "?? .a4c") || strings.Contains(l, " .a4c/") {
				continue
			}
			dirtyLines = append(dirtyLines, l)
		}
		if len(dirtyLines) > 0 {
			return nil, fmt.Errorf("%w: %s", ErrWorkingTreeDirty, strings.Join(dirtyLines, ", "))
		}
	}

	workDir := s.workspaceDir
	if s.wtMgr != nil {
		if wt, err := s.wtMgr.GetWorktree(req.TaskID); err == nil && wt != nil && wt.WorktreePath != "" {
			workDir = wt.WorktreePath
		}
	}

	// 3. Apply changes from worktree to workspaceDir
	filesChanged := 0
	commitSHA := currentSnapshot

	// If a git worktree branch exists, commit working changes in worktree and perform merge into main workspace
	if s.wtMgr != nil {
		if wt, err := s.wtMgr.GetWorktree(req.TaskID); err == nil && wt != nil {
			if wt.WorktreePath != "" {
				// Stage and commit approved changes in the worktree
				cmdAdd := exec.CommandContext(ctx, "git", "add", "-A")
				cmdAdd.Dir = wt.WorktreePath
				_ = cmdAdd.Run()

				cmdCommit := exec.CommandContext(ctx, "git", "commit", "-m", fmt.Sprintf("feat(a4c): task %s approved changes", req.TaskID))
				cmdCommit.Dir = wt.WorktreePath
				_ = cmdCommit.Run()
			}

			if wt.Branch != "" {
				// Merge the branch into current checkout
				cmdMerge := exec.CommandContext(ctx, "git", "merge", "--no-ff", "-m", fmt.Sprintf("integrate(a4c): task %s", req.TaskID), wt.Branch)
				cmdMerge.Dir = s.workspaceDir
				if mOut, mErr := cmdMerge.CombinedOutput(); mErr != nil {
					// Abort on conflict
					_ = exec.CommandContext(ctx, "git", "merge", "--abort").Run()
					return nil, fmt.Errorf("service: git merge failed during integration: %s (%w)", string(mOut), mErr)
				}
				// Resolve new commit SHA
				cmdRev := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
				cmdRev.Dir = s.workspaceDir
				if revOut, err := cmdRev.CombinedOutput(); err == nil {
					commitSHA = strings.TrimSpace(string(revOut))
				}
			}
		}
	}

	// File copy fallback if direct directory was used (e.g. in non-branch test mode)
	if workDir != s.workspaceDir && commitSHA == "" {
		_ = filepath.Walk(workDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(workDir, path)
			if strings.HasPrefix(rel, ".git") || strings.HasPrefix(rel, ".a4c") || strings.HasPrefix(rel, ".codetopo") {
				return nil
			}
			dest := filepath.Join(s.workspaceDir, rel)
			data, err := os.ReadFile(path)
			if err == nil {
				_ = os.MkdirAll(filepath.Dir(dest), 0755)
				_ = os.WriteFile(dest, data, 0644)
				filesChanged++
			}
			return nil
		})
	}

	lifecycle.Integrated = true
	lifecycle.IntegratedAt = time.Now()
	lifecycle.IntegratedCommit = commitSHA

	if err := s.lifecycleStore.Save(lifecycle); err != nil {
		return nil, fmt.Errorf("service: save integration fact: %w", err)
	}

	return &IntegrateResult{
		TaskID:           req.TaskID,
		IntegratedCommit: commitSHA,
		IntegratedAt:     lifecycle.IntegratedAt,
		FilesChanged:     filesChanged,
	}, nil
}

// RejectTask records explicit human rejection.
func (s *TeamServiceImpl) RejectTask(ctx context.Context, req RejectTaskRequest) (*RejectResult, error) {
	_, lifecycle, err := s.deriveTaskState(req.TaskID)
	if err != nil {
		return nil, err
	}

	lifecycle.Rejected = true
	lifecycle.RejectedAt = time.Now()
	lifecycle.RejectionReason = req.Reason
	lifecycle.Accepted = false

	if err := s.lifecycleStore.Save(lifecycle); err != nil {
		return nil, fmt.Errorf("service: save rejection: %w", err)
	}

	return &RejectResult{
		TaskID:     req.TaskID,
		RejectedAt: lifecycle.RejectedAt,
		Reason:     req.Reason,
	}, nil
}

// ResumeTask resolves blocked decisions or uncommitted intents.
func (s *TeamServiceImpl) ResumeTask(ctx context.Context, taskID string) error {
	_, err := s.store.Get(taskID)
	if err != nil {
		return ErrTaskNotFound
	}

	// Reconcile uncommitted intents if reconciler available
	if s.journal != nil && s.coordinator != nil && s.coordinator.Reconciler != nil {
		_, _ = s.journal.ReconcileUnfinished(ctx, s.coordinator.Reconciler)
	}
	return nil
}

// ExecuteTask executes up to maxSteps for activities under this task tree.
func (s *TeamServiceImpl) ExecuteTask(ctx context.Context, taskID string, maxSteps int) ([]*ActivityResult, error) {
	_, err := s.store.Get(taskID)
	if err != nil {
		return nil, ErrTaskNotFound
	}

	if maxSteps <= 0 {
		maxSteps = 10
	}

	var results []*ActivityResult
	for i := 0; i < maxSteps; i++ {
		if !s.store.HasClaimable() {
			break
		}
		res, err := s.coordinator.Step(ctx)
		if err != nil {
			return results, err
		}
		if res == nil {
			break
		}
		results = append(results, res)
	}

	return results, nil
}

// deriveTaskState derives the active lifecycle state from authoritative activity state and lifecycle facts.
func (s *TeamServiceImpl) deriveTaskState(rootID string) (TaskLifecycleState, *HumanLifecycleRecord, error) {
	rootRec, err := s.store.Get(rootID)
	if err != nil {
		return "", nil, ErrTaskNotFound
	}

	lifecycle, _ := s.lifecycleStore.Get(rootID)
	if lifecycle == nil {
		lifecycle = &HumanLifecycleRecord{TaskID: rootID}
	}

	// 1. Human Lifecycle Facts
	if lifecycle.Integrated {
		return TaskStateIntegrated, lifecycle, nil
	}

	if lifecycle.Accepted {
		// Verify snapshot identity: modifications after acceptance invalidate authorization (Correction 2)
		currentSnap := s.resolveCurrentSnapshot(rootID)
		if lifecycle.AcceptedSnapshot != "" && currentSnap != "" && currentSnap != lifecycle.AcceptedSnapshot {
			return TaskStateAwaitingHumanReview, lifecycle, nil
		}
		return TaskStateAccepted, lifecycle, nil
	}

	if lifecycle.Rejected {
		return TaskStateRejected, lifecycle, nil
	}

	// 2. Authoritative Activity Execution State
	subtasks := s.store.GetTaskTree(rootID)

	hasFailures := false
	hasAwaitingDecision := false
	hasRunning := false
	hasApprovedReview := false
	allCompleted := true

	for _, rec := range subtasks {
		switch rec.Status {
		case StatusFailed:
			hasFailures = true
		case StatusAwaitingHuman:
			hasAwaitingDecision = true
		case StatusClaimed:
			hasRunning = true
			allCompleted = false
		case StatusPending:
			if rec.Request.ID != rootID {
				hasRunning = true
			}
			allCompleted = false
		case StatusCompleted:
			if (rec.Request.AgentID == "reviewer" || rec.Request.AgentID == "charlie" || strings.Contains(rec.Request.ID, "review")) && rec.Result != nil {
				if app, ok := rec.Result.Outputs["approved"].(bool); ok && app {
					hasApprovedReview = true
				}
			}
		}
	}

	if hasFailures {
		return TaskStateFailed, lifecycle, nil
	}
	if hasAwaitingDecision {
		return TaskStateAwaitingDecision, lifecycle, nil
	}
	if hasApprovedReview {
		return TaskStateAwaitingHumanReview, lifecycle, nil
	}

	// If latest action was human feedback, check if revision activity is still active
	if len(lifecycle.Revisions) > 0 {
		latestRev := lifecycle.Revisions[len(lifecycle.Revisions)-1]
		if latestRev.RevisionActivityID != "" {
			revRec, err := s.store.Get(latestRev.RevisionActivityID)
			if err == nil && revRec.Status != StatusCompleted {
				if revRec.Status == StatusClaimed {
					return TaskStateRunning, lifecycle, nil
				}
				return TaskStateRevisionRequested, lifecycle, nil
			}
		}
	}

	if hasRunning {
		return TaskStateRunning, lifecycle, nil
	}

	// Check if entire tree finished
	if allCompleted {
		if hasApprovedReview || (rootRec.Request.Strategy == StrategyDirect && rootRec.Status == StatusCompleted) {
			return TaskStateAwaitingHumanReview, lifecycle, nil
		}
		if rootRec.Status == StatusCompleted {
			return TaskStateAwaitingHumanReview, lifecycle, nil
		}
	}

	if rootRec.Status == StatusPending && len(subtasks) <= 1 {
		return TaskStateQueued, lifecycle, nil
	}

	return TaskStateRunning, lifecycle, nil
}

// absPath resolves a possibly-relative path against the workspace directory (never the
// process CWD). Relative worktree paths were previously resolved against CWD, which made
// every snapshot lookup silently fail when the CLI ran from another directory.
func (s *TeamServiceImpl) absPath(p string) string {
	if p == "" {
		p = "."
	}
	if !filepath.IsAbs(p) {
		base, err := filepath.Abs(s.workspaceDir)
		if err == nil {
			p = filepath.Join(base, p)
		}
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

// resolveCurrentSnapshot returns the content-addressed identity of the task's worktree.
// A git commit SHA is NOT used: it does not change when the working tree changes, so it
// cannot bind an approval to what a human or reviewer actually saw.
func (s *TeamServiceImpl) resolveCurrentSnapshot(taskID string) string {
	workDir := s.absPath(s.workspaceDir)
	if s.wtMgr != nil {
		if wt, err := s.wtMgr.GetWorktree(taskID); err == nil && wt != nil && wt.WorktreePath != "" {
			workDir = s.absPath(wt.WorktreePath)
		}
	}

	if id, err := NewStrictWorkspaceSnapshotter().ComputeSnapshot(workDir); err == nil {
		return id
	}

	// Fallback to evidence snapshot if available
	subtasks := s.store.GetTaskTree(taskID)
	for _, sub := range subtasks {
		if sub.Result != nil {
			if snap, ok := sub.Result.Outputs["snapshot_id"].(string); ok && snap != "" {
				return snap
			}
		}
	}
	return ""
}

// coderCompletionSnapshot returns the snapshot recorded by the most recent completed
// non-reviewer activity of the task, or "" if none was recorded.
func (s *TeamServiceImpl) coderCompletionSnapshot(taskID string) string {
	var best string
	var bestAt time.Time
	for _, rec := range s.store.GetTaskTree(taskID) {
		if rec.Result == nil || rec.Status != StatusCompleted {
			continue
		}
		agent := strings.ToLower(rec.Request.AgentID)
		if agent == "charlie" || agent == "reviewer" || strings.Contains(rec.Request.ID, "review") {
			continue
		}
		snap, _ := rec.Result.Outputs["snapshot_id"].(string)
		if snap == "" {
			continue
		}
		if best == "" || rec.Request.CreatedAt.After(bestAt) {
			best, bestAt = snap, rec.Request.CreatedAt
		}
	}
	return best
}

// reviewedSnapshot returns the snapshot of the latest approved review, or "".
func (s *TeamServiceImpl) reviewedSnapshot(taskID string) string {
	var best string
	var bestAt time.Time
	for _, rec := range s.store.GetTaskTree(taskID) {
		if rec.Result == nil || rec.Status != StatusCompleted {
			continue
		}
		if approved, _ := rec.Result.Outputs["approved"].(bool); !approved {
			continue
		}
		snap, _ := rec.Result.Outputs["snapshot_sha"].(string)
		if snap == "" {
			continue
		}
		if best == "" || rec.Request.CreatedAt.After(bestAt) {
			best, bestAt = snap, rec.Request.CreatedAt
		}
	}
	return best
}


func parseGitDiff(raw string) ([]FileDiff, int, int) {
	if strings.TrimSpace(raw) == "" {
		return nil, 0, 0
	}

	var files []FileDiff
	totalAdds := 0
	totalDels := 0

	chunks := strings.Split(raw, "diff --git ")
	for _, chunk := range chunks {
		if strings.TrimSpace(chunk) == "" {
			continue
		}
		lines := strings.Split(chunk, "\n")
		path := ""
		if len(lines) > 0 {
			parts := strings.Split(lines[0], " ")
			if len(parts) >= 2 {
				path = strings.TrimPrefix(parts[1], "b/")
			}
		}

		adds := 0
		dels := 0
		status := "modified"

		for _, l := range lines {
			if strings.HasPrefix(l, "new file") {
				status = "added"
			} else if strings.HasPrefix(l, "deleted file") {
				status = "deleted"
			} else if strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++") {
				adds++
			} else if strings.HasPrefix(l, "-") && !strings.HasPrefix(l, "---") {
				dels++
			}
		}

		totalAdds += adds
		totalDels += dels

		if path != "" {
			files = append(files, FileDiff{
				Path:      path,
				Status:    status,
				Additions: adds,
				Deletions: dels,
				Patch:     "diff --git " + chunk,
			})
		}
	}

	return files, totalAdds, totalDels
}

// GetTeamContext returns environmental context for host-native coordinators.
func (s *TeamServiceImpl) GetTeamContext(ctx context.Context) (*TeamContextResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	branch := "main"
	commit := ""
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = s.workspaceDir
	if out, err := cmd.Output(); err == nil {
		branch = strings.TrimSpace(string(out))
	}
	cmd = exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	cmd.Dir = s.workspaceDir
	if out, err := cmd.Output(); err == nil {
		commit = strings.TrimSpace(string(out))
	}

	var members []TeamMemberInfo
	if s.roster != nil {
		for _, m := range s.roster.ListMembers() {
			members = append(members, TeamMemberInfo{
				ID:           m.ID,
				Name:         m.Name,
				Role:         m.Role,
				Charter:      m.Charter,
				AllowedTools: m.AllowedTools,
			})
		}
	}

	var activeTasks []TaskSummary
	if s.store != nil {
		all := s.store.ListAllActivities()
		seenRoots := make(map[string]bool)
		for _, act := range all {
			rootID := act.Request.ID
			if act.Request.ParentID != "" {
				rootID = act.Request.ParentID
			}
			if seenRoots[rootID] {
				continue
			}
			seenRoots[rootID] = true
			state, _, _ := s.deriveTaskState(rootID)
			activeTasks = append(activeTasks, TaskSummary{
				TaskID:    rootID,
				Objective: act.Request.Objective,
				State:     state,
				Strategy:  act.Request.Strategy,
				CreatedAt: act.Request.CreatedAt,
			})
		}
	}

	var memories []string
	if s.memory != nil {
		for _, mem := range s.memory.ListVerified() {
			memories = append(memories, fmt.Sprintf("[%s (rev %d)]: %s", mem.Key, mem.Revision, mem.Content))
		}
	}

	return &TeamContextResult{
		WorkspaceRoot:  s.workspaceDir,
		CurrentBranch:  branch,
		HeadCommit:     commit,
		TeamMembers:    members,
		ActiveTasks:    activeTasks,
		RecentMemories: memories,
	}, nil
}

// BeginTask initializes an engineering task and allocates an isolated worktree.
// Crucially: it does NOT launch a background LLM inference loop.
func (s *TeamServiceImpl) BeginTask(ctx context.Context, req BeginTaskRequest) (*BeginTaskResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.TrimSpace(req.Objective) == "" {
		return nil, fmt.Errorf("service: objective is required")
	}

	taskID := fmt.Sprintf("task-%d", time.Now().UnixNano())
	if req.BudgetTokens <= 0 {
		req.BudgetTokens = 50000.0
	}
	if req.Strategy == "" {
		req.Strategy = StrategyTeam
	}
	if req.Coordinator == "" {
		req.Coordinator = "alice"
	}

	branchName := fmt.Sprintf("a4c/%s", taskID)
	wtPath := s.workspaceDir
	if s.wtMgr != nil {
		rec, err := s.wtMgr.AcquireWorktree(ctx, taskID, req.Coordinator)
		if err == nil && rec != nil {
			wtPath = rec.WorktreePath
			branchName = rec.Branch
		}
	}

	actReq := ActivityRequest{
		ID:        taskID,
		AgentID:   req.Coordinator,
		Objective: req.Objective,
		Budget:    Budget{Allocated: req.BudgetTokens},
		Strategy:  req.Strategy,
		Runtime:   req.Runtime,
		Inputs: map[string]any{
			"worktree_path": wtPath,
			"branch":        branchName,
			"coordinator":   req.Coordinator,
		},
		CreatedAt: time.Now(),
	}

	if s.store != nil {
		if err := s.store.Enqueue(actReq); err != nil {
			return nil, fmt.Errorf("service: failed to record task activity: %w", err)
		}
	}

	if s.lifecycleStore != nil {
		_ = s.lifecycleStore.Save(&HumanLifecycleRecord{
			TaskID:    taskID,
			UpdatedAt: time.Now(),
		})
	}

	if s.journal != nil {
		_ = s.journal.RecordIntent(IntentRecord{
			IntentID:   fmt.Sprintf("intent-%s-begin", taskID),
			ActivityID: taskID,
			Action:     "begin_task",
			Target:     branchName,
			Status:     IntentCommitted,
		})
	}

	return &BeginTaskResult{
		TaskID:         taskID,
		State:          TaskStateRunning,
		Coordinator:    req.Coordinator,
		WorktreePath:   wtPath,
		WorktreeBranch: branchName,
		CreatedAt:      actReq.CreatedAt,
	}, nil
}

// AssignActivity records that the coordinator dispatched work to a specialist (Bob or Charlie).
func (s *TeamServiceImpl) AssignActivity(ctx context.Context, req AssignActivityRequest) (*ActivityRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.TrimSpace(req.TaskID) == "" {
		return nil, fmt.Errorf("service: task_id is required")
	}
	if strings.TrimSpace(req.Agent) == "" {
		return nil, fmt.Errorf("service: agent is required")
	}

	wtPath := s.workspaceDir
	if s.wtMgr != nil {
		if rec, err := s.wtMgr.GetWorktree(req.TaskID); err == nil && rec != nil {
			wtPath = rec.WorktreePath
		}
	}

	role := req.Role
	if role == "" {
		if strings.EqualFold(req.Agent, "bob") || strings.Contains(strings.ToLower(req.Agent), "coder") {
			role = "coder"
		} else if strings.EqualFold(req.Agent, "charlie") || strings.Contains(strings.ToLower(req.Agent), "review") {
			role = "reviewer"
		} else {
			role = "specialist"
		}
	}

	actID := fmt.Sprintf("%s-%s-%d", req.TaskID, req.Agent, time.Now().UnixNano()%1000000)
	actReq := ActivityRequest{
		ID:        actID,
		ParentID:  req.TaskID,
		AgentID:   req.Agent,
		Objective: req.Objective,
		Inputs: map[string]any{
			"role":          role,
			"input":         req.Input,
			"allowed_tools": req.AllowedTools,
			"worktree_path": wtPath,
		},
		CreatedAt: time.Now(),
	}

	if s.store != nil {
		if err := s.store.Enqueue(actReq); err != nil {
			return nil, fmt.Errorf("service: failed to enqueue specialist activity: %w", err)
		}
	}

	if s.journal != nil {
		_ = s.journal.RecordIntent(IntentRecord{
			IntentID:   fmt.Sprintf("intent-%s-assign", actID),
			ActivityID: actID,
			Action:     "assign_activity",
			Target:     req.Agent,
			Status:     IntentCommitted,
		})
	}

	return &ActivityRef{
		ActivityID:   actID,
		TaskID:       req.TaskID,
		Agent:        req.Agent,
		Role:         role,
		WorktreePath: wtPath,
		AssignedAt:   actReq.CreatedAt,
	}, nil
}

// RecordCheckpoint records intermediate progress and findings durably.
func (s *TeamServiceImpl) RecordCheckpoint(ctx context.Context, req CheckpointRequest) (*CheckpointResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.TrimSpace(req.TaskID) == "" {
		return nil, fmt.Errorf("service: task_id is required")
	}

	wtPath := s.workspaceDir
	if s.wtMgr != nil {
		if rec, err := s.wtMgr.GetWorktree(req.TaskID); err == nil && rec != nil {
			wtPath = rec.WorktreePath
		}
	}

	commit := s.resolveCurrentSnapshot(req.TaskID)
	if commit == "" {
		cmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
		cmd.Dir = wtPath
		if out, err := cmd.Output(); err == nil {
			commit = strings.TrimSpace(string(out))
		}
	}

	chkID := fmt.Sprintf("chk-%s-%d", req.TaskID, time.Now().UnixNano()%1000000)
	if s.memory != nil {
		content := fmt.Sprintf("Checkpoint by %s: %s | Assumptions: %s | Findings: %s | Next: %s | SHA: %s",
			req.Actor, req.Summary,
			strings.Join(req.Assumptions, "; "),
			strings.Join(req.Findings, "; "),
			strings.Join(req.NextSteps, "; "),
			commit,
		)
		key := fmt.Sprintf("checkpoint.%s.%s", req.TaskID, chkID)
		_, _ = s.memory.Propose(key, content, req.Actor, req.ActivityID)
	}

	if s.journal != nil {
		_ = s.journal.RecordIntent(IntentRecord{
			IntentID:   fmt.Sprintf("intent-%s", chkID),
			ActivityID: req.TaskID,
			Action:     "checkpoint",
			Target:     commit,
			Status:     IntentCommitted,
		})
	}

	return &CheckpointResult{
		CheckpointID: chkID,
		TaskID:       req.TaskID,
		Actor:        req.Actor,
		RecordedAt:   time.Now(),
		SnapshotSHA:  commit,
	}, nil
}

// CompleteActivity records completion of a specialist activity.
func (s *TeamServiceImpl) CompleteActivity(ctx context.Context, req CompleteActivityRequest) (*HostActivityResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.TrimSpace(req.ActivityID) == "" {
		return nil, fmt.Errorf("service: activity_id is required")
	}

	var evidenceIDs []string
	if req.ValidationEvidence != "" {
		evidenceIDs = append(evidenceIDs, req.ValidationEvidence)
	}

	status := StatusCompleted
	if !req.Success {
		status = StatusFailed
	}

	result := ActivityResult{
		ID:          req.ActivityID,
		Status:      status,
		EvidenceIDs: evidenceIDs,
		Outputs: map[string]any{
			"summary":        req.Summary,
			"files_modified": req.FilesModified,
			"evidence":       req.ValidationEvidence,
			"actor":          req.Actor,
		},
	}
	if req.TaskID != "" {
		if snap := s.resolveCurrentSnapshot(req.TaskID); snap != "" {
			result.Outputs["snapshot_id"] = snap
		}
	}

	if s.store != nil {
		_ = s.store.Complete(req.ActivityID, result)
	}

	if s.journal != nil {
		_ = s.journal.RecordIntent(IntentRecord{
			IntentID:   fmt.Sprintf("intent-%s-complete", req.ActivityID),
			ActivityID: req.ActivityID,
			Action:     "complete_activity",
			Target:     req.Actor,
			Status:     IntentCommitted,
		})
	}

	return &HostActivityResult{
		ActivityID:    req.ActivityID,
		TaskID:        req.TaskID,
		Actor:         req.Actor,
		Status:        string(status),
		Summary:       req.Summary,
		FilesModified: req.FilesModified,
		Evidence:      evidenceIDs,
		CompletedAt:   time.Now(),
	}, nil
}

// RecordReview records an independent reviewer verdict and validates non-mutating reviewer boundaries.
func (s *TeamServiceImpl) RecordReview(ctx context.Context, req RecordReviewRequest) (*HostReviewResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.TrimSpace(req.TaskID) == "" {
		return nil, fmt.Errorf("service: task_id is required")
	}
	if strings.TrimSpace(req.Reviewer) == "" {
		req.Reviewer = "charlie"
	}

	current := s.resolveCurrentSnapshot(req.TaskID)

	// A review is bound to the exact content snapshot the reviewer examined.
	// Allow "HEAD" or worktree git commit SHA to resolve to the current snapshot identity.
	if strings.EqualFold(req.SnapshotSHA, "head") {
		req.SnapshotSHA = current
	} else if req.SnapshotSHA != "" && current != "" && req.SnapshotSHA != current {
		wtPath := s.absPath(s.workspaceDir)
		if s.wtMgr != nil {
			if rec, err := s.wtMgr.GetWorktree(req.TaskID); err == nil && rec != nil {
				wtPath = s.absPath(rec.WorktreePath)
			}
		}
		cmdRev := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
		cmdRev.Dir = wtPath
		if out, err := cmdRev.CombinedOutput(); err == nil {
			gitHead := strings.TrimSpace(string(out))
			if strings.HasPrefix(gitHead, req.SnapshotSHA) || strings.HasPrefix(req.SnapshotSHA, gitHead) {
				req.SnapshotSHA = current
			}
		}
	}

	if req.SnapshotSHA != "" && current != "" && req.SnapshotSHA != current {
		_ = AppendAudit(s.workspaceDir, "approvals", map[string]any{
			"op": "review", "task_id": req.TaskID, "outcome": "denied",
			"reason": "stale_review_snapshot", "reviewer": req.Reviewer,
			"claimed_snapshot": req.SnapshotSHA, "current_snapshot": current,
		})
		return nil, fmt.Errorf("%w: review references %s but worktree is at %s", ErrSnapshotMismatch, req.SnapshotSHA, current)
	}

	// Non-Mutating Reviewer Boundary Invariant (DETECTED, not prevented, at this layer):
	// the worktree content must be identical to what it was when the coder completed.
	// This replaces the earlier "worktree is dirty" test, which misclassified the coder's
	// own uncommitted work as a reviewer mutation.
	if baseline := s.coderCompletionSnapshot(req.TaskID); baseline != "" {
		if current != "" && baseline != current {
			_ = AppendAudit(s.workspaceDir, "approvals", map[string]any{
				"op": "review", "task_id": req.TaskID, "outcome": "denied",
				"reason": "reviewer_invariant", "reviewer": req.Reviewer,
				"baseline_snapshot": baseline, "current_snapshot": current,
			})
			return nil, fmt.Errorf("governance: non-mutating reviewer invariant violated: reviewer %s observed workspace changes since coder completion (snapshot %s -> %s)", req.Reviewer, baseline, current)
		}
	} else {
		// No coder completion recorded yet: fall back to requiring a clean worktree.
		wtPath := s.absPath(s.workspaceDir)
		if s.wtMgr != nil {
			if rec, err := s.wtMgr.GetWorktree(req.TaskID); err == nil && rec != nil {
				wtPath = s.absPath(rec.WorktreePath)
			}
		}
		cmdStat := exec.CommandContext(ctx, "git", "status", "--porcelain")
		cmdStat.Dir = wtPath
		if out, err := cmdStat.CombinedOutput(); err == nil {
			if dirty := strings.TrimSpace(string(out)); len(dirty) > 0 {
				return nil, fmt.Errorf("governance: non-mutating reviewer invariant violated: reviewer %s produced workspace modifications (%s)", req.Reviewer, dirty)
			}
		}
	}

	commit := current
	if commit == "" {
		commit = req.SnapshotSHA
	}


	revActID := fmt.Sprintf("%s-review-%d", req.TaskID, time.Now().UnixNano()%1000000)
	isApproved := strings.EqualFold(req.Verdict, "approved")
	revResult := ActivityResult{
		ID:          revActID,
		Status:      StatusCompleted,
		EvidenceIDs: req.Findings,
		Outputs: map[string]any{
			"reviewer":     req.Reviewer,
			"approved":     isApproved,
			"comments":     req.Comments,
			"verdict":      req.Verdict,
			"snapshot_sha": commit,
		},
	}

	if s.store != nil {
		_ = s.store.Enqueue(ActivityRequest{
			ID:        revActID,
			ParentID:  req.TaskID,
			AgentID:   req.Reviewer,
			Objective: fmt.Sprintf("Independent code review of snapshot %s", commit),
			CreatedAt: time.Now(),
		})
		_ = s.store.Complete(revActID, revResult)
	}

	targetState := TaskStateAwaitingHumanReview
	if strings.EqualFold(req.Verdict, "rejected") {
		targetState = TaskStateAwaitingDecision
	} else if strings.EqualFold(req.Verdict, "revisions_requested") {
		targetState = TaskStateRevisionRequested
	}

	if s.lifecycleStore != nil {
		rec, _ := s.lifecycleStore.Get(req.TaskID)
		if rec == nil {
			rec = &HumanLifecycleRecord{
				TaskID: req.TaskID,
			}
		}
		rec.UpdatedAt = time.Now()
		_ = s.lifecycleStore.Save(rec)
	}

	if s.journal != nil {
		_ = s.journal.RecordIntent(IntentRecord{
			IntentID:   fmt.Sprintf("intent-%s-review", revActID),
			ActivityID: req.TaskID,
			Action:     "record_review",
			Target:     req.Verdict,
			Status:     IntentCommitted,
		})
	}

	return &HostReviewResult{
		TaskID:         req.TaskID,
		Reviewer:       req.Reviewer,
		Verdict:        req.Verdict,
		ReviewedAt:     time.Now(),
		LifecycleState: targetState,
		SnapshotSHA:    commit,
	}, nil
}

