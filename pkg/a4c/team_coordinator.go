package a4c

import (
	"context"
	"encoding/json"
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
	Provider      AgentProvider
	Reconciler    EffectReconciler
	MaxRepair     int
	MaxRevisions  int
	revisionCount int
	lastEvidence           *ValidationEvidence
	sessions               map[string]AgentSession
	Handoff                *HandoffBuilder
	Structural             StructuralProvider
	Capabilities           *CapabilityRegistry
	StructuralStore        *DurableStructuralStore
	lastStructuralArtifact *StructuralContextArtifact
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

	return &TeamCoordinator{
		WorkspaceDir: workspaceDir,
		Store:        store,
		Roster:       roster,
		Memory:       memory,
		Governor:     governor,
		Journal:      journal,
		Snapshotter:  NewWorkspaceSnapshotter(),
		TestRunner:   runner,
		Provider:     provider,
		Reconciler:   &LocalEffectReconciler{WorkspaceDir: workspaceDir},
		MaxRepair:    3,
		MaxRevisions: 2,
		sessions:     make(map[string]AgentSession),
		Handoff:      NewHandoffBuilder(),
		Capabilities: NewCapabilityRegistry(),
	}
}


// SetWorktreeManager sets the Git worktree manager for task isolation.
func (c *TeamCoordinator) SetWorktreeManager(mgr *WorktreeManager) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.WorktreeMgr = mgr
}

// SetStructuralProvider sets the optional code graph intelligence provider (CodeTopo).
func (c *TeamCoordinator) SetStructuralProvider(p StructuralProvider) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Structural = p
}

// SetStructuralStore connects durable storage for shared structural context artifacts.
func (c *TeamCoordinator) SetStructuralStore(s *DurableStructuralStore) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.StructuralStore = s
}

// SetStructuralArtifact caches and persists a shared structural artifact.
func (c *TeamCoordinator) SetStructuralArtifact(art *StructuralContextArtifact) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setStructuralArtifactLocked(art)
}

func (c *TeamCoordinator) setStructuralArtifactLocked(art *StructuralContextArtifact) {
	c.lastStructuralArtifact = art
	if c.StructuralStore != nil && art != nil {
		_ = c.StructuralStore.Save(art)
	}
}

// GetStructuralArtifact retrieves a structural artifact by ID.
func (c *TeamCoordinator) GetStructuralArtifact(id string) (*StructuralContextArtifact, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.getStructuralArtifactLocked(id)
}

