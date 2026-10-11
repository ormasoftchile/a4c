package a4c

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type LocalEffectReconciler struct {
	WorkspaceDir string
}

func (r *LocalEffectReconciler) Reconcile(ctx context.Context, intent IntentRecord) (bool, string, error) {
	if intent.Action == "apply_patch" {
		target := filepath.Join(r.WorkspaceDir, intent.Target)
		if _, err := os.Stat(target); err == nil {
			return true, fmt.Sprintf("local-file-%s", intent.Target), nil
		}
	}
	return false, "", nil
}

// ErrReviewerMutation is returned when a reviewer agent attempts unauthorized file modifications.
var ErrReviewerMutation = errors.New("reviewer attempted unauthorized file modifications; changes rolled back")

// parseStringMap safely parses a map of strings whether typed as map[string]string or map[string]any.
func parseStringMap(val any) map[string]string {
	if val == nil {
		return nil
	}
	if sm, ok := val.(map[string]string); ok {
		return sm
	}
	if am, ok := val.(map[string]any); ok {
		res := make(map[string]string)
		for k, v := range am {
			if s, ok := v.(string); ok {
				res[k] = s
			}
		}
		return res
	}
	return nil
}

type TeamCoordinator struct {
	mu            sync.Mutex
	WorkspaceDir  string
	WorktreeMgr   *WorktreeManager
	Store         *DurableActivityStore
	Roster        *TeamRoster
	Memory        *DurableMemoryStore
	Governor      *PolicyGovernor
	Journal       *DurableIntentJournal
	Snapshotter   *WorkspaceSnapshotter
	TestRunner    TestRunner
	Provider               AgentProvider
	Providers              map[string]AgentProvider
	Reconciler             EffectReconciler
	MaxRepair              int
	MaxRevisions           int
	MaxToolCalls           int
	MaxReadFileLines       int
	Limits                 ActivityLimits
	lock                   *WorkspaceLock
	coordID                string
	revisionCount          int
	lastEvidence           *ValidationEvidence
	sessions               map[string]AgentSession
	Handoff      *HandoffBuilder
	Capabilities *CapabilityRegistry
}

func NewTeamCoordinator(
	workspaceDir string,
	store *DurableActivityStore,
	roster *TeamRoster,
	memory *DurableMemoryStore,
	governor *PolicyGovernor,
	journal *DurableIntentJournal,
	runner TestRunner,
	provider AgentProvider,
) *TeamCoordinator {
	// Configure role-based tool privileges on the governor
	for _, member := range roster.ListMembers() {
		governor.SetAgentTools(member.ID, member.AllowedTools)
	}

	if workspaceDir != "" {
		_ = governor.SetStorageDir(filepath.Join(workspaceDir, ".a4c", "budgets"))
	}

	coordID := fmt.Sprintf("coord-%d", time.Now().UnixNano())
	coord := &TeamCoordinator{
		WorkspaceDir:     workspaceDir,
		Store:            store,
		Roster:           roster,
		Memory:           memory,
		Governor:         governor,
		Journal:          journal,
		Snapshotter:      NewWorkspaceSnapshotter(),
		TestRunner:       runner,
		Provider:         provider,
		Reconciler:       &LocalEffectReconciler{WorkspaceDir: workspaceDir},
		MaxRepair:        3,
		MaxRevisions:     2,
		MaxToolCalls:     10,
		MaxReadFileLines: 300,
		coordID:          coordID,
		Limits: ActivityLimits{
			MaxModelTurns:       10,
			MaxToolCalls:        10,
			MaxReadFileLines:    300,
			MaxDiffLines:        150,
			MaxValidationOutput: 4096,
			MaxRevisions:        2,
		},
		Providers:    make(map[string]AgentProvider),
		sessions:     make(map[string]AgentSession),
		Handoff:      NewHandoffBuilder(),
		Capabilities: NewCapabilityRegistry(),
	}

	if provider != nil {
		coord.Providers["default"] = provider
		coord.Providers["copilot"] = provider
	}

	if governor != nil && store != nil {
		_ = governor.ReconcileWithStore(store)
	}

	return coord
}

// RegisterProvider registers an agent execution runtime by name (e.g. "copilot", "opencode").
func (c *TeamCoordinator) RegisterProvider(name string, p AgentProvider) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Providers == nil {
		c.Providers = make(map[string]AgentProvider)
	}
	c.Providers[name] = p
}

// SetDefaultProvider sets the default agent provider.
func (c *TeamCoordinator) SetDefaultProvider(p AgentProvider) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Provider = p
}

func (c *TeamCoordinator) resolveProvider(req *ActivityRequest) AgentProvider {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.resolveProviderLocked(req)
}

func (c *TeamCoordinator) resolveProviderLocked(req *ActivityRequest) AgentProvider {
	if req != nil && req.Runtime != "" && c.Providers != nil {
		if p, ok := c.Providers[req.Runtime]; ok {
			return p
		}
	}
	if c.Provider != nil {
		return c.Provider
	}
	if c.Providers != nil {
		if fake, ok := c.Providers["fake"]; ok {
			return fake
		}
		for _, p := range c.Providers {
			return p
		}
	}
	return nil
}


// SetWorktreeManager sets the Git worktree manager for task isolation.
func (c *TeamCoordinator) SetWorktreeManager(mgr *WorktreeManager) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.WorktreeMgr = mgr
}

// AcquireWorkspaceLock explicitly acquires the exclusive OS kernel lock on .a4c/workspace.lock.
func (c *TeamCoordinator) AcquireWorkspaceLock() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.lock != nil && c.lock.IsLocked() {
		return nil
	}
	lock, err := AcquireWorkspaceLock(c.WorkspaceDir, c.coordID, "")
	if err != nil {
		return err
	}
	c.lock = lock
	return nil
}

// ReleaseWorkspaceLock explicitly releases the exclusive OS kernel lock.
func (c *TeamCoordinator) ReleaseWorkspaceLock() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.lock == nil {
		return nil
	}
	err := c.lock.Release()
	c.lock = nil
	return err
}

// Close releases any held workspace locks and resources.
func (c *TeamCoordinator) Close() error {
	return c.ReleaseWorkspaceLock()
}

func (c *TeamCoordinator) getRootTaskID(req *ActivityRequest) string {
	rootID := req.ID
	curr := req
	for curr.ParentID != "" {
		parentRec, err := c.Store.Get(curr.ParentID)
		if err != nil {
			break
		}
		curr = &parentRec.Request
		rootID = curr.ID
	}
	return rootID
}

func (c *TeamCoordinator) resolveWorkspace(req *ActivityRequest) string {
	if c.WorktreeMgr != nil {
		rootID := c.getRootTaskID(req)
		if wt, err := c.WorktreeMgr.GetWorktree(rootID); err == nil && wt.WorktreePath != "" {
			p := wt.WorktreePath
			if !filepath.IsAbs(p) {
				base, err := filepath.Abs(c.WorkspaceDir)
				if err == nil {
					p = filepath.Join(base, p)
				} else {
					p = filepath.Join(c.WorkspaceDir, p)
				}
			}
			abs, err := filepath.Abs(p)
			if err == nil {
				return abs
			}
			return p
		}
	}
	abs, err := filepath.Abs(c.WorkspaceDir)
	if err == nil {
		return abs
	}
	return c.WorkspaceDir
}


