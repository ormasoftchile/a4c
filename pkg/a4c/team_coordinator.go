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
	lastEvidence  *ValidationEvidence
	sessions      map[string]AgentSession
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
	}
}

// SetWorktreeManager sets the Git worktree manager for task isolation.
func (c *TeamCoordinator) SetWorktreeManager(mgr *WorktreeManager) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.WorktreeMgr = mgr
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
	req := ActivityRequest{
		ID:        fmt.Sprintf("task-%d", time.Now().UnixNano()),
		AgentID:   "lead", // Defaults to team architect / lead
		Objective: objective,
		Inputs:    inputs,
		Budget:    budget,
		CreatedAt: time.Now(),
	}

	if c.WorktreeMgr != nil {
		_, _ = c.WorktreeMgr.AcquireWorktree(context.Background(), req.ID, "team")
	}

	if err := c.Store.Enqueue(req); err != nil {
		return nil, fmt.Errorf("coordinator: failed to enqueue initial task: %w", err)
	}
	return &req, nil
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

	config := AgentConfig{
		AgentID:       ident.ID,
		Charter:       ident.Charter,
		WorkspaceDir:  activeWorkspace,
		Budget:        req.Budget,
		Governor:      c.Governor,
		IntentJournal: c.Journal,
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

	// 6. Build prompt with injected verified team memory & role-appropriate context
	verifiedContext := c.Memory.FormatContext()
	var promptBuilder strings.Builder
	promptBuilder.WriteString(fmt.Sprintf("Objective: %s\n", req.Objective))
	promptBuilder.WriteString(fmt.Sprintf("Agent Role: %s (%s)\n", ident.Name, ident.Role))
	promptBuilder.WriteString(fmt.Sprintf("%s\n", verifiedContext))

	if len(req.Inputs) > 0 {
		promptBuilder.WriteString(fmt.Sprintf("Inputs: %v\n", req.Inputs))
	}

	// Role-specific context additions
	if ident.ID == "lead" {
		promptBuilder.WriteString("Instructions: You are the Lead Engineer. Inspect the repository if needed, then delegate implementation to 'coder' using the governed_propose_activity tool.\n")
	} else if ident.ID == "coder" {
		promptBuilder.WriteString("Instructions: You are the Senior Programmer. Read repository code using governed_read_file and apply code changes using governed_apply_patch.\n")
	} else if ident.ID == "reviewer" {
		diff, _ := c.getWorkspaceDiff(ctx, activeWorkspace)
		evidenceID := "none"
		if c.lastEvidence != nil {
			evidenceID = c.lastEvidence.EvidenceID
		}
		promptBuilder.WriteString(fmt.Sprintf("Code Changes Under Review:\n%s\n", diff))
		promptBuilder.WriteString(fmt.Sprintf("Native Validation Evidence ID: %s (ExitCode: 0)\n", evidenceID))
		promptBuilder.WriteString("Instructions: Review the code changes against the objective and submit your evaluation using governed_submit_review.\n")
	}

	// 7. Dispatch to agent session
	resp, err := session.Send(ctx, promptBuilder.String())
	if err != nil {
		_ = c.Store.Fail(req.ID, err)
		return nil, err
	}

	usage := session.Usage()
	consumed := Budget{Consumed: float64(usage.TotalTokens)}

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
		ID:       req.ID,
		Status:   StatusCompleted,
		Consumed: consumed,
		Outputs: map[string]any{
			"summary":    resp.Content,
			"session_id": session.SessionID(),
		},
	}

	switch ident.ID {
	case "lead":
		// Authorize and enqueue delegated subtasks
		for idx, prop := range proposedActivities {
			childID := prop.ProposalID
			if childID == "" {
				childID = fmt.Sprintf("%s-sub-%d", req.ID, idx+1)
			}
			authReq, authErr := c.Governor.AuthorizeProposal(*req, prop, childID)
			if authErr != nil {
				return nil, fmt.Errorf("coordinator: proposal rejected by governor: %w", authErr)
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
			repairReq := ActivityRequest{
				ID:        revisionID,
				ParentID:  req.ID,
				AgentID:   "coder",
				Objective: fmt.Sprintf("Address review rejection: %s", comments),
				Budget:    Budget{Allocated: req.Budget.Available() / 2},
				CreatedAt: time.Now(),
			}
			_ = c.Store.Enqueue(repairReq)

			// Re-enqueue reviewer with explicit DAG dependency on the revision subtask
			reReviewReq := ActivityRequest{
				ID:        reReviewID,
				ParentID:  req.ID,
				AgentID:   "reviewer",
				Objective: fmt.Sprintf("Re-review revision for: %s", req.Objective),
				Budget:    Budget{Allocated: req.Budget.Available() / 2},
				DependsOn: []string{revisionID},
				CreatedAt: time.Now(),
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