func (c *TeamCoordinator) getStructuralArtifactLocked(id string) (*StructuralContextArtifact, error) {
	if c.lastStructuralArtifact != nil && c.lastStructuralArtifact.ID == id {
		return c.lastStructuralArtifact, nil
	}
	if c.StructuralStore != nil {
		return c.StructuralStore.Get(id)
	}
	return nil, fmt.Errorf("structural artifact %q not found", id)
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
			return wt.WorktreePath
		}
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
	if strategy == "" {
		strategy = StrategyTeam
	}

	taskID := fmt.Sprintf("task-%d", time.Now().UnixNano())

	if c.WorktreeMgr != nil {
		_, _ = c.WorktreeMgr.AcquireWorktree(context.Background(), taskID, "team")
	}

	switch strategy {
	case StrategyDirect:
		// Mode B / Direct: Single programmer handles the task directly under governance & native tests
		req := ActivityRequest{
			ID:        taskID,
			AgentID:   "coder",
			Objective: objective,
			Inputs:    inputs,
			Budget:    budget,
			Strategy:  StrategyDirect,
			CreatedAt: time.Now(),
		}
		if err := c.Store.Enqueue(req); err != nil {
			return nil, fmt.Errorf("coordinator: failed to enqueue direct task: %w", err)
		}
		return &req, nil

	case StrategyReviewed:
		// Programmer implements + Independent Reviewer verifies
		coderReq := ActivityRequest{
			ID:        taskID,
			AgentID:   "coder",
			Objective: objective,
			Inputs:    inputs,
			Budget:    Budget{Allocated: budget.Allocated * 0.7},
			Strategy:  StrategyReviewed,
			CreatedAt: time.Now(),
		}
		reviewerReq := ActivityRequest{
			ID:        fmt.Sprintf("%s-rev", taskID),
			ParentID:  taskID,
			AgentID:   "reviewer",
			Objective: fmt.Sprintf("Review implementation of: %s", objective),
			Budget:    Budget{Allocated: budget.Allocated * 0.3},
			DependsOn: []string{taskID},
			Strategy:  StrategyReviewed,
			CreatedAt: time.Now(),
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

	config := AgentConfig{
		AgentID:       ident.ID,
		Charter:       ident.Charter,
		WorkspaceDir:  activeWorkspace,
		Budget:        req.Budget,
		Governor:      c.Governor,
		IntentJournal: c.Journal,
		Structural:    c.Structural,
		Profile:       &profile,
		AllowedTools:  profile.AllowedTools,
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
	session, err := c.Provider.CreateSession(ctx, config)
	if err != nil {
		_ = c.Store.Fail(req.ID, err)
		return nil, fmt.Errorf("coordinator: failed to initialize session for %s: %w", ident.ID, err)
	}
	defer session.Close()

	// 6. Build prompt with injected verified team memory & role-appropriate context via HandoffBuilder
	diff := ""
	if ident.ID == "reviewer" {
		diff, _ = c.getWorkspaceDiff(ctx, activeWorkspace)
	}
	evidenceID := "none"
	if c.lastEvidence != nil {
		evidenceID = c.lastEvidence.EvidenceID
	}

	// Resolve shared structural projection if available
	var structProj string
	structArt := c.lastStructuralArtifact
	if req.StructuralArtifactID != "" {
		if loaded, err := c.getStructuralArtifactLocked(req.StructuralArtifactID); err == nil && loaded != nil {
			structArt = loaded
		}
	}
	if structArt != nil {
		switch ident.ID {
		case "coder":
			structProj = structArt.ProjectForCoder(40)
		case "reviewer":
			structProj = structArt.ProjectForReviewer(35)
		}
	}

	artifact := HandoffArtifact{
		Objective:            req.Objective,
		Role:                 fmt.Sprintf("%s (%s)", ident.Name, ident.Role),
		AgentID:              ident.ID,
		ActiveWorkspace:      activeWorkspace,
		Diff:                 diff,
		ValidationEvidence:   evidenceID,
		VerifiedMemories:     c.Memory.FormatContext(),
		Inputs:               req.Inputs,
		StructuralProjection: structProj,
		ProfileDescription:   profile.Format(),
	}

	prompt := c.Handoff.BuildPrompt(artifact)

	// 7. Dispatch to agent session with latency tracking
	startWall := time.Now()
	startModel := time.Now()
	resp, err := session.Send(ctx, prompt)
	modelDuration := time.Since(startModel)
	if err != nil {
		_ = c.Store.Fail(req.ID, err)
		return nil, err
	}

	usage := session.Usage()
	consumed := Budget{Consumed: float64(usage.TotalTokens)}

	metrics := &ExecutionMetrics{
		WallClockDuration:      time.Since(startWall),
		ModelDuration:          modelDuration,
		PromptTokens:           usage.PromptTokens,
		CompletionTokens:       usage.CompletionTokens,
		TotalTokens:            usage.TotalTokens,
		ModelInvocations:       1,
		StructuralQueriesCount: structuralQueriesCount,
		FileReadsCount:         fileReadsCount,
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
		ID:                   req.ID,
		Status:               StatusCompleted,
		Consumed:             consumed,
		Metrics:              metrics,
		CapabilityProfile:    &profile,
		StructuralArtifactID: req.StructuralArtifactID,
		Outputs: map[string]any{
			"summary":    resp.Content,
			"session_id": session.SessionID(),
		},
	}

	switch ident.ID {
	case "lead":
		// Auto-capture structural context artifact if structural provider is available
		if c.Structural != nil && c.Structural.Available() {
			freshness, _ := c.Structural.CheckFreshness(ctx, activeWorkspace)
			snapSHA, _ := c.Snapshotter.ComputeSnapshot(activeWorkspace)
			artID := fmt.Sprintf("struct-%s", req.ID)
			art := &StructuralContextArtifact{
				ID:                artID,
				RepositoryID:      req.ID,
				WorkspaceSnapshot: snapSHA,
				IndexedSnapshot:   snapSHA,
				Freshness:         freshness,
				Completeness:      "complete",
				CreatedAt:         time.Now(),
			}
			c.setStructuralArtifactLocked(art)
			result.StructuralArtifactID = artID
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
				return nil, fmt.Errorf("coordinator: proposal rejected by governor: %w", authErr)
			}

			// Assign role-specific capability profile and structural artifact link
			childProfile := c.Capabilities.ResolveProfile(prop.ProposedAgentID, "", prop.Objective, prop.RequestedProfile, c.Governor)
			authReq.CapabilityProfile = &childProfile
			if result.StructuralArtifactID != "" {
				authReq.StructuralArtifactID = result.StructuralArtifactID
			} else if c.lastStructuralArtifact != nil {
				authReq.StructuralArtifactID = c.lastStructuralArtifact.ID
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


	case "coder":
		// Deterministic native testing and evidence collection
		beforeSnapshot, err := c.Snapshotter.ComputeSnapshot(activeWorkspace)
		if err != nil {
			_ = c.Store.Fail(req.ID, err)
			return nil, err
		}

		exitCode, testOut, err := c.TestRunner.RunTests(ctx, activeWorkspace)
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
				repairPrompt := fmt.Sprintf("Tests failed with exit code %d:\n%s\nPlease fix using governed_apply_patch.", exitCode, testOut)
				_, rErr := session.Send(ctx, repairPrompt)
				if rErr != nil {
					break
				}

				preSnap, _ := c.Snapshotter.ComputeSnapshot(activeWorkspace)
				exitCode, testOut, _ = c.TestRunner.RunTests(ctx, activeWorkspace)
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
		evidence := ValidationEvidence{
			EvidenceID:          fmt.Sprintf("evi-%s-%d", req.ID, time.Now().UnixNano()),
			WorkspaceSnapshotID: afterSnapshot,
			Command:             []string{"go", "test", "./..."},
			ExitCode:            0,
			Valid:               true,
		}
		c.lastEvidence = &evidence
		result.EvidenceIDs = append(result.EvidenceIDs, evidence.EvidenceID)
		result.Outputs["snapshot_id"] = afterSnapshot

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


	case "reviewer":
		// Reviewer verification
		approved := true
		comments := "Changes meet specifications and native tests passed."
		if reviewResult != nil {
			approved = reviewResult.Approved
			comments = reviewResult.Comments
		}

		// Verify evidence is present and valid; model approval CANNOT replace native evidence
		if c.lastEvidence == nil || !c.lastEvidence.Valid {
			approved = false
			comments = "Rejected: no valid native test evidence exists for these changes."
		} else if err := c.Snapshotter.ValidateAgainstEvidence(activeWorkspace, *c.lastEvidence); err != nil {
			approved = false
			comments = fmt.Sprintf("Rejected: workspace modified after validation (%v)", err)
		}

		result.Outputs["approved"] = approved
		result.Outputs["review_comments"] = comments

		if !approved && c.revisionCount < c.MaxRevisions {
			c.revisionCount++
			// Invalidate existing evidence to force coder to re-run native tests
			c.lastEvidence = nil

			revisionID := fmt.Sprintf("%s-revision-%d", req.ID, c.revisionCount)
			reReviewID := fmt.Sprintf("%s-rereview-%d", req.ID, c.revisionCount)

			// Dispatch revision subtask back to coder
			repairProfile := c.Capabilities.ResolveProfile("coder", "Senior Software Engineer", fmt.Sprintf("Address review rejection: %s", comments), nil, c.Governor)
			repairReq := ActivityRequest{
				ID:                   revisionID,
				ParentID:             req.ID,
				AgentID:              "coder",
				Objective:            fmt.Sprintf("Address review rejection: %s", comments),
				Budget:               Budget{Allocated: req.Budget.Available() / 2},
				CapabilityProfile:    &repairProfile,
				StructuralArtifactID: req.StructuralArtifactID,
				CreatedAt:            time.Now(),
			}
			_ = c.Store.Enqueue(repairReq)

			// Re-enqueue reviewer with explicit DAG dependency on the revision subtask
			reReviewProfile := c.Capabilities.ResolveProfile("reviewer", "Code Reviewer & Quality Engineer", fmt.Sprintf("Re-review revision for: %s", req.Objective), nil, c.Governor)
			reReviewReq := ActivityRequest{
				ID:                   reReviewID,
				ParentID:             req.ID,
				AgentID:              "reviewer",
				Objective:            fmt.Sprintf("Re-review revision for: %s", req.Objective),
				Budget:               Budget{Allocated: req.Budget.Available() / 2},
				DependsOn:            []string{revisionID},
				CapabilityProfile:    &reReviewProfile,
				StructuralArtifactID: req.StructuralArtifactID,
				CreatedAt:            time.Now(),
			}
			_ = c.Store.Enqueue(reReviewReq)
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
			if c.WorktreeMgr != nil {
				rootID := c.getRootTaskID(req)
				_ = c.WorktreeMgr.ReleaseWorktree(rootID, false)
			}
		}

		_ = c.Store.Complete(req.ID, *result)

	default:
		_ = c.Store.Complete(req.ID, *result)
	}

	return result, nil
}

// RunLoop executes steps sequentially until no claimable activities remain or human escalation is needed.
func (c *TeamCoordinator) RunLoop(ctx context.Context, maxSteps int) ([]*ActivityResult, error) {
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

	// Fallback text parsing for review: [REVIEW_APPROVED] or [REVIEW_REJECTED: reason]
	if strings.Contains(content, "[REVIEW_APPROVED]") {
		*rev = &ReviewResult{
			ActivityID: activityID,
			ReviewerID: agentID,
			Approved:   true,
			Comments:   "Approved via model directive",
			Timestamp:  time.Now(),
		}
	} else if strings.Contains(content, "[REVIEW_REJECTED:") {
		start := strings.Index(content, "[REVIEW_REJECTED:") + len("[REVIEW_REJECTED:")
		end := strings.Index(content[start:], "]")
		comments := "Rejected"
		if end != -1 {
			comments = strings.TrimSpace(content[start : start+end])
		}
		*rev = &ReviewResult{
			ActivityID: activityID,
			ReviewerID: agentID,
			Approved:   false,
			Comments:   comments,
			Timestamp:  time.Now(),
		}
	}

	// Fallback JSON block parsing
	if strings.Contains(content, "```json") {
		start := strings.Index(content, "```json") + 7
		end := strings.Index(content[start:], "```")
		if end != -1 {
			jsonBlock := strings.TrimSpace(content[start : start+end])
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
}