// SubmitTask allows the human engineer to assign a feature request to the persistent team.
func (c *TeamCoordinator) SubmitTask(objective string, budget Budget, inputs map[string]any) (*ActivityRequest, error) {
	return c.SubmitTaskWithStrategy(objective, budget, inputs, StrategyTeam)
}

// SubmitTaskWithStrategy schedules a task according to the selected execution strategy.
func (c *TeamCoordinator) SubmitTaskWithStrategy(
	objective string,
	budget Budget,
	inputs map[string]any,
	strategy ExecutionStrategy,
) (*ActivityRequest, error) {
	c.mu.Lock()
	alreadyLocked := c.lock != nil && c.lock.IsLocked()
	c.mu.Unlock()

	if !alreadyLocked {
		if err := c.AcquireWorkspaceLock(); err != nil {
			return nil, err
		}
		defer c.ReleaseWorkspaceLock()
	}

	if strategy == "" {
		strategy = StrategyDirect
	}

	taskID := fmt.Sprintf("task-%d", time.Now().UnixNano())

	if c.Governor != nil {
		c.Governor.SetBudget(taskID, budget)
	}

	if c.WorktreeMgr != nil {
		_, _ = c.WorktreeMgr.AcquireWorktree(context.Background(), taskID, "team")
	}

	var runtime, model string
	specialist := "coder"
	reviewer := "reviewer"
	if inputs != nil {
		if r, ok := inputs["runtime"].(string); ok {
			runtime = r
		}
		if m, ok := inputs["model"].(string); ok {
			model = m
		}
		if s, ok := inputs["specialist"].(string); ok && s != "" {
			specialist = s
		}
		if r, ok := inputs["reviewer"].(string); ok && r != "" {
			reviewer = r
		}
	}

	switch strategy {
	case StrategyDirect:
		// Zero-Coordinator Fast Path: Exactly one specialist coding session
		req := ActivityRequest{
			ID:        taskID,
			AgentID:   specialist,
			Objective: objective,
			Inputs:    inputs,
			Budget:    budget,
			Strategy:  StrategyDirect,
			Runtime:   runtime,
			Model:     model,
			CreatedAt: time.Now(),
		}
		if err := c.Store.Enqueue(req); err != nil {
			return nil, fmt.Errorf("coordinator: failed to enqueue direct task: %w", err)
		}
		return &req, nil

	case StrategyReviewed:
		// Specialist implements + Independent Reviewer verifies (zero coordinator sessions)
		coderBudget := budget.Allocated * 0.7
		if coderBudget < 35000 {
			coderBudget = 35000
		}
		revBudget := budget.Allocated * 0.5
		if revBudget < 30000 {
			revBudget = 30000
		}
		revRuntime := runtime
		if rr, ok := inputs["reviewer_runtime"].(string); ok && rr != "" {
			revRuntime = rr
		}
		revModel := model
		if rm, ok := inputs["reviewer_model"].(string); ok && rm != "" {
			revModel = rm
		}
		coderReq := ActivityRequest{
			ID:        taskID,
			AgentID:   specialist,
			Objective: objective,
			Inputs:    inputs,
			Budget:    Budget{Allocated: coderBudget},
			Strategy:  StrategyReviewed,
			Runtime:   runtime,
			Model:     model,
			CreatedAt: time.Now(),
		}
		reviewerReq := ActivityRequest{
			ID:        fmt.Sprintf("%s-rev", taskID),
			ParentID:  taskID,
			AgentID:   reviewer,
			Objective: fmt.Sprintf("Review implementation of: %s", objective),
			Budget:    Budget{Allocated: revBudget},
			DependsOn: []string{taskID},
			Strategy:  StrategyReviewed,
			Runtime:   revRuntime,
			Model:     revModel,
			CreatedAt: time.Now(),
		}
		if c.Governor != nil {
			c.Governor.SetBudget(reviewerReq.ID, reviewerReq.Budget)
		}
		if err := c.Store.Enqueue(coderReq); err != nil {
			return nil, err
		}
		if err := c.Store.Enqueue(reviewerReq); err != nil {
			return nil, err
		}
		return &coderReq, nil

	case StrategyTeam:
		fallthrough
	default:
		// Mode A / Full team: Lead coordinates, delegates to Coder & Reviewer
		req := ActivityRequest{
			ID:        taskID,
			AgentID:   "lead",
			Objective: objective,
			Inputs:    inputs,
			Budget:    budget,
			Strategy:  StrategyTeam,
			Runtime:   runtime,
			Model:     model,
			CreatedAt: time.Now(),
		}
		if err := c.Store.Enqueue(req); err != nil {
			return nil, fmt.Errorf("coordinator: failed to enqueue team task: %w", err)
		}
		return &req, nil
	}
}


