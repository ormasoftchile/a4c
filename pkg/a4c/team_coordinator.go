package a4c

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ormasoftchile/a4c/pkg/a4c/knowledge"
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
	mu               sync.Mutex
	WorkspaceDir     string
	WorktreeMgr      *WorktreeManager
	Store            *DurableActivityStore
	Roster           *TeamRoster
	Memory           *DurableMemoryStore
	Governor         *PolicyGovernor
	Journal          *DurableIntentJournal
	Snapshotter      *WorkspaceSnapshotter
	TestRunner       TestRunner
	Provider         AgentProvider
	Providers        map[string]AgentProvider
	Reconciler       EffectReconciler
	MaxRepair        int
	MaxRevisions     int
	MaxToolCalls     int
	MaxReadFileLines int
	MaxBudgetRetries int
	// MaxVerificationAttempts bounds evidence-collection retries for reviews
	// that end verification_blocked. Independent of MaxRevisions.
	MaxVerificationAttempts   int
	Limits                    ActivityLimits
	lock                      *WorkspaceLock
	coordID                   string
	revisionCount             int
	verificationAttempts      int
	operationalReviewFailures int
	lastEvidence              *ValidationEvidence
	lastEvidenceFingerprint   string
	sessions                  map[string]AgentSession
	Handoff                   *HandoffBuilder
	Capabilities              *CapabilityRegistry
	knowledge                 *knowledge.Service
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
		MaxBudgetRetries: 1,
		// Verification is cheaper than a coding revision and must not be
		// starved: allow a couple of evidence-collection retries before
		// declaring the review terminally blocked.
		MaxVerificationAttempts: 2,
		coordID:                 coordID,
		Limits: ActivityLimits{
			MaxModelTurns:           10,
			MaxToolCalls:            10,
			MaxReadFileLines:        300,
			MaxDiffLines:            150,
			MaxValidationOutput:     4096,
			MaxRevisions:            2,
			MaxVerificationAttempts: 2,
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

// ListHostCapabilities reports discoverable technical capabilities across all registered providers.
func (c *TeamCoordinator) ListHostCapabilities() []HostCapabilities {
	c.mu.Lock()
	defer c.mu.Unlock()
	var result []HostCapabilities
	for name, p := range c.Providers {
		result = append(result, GetHostCapability(p, name))
	}
	if len(result) == 0 && c.Provider != nil {
		result = append(result, GetHostCapability(c.Provider, "default"))
	}
	return result
}

// IsHostAvailable checks whether a specific host runtime is registered and operational.
func (c *TeamCoordinator) IsHostAvailable(hostID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if hostID == "" {
		return true
	}
	// If no providers are configured at all in the coordinator (e.g. lightweight unit tests)
	if len(c.Providers) == 0 && c.Provider == nil {
		return true
	}
	if c.Providers != nil {
		if p, ok := c.Providers[hostID]; ok && p != nil {
			caps := GetHostCapability(p, hostID)
			return caps.Available
		}
	}
	if c.Provider != nil {
		caps := GetHostCapability(c.Provider, hostID)
		return caps.Available
	}
	return false
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

// SetKnowledge wires the optional A4C Knowledge service so the coordinator can
// issue per-activity execution-context tokens for host sessions.
func (c *TeamCoordinator) SetKnowledge(ks *knowledge.Service) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.knowledge = ks
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

	// Worktree-isolation guard: when an isolated worktree is assigned, snapshot
	// the source checkout's dirty state before the session runs so we can detect
	// an agent writing OUTSIDE its assigned workspace. A coding activity that
	// leaves the source checkout dirty while its worktree stays clean is an
	// isolation violation and must not be reported as a successful isolated run.
	isolatedWorktree := !strings.EqualFold(activeWorkspace, c.WorkspaceDir)
	mainRepoBefore := map[string]bool{}
	if isolatedWorktree {
		mainRepoBefore = c.gitDirtySet(c.WorkspaceDir)
	}

	var structuralQueriesCount int
	var fileReadsCount int
	var toolLimitHits, proposalsRecorded int
	var lastToolLimitHit string

	// Resolve capability profile for this activity
	profile := c.Capabilities.ResolveProfile(ident.ID, ident.Role, req.Objective, req.CapabilityProfile, c.Governor)
	req.CapabilityProfile = &profile

	// Lead must always be able to delegate; refuse to start a lead session
	// without governed_propose_activity rather than letting the model discover
	// the missing tool after burning exploration turns.
	isLeadAgent := strings.EqualFold(ident.ID, "lead") || strings.EqualFold(ident.Role, "lead") || strings.EqualFold(ident.ID, "alice")
	if isLeadAgent {
		hasPropose := false
		for _, t := range profile.AllowedTools {
			if t == "governed_propose_activity" {
				hasPropose = true
				break
			}
		}
		if !hasPropose {
			cfgErr := fmt.Errorf("coordinator: lead profile missing governed_propose_activity; cannot delegate")
			_ = c.Store.Fail(req.ID, cfgErr)
			return nil, cfgErr
		}
	}

	// Resolve effective economic and activity limits
	effectiveLimits := ActivityLimits{
		MaxModelTurns:       10,
		MaxToolCalls:        c.MaxToolCalls,
		MaxReadFileLines:    c.MaxReadFileLines,
		MaxDiffLines:        150,
		MaxValidationOutput: 4096,
		MaxRevisions:        c.MaxRevisions,
	}
	if c.Limits.MaxModelTurns > 0 {
		effectiveLimits.MaxModelTurns = c.Limits.MaxModelTurns
	}
	if c.Limits.MaxToolCalls > 0 {
		effectiveLimits.MaxToolCalls = c.Limits.MaxToolCalls
	}
	if c.Limits.MaxReadFileLines > 0 {
		effectiveLimits.MaxReadFileLines = c.Limits.MaxReadFileLines
	}
	if c.Limits.MaxDiffLines > 0 {
		effectiveLimits.MaxDiffLines = c.Limits.MaxDiffLines
	}
	maxRevs := c.MaxRevisions
	if c.Limits.MaxRevisions != 2 {
		maxRevs = c.Limits.MaxRevisions
	}
	maxVerifAttempts := c.MaxVerificationAttempts
	if c.Limits.MaxVerificationAttempts > 0 {
		maxVerifAttempts = c.Limits.MaxVerificationAttempts
	}
	if profile.Limits != nil {
		if profile.Limits.MaxModelTurns > 0 {
			effectiveLimits.MaxModelTurns = profile.Limits.MaxModelTurns
		}
		if profile.Limits.MaxToolCalls > 0 {
			effectiveLimits.MaxToolCalls = profile.Limits.MaxToolCalls
		}
		if profile.Limits.MaxReadFileLines > 0 {
			effectiveLimits.MaxReadFileLines = profile.Limits.MaxReadFileLines
		}
		if profile.Limits.MaxDiffLines > 0 {
			effectiveLimits.MaxDiffLines = profile.Limits.MaxDiffLines
		}
		if profile.Limits.MaxValidationOutput > 0 {
			effectiveLimits.MaxValidationOutput = profile.Limits.MaxValidationOutput
		}
		// Only override the revision budget when the profile explicitly sets
		// one. A profile that merely carries read/tool limits (e.g. the
		// read-only reviewer profile) must not silently zero out MaxRevisions.
		if profile.Limits.MaxRevisions > 0 {
			maxRevs = profile.Limits.MaxRevisions
		}
		if profile.Limits.MaxVerificationAttempts > 0 {
			maxVerifAttempts = profile.Limits.MaxVerificationAttempts
		}
	}

	if req.Limits != nil {
		if req.Limits.MaxModelTurns > 0 {
			effectiveLimits.MaxModelTurns = req.Limits.MaxModelTurns
		}
		if req.Limits.MaxToolCalls > 0 {
			effectiveLimits.MaxToolCalls = req.Limits.MaxToolCalls
		}
		if req.Limits.MaxReadFileLines > 0 {
			effectiveLimits.MaxReadFileLines = req.Limits.MaxReadFileLines
		}
		if req.Limits.MaxDiffLines > 0 {
			effectiveLimits.MaxDiffLines = req.Limits.MaxDiffLines
		}
		if req.Limits.MaxValidationOutput > 0 {
			effectiveLimits.MaxValidationOutput = req.Limits.MaxValidationOutput
		}
		if req.Limits.MaxRevisions > 0 {
			maxRevs = req.Limits.MaxRevisions
		}
		if req.Limits.MaxVerificationAttempts > 0 {
			maxVerifAttempts = req.Limits.MaxVerificationAttempts
		}
	}

	effectiveLimits.MaxRevisions = maxRevs
	effectiveLimits.MaxVerificationAttempts = maxVerifAttempts
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

	// Issue a per-activity execution-context token when A4C Knowledge is
	// enabled, so host knowledge calls are attributed to THIS activity. A
	// delayed call from a completed activity fails its StatusClaimed check.
	knowledgeToken := ""
	knowledgeDir := ""
	if c.knowledge != nil {
		knowledgeDir = c.WorkspaceDir // main repo owns the shared knowledge DB
		if tok, terr := c.knowledge.IssueToken(knowledge.ExecContext{
			ProjectID:  c.knowledge.Project(),
			TaskID:     rootID,
			ActivityID: req.ID,
			Role:       ident.ID,
			Host:       req.Runtime,
		}); terr == nil {
			knowledgeToken = tok
		}
	}

	config := AgentConfig{
		AgentID:          ident.ID,
		Charter:          ident.Charter,
		WorkspaceDir:     sessionWorkspace,
		KnowledgeToken:   knowledgeToken,
		KnowledgeDir:     knowledgeDir,
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
			proposalsRecorded++
		},
		OnToolLimitHit: func(toolName string) {
			toolLimitHits++
			lastToolLimitHit = toolName
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
			// First recorded verdict wins; a duplicate submission cannot
			// overwrite a settled outcome.
			if reviewResult == nil {
				reviewResult = &rev
			}
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
		pe := &ProviderError{
			Kind:        ProviderFailureTemporaryOutage,
			HostID:      req.Runtime,
			Message:     err.Error(),
			OriginalErr: err,
		}
		intResult := ActivityResult{
			ID:     req.ID,
			Status: StatusInterrupted,
			Outputs: map[string]any{
				"error":                err.Error(),
				"interruption_reason":  string(pe.Kind),
				"interruption_message": pe.Message,
				"host_id":              req.Runtime,
			},
		}
		_ = c.Store.InterruptWithResult(req.ID, intResult, pe)
		return &intResult, pe
	}
	// Same-host continuation: if this activity previously ran on this host and
	// the host supports session resumption, resume the original session to
	// preserve conversational context rather than starting fresh. Only used for
	// a resumed/re-claimed activity (a fresh activity has no session ref).
	var session AgentSession
	if ref := c.LoadSessionRef(req.ID); ref != nil && ref.SessionID != "" && ref.Runtime == req.Runtime {
		if prov.Capabilities().SessionResumption {
			if resumed, rerr := prov.ResumeSession(ctx, ref.SessionID, config); rerr == nil && resumed != nil {
				session = resumed
			}
		}
	}
	if session == nil {
		var err error
		session, err = prov.CreateSession(ctx, config)
		if err != nil {
			if pe := ClassifyProviderError(req.Runtime, req.Model, err, ""); pe != nil {
				intResult := ActivityResult{
					ID:     req.ID,
					Status: StatusInterrupted,
					Outputs: map[string]any{
						"error":                err.Error(),
						"interruption_reason":  string(pe.Kind),
						"interruption_message": pe.Message,
						"host_id":              pe.HostID,
						"model":                pe.Model,
					},
				}
				_ = c.Store.InterruptWithResult(req.ID, intResult, pe)
				return &intResult, pe
			}
			_ = c.Store.Fail(req.ID, err)
			return nil, fmt.Errorf("coordinator: failed to initialize session for %s: %w", ident.ID, err)
		}
		// Persist the session reference so an interrupted task can resume it.
		c.SaveSessionRef(req.ID, req.Runtime, session.SessionID(), req.Model)
	}
	defer session.Close()

	// 6. Build prompt with injected verified team memory & role-appropriate context via HandoffBuilder
	diff := ""
	evStatus := "none"
	var bundle *reviewBundle
	var (
		reviewEvidenceDir   string
		reviewFilesChanged  []string
		coderHandoff        *CoderHandoff
		priorFindings       []ReviewFinding
		diffHygieneWarnings []string
	)
	if isReviewerRole {
		diff, _ = c.getWorkspaceDiff(ctx, activeWorkspace)
		c.lastEvidence, evStatus = c.resolveValidationEvidence(req, activeWorkspace)
		// Materialize the evidence bundle BEFORE constructing the summary so
		// every evidence reference handed to the reviewer resolves inside the
		// reviewer's own workspace (never the coder's .a4c paths).
		reviewFilesChanged = c.listChangedFiles(ctx, activeWorkspace)
		coderHandoff = c.buildCoderHandoff(rootID)
		priorFindings = c.collectPriorFindings(rootID, req.ID)
		diffHygieneWarnings = c.checkDiffHygiene(ctx, activeWorkspace, reviewFilesChanged, req.Objective)
		bundle = c.materializeReviewEvidence(sessionWorkspace, req, diff, reviewFilesChanged, coderHandoff, priorFindings, c.lastEvidence, evStatus)
		reviewEvidenceDir = bundle.Dir
	}

	// Evidence summary references only review-workspace-relative paths that
	// were verified to resolve, so the reviewer can actually read them.
	evidenceSummary := "none"
	if c.lastEvidence != nil {
		ev := c.lastEvidence
		evidenceSummary = fmt.Sprintf(
			"Evidence ID: %s | Status: %s | Command: %s | ExitCode: %d | TestsExecuted: %d | Vacuous: %t | Duration: %v | Valid: %t",
			ev.EvidenceID, evStatus, strings.Join(ev.Command, " "), ev.ExitCode,
			ev.TestsExecuted, ev.Vacuous, ev.Duration.Round(time.Millisecond), ev.Valid)
		if bundle != nil && bundle.LogAvailable {
			evidenceSummary += fmt.Sprintf("\nFull test log (read this file for complete output): %s", bundle.LogPath)
		} else if bundle != nil && len(bundle.Missing) > 0 {
			evidenceSummary += "\nFull test log: UNAVAILABLE in the review workspace. Reason: " + strings.Join(bundle.Missing, "; ")
		}
		evidenceSummary += fmt.Sprintf("\nTest Output (summary):\n%s", ev.OutputSummary)
		if ev.Vacuous || ev.TestsExecuted == 0 {
			evidenceSummary += "\nWARNING: this evidence ran but exercised ZERO tests. It is NOT proof of correctness. Treat as a verification gap, not a pass."
		}
	} else if isReviewerRole && bundle != nil && len(bundle.Missing) > 0 {
		evidenceSummary = "No validation evidence is available. Reasons: " + strings.Join(bundle.Missing, "; ")
	}

	var checkpoint *TaskCheckpoint
	if req.Inputs != nil {
		checkpoint = CheckpointFromInput(req.Inputs["checkpoint"])
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

		ReviewEvidenceDir:   reviewEvidenceDir,
		FilesChanged:        reviewFilesChanged,
		CoderHandoff:        coderHandoff,
		PriorFindings:       priorFindings,
		DiffHygieneWarnings: diffHygieneWarnings,
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
	consumed := Budget{Consumed: usage.BilledConsumption()}
	isLeadEarly := strings.EqualFold(ident.ID, "lead") || strings.EqualFold(ident.Role, "lead") || strings.EqualFold(ident.ID, "alice")

	metrics := &ExecutionMetrics{
		WallClockDuration:      time.Since(startWall),
		ProviderWaitDuration:   modelDur,
		ModelDuration:          modelDur,
		ToolDuration:           toolDur,
		PromptTokens:           usage.PromptTokens,
		CompletionTokens:       usage.CompletionTokens,
		ReasoningTokens:        usage.ReasoningTokens,
		CachedTokens:           usage.CachedTokens,
		BilledTokens:           usage.BilledTokens,
		ReportedCostUSD:        usage.ReportedCostUSD,
		TotalTokens:            usage.TotalTokens,
		PeakContextTokens:      usage.PeakContextTokens,
		OverrunTokens:          usage.OverrunTokens,
		ModelInvocations:       1,
		StructuralQueriesCount: structuralQueriesCount,
		FileReadsCount:         fileReadsCount,
		ProposalsRecorded:      proposalsRecorded,
		ToolLimitHits:          toolLimitHits,
	}

	if c.Governor != nil {
		c.Governor.RecordConsumption(req.ID, rootID, consumed.Consumed)
	}

	if err != nil {
		pe, isProviderFail := AsProviderFailure(err)
		if !isProviderFail {
			pe = ClassifyProviderError(req.Runtime, req.Model, err, "")
			if pe != nil {
				isProviderFail = true
			}
		}

		if isProviderFail {
			intResult := ActivityResult{
				ID:       req.ID,
				Status:   StatusInterrupted,
				Consumed: consumed,
				Metrics:  metrics,
				Outputs: map[string]any{
					"error":                err.Error(),
					"interruption_reason":  string(pe.Kind),
					"interruption_message": pe.Message,
					"host_id":              pe.HostID,
					"model":                pe.Model,
				},
			}
			_ = c.Store.InterruptWithResult(req.ID, intResult, pe)
			return &intResult, pe
		}

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
			// Specialist mid-session exhaustion: the agentic loop may already
			// have produced a verified workspace. Validate before retrying.
			if !isLeadEarly && !isReviewerRole && c.TestRunner != nil {
				if vres, testErr := c.runProjectVerification(ctx, activeWorkspace); testErr == nil && vres.ExitCode == 0 && !vres.Vacuous && !vres.Unavailable {
					failResult.Status = StatusCompleted
					failResult.Outputs["budget_overrun"] = true
					failResult.Outputs["budget_overrun_tokens"] = consumed.Consumed - req.Budget.Allocated
					failResult.Outputs["budget_overrun_note"] = "session exhausted budget but project tests pass on the resulting workspace; completing verified work"
					failResult.Outputs["test_output_summary"] = truncateSummary(vres.Output, 500)
					c.attachSalvageEvidence(req, activeWorkspace, vres, &failResult)
					_ = c.Store.Complete(req.ID, failResult)
					return &failResult, nil
				}
			}
			if rec, ok := c.maybeReallocateBudget(req, consumed.Consumed, &failResult); ok {
				return rec, nil
			}
		}
		_ = c.Store.FailWithResult(req.ID, failResult, err)
		return nil, err
	}

	// Post-flight budget check: verify activity didn't exceed allocated budget.
	// For specialists that already ran native tests inside the agentic Send
	// loop (Copilot multi-turn), the workspace may hold a verified fix even
	// when the session overran. Prefer completing verified work over
	// discarding it and re-running the full suite from scratch.
	if req.Budget.Allocated > 0 && consumed.Consumed > req.Budget.Allocated {
		overrunBy := consumed.Consumed - req.Budget.Allocated
		// Specialists only: lead/reviewer never produce implementation to salvage.
		if !isLeadEarly && !isReviewerRole && c.TestRunner != nil {
			if vres, testErr := c.runProjectVerification(ctx, activeWorkspace); testErr == nil && vres.ExitCode == 0 && !vres.Vacuous && !vres.Unavailable {
				salvage := ActivityResult{
					ID:       req.ID,
					Status:   StatusCompleted,
					Consumed: consumed,
					Metrics:  metrics,
					Outputs: map[string]any{
						"summary":               truncateSummary(resp.Content, 500),
						"session_id":            session.SessionID(),
						"budget_overrun":        true,
						"budget_overrun_tokens": overrunBy,
						"budget_overrun_note": fmt.Sprintf(
							"session exceeded allocation by %.0f tokens but project tests pass on the resulting workspace; completing verified work",
							overrunBy),
						"test_output_summary": truncateSummary(vres.Output, 500),
					},
				}
				c.attachSalvageEvidence(req, activeWorkspace, vres, &salvage)
				if c.Governor != nil {
					c.Governor.RecordConsumption(req.ID, rootID, overrunBy)
				}
				_ = c.Store.Complete(req.ID, salvage)
				return &salvage, nil
			}
		}
		overrunErr := fmt.Errorf("%w: activity %s consumed %.0f tokens exceeds allocated %.0f",
			ErrBudgetExceeded, req.ID, consumed.Consumed, req.Budget.Allocated)
		failResult := ActivityResult{
			ID:       req.ID,
			Status:   StatusFailed,
			Consumed: consumed,
			Metrics:  metrics,
			Outputs: map[string]any{
				"error":             overrunErr.Error(),
				"failure_reason":    "budget_exhausted",
				"billed_tokens":     metrics.BilledTokens,
				"total_tokens_host": metrics.TotalTokens,
				"cached_tokens":     metrics.CachedTokens,
			},
		}
		if rec, ok := c.maybeReallocateBudget(req, consumed.Consumed, &failResult); ok {
			return rec, nil
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
			"summary":            resp.Content,
			"session_id":         session.SessionID(),
			"proposals_recorded": proposalsRecorded,
			"tool_limit_hits":    toolLimitHits,
		},
	}

	isLead := strings.EqualFold(ident.ID, "lead") || strings.EqualFold(ident.Role, "lead") || strings.EqualFold(ident.ID, "alice")

	if isLead {
		// A lead that finishes with zero delegated children and no human
		// escalation has failed its role — unless child activities were
		// already enqueued for this task. Perform ONE bounded recovery: re-prompt
		// the lead for an executable assignment before declaring coordination
		// failure. A planning-only result cannot satisfy an implementation
		// obligation, and the model must not decide whether delegation exists.
		if len(proposedActivities) == 0 && decisionRequest == nil && !c.storeHasChildren(req.ID) {
			corrective := "You ended your turn without delegating any work. This task REQUIRES delegated implementation. " +
				"Propose at least one coder subtask now by returning ONLY this JSON block, filled in for the task:\n" +
				"```json\n{\"proposals\": [{\"proposal_id\": \"impl-1\", \"proposed_agent_id\": \"coder\", \"objective\": \"<implement the objective>\", \"budget_tokens\": 150000}]}\n```"
			if retryResp, rerr := session.Send(ctx, corrective); rerr == nil && retryResp != nil {
				// The host adapter re-invokes OnProposeActivity for this turn,
				// appending any proposals to proposedActivities.
				_ = retryResp
			}
			// Re-read usage so the corrective turn is accounted for.
			if retryUsage := session.Usage(); float64(retryUsage.TotalTokens) > consumed.Consumed {
				delta := retryUsage.BilledConsumption() - consumed.Consumed
				consumed = Budget{Consumed: retryUsage.BilledConsumption()}
				metrics.BilledTokens = retryUsage.BilledTokens
				metrics.TotalTokens = retryUsage.TotalTokens
				metrics.PromptTokens = retryUsage.PromptTokens
				metrics.CompletionTokens = retryUsage.CompletionTokens
				if c.Governor != nil && delta > 0 {
					c.Governor.RecordConsumption(req.ID, rootID, delta)
				}
			}
		}
		if len(proposedActivities) == 0 && decisionRequest == nil && !c.storeHasChildren(req.ID) {
			noDelegErr := fmt.Errorf("coordinator: lead completed without proposing any child activities")
			guidance := "delegate implementation to 'coder' via governed_propose_activity before ending the turn; coordination tools (propose_activity, request_human_decision) are exempt from the tool-call cap"
			if toolLimitHits > 0 {
				guidance = fmt.Sprintf("%s (last tool-limit rejection: %s, total hits: %d)", guidance, lastToolLimitHit, toolLimitHits)
			}
			failResult := ActivityResult{
				ID:       req.ID,
				Status:   StatusFailed,
				Consumed: consumed,
				Metrics:  metrics,
				Outputs: map[string]any{
					"error":              noDelegErr.Error(),
					"failure_reason":     "no_delegation",
					"lead_summary":       resp.Content,
					"proposals_recorded": proposalsRecorded,
					"tool_limit_hits":    toolLimitHits,
					"last_tool_limit":    lastToolLimitHit,
					"guidance":           guidance,
				},
			}
			_ = c.Store.FailWithResult(req.ID, failResult, noDelegErr)
			return &failResult, noDelegErr
		}

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
				// Explicit actionable terminal state: the delegation itself
				// could not be funded. Record it on the lead activity instead
				// of stranding it in-progress.
				rejectResult := ActivityResult{
					ID:       req.ID,
					Status:   StatusFailed,
					Consumed: consumed,
					Metrics:  metrics,
					Outputs: map[string]any{
						"error":              authErr.Error(),
						"failure_reason":     "budget_insufficient",
						"rejected_agent":     prop.ProposedAgentID,
						"rejected_objective": prop.Objective,
						"guidance":           "propose a smaller-scope activity, reduce prior allocations, or escalate via governed_request_human_decision",
					},
				}
				_ = c.Store.FailWithResult(req.ID, rejectResult, authErr)
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

		// Policy-owned enforcement: a Team-strategy implementation task
		// requires an independent review of the final candidate. The lead may
		// cast the reviewer dynamically, but it cannot OMIT mandatory review.
		// If the lead proposed implementation without a reviewer, the
		// coordinator schedules one through existing dynamic-casting and
		// activity-creation mechanisms. This is policy, not a prompt.
		if req.Strategy == StrategyTeam {
			c.enforceMandatoryReview(req, result)
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
		vres, verr := c.runProjectVerification(ctx, activeWorkspace)
		valDuration := time.Since(valStart)
		if verr != nil {
			_ = c.Store.Fail(req.ID, verr)
			return nil, verr
		}
		exitCode, testOut := vres.ExitCode, vres.Output

		afterSnapshot, err := c.Snapshotter.ComputeSnapshot(activeWorkspace)
		if err != nil {
			_ = c.Store.Fail(req.ID, err)
			return nil, err
		}

		// Generated build artifacts are excluded from the snapshot (the
		// snapshotter ignores the repository's build/ output dir, matching
		// .gitignore), so a legitimate C build does not trip this check. A
		// difference here therefore indicates a genuine unauthorized source
		// modification during validation, which must still fail closed.
		if err := c.Snapshotter.ValidateIntegrity(beforeSnapshot, afterSnapshot); err != nil {
			_ = c.Store.Fail(req.ID, err)
			return nil, err
		}

		// Bounded repair loop if tests fail
		if exitCode != 0 {
			repaired := false
			repairAttempts := 0
			var repairErr error
			for repairAttempts < c.MaxRepair {
				repairAttempts++
				repairPrompt := fmt.Sprintf("Tests failed with exit code %d:\n%s\nPlease fix the failure in the workspace and ensure all tests pass.", exitCode, testOut)
				_, rErr := session.Send(ctx, repairPrompt)
				if rErr != nil {
					repairErr = rErr
					break
				}

				preSnap, _ := c.Snapshotter.ComputeSnapshot(activeWorkspace)
				rValStart := time.Now()
				rres, _ := c.runProjectVerification(ctx, activeWorkspace)
				exitCode, testOut = rres.ExitCode, rres.Output
				vres = rres
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
				// Budget exhaustion during repair is a budget outcome, not a
				// coding failure: route it through recovery/terminal budget
				// handling instead of mislabeling it as failed tests.
				if repairErr != nil && errors.Is(repairErr, ErrBudgetExceeded) {
					repUsage := session.Usage()
					repConsumed := Budget{Consumed: repUsage.BilledConsumption()}
					metrics.PromptTokens = repUsage.PromptTokens
					metrics.CompletionTokens = repUsage.CompletionTokens
					metrics.CachedTokens = repUsage.CachedTokens
					metrics.BilledTokens = repUsage.BilledTokens
					metrics.TotalTokens = repUsage.TotalTokens
					metrics.OverrunTokens = repUsage.OverrunTokens
					if c.Governor != nil {
						c.Governor.RecordConsumption(req.ID, rootID, repConsumed.Consumed-consumed.Consumed)
						consumed = repConsumed
					}
					budgetFail := ActivityResult{
						ID:       req.ID,
						Status:   StatusFailed,
						Consumed: repConsumed,
						Metrics:  metrics,
						Outputs: map[string]any{
							"error":          repairErr.Error(),
							"failure_reason": "budget_exhausted",
						},
					}
					if rec, ok := c.maybeReallocateBudget(req, repConsumed.Consumed, &budgetFail); ok {
						return rec, nil
					}
					_ = c.Store.FailWithResult(req.ID, budgetFail, repairErr)
					return nil, repairErr
				}
				failErr := fmt.Errorf("tests failed with exit code %d after %d repairs: %s", exitCode, repairAttempts, testOut)
				_ = c.Store.Fail(req.ID, failErr)
				if c.WorktreeMgr != nil {
					rootID := c.getRootTaskID(req)
					_ = c.WorktreeMgr.ReleaseWorktree(rootID, false)
				}
				return nil, failErr
			}
		}

		// Worktree-isolation enforcement: a mutating coding activity must leave
		// its changes in the ASSIGNED worktree. If the worktree is clean but the
		// source checkout gained new dirty files during this activity, the agent
		// wrote outside its workspace — fail explicitly rather than accept an
		// apparently successful isolated execution.
		if escaped := c.detectWorkspaceEscape(ctx, activeWorkspace, mainRepoBefore); len(escaped) > 0 {
			isoErr := fmt.Errorf("workspace isolation violated: agent modified the source checkout, not the assigned worktree: %s", strings.Join(escaped, ", "))
			isoResult := ActivityResult{
				ID:       req.ID,
				Status:   StatusFailed,
				Consumed: consumed,
				Metrics:  metrics,
				Outputs: map[string]any{
					"error":              isoErr.Error(),
					"failure_reason":     "workspace_escape",
					"escaped_paths":      escaped,
					"assigned_workspace": activeWorkspace,
					"source_checkout":    c.WorkspaceDir,
					"guidance":           "the agent must perform repository mutations inside its assigned isolated worktree, not the source checkout",
				},
			}
			_ = c.Store.FailWithResult(req.ID, isoResult, isoErr)
			if c.WorktreeMgr != nil {
				_ = c.WorktreeMgr.ReleaseWorktree(rootID, false)
			}
			return &isoResult, isoErr
		}

		// Native tests verified! Create evidence and promote memories
		metrics.ValidationDuration = valDuration
		outSummary := testOut
		if len(outSummary) > 500 {
			outSummary = outSummary[:500] + "... [truncated]"
		}
		evidenceID := fmt.Sprintf("evi-%s-%d", req.ID, time.Now().UnixNano())
		logPath := c.persistEvidenceLog(evidenceID, testOut)
		evidence := ValidationEvidence{
			EvidenceID:          evidenceID,
			ActivityID:          req.ID,
			TaskID:              rootID,
			WorkspaceDir:        activeWorkspace,
			WorkspaceSnapshotID: afterSnapshot,
			Command:             vres.Command,
			ExitCode:            exitCode,
			OutputSummary:       outSummary,
			Duration:            valDuration,
			LogPath:             logPath,
			HostID:              req.Runtime,
			AgentID:             ident.ID,
			Valid:               exitCode == 0 && !vres.Vacuous,
			TestsExecuted:       vres.TestsExecuted,
			Vacuous:             vres.Vacuous,
			SourceRevision:      vres.SourceRevision,
			StartedAt:           valStart,
			CompletedAt:         time.Now(),
		}
		// A verification that could not establish what ran (unavailable or
		// vacuous) is never a valid pass.
		if vres.Unavailable || vres.Vacuous {
			evidence.Valid = false
		}
		c.lastEvidence = &evidence
		result.EvidenceIDs = append(result.EvidenceIDs, evidence.EvidenceID)
		result.Outputs["snapshot_id"] = afterSnapshot
		result.Outputs["test_output_summary"] = outSummary
		result.Outputs["validation_duration_ms"] = valDuration.Milliseconds()
		result.Outputs["validation_evidence"] = evidence
		if vres.Command != nil {
			result.Outputs["verification_commands"] = []string{strings.Join(vres.Command, " ")}
		}
		result.Outputs["test_exit_codes"] = []int{exitCode}
		if logPath != "" {
			result.Outputs["test_log_paths"] = []string{logPath}
		}
		// Publish the changed-file list so a reviewer can verify without
		// re-deriving what the implementer already established.
		changedFiles := c.listChangedFiles(ctx, activeWorkspace)
		result.Outputs["files_changed"] = changedFiles
		// Extract concrete test files that were added/modified so the reviewer
		// knows exactly where the new coverage lives.
		var testsAdded []string
		for _, f := range changedFiles {
			if strings.Contains(strings.ToLower(f), "test") {
				testsAdded = append(testsAdded, f)
			}
		}
		if len(testsAdded) > 0 {
			result.Outputs["tests_added"] = testsAdded
		}

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
			case ReviewVerdictVerificationBlocked:
				// Reviewer explicitly reported it could not verify. This is a
				// capability/evidence gap, not an asserted defect, so it must
				// never consume a coding revision.
				verdict = ReviewVerdictVerificationBlocked
				approved = false
				comments = reviewResult.Comments
			case ReviewVerdictApproved:
				// Explicit approval from reviewer: verify validation evidence and snapshot integrity
				ev, status := c.resolveValidationEvidence(req, activeWorkspace)
				c.lastEvidence = ev
				switch status {
				case "MISSING":
					// The reviewer approved but no evidence exists. That is a
					// verification gap, not proof of a defect.
					verdict = ReviewVerdictVerificationBlocked
					approved = false
					comments = "Verification blocked: no native test evidence exists for these changes. Evidence collection required; no implementation defect demonstrated."
				case "STALE":
					verdict = ReviewVerdictVerificationBlocked
					approved = false
					comments = fmt.Sprintf("Verification blocked: test evidence %s is STALE (workspace modified after validation). Re-verification required.", ev.EvidenceID)
				case "FAILED":
					// Tests actually ran and failed: a demonstrated defect.
					verdict = ReviewVerdictRejected
					approved = false
					comments = fmt.Sprintf("Rejected: test evidence %s records a failing test (exit code %d).", ev.EvidenceID, ev.ExitCode)
				case "VACUOUS":
					// Evidence exists but exercised no tests (e.g. `go test`
					// on a repository with no Go test files). It proves
					// nothing, so approval cannot stand -- but this is a
					// verification gap, not an implementation defect.
					verdict = ReviewVerdictVerificationBlocked
					approved = false
					comments = fmt.Sprintf("Verification blocked: evidence %s ran %q but exercised no tests (vacuous). Real verification is required; no implementation defect demonstrated.", ev.EvidenceID, strings.Join(ev.Command, " "))
				case "INCOMPLETE":
					verdict = ReviewVerdictVerificationBlocked
					approved = false
					comments = "Verification blocked: validation evidence metadata is incomplete or unverifiable."
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

		// Verification-blocked routing: the reviewer could not establish
		// correctness. This is an evidence/capability gap, not an asserted
		// implementation defect, so it must NOT consume MaxRevisions and must
		// NOT schedule a coding revision. Instead we collect evidence (bounded)
		// and retry verification without re-running the coder.
		if verdict == ReviewVerdictVerificationBlocked {
			result.Outputs["failure_reason"] = "verification_blocked"
			result.Outputs["verification_gap"] = comments

			// Capture the current evidence fingerprint. If a prior review was
			// blocked for the same reason against the same unchanged evidence,
			// repeating the cycle adds no signal: stop with an explicit
			// diagnostic rather than looping.
			fingerprint := c.evidenceFingerprint(req, activeWorkspace)
			repeated := fingerprint != "" && fingerprint == c.lastEvidenceFingerprint
			c.lastEvidenceFingerprint = fingerprint

			if repeated {
				// Same gap, same evidence, no new capability: terminate safely.
				result.Outputs["failure_reason"] = "verification_blocked_repeated"
				result.Outputs["verification_note"] = "reviewer reported the same verification gap against unchanged evidence; stopping to avoid a futile review loop"
				decReq := HumanDecisionRequest{
					DecisionID: fmt.Sprintf("dec-verification-blocked-%d", time.Now().UnixNano()),
					ActivityID: req.ID,
					ProposedBy: "reviewer",
					Question: fmt.Sprintf(
						"Review is terminally verification-blocked: %s. No implementation defect was demonstrated, but evidence could not be established after repeated attempts. Human decision required.",
						comments),
					Context: map[string]any{
						"root_id":          rootID,
						"verification_gap": comments,
						"evidence_status":  c.evidenceStatusString(req, activeWorkspace),
					},
					Status:    HumanDecisionPending,
					CreatedAt: time.Now(),
				}
				_ = c.Store.MarkAwaitingHuman(req.ID, decReq)
				result.Status = StatusAwaitingHuman
				result.Outputs["decision_id"] = decReq.DecisionID
				result.Outputs["question"] = decReq.Question
				_ = c.Store.Complete(req.ID, *result)
				return result, nil
			}

			c.verificationAttempts++
			maxVerif := effectiveLimits.MaxVerificationAttempts
			if maxVerif <= 0 {
				maxVerif = 2
			}

			if c.verificationAttempts > maxVerif {
				// Bounded verification exhausted. Report an explicit blocked
				// state distinct from revision_limit_exhausted.
				result.Outputs["failure_reason"] = "verification_blocked_exhausted"
				result.Outputs["verification_attempts"] = c.verificationAttempts
				decReq := HumanDecisionRequest{
					DecisionID: fmt.Sprintf("dec-verification-blocked-%d", time.Now().UnixNano()),
					ActivityID: req.ID,
					ProposedBy: "reviewer",
					Question: fmt.Sprintf(
						"Review is verification-blocked after %d bounded evidence-collection attempts: %s. No implementation defect was demonstrated. Human decision required.",
						c.verificationAttempts, comments),
					Context: map[string]any{
						"root_id":               rootID,
						"verification_gap":      comments,
						"verification_attempts": c.verificationAttempts,
					},
					Status:    HumanDecisionPending,
					CreatedAt: time.Now(),
				}
				_ = c.Store.MarkAwaitingHuman(req.ID, decReq)
				result.Status = StatusAwaitingHuman
				result.Outputs["decision_id"] = decReq.DecisionID
				result.Outputs["question"] = decReq.Question
				_ = c.Store.Complete(req.ID, *result)
				return result, nil
			}

			// Bounded evidence collection: the coordinator (not the reviewer)
			// holds execution authority, so it re-runs native tests to produce
			// durable, reviewer-readable evidence, then retries the review
			// WITHOUT scheduling a coding revision.
			collected := c.collectVerificationEvidence(ctx, req, activeWorkspace, result)
			if !collected {
				result.Outputs["failure_reason"] = "verification_blocked_no_evidence"
				result.Outputs["verification_note"] = "evidence collection did not produce usable verification; human decision required"
				decReq := HumanDecisionRequest{
					DecisionID: fmt.Sprintf("dec-verification-blocked-%d", time.Now().UnixNano()),
					ActivityID: req.ID,
					ProposedBy: "reviewer",
					Question: fmt.Sprintf(
						"Review is verification-blocked and automatic evidence collection failed: %s. No implementation defect was demonstrated. Human decision required.",
						comments),
					Context: map[string]any{
						"root_id":          rootID,
						"verification_gap": comments,
					},
					Status:    HumanDecisionPending,
					CreatedAt: time.Now(),
				}
				_ = c.Store.MarkAwaitingHuman(req.ID, decReq)
				result.Status = StatusAwaitingHuman
				result.Outputs["decision_id"] = decReq.DecisionID
				result.Outputs["question"] = decReq.Question
				_ = c.Store.Complete(req.ID, *result)
				return result, nil
			}

			// Evidence was collected. Re-enqueue a verification retry that
			// depends on nothing new from the coder; it only re-reads the now
			// available evidence. This deliberately does NOT touch
			// revisionCount.
			result.Outputs["evidence_collected"] = true
			result.Outputs["verification_attempt"] = c.verificationAttempts
			parentReq := *req
			if rootRec, err := c.Store.Get(rootID); err == nil && rootRec != nil {
				parentReq = rootRec.Request
			}
			verifyID := fmt.Sprintf("%s-verify-%d", req.ID, c.verificationAttempts)
			verifyBudget := float64(20000)
			if tree := c.Store.GetTaskTree(rootID); len(tree) > 0 {
				var totalConsumed float64
				for _, r := range tree {
					if r.Result != nil {
						totalConsumed += r.Result.Consumed.Consumed
					}
				}
				if rootRec, err := c.Store.Get(rootID); err == nil && rootRec != nil {
					if alloc := rootRec.Request.Budget.Allocated; alloc > 0 {
						if remain := alloc - totalConsumed; remain > verifyBudget {
							verifyBudget = remain * 0.2
						}
					}
				}
			}
			verifyProposal := ActivityProposal{
				ProposalID:      verifyID,
				ProposedAgentID: "reviewer",
				Objective: fmt.Sprintf(
					"Re-verify after evidence collection for: %s. Evidence has now been collected; inspect it and the diff, then submit your verdict. If you still cannot verify, report [REVIEW_BLOCKED: reason].",
					req.Objective),
				RequestedBudget: Budget{Allocated: verifyBudget},
				DependsOn:       []string{req.ID},
			}
			if authVerify, verr := c.Governor.AuthorizeProposal(parentReq, verifyProposal, verifyID); verr == nil {
				authVerify.ParentID = req.ID
				verifyProfile := c.Capabilities.ResolveProfile("reviewer", "Code Reviewer & Quality Engineer", authVerify.Objective, nil, c.Governor)
				authVerify.CapabilityProfile = &verifyProfile
				authVerify.DependsOn = []string{req.ID}
				if err := c.Store.Enqueue(*authVerify); err == nil {
					_ = c.Store.Complete(req.ID, *result)
					return result, nil
				}
			}
			// Fall through to terminal blocked state if enqueue failed.
			result.Outputs["failure_reason"] = "verification_blocked_enqueue_failed"
			_ = c.Store.Complete(req.ID, *result)
			return result, nil
		}

		// Operational-failure routing: the reviewer did not produce a valid
		// verdict (session/tool/lifecycle failure), NOT an implementation
		// defect. It must never schedule a coding revision or consume
		// MaxRevisions. Instead we retry the REVIEW a bounded number of times
		// so correcting the operational failure resumes review without
		// repeating implementation.
		if verdict == ReviewVerdictIncomplete {
			c.operationalReviewFailures++
			maxOpFailures := 2
			if effectiveLimits.MaxVerificationAttempts > 0 {
				maxOpFailures = effectiveLimits.MaxVerificationAttempts
			}
			result.Outputs["failure_reason"] = "review_operational_failure"
			result.Outputs["operational_failure"] = comments
			if c.operationalReviewFailures > maxOpFailures {
				result.Outputs["failure_reason"] = "review_operational_failure_exhausted"
				decReq := HumanDecisionRequest{
					DecisionID: fmt.Sprintf("dec-review-operational-%d", time.Now().UnixNano()),
					ActivityID: req.ID,
					ProposedBy: "reviewer",
					Question: fmt.Sprintf(
						"Review could not be completed after %d attempts due to an operational failure (no valid verdict): %s. No implementation defect was demonstrated. Human decision required.",
						c.operationalReviewFailures, comments),
					Context: map[string]any{
						"root_id":  rootID,
						"failure":  comments,
						"attempts": c.operationalReviewFailures,
					},
					Status:    HumanDecisionPending,
					CreatedAt: time.Now(),
				}
				_ = c.Store.MarkAwaitingHuman(req.ID, decReq)
				result.Status = StatusAwaitingHuman
				result.Outputs["decision_id"] = decReq.DecisionID
				result.Outputs["question"] = decReq.Question
				_ = c.Store.Complete(req.ID, *result)
				return result, nil
			}
			// Re-enqueue a reviewer retry. This does NOT touch revisionCount
			// and does NOT schedule a coder.
			parentReq := *req
			if rootRec, err := c.Store.Get(rootID); err == nil && rootRec != nil {
				parentReq = rootRec.Request
			}
			retryID := fmt.Sprintf("%s-reviewretry-%d", req.ID, c.operationalReviewFailures)
			retryBudget := float64(20000)
			retryProposal := ActivityProposal{
				ProposalID:      retryID,
				ProposedAgentID: "reviewer",
				Objective: fmt.Sprintf(
					"Re-review after an operational failure for: %s. Submit your verdict via governed_submit_review. If you cannot verify, report [REVIEW_BLOCKED: reason].",
					req.Objective),
				RequestedBudget: Budget{Allocated: retryBudget},
				DependsOn:       []string{req.ID},
			}
			if authRetry, rerr := c.Governor.AuthorizeProposal(parentReq, retryProposal, retryID); rerr == nil {
				authRetry.ParentID = req.ID
				retryProfile := c.Capabilities.ResolveProfile("reviewer", "Code Reviewer & Quality Engineer", authRetry.Objective, nil, c.Governor)
				authRetry.CapabilityProfile = &retryProfile
				authRetry.DependsOn = []string{req.ID}
				if err := c.Store.Enqueue(*authRetry); err == nil {
					_ = c.Store.Complete(req.ID, *result)
					return result, nil
				}
			}
			result.Outputs["failure_reason"] = "review_operational_failure_enqueue_failed"
			_ = c.Store.Complete(req.ID, *result)
			return result, nil
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
// RecoverInterruptedActivities performs startup crash recovery, independent of
// budget records. It reconciles unfinished external intents and transitions
// orphaned claimed activities (whose host process terminated) into an explicit,
// resumable interrupted state. It never invents success: an interrupted
// activity is surfaced for reconciliation/resume, not silently completed.
func (c *TeamCoordinator) RecoverInterruptedActivities(ctx context.Context) int {
	// Reconcile any unfinished external mutations first.
	if c.Journal != nil && c.Reconciler != nil {
		_, _ = c.Journal.ReconcileUnfinished(ctx, c.Reconciler)
	}
	recovered := 0
	if c.Store == nil {
		return 0
	}
	for _, rec := range c.Store.QueryActivities(ActivityFilter{Status: StatusClaimed}) {
		if rec == nil {
			continue
		}
		// A claimed activity at startup means the process terminated mid-activity.
		// Never assume it is safe to blindly replay. If a same-host session
		// reference exists and the host supports resumption, reset to pending so
		// the loop resumes that session (host reconciliation preserves context
		// and avoids duplicate effects). Otherwise surface an explicit
		// recovery-needed interrupted state for manual/cross-host resume.
		ref := c.LoadSessionRef(rec.Request.ID)
		resumableSameHost := false
		if ref != nil && ref.SessionID != "" && ref.Runtime != "" {
			if prov, ok := c.Providers[ref.Runtime]; ok && prov != nil && prov.Capabilities().SessionResumption {
				resumableSameHost = true
			}
		}
		if resumableSameHost {
			// Safe same-host resume: the host session will be resumed and
			// reconciled; the checkpoint provides continuation context.
			rec.Status = StatusPending
			if err := c.Store.Update(*rec); err == nil {
				recovered++
			}
			continue
		}
		result := ActivityResult{
			ID:     rec.Request.ID,
			Status: StatusInterrupted,
			Outputs: map[string]any{
				"failure_reason":    "process_terminated",
				"recovery":          "recovery_needed",
				"interruption_note": "A4C restarted while this activity was claimed; run `a4c task resume` to reconcile host state and continue",
			},
		}
		if err := c.Store.InterruptWithResult(rec.Request.ID, result, fmt.Errorf("process terminated during execution (recovered on startup)")); err == nil {
			recovered++
		}
	}
	return recovered
}

func (c *TeamCoordinator) RunLoop(ctx context.Context, maxSteps int) ([]*ActivityResult, error) {
	if err := c.AcquireWorkspaceLock(); err != nil {
		return nil, err
	}
	defer c.ReleaseWorkspaceLock()

	// Crash recovery: reconcile intents and surface interrupted activities so
	// the loop resumes them instead of skipping or replaying them.
	c.RecoverInterruptedActivities(ctx)

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

// storeHasChildren reports whether any activity in the store already has
// parentID as its ParentID. Used to distinguish "lead failed to delegate"
// from "children were pre-enqueued (host-routing / DAG fixtures)".
func (c *TeamCoordinator) storeHasChildren(parentID string) bool {
	if c.Store == nil {
		return false
	}
	for _, rec := range c.Store.QueryActivities(ActivityFilter{ParentID: parentID}) {
		if rec != nil && rec.Request.ID != parentID {
			return true
		}
	}
	return false
}

// isReviewerAgent reports whether an agent identifier denotes a reviewer role.
func isReviewerAgent(id string) bool {
	l := strings.ToLower(id)
	return l == "reviewer" || l == "charlie" || strings.Contains(l, "review")
}

// hasReviewerChild reports whether the dispatched proposals include a reviewer.
func hasReviewerChild(proposals []ActivityProposal) bool {
	for _, p := range proposals {
		if isReviewerAgent(p.ProposedAgentID) {
			return true
		}
	}
	return false
}

// enforceMandatoryReview schedules an independent review of the final candidate
// when a Team-strategy implementation task did not include one. This is a
// policy-owned obligation: the lead may cast the reviewer dynamically, but it
// cannot omit mandatory review. The reviewer is scheduled through the existing
// Governor authorization, dynamic capability casting, and activity-enqueue
// mechanisms — no hardcoded identity, no second orchestrator.
func (c *TeamCoordinator) enforceMandatoryReview(req *ActivityRequest, result *ActivityResult) {
	// Already reviewed by the lead's own proposal?
	if hasReviewerChild(result.Proposals) {
		return
	}
	// A reviewer child may already be enqueued (e.g. idempotent re-run).
	if recs := c.Store.QueryActivities(ActivityFilter{ParentID: req.ID}); recs != nil {
		for _, rec := range recs {
			if rec != nil && rec.Request.ID != req.ID && isReviewerAgent(rec.Request.AgentID) {
				return
			}
		}
	}
	// Only enforce when implementation work exists (at least one non-reviewer child).
	var implDeps []string
	for _, p := range result.Proposals {
		if !isReviewerAgent(p.ProposedAgentID) {
			implDeps = append(implDeps, p.ProposalID)
		}
	}
	if len(implDeps) == 0 {
		return
	}

	reviewID := fmt.Sprintf("%s-review", req.ID)
	// Idempotency: don't schedule a duplicate review for the same candidate.
	if c.Store.FindActivityByProposalID(req.ID, reviewID) != nil {
		return
	}
	proposal := ActivityProposal{
		ProposalID:      reviewID,
		ProposedAgentID: "reviewer",
		Objective: fmt.Sprintf(
			"Independently review the final implementation candidate for: %s. Inspect the actual diff, tests, and verification evidence, then submit a verdict via governed_submit_review.",
			req.Objective),
		RequestedBudget: Budget{Allocated: 60000},
		DependsOn:       implDeps,
	}
	parentReq := *req
	if rootRec, err := c.Store.Get(c.getRootTaskID(req)); err == nil && rootRec != nil {
		parentReq = rootRec.Request
	}
	authReq, authErr := c.Governor.AuthorizeProposal(parentReq, proposal, reviewID)
	if authErr != nil {
		// Budget could not fund the mandatory review: surface an explicit
		// coordination failure rather than silently completing unreviewed.
		result.Outputs["failure_reason"] = "mandatory_review_unfunded"
		result.Outputs["mandatory_review_error"] = authErr.Error()
		return
	}
	authReq.ParentID = req.ID
	reviewProfile := c.Capabilities.ResolveProfile("reviewer", "Code Reviewer & Quality Engineer", authReq.Objective, nil, c.Governor)
	authReq.CapabilityProfile = &reviewProfile
	authReq.Runtime = req.Runtime
	authReq.Model = req.Model
	if req.Inputs != nil {
		if r, ok := req.Inputs["reviewer_runtime"].(string); ok && r != "" {
			authReq.Runtime = r
		}
		if m, ok := req.Inputs["reviewer_model"].(string); ok && m != "" {
			authReq.Model = m
		}
	}
	if err := c.Store.Enqueue(*authReq); err != nil {
		result.Outputs["failure_reason"] = "mandatory_review_enqueue_failed"
		result.Outputs["mandatory_review_error"] = err.Error()
		return
	}
	result.Proposals = append(result.Proposals, proposal)
	result.Outputs["mandatory_review_scheduled"] = true
}

// truncateSummary bounds a test-output excerpt for durable activity outputs.
func truncateSummary(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "... [truncated]"
}

// ResolveDecision allows the human engineer to approve, reject, or reassign work.
func (c *TeamCoordinator) ResolveDecision(decisionID string, resolution string, approve bool, reassignTo string) error {
	return c.Store.ResolveHumanDecision(decisionID, resolution, approve, reassignTo)
}

// gitDirtySet returns the set of paths reported dirty by `git status
// --porcelain` in dir (tracked modifications plus untracked files, excluding
// A4C-internal paths). It is used for the worktree-isolation guard.
func (c *TeamCoordinator) gitDirtySet(dir string) map[string]bool {
	set := map[string]bool{}
	if dir == "" {
		return set
	}
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return set
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[3:])
		if path == "" || isA4CInternal(path) {
			continue
		}
		set[path] = true
	}
	return set
}

// detectWorkspaceEscape reports source-checkout paths that became dirty during a
// coding activity while the assigned worktree stayed clean. A non-empty result
// means the agent mutated the wrong workspace; the caller must fail explicitly
// rather than report a successful isolated execution. Only mutating specialists
// are checked — a read-only investigation legitimately leaves no worktree diff.
func (c *TeamCoordinator) detectWorkspaceEscape(ctx context.Context, activeWorkspace string, mainRepoBefore map[string]bool) []string {
	if !c.isolatedWorktree(activeWorkspace) {
		return nil
	}
	// A worktree with real changes means isolation held; no escape to detect.
	if len(c.listChangedFiles(ctx, activeWorkspace)) > 0 {
		return nil
	}
	after := c.gitDirtySet(c.WorkspaceDir)
	var escaped []string
	for p := range after {
		if !mainRepoBefore[p] {
			escaped = append(escaped, p)
		}
	}
	return escaped
}

// isolatedWorktree reports whether activeWorkspace is a distinct directory from
// the source checkout.
func (c *TeamCoordinator) isolatedWorktree(activeWorkspace string) bool {
	if activeWorkspace == "" || c.WorkspaceDir == "" {
		return false
	}
	a, err1 := filepath.Abs(activeWorkspace)
	b, err2 := filepath.Abs(c.WorkspaceDir)
	if err1 != nil || err2 != nil {
		return activeWorkspace != c.WorkspaceDir
	}
	return a != b
}

// persistEvidenceLog writes a verification/test log to the MAIN workspace's
// durable evidence store (not the disposable worktree), so the artifact survives
// worktree removal and can be referenced by a knowledge EvidenceRef.locator.
// It returns an absolute path, or "" on failure.
func (c *TeamCoordinator) persistEvidenceLog(evidenceID, output string) string {
	root := c.WorkspaceDir
	if root == "" {
		return ""
	}
	dir := filepath.Join(root, ".a4c", "evidence")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	safeID := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, evidenceID)
	path := filepath.Join(dir, safeID+".log")
	if err := os.WriteFile(path, []byte(output), 0o600); err != nil {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
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

// runProjectVerification executes the project's verification and returns a
// structured result. It prefers a project-aware runner (correct, baseline-locked
// command) and falls back to the legacy exit-code contract. This is the ONLY
// place the coordinator runs tests, so the command is always project-correct.
func (c *TeamCoordinator) runProjectVerification(ctx context.Context, workspaceDir string) (VerificationResult, error) {
	if par, ok := c.TestRunner.(ProjectAwareRunner); ok {
		return par.Verify(ctx, workspaceDir)
	}
	start := time.Now()
	exitCode, output, err := c.TestRunner.RunTests(ctx, workspaceDir)
	if err != nil {
		return VerificationResult{ExitCode: exitCode, Output: output, Unavailable: true,
			StartedAt: start, CompletedAt: time.Now()}, err
	}
	testsExecuted, vacuous := classifyVerification("generic", exitCode, output)
	return VerificationResult{
		Command: nil, ExitCode: exitCode, Output: output,
		TestsExecuted: testsExecuted, Vacuous: vacuous, Failed: exitCode != 0,
		Duration: time.Since(start), StartedAt: start, CompletedAt: time.Now(),
	}, nil
}

// resolveValidationEvidence searches the dependency graph and activity tree for valid test evidence.
// It returns the found ValidationEvidence (if any) and a status string: "VALID", "STALE", "FAILED", "VACUOUS", "INCOMPLETE", or "MISSING".
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

		// A non-zero exit is always a genuine failure, vacuous or not.
		if ev.ExitCode != 0 {
			return ev, "FAILED"
		}

		// Exit 0 but exercised no tests: a verification gap, not a pass. This
		// is checked before the Valid flag so legacy evidence that merely lacks
		// a test count (but ran a real suite) is not misclassified.
		if evidenceIsVacuous(*ev) {
			return ev, "VACUOUS"
		}

		// Marked invalid despite running tests.
		if !ev.Valid {
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
		if c.lastEvidence.ExitCode != 0 {
			return c.lastEvidence, "FAILED"
		}
		if evidenceIsVacuous(*c.lastEvidence) {
			return c.lastEvidence, "VACUOUS"
		}
		if !c.lastEvidence.Valid {
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

// evidenceStatusString returns the current evidence status for a request
// without mutating coordinator state.
func (c *TeamCoordinator) evidenceStatusString(req *ActivityRequest, activeWorkspace string) string {
	saved := c.lastEvidence
	_, status := c.resolveValidationEvidence(req, activeWorkspace)
	c.lastEvidence = saved
	return status
}

// evidenceFingerprint builds a stable identifier for the current evidence
// state: the evidence ID plus the workspace snapshot it attests to. Two
// verification-blocked reviews with the same fingerprint saw identical evidence,
// so retrying adds no signal.
func (c *TeamCoordinator) evidenceFingerprint(req *ActivityRequest, activeWorkspace string) string {
	saved := c.lastEvidence
	ev, status := c.resolveValidationEvidence(req, activeWorkspace)
	c.lastEvidence = saved
	if ev == nil {
		return "none:" + status
	}
	return ev.EvidenceID + ":" + ev.WorkspaceSnapshotID + ":" + status
}

// collectVerificationEvidence runs native tests against the workspace as the
// coordinator (which holds execution authority), records durable evidence, and
// returns true when usable evidence was produced. Vacuous runs are recorded as
// such so a reviewer can distinguish "tests passed" from "no tests ran".
func (c *TeamCoordinator) collectVerificationEvidence(ctx context.Context, req *ActivityRequest, activeWorkspace string, result *ActivityResult) bool {
	if c.TestRunner == nil {
		return false
	}
	beforeSnapshot, err := c.Snapshotter.ComputeSnapshot(activeWorkspace)
	if err != nil {
		return false
	}
	start := time.Now()
	vres, runErr := c.runProjectVerification(ctx, activeWorkspace)
	if runErr != nil {
		return false
	}
	exitCode, out := vres.ExitCode, vres.Output
	afterSnapshot, err := c.Snapshotter.ComputeSnapshot(activeWorkspace)
	if err != nil {
		return false
	}
	if err := c.Snapshotter.ValidateIntegrity(beforeSnapshot, afterSnapshot); err != nil {
		return false
	}

	evidenceID := fmt.Sprintf("evi-%s-collect-%d", req.ID, time.Now().UnixNano())
	logPath := c.persistEvidenceLog(evidenceID, out)

	evidence := ValidationEvidence{
		EvidenceID:          evidenceID,
		ActivityID:          req.ID,
		TaskID:              c.getRootTaskID(req),
		WorkspaceDir:        activeWorkspace,
		WorkspaceSnapshotID: afterSnapshot,
		Command:             vres.Command,
		ExitCode:            exitCode,
		OutputSummary:       truncateSummary(out, 500),
		Duration:            time.Since(start),
		LogPath:             logPath,
		AgentID:             "coordinator",
		HostID:              req.Runtime,
		Valid:               exitCode == 0 && !vres.Vacuous && !vres.Unavailable,
		TestsExecuted:       vres.TestsExecuted,
		Vacuous:             vres.Vacuous,
		SourceRevision:      vres.SourceRevision,
		StartedAt:           start,
		CompletedAt:         time.Now(),
	}
	c.lastEvidence = &evidence
	c.lastEvidenceFingerprint = evidence.EvidenceID + ":" + afterSnapshot + ":" + evidenceStatusFor(evidence)

	if result != nil {
		result.Outputs["verification_evidence"] = evidence
		result.Outputs["verification_evidence_status"] = evidenceStatusFor(evidence)
	}
	// Usable evidence means the command ran and produced a durable artifact.
	// A vacuous run is still "collected" (the reviewer can see no tests ran),
	// but we report it so the retry can be diagnosed.
	return logPath != "" || exitCode == 0
}

// evidenceStatusFor classifies a concrete evidence record.
func evidenceStatusFor(ev ValidationEvidence) string {
	switch {
	case ev.ExitCode != 0:
		return "FAILED"
	case evidenceIsVacuous(ev):
		return "VACUOUS"
	case !ev.Valid:
		return "FAILED"
	default:
		return "VALID"
	}
}

// evidenceIsVacuous reports whether an evidence record proves nothing because
// its command exercised no tests. Explicit flags win; for legacy records that
// predate TestsExecuted/Vacuous, vacuity is inferred from the recorded output
// summary (e.g. `go test` reporting "[no test files]").
func evidenceIsVacuous(ev ValidationEvidence) bool {
	if ev.Vacuous {
		return true
	}
	if ev.TestsExecuted > 0 {
		return false
	}
	// Legacy evidence with no recorded test count: infer from the summary.
	_, vacuous := AnalyzeGoTestOutput(ev.OutputSummary)
	return vacuous
}

// attachSalvageEvidence records full validation evidence for a salvaged
// (budget-overrun-but-tests-pass) completion, so a later reviewer can resolve
// it instead of finding MISSING evidence. Previously salvage paths set only a
// truncated summary, which reviewers correctly rejected as unverifiable.
func (c *TeamCoordinator) attachSalvageEvidence(req *ActivityRequest, activeWorkspace string, vres VerificationResult, result *ActivityResult) {
	evidenceID := fmt.Sprintf("evi-%s-salvage-%d", req.ID, time.Now().UnixNano())
	logPath := c.persistEvidenceLog(evidenceID, vres.Output)
	evidence := ValidationEvidence{
		EvidenceID:          evidenceID,
		ActivityID:          req.ID,
		TaskID:              c.getRootTaskID(req),
		WorkspaceDir:        activeWorkspace,
		WorkspaceSnapshotID: "",
		Command:             vres.Command,
		ExitCode:            vres.ExitCode,
		OutputSummary:       truncateSummary(vres.Output, 500),
		LogPath:             logPath,
		AgentID:             req.AgentID,
		Valid:               vres.ExitCode == 0 && !vres.Vacuous && !vres.Unavailable,
		TestsExecuted:       vres.TestsExecuted,
		Vacuous:             vres.Vacuous,
		SourceRevision:      vres.SourceRevision,
		StartedAt:           time.Now(),
		CompletedAt:         time.Now(),
	}
	if snap, err := c.Snapshotter.ComputeSnapshot(activeWorkspace); err == nil {
		evidence.WorkspaceSnapshotID = snap
	}
	result.Outputs["validation_evidence"] = evidence
	if result.EvidenceIDs == nil {
		result.EvidenceIDs = []string{}
	}
	result.EvidenceIDs = append(result.EvidenceIDs, evidenceID)
	c.lastEvidence = &evidence
}

// listChangedFiles returns the paths changed relative to HEAD, including
// untracked files. It is the authoritative file list a reviewer inspects.
func (c *TeamCoordinator) listChangedFiles(ctx context.Context, dir string) []string {
	if dir == "" {
		dir = c.WorkspaceDir
	}
	files := []string{}
	cmd := exec.CommandContext(ctx, "git", "diff", "--name-only", "HEAD")
	cmd.Dir = dir
	if out, err := cmd.Output(); err == nil {
		for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				files = append(files, l)
			}
		}
	}
	// Untracked (added) files do not appear in `git diff HEAD`.
	cmdUntracked := exec.CommandContext(ctx, "git", "ls-files", "--others", "--exclude-standard")
	cmdUntracked.Dir = dir
	if out, err := cmdUntracked.Output(); err == nil {
		seen := make(map[string]bool, len(files))
		for _, f := range files {
			seen[f] = true
		}
		for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			l = strings.TrimSpace(l)
			if l != "" && !seen[l] {
				files = append(files, l)
			}
		}
	}
	// A4C operational state (.a4c/) is never part of the deliverable diff.
	return excludeA4CInternal(files)
}

// isA4CInternal reports whether a path is A4C operational state, which must
// never appear in the deliverable diff or the reviewer's changed-file list.
func isA4CInternal(path string) bool {
	clean := filepath.ToSlash(filepath.Clean(path))
	return clean == ".a4c" || strings.HasPrefix(clean, ".a4c/")
}

// excludeA4CInternal filters A4C operational paths from a file list.
func excludeA4CInternal(files []string) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		if !isA4CInternal(f) {
			out = append(out, f)
		}
	}
	return out
}

// filterReviewVisiblePaths drops producer-workspace-internal paths (anything
// under .a4c/) that do not resolve inside a reviewer's workspace. The readable
// log is delivered via the review-evidence bundle instead.
func filterReviewVisiblePaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		clean := filepath.ToSlash(filepath.Clean(p))
		if strings.HasPrefix(clean, ".a4c/") || strings.HasPrefix(clean, "/.a4c/") {
			continue
		}
		out = append(out, p)
	}
	return out
}

// buildCoderHandoff reconstructs the structured completion handoff from the
// most recent completed coder activity in the task tree, so a reviewer receives
// the coder's files, tests, verification commands, exit codes, and log paths
// without re-deriving them.
func (c *TeamCoordinator) buildCoderHandoff(rootID string) *CoderHandoff {
	if rootID == "" {
		return nil
	}
	tree := c.Store.GetTaskTree(rootID)
	for i := len(tree) - 1; i >= 0; i-- {
		rec := tree[i]
		if rec.Result == nil {
			continue
		}
		// Skip lead and reviewer records; we want the implementer's handoff.
		agent := strings.ToLower(rec.Request.AgentID)
		if agent == "lead" || agent == "reviewer" || agent == "charlie" || agent == "alice" {
			continue
		}
		o := rec.Result.Outputs
		h := &CoderHandoff{}
		if s, ok := o["summary"].(string); ok {
			h.BehaviorSummary = truncateSummary(s, 800)
		}
		if fc, ok := o["files_changed"].([]string); ok {
			h.FilesChanged = fc
		}
		if ta, ok := o["tests_added"].([]string); ok {
			h.TestsAdded = ta
		}
		if tu, ok := o["tests_updated"].([]string); ok {
			h.TestsUpdated = tu
		}
		if vc, ok := o["verification_commands"].([]string); ok {
			h.VerificationCmds = vc
		}
		if ec, ok := o["test_exit_codes"].([]int); ok {
			h.TestExitCodes = ec
		}
		if lp, ok := o["test_log_paths"].([]string); ok {
			h.TestLogPaths = filterReviewVisiblePaths(lp)
		}
		if kl, ok := o["known_limitations"].([]string); ok {
			h.KnownLimitations = kl
		}
		// Surface the evidence identity, but never the producer's .a4c path
		// (unreachable in the review workspace). The readable log is delivered
		// via the review-evidence bundle.
		if ev, ok := o["validation_evidence"].(ValidationEvidence); ok {
			h.ValidationEvidenceID = ev.EvidenceID
			if len(h.VerificationCmds) == 0 && len(ev.Command) > 0 {
				h.VerificationCmds = []string{strings.Join(ev.Command, " ")}
			}
			if len(h.TestExitCodes) == 0 {
				h.TestExitCodes = []int{ev.ExitCode}
			}
		}
		if h.BehaviorSummary == "" && len(h.TestLogPaths) == 0 && h.ValidationEvidenceID == "" {
			continue
		}
		return h
	}
	return nil
}

// collectPriorFindings gathers earlier review verdicts and the coder's
// responses so a new reviewer does not re-derive established conclusions.
func (c *TeamCoordinator) collectPriorFindings(rootID, currentActivityID string) []ReviewFinding {
	if rootID == "" {
		return nil
	}
	findings := []ReviewFinding{}
	tree := c.Store.GetTaskTree(rootID)
	for _, rec := range tree {
		if rec.Request.ID == currentActivityID || rec.Result == nil {
			continue
		}
		o := rec.Result.Outputs
		verdict, _ := o["verdict"].(string)
		if verdict == "" {
			continue
		}
		comments, _ := o["review_comments"].(string)
		if comments == "" {
			continue
		}
		kind := "defect"
		switch verdict {
		case string(ReviewVerdictVerificationBlocked):
			kind = "verification_gap"
		case string(ReviewVerdictIncomplete):
			kind = "verification_gap"
		}
		findings = append(findings, ReviewFinding{
			Kind:    kind,
			Message: fmt.Sprintf("[%s by %s]: %s", verdict, rec.Request.AgentID, truncateSummary(comments, 400)),
		})
		// Surface blockers as concrete defect candidates.
		if blockers, ok := o["blockers"].([]string); ok {
			for _, b := range blockers {
				findings = append(findings, ReviewFinding{Kind: "defect", Message: b})
			}
		}
	}
	return findings
}

// suspiciousFilePatterns are files that are almost never part of a focused
// implementation task and are usually tooling/build accidents.
var suspiciousFilePatterns = []string{
	".DS_Store", ".idea/", ".vscode/", "*.swp", "*.swo", "*~",
	"node_modules/", ".terraform/", ".bundle/",
}

// FlagDiffHygiene flags changed files that look unrelated to the task so a
// reviewer can assess relevance. It is a pure function and never rejects or
// deletes anything; the reviewer adjudicates. repoIsGo indicates whether the
// repository is a Go module (used to spot foreign language manifests).
func FlagDiffHygiene(files []string, objective string, repoIsGo bool) []string {
	warnings := []string{}
	objLower := strings.ToLower(objective)

	for _, f := range files {
		base := filepath.Base(f)
		// Editor/OS junk.
		for _, pat := range suspiciousFilePatterns {
			if strings.Contains(f, strings.TrimSuffix(pat, "*")) || (strings.HasSuffix(pat, "*") && strings.HasPrefix(base, strings.TrimSuffix(pat, "*"))) {
				warnings = append(warnings, fmt.Sprintf("%s: looks like an editor/OS artifact, not task-related", f))
				break
			}
		}
		// Foreign language manifests in a non-Go repo (e.g. doc.go/go.mod
		// added to a C repository).
		if !repoIsGo && (base == "go.mod" || base == "go.sum" || base == "package.json" || base == "Cargo.toml") {
			warnings = append(warnings, fmt.Sprintf("%s: language manifest unrelated to this repository's primary build system", f))
		}
		// Generated binaries / objects.
		if strings.HasSuffix(base, ".o") || strings.HasSuffix(base, ".a") || strings.HasSuffix(base, ".so") ||
			strings.HasSuffix(base, ".dylib") || strings.HasSuffix(base, ".exe") || strings.HasSuffix(base, ".dll") {
			warnings = append(warnings, fmt.Sprintf("%s: compiled artifact committed to the tree", f))
		}
		// Root-level Go doc file in a non-Go repo.
		if !repoIsGo && f == "doc.go" {
			warnings = append(warnings, fmt.Sprintf("%s: Go package doc file in a repository that is not a Go module", f))
		}
		// A build config change when the objective says nothing about build system.
		if (base == "Makefile" || base == "Makefile.cbm" || base == "CMakeLists.txt") &&
			!strings.Contains(objLower, "makefile") && !strings.Contains(objLower, "build") && !strings.Contains(objLower, "make") {
			warnings = append(warnings, fmt.Sprintf("%s: build file modified but objective does not mention build-system changes", f))
		}
	}
	return warnings
}

// checkDiffHygiene detects the repo's primary build system and delegates to FlagDiffHygiene.
func (c *TeamCoordinator) checkDiffHygiene(ctx context.Context, dir string, files []string, objective string) []string {
	if dir == "" {
		dir = c.WorkspaceDir
	}
	repoIsGo := fileExists(filepath.Join(dir, "go.mod"))
	return FlagDiffHygiene(files, objective, repoIsGo)
}

// reviewBundle describes the materialized, reviewer-visible evidence artifacts.
// Every path is RELATIVE to the reviewer's own workspace so it is valid by
// construction — the reviewer can never be handed a path that does not resolve
// inside its allowed workspace.
type reviewBundle struct {
	Dir          string // "review-evidence"
	DiffPath     string // "review-evidence/diff.patch"
	FilesPath    string
	HandoffPath  string
	PriorPath    string
	EvidencePath string
	// LogPath is the review-workspace-relative path to the full test log, or ""
	// if no log could be materialized. LogAvailable reports whether it resolves.
	LogPath       string
	LogAvailable  bool
	SummaryPath   string
	ObjectivePath string
	// Missing records explicit human-readable reasons a referenced artifact is
	// absent, so the reviewer is told evidence is unavailable rather than
	// silently receiving an unresolvable path.
	Missing []string
}

// materializeReviewEvidence writes a durable, read-only evidence bundle into
// the reviewer's disposable workspace and returns the bundle descriptor with
// verified, review-workspace-relative paths. Accessibility is checked before
// the paths are handed to the reviewer.
func (c *TeamCoordinator) materializeReviewEvidence(
	reviewWorkspace string,
	req *ActivityRequest,
	fullDiff string,
	files []string,
	handoff *CoderHandoff,
	prior []ReviewFinding,
	evidence *ValidationEvidence,
	evStatus string,
) *reviewBundle {
	if reviewWorkspace == "" {
		return &reviewBundle{Missing: []string{"review workspace unavailable"}}
	}
	bundleDir := filepath.Join(reviewWorkspace, "review-evidence")
	if err := os.MkdirAll(bundleDir, 0o700); err != nil {
		return &reviewBundle{Missing: []string{"could not create evidence bundle directory"}}
	}
	b := &reviewBundle{Dir: "review-evidence"}
	rel := func(name string) string { return filepath.Join("review-evidence", name) }
	write := func(name string, data []byte) string {
		if err := os.WriteFile(filepath.Join(bundleDir, name), data, 0o600); err == nil {
			return rel(name)
		}
		return ""
	}

	// 1. Full, untruncated diff.
	b.DiffPath = write("diff.patch", []byte(fullDiff))

	// 2. Changed-files list, enriched with per-file line counts and test names
	// so the reviewer can establish test coverage without paging whole files.
	b.FilesPath = write("changed-files.txt", []byte(buildChangedFilesSummary(reviewWorkspace, files)))

	// 3. Coder handoff (structured).
	if handoff != nil {
		if data, err := json.MarshalIndent(handoff, "", "  "); err == nil {
			b.HandoffPath = write("coder-handoff.json", data)
		}
	}

	// 4. Prior review findings.
	if len(prior) > 0 {
		if data, err := json.MarshalIndent(prior, "", "  "); err == nil {
			b.PriorPath = write("prior-reviews.json", data)
		}
	}

	// 5. Evidence metadata + the durable full test log, copied in so it is
	// reachable inside the disposable workspace (which excludes .a4c).
	if evidence != nil {
		meta := map[string]any{
			"evidence_id":       evidence.EvidenceID,
			"status":            evStatus,
			"command":           strings.Join(evidence.Command, " "),
			"exit_code":         evidence.ExitCode,
			"valid":             evidence.Valid,
			"vacuous":           evidence.Vacuous,
			"tests_executed":    evidence.TestsExecuted,
			"source_revision":   evidence.SourceRevision,
			"producer_log_path": evidence.LogPath,
		}
		if data, err := json.MarshalIndent(meta, "", "  "); err == nil {
			b.EvidencePath = write("evidence.json", data)
		}
		if evidence.LogPath != "" {
			absLog := evidence.LogPath
			if !filepath.IsAbs(absLog) && evidence.WorkspaceDir != "" {
				absLog = filepath.Join(evidence.WorkspaceDir, evidence.LogPath)
			}
			if data, err := os.ReadFile(absLog); err == nil {
				if p := write("test-output.log", data); p != "" {
					b.LogPath = p
					// Verify it actually resolves in the review workspace.
					if _, statErr := os.Stat(filepath.Join(reviewWorkspace, p)); statErr == nil {
						b.LogAvailable = true
					} else {
						b.LogPath = ""
						b.Missing = append(b.Missing, "test log could not be verified in the review workspace")
					}
				} else {
					b.Missing = append(b.Missing, "test log could not be written to the evidence bundle")
				}
			} else {
				b.Missing = append(b.Missing, fmt.Sprintf("test log %s was not readable from the producer workspace", evidence.LogPath))
			}
		} else {
			b.Missing = append(b.Missing, "no durable test log was recorded for this change")
		}
		if evidence.OutputSummary != "" {
			b.SummaryPath = write("test-output-summary.txt", []byte(evidence.OutputSummary))
		}
	} else {
		b.Missing = append(b.Missing, "no validation evidence exists for this change")
	}

	// 6. The request objective for convenience.
	b.ObjectivePath = write("objective.txt", []byte(req.Objective+"\n"))

	return b
}

// buildChangedFilesSummary produces a reviewer-oriented index of changed files:
// per-file line count and, for test files, the test function names and their
// line numbers. This lets a reviewer confirm tests exist without reading a
// 2,800-line file end-to-end.
func buildChangedFilesSummary(workspace string, files []string) string {
	var sb strings.Builder
	for _, f := range files {
		abs := filepath.Join(workspace, f)
		lineCount := 0
		if data, err := os.ReadFile(abs); err == nil {
			lineCount = strings.Count(string(data), "\n") + 1
		}
		sb.WriteString(fmt.Sprintf("%s (%d lines)\n", f, lineCount))
		lower := strings.ToLower(f)
		if strings.Contains(lower, "test") {
			if names := extractTestNames(abs); len(names) > 0 {
				sb.WriteString("  tests: " + strings.Join(names, ", ") + "\n")
			}
		}
	}
	return sb.String()
}

// testFuncRegex matches test function definitions in C and Go:
// "func TestFoo(", "void TestFoo(", "int test_foo(", etc.
var testFuncRegex = regexp.MustCompile(`(?m)^\s*(?:void\s+|int\s+|bool\s+|func\s+)?(?:Test|test_)[A-Za-z0-9_]+\s*\(`)

// extractTestNames returns test function names defined in a file (bounded).
func extractTestNames(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	matches := testFuncRegex.FindAllStringSubmatch(string(data), 64)
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		// m[0] is the full match; the Test* identifier is the last token before "(".
		full := strings.TrimSpace(m[0])
		full = strings.TrimSuffix(full, "(")
		// Strip any leading type keyword (void/int/bool/func).
		for _, kw := range []string{"void ", "int ", "bool ", "func "} {
			full = strings.TrimPrefix(full, kw)
		}
		names = append(names, strings.TrimSpace(full))
	}
	return names
}

