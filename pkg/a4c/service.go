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
	lifecycle.Accepted = false // Invalidate any prior acceptance
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

	snapshotID := s.resolveCurrentSnapshot(req.TaskID)

	lifecycle.Accepted = true
	lifecycle.AcceptedAt = time.Now()
	lifecycle.AcceptedSnapshot = snapshotID
	lifecycle.AcceptComments = req.Comments
	lifecycle.Rejected = false

	if err := s.lifecycleStore.Save(lifecycle); err != nil {
		return nil, fmt.Errorf("service: save acceptance: %w", err)
	}

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
			return nil, fmt.Errorf("%w: accepted %s != current %s", ErrSnapshotMismatch, lifecycle.AcceptedSnapshot, currentSnapshot)
		}
	}
	if state != TaskStateAccepted {
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

	// If a git worktree branch exists, perform merge into main workspace
	if s.wtMgr != nil {
		if wt, err := s.wtMgr.GetWorktree(req.TaskID); err == nil && wt != nil && wt.Branch != "" {
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

	// File copy fallback if direct directory was used (e.g. in non-branch test mode)
	if workDir != s.workspaceDir {
		_ = filepath.Walk(workDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || strings.Contains(path, ".git") || strings.Contains(path, ".a4c") {
				return nil
			}
			rel, _ := filepath.Rel(workDir, path)
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
			if rec.Request.AgentID == "reviewer" && rec.Result != nil {
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

func (s *TeamServiceImpl) resolveCurrentSnapshot(taskID string) string {
	workDir := s.workspaceDir
	if s.wtMgr != nil {
		if wt, err := s.wtMgr.GetWorktree(taskID); err == nil && wt != nil && wt.WorktreePath != "" {
			workDir = wt.WorktreePath
		}
	}

	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = workDir
	if out, err := cmd.CombinedOutput(); err == nil {
		return strings.TrimSpace(string(out))
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