// Step processes the next claimable activity whose dependencies are satisfied.
func (c *TeamCoordinator) Step(ctx context.Context) (*ActivityResult, error) {
	if err := c.AcquireWorkspaceLock(); err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// 1. Reconcile any unfinished external mutations on startup
	if c.Journal != nil && c.Reconciler != nil {
		if _, err := c.Journal.ReconcileUnfinished(ctx, c.Reconciler); err != nil {
			return nil, err
		}
	}

	// 2. Claim next available activity (DurableActivityStore enforces DAG dependencies)
	req, err := c.Store.ClaimNext()
	if err != nil {
		return nil, err
	}
	if req == nil {
		return nil, nil // No ready pending work
	}

	// Pre-flight hierarchical budget verification across task tree
	rootID := c.getRootTaskID(req)
	tree := c.Store.GetTaskTree(rootID)
	var rootTotalConsumed float64
	for _, r := range tree {
		if r.Result != nil {
			rootTotalConsumed += r.Result.Consumed.Consumed
		}
	}

	rootRec, _ := c.Store.Get(rootID)
	if rootRec != nil && rootRec.Request.Budget.Allocated > 0 {
		rootAlloc := rootRec.Request.Budget.Allocated
		if rootTotalConsumed >= rootAlloc {
			// Root budget exhausted! Escalate to human engineer.
			decReq := HumanDecisionRequest{
				DecisionID: fmt.Sprintf("dec-budget-%d", time.Now().UnixNano()),
				ActivityID: req.ID,
				ProposedBy: "coordinator",
				Question:   fmt.Sprintf("Task tree %s budget exhausted (consumed %.0f >= allocated %.0f). Human authorization required.", rootID, rootTotalConsumed, rootAlloc),
				Context: map[string]any{
					"root_id":          rootID,
					"total_consumed":   rootTotalConsumed,
					"allocated_budget": rootAlloc,
					"activity_id":      req.ID,
				},
				Status:    HumanDecisionPending,
				CreatedAt: time.Now(),
			}
			if err := c.Store.MarkAwaitingHuman(req.ID, decReq); err != nil {
				return nil, err
			}
			return &ActivityResult{
				ID:       req.ID,
				Status:   StatusAwaitingHuman,
				Consumed: Budget{Consumed: 0},
				Outputs: map[string]any{
					"decision_id": decReq.DecisionID,
					"question":    decReq.Question,
					"reason":      "root_budget_exhausted",
				},
			}, nil
		}
	}

	// Activity pre-flight check
	if req.Budget.Allocated > 0 && req.Budget.Consumed >= req.Budget.Allocated {
		failErr := fmt.Errorf("%w: activity %s budget already exhausted (consumed %.0f >= allocated %.0f)",
			ErrBudgetExceeded, req.ID, req.Budget.Consumed, req.Budget.Allocated)
		_ = c.Store.Fail(req.ID, failErr)
		return nil, failErr
	}

	// 3. Resolve agent identity
	agentID := req.AgentID
	if agentID == "" || agentID == "team" {
		agentID = "lead"
	}

	ident, found := c.Roster.GetMember(agentID)
	if !found {
		failErr := fmt.Errorf("coordinator: agent %q not found in team roster", agentID)
		_ = c.Store.Fail(req.ID, failErr)
		return nil, failErr
	}

	// 4. Set up activity collectors for governed tools
	var proposedActivities []ActivityProposal
	var proposedMemories []struct{ Key, Content string }
	var decisionRequest *HumanDecisionRequest
	var reviewResult *ReviewResult

	activeWorkspace := c.resolveWorkspace(req)

	var structuralQueriesCount int
	var fileReadsCount int

	// Resolve capability profile for this activity
	profile := c.Capabilities.ResolveProfile(ident.ID, ident.Role, req.Objective, req.CapabilityProfile, c.Governor)
	req.CapabilityProfile = &profile

	// Resolve effective economic and activity limits
	effectiveLimits := ActivityLimits{
		MaxModelTurns:       10,
		MaxToolCalls:        c.MaxToolCalls,
		MaxReadFileLines:    c.MaxReadFileLines,
		MaxDiffLines:        150,
		MaxValidationOutput: 4096,
		MaxRevisions:        c.MaxRevisions,
	}
	if c.Limits.MaxModelTurns > 0 { effectiveLimits.MaxModelTurns = c.Limits.MaxModelTurns }
	if c.Limits.MaxToolCalls > 0 { effectiveLimits.MaxToolCalls = c.Limits.MaxToolCalls }
	if c.Limits.MaxReadFileLines > 0 { effectiveLimits.MaxReadFileLines = c.Limits.MaxReadFileLines }
	if c.Limits.MaxDiffLines > 0 { effectiveLimits.MaxDiffLines = c.Limits.MaxDiffLines }
	maxRevs := c.MaxRevisions
	if c.Limits.MaxRevisions != 2 {
		maxRevs = c.Limits.MaxRevisions
	}
	if profile.Limits != nil {
		if profile.Limits.MaxModelTurns > 0 { effectiveLimits.MaxModelTurns = profile.Limits.MaxModelTurns }
		if profile.Limits.MaxToolCalls > 0 { effectiveLimits.MaxToolCalls = profile.Limits.MaxToolCalls }
		if profile.Limits.MaxReadFileLines > 0 { effectiveLimits.MaxReadFileLines = profile.Limits.MaxReadFileLines }
		if profile.Limits.MaxDiffLines > 0 { effectiveLimits.MaxDiffLines = profile.Limits.MaxDiffLines }
		if profile.Limits.MaxValidationOutput > 0 { effectiveLimits.MaxValidationOutput = profile.Limits.MaxValidationOutput }
		maxRevs = profile.Limits.MaxRevisions
	}

	if req.Limits != nil {
		if req.Limits.MaxModelTurns > 0 { effectiveLimits.MaxModelTurns = req.Limits.MaxModelTurns }
		if req.Limits.MaxToolCalls > 0 { effectiveLimits.MaxToolCalls = req.Limits.MaxToolCalls }
		if req.Limits.MaxReadFileLines > 0 { effectiveLimits.MaxReadFileLines = req.Limits.MaxReadFileLines }
		if req.Limits.MaxDiffLines > 0 { effectiveLimits.MaxDiffLines = req.Limits.MaxDiffLines }
		if req.Limits.MaxValidationOutput > 0 { effectiveLimits.MaxValidationOutput = req.Limits.MaxValidationOutput }
		maxRevs = req.Limits.MaxRevisions
	}

	effectiveLimits.MaxRevisions = maxRevs
	c.Handoff.MaxDiffLines = effectiveLimits.MaxDiffLines

	isReviewerRole := strings.EqualFold(ident.ID, "reviewer") ||
		strings.EqualFold(ident.ID, "charlie") ||
		strings.Contains(strings.ToLower(ident.Role), "reviewer")

	sessionWorkspace := activeWorkspace
	var cleanupReviewWorkspace func()
	if isReviewerRole {
		dispWS, cleanup, err := c.createDisposableReviewWorkspace(activeWorkspace, rootID)
		if err == nil {
			sessionWorkspace = dispWS
			cleanupReviewWorkspace = cleanup
		}
	}
	if cleanupReviewWorkspace != nil {
		defer cleanupReviewWorkspace()
	}

	config := AgentConfig{
		AgentID:          ident.ID,
		Charter:          ident.Charter,
		WorkspaceDir:     sessionWorkspace,
		Budget:           req.Budget,
		Governor:         c.Governor,
		IntentJournal:    c.Journal,
		Profile:          &profile,
		AllowedTools:     profile.AllowedTools,
		MaxToolCalls:     effectiveLimits.MaxToolCalls,
		MaxReadFileLines: effectiveLimits.MaxReadFileLines,
		Limits:           &effectiveLimits,
		Runtime:          req.Runtime,
		Model:            req.Model,
		OnRecordMetric: func(metric string, delta int) {
			if metric == "structural_queries" {
				structuralQueriesCount += delta
			} else if metric == "file_reads" {
				fileReadsCount += delta
			}
		},
		OnProposeActivity: func(prop ActivityProposal) {
			proposedActivities = append(proposedActivities, prop)
		},
		OnSubmitReview: func(rev ReviewResult) {
			rev.ActivityID = req.ID
			if rev.Verdict == "" {
				if rev.Approved {
					rev.Verdict = ReviewVerdictApproved
				} else {
					rev.Verdict = ReviewVerdictRejected
				}
			}
			reviewResult = &rev
		},
		OnRequestHumanDecision: func(dec HumanDecisionRequest) {
			dec.ActivityID = req.ID
			decisionRequest = &dec
		},
		OnProposeMemory: func(key, content string) {
			proposedMemories = append(proposedMemories, struct{ Key, Content string }{Key: key, Content: content})
		},
	}

	// 5. Initialize or resume independent agent session
	prov := c.resolveProviderLocked(req)
	if prov == nil {
		err := fmt.Errorf("coordinator: no agent provider configured for runtime %q", req.Runtime)
		_ = c.Store.Fail(req.ID, err)
		return nil, err
	}
	session, err := prov.CreateSession(ctx, config)
	if err != nil {
		_ = c.Store.Fail(req.ID, err)
		return nil, fmt.Errorf("coordinator: failed to initialize session for %s: %w", ident.ID, err)
	}
	defer session.Close()

	// 6. Build prompt with injected verified team memory & role-appropriate context via HandoffBuilder
	diff := ""
	evStatus := "none"
	if isReviewerRole {
		diff, _ = c.getWorkspaceDiff(ctx, activeWorkspace)
		c.lastEvidence, evStatus = c.resolveValidationEvidence(req, activeWorkspace)
	}
	evidenceSummary := "none"
	if c.lastEvidence != nil {
		evidenceSummary = fmt.Sprintf("Evidence ID: %s | Status: %s | Command: %s | ExitCode: %d | Duration: %v | Valid: %t\nTest Output:\n%s",
			c.lastEvidence.EvidenceID, evStatus, strings.Join(c.lastEvidence.Command, " "), c.lastEvidence.ExitCode,
			c.lastEvidence.Duration.Round(time.Millisecond), c.lastEvidence.Valid, c.lastEvidence.OutputSummary)
	}

	var checkpoint *TaskCheckpoint
	if req.Inputs != nil {
		if chk, ok := req.Inputs["checkpoint"].(*TaskCheckpoint); ok {
			checkpoint = chk
		}
	}

	artifact := HandoffArtifact{
		Objective:          req.Objective,
		Role:               fmt.Sprintf("%s (%s)", ident.Name, ident.Role),
		AgentID:            ident.ID,
		Strategy:           req.Strategy,
		ActiveWorkspace:    activeWorkspace,
		Diff:               diff,
		ValidationEvidence: evidenceSummary,
		VerifiedMemories:   c.Memory.FormatContext(),
		Inputs:             req.Inputs,
		ProfileDescription: profile.Format(),
		Checkpoint:         checkpoint,
	}

	prompt := c.Handoff.BuildPrompt(artifact)

	// 7. Dispatch to agent session with latency tracking
	var preReviewSnap string
	var snapErr error
	if isReviewerRole {
		preReviewSnap, snapErr = c.Snapshotter.ComputeSnapshot(sessionWorkspace)
	}

	startWall := time.Now()
	startModel := time.Now()
	resp, err := session.Send(ctx, prompt)
	modelDuration := time.Since(startModel)

	var reviewerMutated bool
	if isReviewerRole && snapErr == nil && preReviewSnap != "" {
		postReviewSnap, postSnapErr := c.Snapshotter.ComputeSnapshot(sessionWorkspace)
		if postSnapErr == nil && preReviewSnap != postReviewSnap {
			// Unauthorized mutation detected! Mark review mutated without touching authoritative activeWorkspace.
			reviewerMutated = true
		}
	}

	modelDur, toolDur := session.Durations()
	if modelDur == 0 && toolDur == 0 {
		modelDur = modelDuration
	}

	usage := session.Usage()
	consumed := Budget{Consumed: float64(usage.TotalTokens)}

	metrics := &ExecutionMetrics{
		WallClockDuration:      time.Since(startWall),
		ProviderWaitDuration:   modelDur,
		ModelDuration:          modelDur,
		ToolDuration:           toolDur,
		PromptTokens:           usage.PromptTokens,
		CompletionTokens:       usage.CompletionTokens,
		ReasoningTokens:        usage.ReasoningTokens,
		CachedTokens:           usage.CachedTokens,
		ReportedCostUSD:        usage.ReportedCostUSD,
		TotalTokens:            usage.TotalTokens,
		PeakContextTokens:      usage.PeakContextTokens,
		OverrunTokens:          usage.OverrunTokens,
		ModelInvocations:       1,
		StructuralQueriesCount: structuralQueriesCount,
		FileReadsCount:         fileReadsCount,
	}

	if c.Governor != nil {
		c.Governor.RecordConsumption(req.ID, rootID, consumed.Consumed)
	}

	if err != nil {
		failResult := ActivityResult{
			ID:       req.ID,
			Status:   StatusFailed,
			Consumed: consumed,
			Metrics:  metrics,
			Outputs: map[string]any{
				"error": err.Error(),
			},
		}
		if errors.Is(err, ErrTurnLimitExceeded) {
			failResult.Outputs["failure_reason"] = "turn_limit_exhausted"
		} else if errors.Is(err, ErrToolCallLimitExceeded) {
			failResult.Outputs["failure_reason"] = "tool_limit_exhausted"
		} else if errors.Is(err, ErrBudgetExceeded) {
			failResult.Outputs["failure_reason"] = "budget_exhausted"
		}
		_ = c.Store.FailWithResult(req.ID, failResult, err)
		return nil, err
	}

	// Post-flight budget check: verify activity didn't exceed allocated budget
	if req.Budget.Allocated > 0 && consumed.Consumed > req.Budget.Allocated {
		overrunErr := fmt.Errorf("%w: activity %s consumed %.0f tokens exceeds allocated %.0f",
			ErrBudgetExceeded, req.ID, consumed.Consumed, req.Budget.Allocated)
		failResult := ActivityResult{
			ID:       req.ID,
			Status:   StatusFailed,
			Consumed: consumed,
			Metrics:  metrics,
			Outputs: map[string]any{
				"error": overrunErr.Error(),
			},
		}
		_ = c.Store.FailWithResult(req.ID, failResult, overrunErr)
		return nil, overrunErr
	}

	// Parse fallback proposals from response text if tools were not invoked directly
	c.parseTextDirectives(resp.Content, &proposedActivities, &proposedMemories, &decisionRequest, &reviewResult, req.ID, ident.ID)

	// 8. Handle human decision escalation if requested
	if decisionRequest != nil {
		decisionRequest.ActivityID = req.ID
		decisionRequest.ProposedBy = ident.ID
		if err := c.Store.MarkAwaitingHuman(req.ID, *decisionRequest); err != nil {
			return nil, err
		}
		return &ActivityResult{
			ID:       req.ID,
			Status:   StatusAwaitingHuman,
			Consumed: consumed,
			Metrics:  metrics,
			Outputs: map[string]any{
				"decision_id": decisionRequest.DecisionID,
				"question":    decisionRequest.Question,
			},
		}, nil
	}

	// 9. Process any proposed architectural memories
	for _, m := range proposedMemories {
		_, _ = c.Memory.Propose(m.Key, m.Content, ident.ID, req.ID)
	}

	// 10. Process role-specific authoritative lifecycle
	result := &ActivityResult{
		ID:                req.ID,
		Status:            StatusCompleted,
		Consumed:          consumed,
		Metrics:           metrics,
		CapabilityProfile: &profile,
		Outputs: map[string]any{
			"summary":    resp.Content,
			"session_id": session.SessionID(),
		},
	}

	isLead := strings.EqualFold(ident.ID, "lead") || strings.EqualFold(ident.Role, "lead") || strings.EqualFold(ident.ID, "alice")

	if isLead {
		// Authorize and enqueue delegated subtasks
		proposalIDMap := make(map[string]string)
		for idx, prop := range proposedActivities {
			childID := prop.ProposalID
			if childID == "" {
				childID = fmt.Sprintf("%s-sub-%d", req.ID, idx+1)
			}
			if prop.ProposalID != "" {
				proposalIDMap[prop.ProposalID] = childID
			}
			proposalIDMap[prop.ProposedAgentID] = childID
			proposalIDMap[fmt.Sprintf("%d", idx+1)] = childID
			prop.ProposalID = childID

			// Remap DependsOn to actual childIDs if needed
			var remappedDeps []string
			for _, dep := range prop.DependsOn {
				if actual, found := proposalIDMap[dep]; found {
					remappedDeps = append(remappedDeps, actual)
				} else if _, exists := c.Store.Get(dep); exists == nil {
					remappedDeps = append(remappedDeps, dep)
				}
			}
			// If reviewer has no dependencies specified, make it depend on preceding subtask
			if prop.ProposedAgentID == "reviewer" && len(remappedDeps) == 0 && idx > 0 {
				prevChildID := fmt.Sprintf("%s-sub-%d", req.ID, idx)
				remappedDeps = append(remappedDeps, prevChildID)
			}
			prop.DependsOn = remappedDeps

			authReq, authErr := c.Governor.AuthorizeProposal(*req, prop, childID)
			if authErr != nil {
				return nil, fmt.Errorf("coordinator: proposal rejected by governor: %w", authErr)
			}

			// Assign role-specific capability profile and structural artifact link
			childProfile := c.Capabilities.ResolveProfile(prop.ProposedAgentID, "", prop.Objective, prop.RequestedProfile, c.Governor)
			authReq.CapabilityProfile = &childProfile
			authReq.Runtime = req.Runtime
			authReq.Model = req.Model
			if prop.Inputs != nil {
				if r, ok := prop.Inputs["runtime"].(string); ok && r != "" {
					authReq.Runtime = r
				}
				if m, ok := prop.Inputs["model"].(string); ok && m != "" {
					authReq.Model = m
				}
			}
			if req.Inputs != nil {
				rMap := parseStringMap(req.Inputs["agent_runtimes"])
				if r, exists := rMap[prop.ProposedAgentID]; exists && r != "" {
					authReq.Runtime = r
				}
				mMap := parseStringMap(req.Inputs["agent_models"])
				if m, exists := mMap[prop.ProposedAgentID]; exists && m != "" {
					authReq.Model = m
				}
				if prop.ProposedAgentID == "reviewer" {
					if r, ok := req.Inputs["reviewer_runtime"].(string); ok && r != "" {
						authReq.Runtime = r
					}
					if m, ok := req.Inputs["reviewer_model"].(string); ok && m != "" {
						authReq.Model = m
					}
				}
			}

			// Idempotency check: don't double enqueue if already exists
			if c.Store.FindActivityByProposalID(req.ID, prop.Objective) == nil {
				if err := c.Store.Enqueue(*authReq); err != nil {
					return nil, err
				}
			}
			result.Proposals = append(result.Proposals, prop)
		}
		_ = c.Store.Complete(req.ID, *result)


	} else if !isReviewerRole {
		// Specialist implementer: deterministic native testing and evidence collection
		beforeSnapshot, err := c.Snapshotter.ComputeSnapshot(activeWorkspace)
		if err != nil {
			_ = c.Store.Fail(req.ID, err)
			return nil, err
		}

		valStart := time.Now()
		exitCode, testOut, err := c.TestRunner.RunTests(ctx, activeWorkspace)
		valDuration := time.Since(valStart)
		if err != nil {
			_ = c.Store.Fail(req.ID, err)
			return nil, err
		}

		afterSnapshot, err := c.Snapshotter.ComputeSnapshot(activeWorkspace)
		if err != nil {
			_ = c.Store.Fail(req.ID, err)
			return nil, err
		}

		if err := c.Snapshotter.ValidateIntegrity(beforeSnapshot, afterSnapshot); err != nil {
			_ = c.Store.Fail(req.ID, err)
			return nil, err
		}

		// Bounded repair loop if tests fail
		if exitCode != 0 {
			repaired := false
			repairAttempts := 0
			for repairAttempts < c.MaxRepair {
				repairAttempts++
				repairPrompt := fmt.Sprintf("Tests failed with exit code %d:\n%s\nPlease fix the failure in the workspace and ensure all tests pass.", exitCode, testOut)
				_, rErr := session.Send(ctx, repairPrompt)
				if rErr != nil {
					break
				}

				preSnap, _ := c.Snapshotter.ComputeSnapshot(activeWorkspace)
				rValStart := time.Now()
				exitCode, testOut, _ = c.TestRunner.RunTests(ctx, activeWorkspace)
				valDuration += time.Since(rValStart)
				postSnap, _ := c.Snapshotter.ComputeSnapshot(activeWorkspace)
				if err := c.Snapshotter.ValidateIntegrity(preSnap, postSnap); err != nil {
					break
				}

				if exitCode == 0 {
					repaired = true
					afterSnapshot = postSnap
					result.Outputs["repaired"] = true
					result.Outputs["repair_attempts"] = repairAttempts
					break
				}
			}

			if !repaired {
				failErr := fmt.Errorf("tests failed with exit code %d after %d repairs: %s", exitCode, repairAttempts, testOut)
				_ = c.Store.Fail(req.ID, failErr)
				if c.WorktreeMgr != nil {
					rootID := c.getRootTaskID(req)
					_ = c.WorktreeMgr.ReleaseWorktree(rootID, false)
				}
				return nil, failErr
			}
		}

		// Native tests verified! Create evidence and promote memories
		metrics.ValidationDuration = valDuration
		outSummary := testOut
		if len(outSummary) > 500 {
			outSummary = outSummary[:500] + "... [truncated]"
		}
		evidence := ValidationEvidence{
			EvidenceID:          fmt.Sprintf("evi-%s-%d", req.ID, time.Now().UnixNano()),
			ActivityID:          req.ID,
			TaskID:              rootID,
			WorkspaceDir:        activeWorkspace,
			WorkspaceSnapshotID: afterSnapshot,
			Command:             []string{"go", "test", "-short", "./..."},
			ExitCode:            exitCode,
			OutputSummary:       outSummary,
			Duration:            valDuration,
			HostID:              req.Runtime,
			AgentID:             ident.ID,
			Valid:               exitCode == 0,
			StartedAt:           valStart,
			CompletedAt:         time.Now(),
		}
		c.lastEvidence = &evidence
		result.EvidenceIDs = append(result.EvidenceIDs, evidence.EvidenceID)
		result.Outputs["snapshot_id"] = afterSnapshot
		result.Outputs["test_output_summary"] = outSummary
		result.Outputs["validation_duration_ms"] = valDuration.Milliseconds()
		result.Outputs["validation_evidence"] = evidence

		// Promote coder-proposed memories because native tests passed
		for _, m := range proposedMemories {
			_, _ = c.Memory.Promote(m.Key, "native_tests")
		}

		if req.Strategy == StrategyDirect {
			result.Outputs["proposed_commit"] = fmt.Sprintf("feat: %s (verified by A4C single agent & native tests)", req.Objective)
			if c.WorktreeMgr != nil {
				rootID := c.getRootTaskID(req)
				_ = c.WorktreeMgr.ReleaseWorktree(rootID, true)
			}
		}

		_ = c.Store.Complete(req.ID, *result)


	} else {
		// Reviewer verification: Fail-Closed Contract
		var (
			verdict  ReviewVerdict = ReviewVerdictIncomplete
			approved               = false
			comments               = "Incomplete: reviewer execution finished without producing an explicit review verdict."
		)

		if err != nil {
			// Reviewer session errored, timed out, or was canceled
			verdict = ReviewVerdictIncomplete
			approved = false
			comments = fmt.Sprintf("Incomplete: reviewer execution failed: %v", err)
		} else if reviewerMutated {
			// Reviewer attempted unauthorized workspace modifications
			verdict = ReviewVerdictRejected
			approved = false
			comments = "Rejected: reviewer attempted unauthorized workspace mutations."
			result.Outputs["reviewer_mutation_detected"] = true
		} else if reviewResult == nil {
			// Reviewer completed without providing any verdict
			verdict = ReviewVerdictIncomplete
			approved = false
			comments = "Incomplete: reviewer completed without producing an explicit review verdict."
		} else {
			// Reviewer produced an explicit verdict
			switch reviewResult.Verdict {
			case ReviewVerdictRejected:
				verdict = ReviewVerdictRejected
				approved = false
				comments = reviewResult.Comments
			case ReviewVerdictApproved:
				// Explicit approval from reviewer: verify validation evidence and snapshot integrity
				ev, status := c.resolveValidationEvidence(req, activeWorkspace)
				c.lastEvidence = ev
				switch status {
				case "MISSING":
					verdict = ReviewVerdictRejected
					approved = false
					comments = "Rejected: no valid native test evidence exists for these changes."
				case "STALE":
					verdict = ReviewVerdictRejected
					approved = false
					comments = fmt.Sprintf("Rejected: test evidence %s is STALE (workspace modified after validation).", ev.EvidenceID)
				case "FAILED":
					verdict = ReviewVerdictRejected
					approved = false
					comments = fmt.Sprintf("Rejected: test evidence %s records a failing test (exit code %d).", ev.EvidenceID, ev.ExitCode)
				case "INCOMPLETE":
					verdict = ReviewVerdictIncomplete
					approved = false
					comments = "Incomplete: validation evidence metadata is incomplete or unverifiable."
				case "VALID":
					if ev != nil && ev.Valid && ev.ExitCode == 0 {
						verdict = ReviewVerdictApproved
						approved = true
						comments = reviewResult.Comments
					} else {
						verdict = ReviewVerdictRejected
						approved = false
						comments = "Rejected: test evidence is invalid or recorded non-zero exit code."
					}
				default:
					verdict = ReviewVerdictIncomplete
					approved = false
					comments = fmt.Sprintf("Incomplete: unrecognized validation evidence status %s.", status)
				}
			case ReviewVerdictIncomplete:
				verdict = ReviewVerdictIncomplete
				approved = false
				comments = reviewResult.Comments
			default:
				if !reviewResult.Approved {
					verdict = ReviewVerdictRejected
					approved = false
					comments = reviewResult.Comments
				} else {
					verdict = ReviewVerdictIncomplete
					approved = false
					comments = "Incomplete: reviewer returned unrecognized verdict."
				}
			}
		}

		result.Outputs["verdict"] = string(verdict)
		result.Outputs["approved"] = approved
		result.Outputs["review_comments"] = comments
		result.Outputs["reviewed_snapshot"] = preReviewSnap
		if req.Runtime != "" {
			result.Outputs["host_id"] = req.Runtime
		}
		if req.Model != "" {
			result.Outputs["model"] = req.Model
		}
		if reviewResult != nil && len(reviewResult.Blockers) > 0 {
			result.Outputs["blockers"] = reviewResult.Blockers
		}

		if !approved && c.revisionCount < effectiveLimits.MaxRevisions {
			c.revisionCount++
			// Invalidate existing evidence to force coder to re-run native tests
			c.lastEvidence = nil

			// Check remaining root budget before authorizing revisions
			tree := c.Store.GetTaskTree(rootID)
			var currentTotalConsumed float64
			for _, r := range tree {
				if r.Result != nil {
					currentTotalConsumed += r.Result.Consumed.Consumed
				}
			}

			rootRec, _ := c.Store.Get(rootID)
			rootAlloc := float64(0)
			if rootRec != nil {
				rootAlloc = rootRec.Request.Budget.Allocated
			}
			remainingRootBudget := float64(0)
			if rootAlloc > 0 {
				remainingRootBudget = rootAlloc - currentTotalConsumed
			}

			if rootAlloc > 0 && remainingRootBudget <= 0 {
				decReq := HumanDecisionRequest{
					DecisionID: fmt.Sprintf("dec-revision-budget-%d", time.Now().UnixNano()),
					ActivityID: req.ID,
					ProposedBy: "reviewer",
					Question:   fmt.Sprintf("Code rejected by reviewer (%s), but root task budget is exhausted (consumed %.0f >= allocated %.0f). Human authorization required to extend budget.", comments, currentTotalConsumed, rootAlloc),
					Context: map[string]any{
						"root_id":          rootID,
						"total_consumed":   currentTotalConsumed,
						"allocated_budget": rootAlloc,
						"review_comments":  comments,
					},
					Status:    HumanDecisionPending,
					CreatedAt: time.Now(),
				}
				_ = c.Store.MarkAwaitingHuman(req.ID, decReq)
				result.Status = StatusAwaitingHuman
				result.Outputs["decision_id"] = decReq.DecisionID
				result.Outputs["question"] = decReq.Question
				return result, nil
			}

			revisionID := fmt.Sprintf("%s-revision-%d", req.ID, c.revisionCount)
			reReviewID := fmt.Sprintf("%s-rereview-%d", req.ID, c.revisionCount)

			revisionPool := remainingRootBudget * 0.5
			if rootAlloc == 0 {
				revisionPool = 20000
			}
			coderRevisionBudget := revisionPool * 0.7
			reviewerRevisionBudget := revisionPool * 0.3

			parentReq := *req
			if rootRec != nil {
				parentReq = rootRec.Request
			}

			// Authorize revision subtask via Governor
			coderProposal := ActivityProposal{
				ProposalID:      revisionID,
				ProposedAgentID: "coder",
				Objective:       fmt.Sprintf("Address review rejection: %s", comments),
				RequestedBudget: Budget{Allocated: coderRevisionBudget},
			}
			authCoderReq, authCoderErr := c.Governor.AuthorizeProposal(parentReq, coderProposal, revisionID)
			if authCoderErr != nil {
				decReq := HumanDecisionRequest{
					DecisionID: fmt.Sprintf("dec-auth-rejected-%d", time.Now().UnixNano()),
					ActivityID: req.ID,
					ProposedBy: "governor",
					Question:   fmt.Sprintf("Governor rejected revision proposal: %v. Human decision required.", authCoderErr),
					Context: map[string]any{
						"reason": authCoderErr.Error(),
					},
					Status:    HumanDecisionPending,
					CreatedAt: time.Now(),
				}
				_ = c.Store.MarkAwaitingHuman(req.ID, decReq)
				result.Status = StatusAwaitingHuman
				result.Outputs["decision_id"] = decReq.DecisionID
				result.Outputs["question"] = decReq.Question
				return result, nil
			}

			authCoderReq.ParentID = req.ID
			repairProfile := c.Capabilities.ResolveProfile("coder", "Senior Software Engineer", authCoderReq.Objective, nil, c.Governor)
			authCoderReq.CapabilityProfile = &repairProfile
			if err := c.Store.Enqueue(*authCoderReq); err != nil {
				return nil, err
			}

			// Authorize re-review subtask via Governor
			reReviewProposal := ActivityProposal{
				ProposalID:      reReviewID,
				ProposedAgentID: "reviewer",
				Objective:       fmt.Sprintf("Re-review revision for: %s", req.Objective),
				RequestedBudget: Budget{Allocated: reviewerRevisionBudget},
				DependsOn:       []string{revisionID},
			}
			authReviewReq, authReviewErr := c.Governor.AuthorizeProposal(parentReq, reReviewProposal, reReviewID)
			if authReviewErr == nil {
				authReviewReq.ParentID = req.ID
				reReviewProfile := c.Capabilities.ResolveProfile("reviewer", "Code Reviewer & Quality Engineer", authReviewReq.Objective, nil, c.Governor)
				authReviewReq.CapabilityProfile = &reReviewProfile
				authReviewReq.DependsOn = []string{revisionID}
				_ = c.Store.Enqueue(*authReviewReq)
			}
		} else if approved {
			// Promote memories reviewed by quality reviewer
			for _, m := range proposedMemories {
				_, _ = c.Memory.Promote(m.Key, fmt.Sprintf("reviewer:%s", ident.ID))
			}
			result.Outputs["proposed_commit"] = fmt.Sprintf("feat: %s (verified by A4C %s & native tests)", req.Objective, ident.ID)
			if c.WorktreeMgr != nil {
				rootID := c.getRootTaskID(req)
				_ = c.WorktreeMgr.ReleaseWorktree(rootID, true)
			}
		} else {
			// Not approved and maximum revisions exceeded
			result.Outputs["failure_reason"] = "revision_limit_exhausted"
			if c.WorktreeMgr != nil {
				rootID := c.getRootTaskID(req)
				_ = c.WorktreeMgr.ReleaseWorktree(rootID, false)
			}
		}

		_ = c.Store.Complete(req.ID, *result)
	}

	return result, nil
}