// fileExists reports whether a path exists.
func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
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

	// Fallback text parsing for review: [REVIEW_BLOCKED: reason], [REVIEW_REJECTED: reason],
	// [REVIEW_APPROVED], or [SUBMIT_REVIEW: ...]
	if strings.Contains(content, "[REVIEW_BLOCKED:") {
		comments := "Verification blocked"
		start := strings.Index(content, "[REVIEW_BLOCKED:") + len("[REVIEW_BLOCKED:")
		end := strings.Index(content[start:], "]")
		if end != -1 {
			comments = strings.TrimSpace(content[start : start+end])
		}
		*rev = &ReviewResult{
			ActivityID: activityID,
			ReviewerID: agentID,
			Verdict:    ReviewVerdictVerificationBlocked,
			Approved:   false,
			Comments:   comments,
			Timestamp:  time.Now(),
		}
	} else if strings.Contains(content, "[REVIEW_REJECTED:") || (strings.Contains(content, "[SUBMIT_REVIEW:") && strings.Contains(content, "approved=false")) {
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
// maybeReallocateBudget performs one bounded, explicit recovery from budget
// exhaustion on a CHILD activity: if the parent still has capacity and the
// activity has not used up its budget retries, the activity is requeued with
// an enlarged viable allocation (drawn from the parent's remaining budget,
// never implicitly granted). The workspace is preserved so completed work
// counts toward the retried attempt. Returns the recovery result when the
// activity was requeued; ok=false means the failure is terminal and must be
// recorded by the caller with the explicit budget-exhaustion state.
func (c *TeamCoordinator) maybeReallocateBudget(req *ActivityRequest, consumed float64, fail *ActivityResult) (*ActivityResult, bool) {
	if c.Governor == nil || c.MaxBudgetRetries <= 0 {
		return nil, false
	}
	if req.ParentID == "" || req.ParentID == req.ID {
		// Root activities are bounded by the human-set task budget; never
		// extend them implicitly. Only children can draw on a parent.
		return nil, false
	}
	retries := 0
	if req.Inputs != nil {
		switch v := req.Inputs["budget_retries"].(type) {
		case int:
			retries = v
		case float64:
			retries = int(v)
		}
	}
	if retries >= c.MaxBudgetRetries {
		return nil, false
	}

	floor := c.Governor.MinChildBudget()
	if floor <= 0 {
		floor = DefaultMinChildBudgetTokens
	}
	reserve := c.Governor.ParentReserve()
	parentAvail := c.Governor.AvailableFor(req.ParentID)
	if parentAvail <= 0 {
		return nil, false
	}

	// Size the retry to actually cover the observed overrun plus headroom,
	// not merely double the original allocation. Doubling 180k→360k still
	// failed on a 217k first attempt that then re-ran a 5,722-test suite.
	// Target: observed consumption + 50% headroom, at least 2× original.
	newAlloc := req.Budget.Allocated * 2
	if withHeadroom := consumed * 1.5; withHeadroom > newAlloc {
		newAlloc = withHeadroom
	}
	if newAlloc < floor {
		newAlloc = floor
	}
	if headroom := parentAvail - reserve; newAlloc > headroom {
		newAlloc = headroom
	}
	if newAlloc < floor {
		return nil, false
	}

	if err := c.Governor.ReallocateChild(req.ParentID, req.ID, newAlloc); err != nil {
		return nil, false
	}

	retried := *req
	retried.Budget = Budget{Allocated: newAlloc}
	if retried.Inputs == nil {
		retried.Inputs = map[string]any{}
	}
	retried.Inputs["budget_retries"] = retries + 1
	retried.Inputs["budget_reallocation"] = fmt.Sprintf(
		"attempt %d exhausted %.0f of %.0f tokens; reallocated to %.0f from parent %s",
		retries+1, consumed, req.Budget.Allocated, newAlloc, req.ParentID)
	// A budget-reallocated retry is a FRESH attempt with a new allocation; it
	// must start a new host session, not resume the exhausted one.
	c.ClearSessionRef(req.ID)
	if err := c.Store.Requeue(retried); err != nil {
		return nil, false
	}

	if fail != nil {
		fail.Outputs["recovery"] = "budget_reallocation"
		fail.Outputs["reallocated_to"] = newAlloc
		fail.Outputs["budget_retries"] = retries + 1
	}
	return &ActivityResult{
		ID:       req.ID,
		Status:   StatusPending,
		Consumed: Budget{Consumed: consumed},
		Metrics:  fail.Metrics,
		Outputs: map[string]any{
			"failure_reason": "budget_exhausted",
			"recovery":       "budget_reallocation",
			"reallocated_to": newAlloc,
			"budget_retries": retries + 1,
			"note":           "activity requeued with viable reallocated budget; workspace progress preserved",
			"previous_error": fail.Outputs["error"],
		},
	}, true
}

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