// RunLoop executes steps sequentially until no claimable activities remain or human escalation is needed.
func (c *TeamCoordinator) RunLoop(ctx context.Context, maxSteps int) ([]*ActivityResult, error) {
	if err := c.AcquireWorkspaceLock(); err != nil {
		return nil, err
	}
	defer c.ReleaseWorkspaceLock()

	var results []*ActivityResult
	for i := 0; i < maxSteps; i++ {
		if !c.Store.HasClaimable() {
			break
		}

		res, err := c.Step(ctx)
		if err != nil {
			return results, err
		}
		if res == nil {
			break
		}
		results = append(results, res)

		if res.Status == StatusAwaitingHuman {
			// Paused waiting for engineer input
			break
		}
	}
	return results, nil
}

// ListPendingDecisions returns decisions awaiting human engineer review.
func (c *TeamCoordinator) ListPendingDecisions() []*HumanDecisionRequest {
	return c.Store.ListPendingDecisions()
}

// ResolveDecision allows the human engineer to approve, reject, or reassign work.
func (c *TeamCoordinator) ResolveDecision(decisionID string, resolution string, approve bool, reassignTo string) error {
	return c.Store.ResolveHumanDecision(decisionID, resolution, approve, reassignTo)
}

func (c *TeamCoordinator) getWorkspaceDiff(ctx context.Context, dir string) (string, error) {
	if dir == "" {
		dir = c.WorkspaceDir
	}
	cmd := exec.CommandContext(ctx, "git", "diff", "HEAD")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil && len(strings.TrimSpace(string(out))) > 0 {
		return string(out), nil
	}

	// Fallback status
	cmdStat := exec.CommandContext(ctx, "git", "status", "--short")
	cmdStat.Dir = dir
	outStat, _ := cmdStat.CombinedOutput()
	if len(strings.TrimSpace(string(outStat))) > 0 {
		return string(outStat), nil
	}
	return "No uncommitted git changes detected.", nil
}

// resolveValidationEvidence searches the dependency graph and activity tree for valid test evidence.
// It returns the found ValidationEvidence (if any) and a status string: "VALID", "STALE", "FAILED", "INCOMPLETE", or "MISSING".
func (c *TeamCoordinator) resolveValidationEvidence(req *ActivityRequest, activeWorkspace string) (*ValidationEvidence, string) {
	// Helper to extract authoritative evidence from an activity record
	extractEvidence := func(rec *ActivityRecord) (*ValidationEvidence, string) {
		if rec == nil || rec.Result == nil {
			return nil, "MISSING"
		}
		if len(rec.Result.EvidenceIDs) == 0 && rec.Result.Outputs["validation_evidence"] == nil {
			return nil, "MISSING"
		}

		var ev *ValidationEvidence
		if rawEv, ok := rec.Result.Outputs["validation_evidence"]; ok && rawEv != nil {
			switch v := rawEv.(type) {
			case *ValidationEvidence:
				ev = v
			case ValidationEvidence:
				ev = &v
			case map[string]any:
				b, _ := json.Marshal(v)
				var parsed ValidationEvidence
				if err := json.Unmarshal(b, &parsed); err == nil {
					ev = &parsed
				}
			}
		}

		if ev == nil {
			return nil, "INCOMPLETE"
		}

		// Require actual authoritative execution fields
		if ev.EvidenceID == "" || ev.WorkspaceSnapshotID == "" || len(ev.Command) == 0 || ev.CompletedAt.IsZero() {
			return ev, "INCOMPLETE"
		}

		// Detect failed tests
		if ev.ExitCode != 0 || !ev.Valid {
			return ev, "FAILED"
		}

		// Verify snapshot freshness against active workspace
		if err := c.Snapshotter.ValidateAgainstEvidence(activeWorkspace, *ev); err != nil {
			return ev, "STALE"
		}

		return ev, "VALID"
	}

	// 1. If in-memory lastEvidence is already available, validate it against active workspace
	if c.lastEvidence != nil {
		if c.lastEvidence.ExitCode != 0 || !c.lastEvidence.Valid {
			return c.lastEvidence, "FAILED"
		}
		if err := c.Snapshotter.ValidateAgainstEvidence(activeWorkspace, *c.lastEvidence); err == nil {
			return c.lastEvidence, "VALID"
		}
		return c.lastEvidence, "STALE"
	}

	// 2. Search direct dependencies first
	for _, depID := range req.DependsOn {
		if depRec, err := c.Store.Get(depID); err == nil && depRec != nil {
			if ev, status := extractEvidence(depRec); status != "MISSING" {
				return ev, status
			}
		}
	}

	// 3. Search the entire task tree (e.g. child coder subtasks under lead parent)
	rootID := req.ID
	curr := req
	for curr != nil && curr.ParentID != "" {
		rootID = curr.ParentID
		if parentRec, err := c.Store.Get(curr.ParentID); err == nil && parentRec != nil {
			curr = &parentRec.Request
		} else {
			break
		}
	}

	tree := c.Store.GetTaskTree(rootID)
	// Iterate in reverse chronological order to find most recent evidence
	for i := len(tree) - 1; i >= 0; i-- {
		rec := tree[i]
		if rec.Request.ID == req.ID {
			continue // skip self
		}
		if ev, status := extractEvidence(rec); status != "MISSING" {
			return ev, status
		}
	}

	return nil, "MISSING"
}

// ClearValidationEvidenceForTest resets the coordinator's in-memory lastEvidence cache for testing.
func (c *TeamCoordinator) ClearValidationEvidenceForTest() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastEvidence = nil
}

// parseTextDirectives extracts structured directives from model text output as fallback.
func (c *TeamCoordinator) parseTextDirectives(
	content string,
	props *[]ActivityProposal,
	mems *[]struct{ Key, Content string },
	dec **HumanDecisionRequest,
	rev **ReviewResult,
	activityID string,
	agentID string,
) {
	// Fallback text parsing for human escalation directive: [HUMAN_DECISION: question]
	if strings.Contains(content, "[HUMAN_DECISION:") {
		start := strings.Index(content, "[HUMAN_DECISION:") + len("[HUMAN_DECISION:")
		end := strings.Index(content[start:], "]")
		if end != -1 {
			q := strings.TrimSpace(content[start : start+end])
			*dec = &HumanDecisionRequest{
				DecisionID: fmt.Sprintf("dec-%d", time.Now().UnixNano()),
				ActivityID: activityID,
				ProposedBy: agentID,
				Question:   q,
				Status:     HumanDecisionPending,
				CreatedAt:  time.Now(),
			}
		}
	}

	// Fallback text parsing for review: [REVIEW_APPROVED] or [REVIEW_REJECTED: reason] or [SUBMIT_REVIEW: ...]
	if strings.Contains(content, "[REVIEW_REJECTED:") || (strings.Contains(content, "[SUBMIT_REVIEW:") && strings.Contains(content, "approved=false")) {
		comments := "Rejected"
		if strings.Contains(content, "[REVIEW_REJECTED:") {
			start := strings.Index(content, "[REVIEW_REJECTED:") + len("[REVIEW_REJECTED:")
			end := strings.Index(content[start:], "]")
			if end != -1 {
				comments = strings.TrimSpace(content[start : start+end])
			}
		} else if strings.Contains(content, "comments=") {
			start := strings.Index(content, "comments=") + len("comments=")
			end := strings.Index(content[start:], "]")
			if end != -1 {
				comments = strings.Trim(strings.TrimSpace(content[start:start+end]), "\"")
			}
		}
		*rev = &ReviewResult{
			ActivityID: activityID,
			ReviewerID: agentID,
			Verdict:    ReviewVerdictRejected,
			Approved:   false,
			Comments:   comments,
			Timestamp:  time.Now(),
		}
	} else if strings.Contains(content, "[REVIEW_APPROVED]") || (strings.Contains(content, "[SUBMIT_REVIEW:") && strings.Contains(content, "approved=true")) {
		*rev = &ReviewResult{
			ActivityID: activityID,
			ReviewerID: agentID,
			Verdict:    ReviewVerdictApproved,
			Approved:   true,
			Comments:   "Approved via model directive",
			Timestamp:  time.Now(),
		}
	}

	// Fallback JSON block parsing
	jsonBlock := ""
	if strings.Contains(content, "```json") {
		start := strings.Index(content, "```json") + 7
		end := strings.Index(content[start:], "```")
		if end != -1 {
			jsonBlock = strings.TrimSpace(content[start : start+end])
		}
	} else if strings.HasPrefix(strings.TrimSpace(content), "{") {
		jsonBlock = strings.TrimSpace(content)
	}

	if jsonBlock != "" {
		var parsed struct {
			Proposals []ActivityProposal `json:"proposals"`
			Memories  map[string]string  `json:"memories"`
		}
		if err := json.Unmarshal([]byte(jsonBlock), &parsed); err == nil {
			for _, p := range parsed.Proposals {
				*props = append(*props, p)
			}
			for k, v := range parsed.Memories {
				*mems = append(*mems, struct{ Key, Content string }{Key: k, Content: v})
			}
		}
	}
}

// createDisposableReviewWorkspace provisions an isolated, disposable copy of the workspace
// specifically for reviewer execution. This ensures the authoritative implementation worktree
// is never modified, reset, or cleaned as a side-effect of reviewing.
func (c *TeamCoordinator) createDisposableReviewWorkspace(src string, taskID string) (string, func(), error) {
	if src == "" {
		src = "."
	}
	absSrc, err := filepath.Abs(src)
	if err != nil {
		absSrc = src
	}

	dispDir, err := os.MkdirTemp("", fmt.Sprintf("a4c-review-%s-*", strings.ReplaceAll(taskID, "/", "-")))
	if err != nil {
		return "", nil, fmt.Errorf("coordinator: create disposable review dir: %w", err)
	}

	cleanup := func() {
		_ = os.RemoveAll(dispDir)
	}

	err = filepath.Walk(absSrc, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == absSrc {
			return nil
		}
		rel, err := filepath.Rel(absSrc, path)
		if err != nil {
			return err
		}
		// Skip internal metadata and transient files
		if info.IsDir() && (info.Name() == ".a4c" || info.Name() == ".codetopo" || info.Name() == "disposable-reviews") {
			return filepath.SkipDir
		}

		target := filepath.Join(dispDir, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode()|0700)
		}

		// Handle symlink
		if info.Mode()&os.ModeSymlink != 0 {
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(linkTarget, target)
		}

		// Regular file
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode())
	})
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("coordinator: copy to disposable review workspace: %w", err)
	}

	return dispDir, cleanup, nil
}
