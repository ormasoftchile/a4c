package a4c

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// ActivityHistoryEntry captures the lifecycle of a single subtask within a task tree.
type ActivityHistoryEntry struct {
	ActivityID   string         `json:"activity_id"`
	ParentID     string         `json:"parent_id,omitempty"`
	AgentID      string         `json:"agent_id"`
	Objective    string         `json:"objective"`
	DependsOn    []string       `json:"depends_on,omitempty"`
	Status       ActivityStatus `json:"status"`
	BudgetUsed   float64        `json:"budget_used"`
	Error        string         `json:"error,omitempty"`
	Outputs      map[string]any    `json:"outputs,omitempty"`
	EvidenceIDs          []string           `json:"evidence_ids,omitempty"`
	Metrics              *ExecutionMetrics  `json:"metrics,omitempty"`
	CapabilityProfile    *CapabilityProfile `json:"capability_profile,omitempty"`
	StructuralArtifactID string             `json:"structural_artifact_id,omitempty"`
	CreatedAt            time.Time          `json:"created_at"`
}

// ReviewHistoryEntry captures reviewer evaluation details.
type ReviewHistoryEntry struct {
	ActivityID string    `json:"activity_id"`
	ReviewerID string    `json:"reviewer_id"`
	Approved   bool      `json:"approved"`
	Comments   string    `json:"comments"`
	Timestamp  time.Time `json:"timestamp"`
}

// TaskHistoryReport aggregates the full end-to-end audit trail of a task.
type TaskHistoryReport struct {
	TaskID              string                 `json:"task_id"`
	OriginalObjective   string                 `json:"original_objective"`
	Strategy            ExecutionStrategy      `json:"strategy,omitempty"`
	CreatedAt           time.Time              `json:"created_at"`
	AllocatedBudget     float64                `json:"allocated_budget"`
	ConsumedBudget      float64                `json:"consumed_budget"`
	Status              ActivityStatus         `json:"status"`
	Branch              string                 `json:"branch,omitempty"`
	ParticipatingAgents []string               `json:"participating_agents"`
	Activities          []ActivityHistoryEntry `json:"activities"`
	ToolExecutions      []IntentRecord         `json:"tool_executions,omitempty"`
	EvidenceIDs         []string               `json:"evidence_ids,omitempty"`
	ReviewDecisions     []ReviewHistoryEntry   `json:"review_decisions,omitempty"`
	HumanInterventions  []HumanDecisionRequest `json:"human_interventions,omitempty"`
	PromotedConventions []MemoryRecord         `json:"promoted_conventions,omitempty"`
	MetricsSummary      *ExecutionMetrics      `json:"metrics_summary,omitempty"`
	FinalOutcome        string                 `json:"final_outcome"`
}


// BuildTaskHistory deterministically reconstructs the complete task history from durable stores.
func BuildTaskHistory(
	taskID string,
	store *DurableActivityStore,
	journal *DurableIntentJournal,
	wtMgr *WorktreeManager,
	memStore *DurableMemoryStore,
) (*TaskHistoryReport, error) {
	rootRec, err := store.Get(taskID)
	if err != nil {
		return nil, fmt.Errorf("task history: root task %q not found: %w", taskID, err)
	}

	report := &TaskHistoryReport{
		TaskID:            taskID,
		OriginalObjective: rootRec.Request.Objective,
		Strategy:          rootRec.Request.Strategy,
		CreatedAt:         rootRec.Request.CreatedAt,
		AllocatedBudget:   rootRec.Request.Budget.Allocated,
		Status:            rootRec.Status,
		Activities:        make([]ActivityHistoryEntry, 0),
		EvidenceIDs:       make([]string, 0),
	}

	// Worktree branch
	if wtMgr != nil {
		if wt, err := wtMgr.GetWorktree(taskID); err == nil {
			report.Branch = wt.Branch
		}
	}

	// Collect related activities (root and all subtasks)
	agentMap := make(map[string]bool)
	subtasks := store.GetTaskTree(taskID)
	var metricsAgg *ExecutionMetrics

	for _, rec := range subtasks {
		if rec.Request.AgentID != "" {
			agentMap[rec.Request.AgentID] = true
		}

		consumed := 0.0
		var outputs map[string]any
		var evIDs []string
		var metrics *ExecutionMetrics

		if rec.Result != nil {
			consumed = rec.Result.Consumed.Consumed
			outputs = rec.Result.Outputs
			evIDs = rec.Result.EvidenceIDs
			metrics = rec.Result.Metrics
			report.ConsumedBudget += consumed
			report.EvidenceIDs = append(report.EvidenceIDs, evIDs...)

			if metrics != nil {
				if metricsAgg == nil {
					metricsAgg = &ExecutionMetrics{}
				}
				metricsAgg.WallClockDuration += metrics.WallClockDuration
				metricsAgg.ProviderWaitDuration += metrics.ProviderWaitDuration
				metricsAgg.ModelDuration += metrics.ModelDuration
				metricsAgg.ToolDuration += metrics.ToolDuration
				metricsAgg.ValidationDuration += metrics.ValidationDuration
				metricsAgg.StructuralDuration += metrics.StructuralDuration
				metricsAgg.PromptTokens += metrics.PromptTokens
				metricsAgg.CompletionTokens += metrics.CompletionTokens
				metricsAgg.TotalTokens += metrics.TotalTokens
				metricsAgg.OverrunTokens += metrics.OverrunTokens
				metricsAgg.ModelInvocations += metrics.ModelInvocations
				metricsAgg.ToolCalls += metrics.ToolCalls
				metricsAgg.StructuralQueriesCount += metrics.StructuralQueriesCount
				metricsAgg.FileReadsCount += metrics.FileReadsCount
			}
		}

		prof := rec.Request.CapabilityProfile
		structID := rec.Request.StructuralArtifactID
		if rec.Result != nil {
			if rec.Result.CapabilityProfile != nil {
				prof = rec.Result.CapabilityProfile
			}
			if rec.Result.StructuralArtifactID != "" {
				structID = rec.Result.StructuralArtifactID
			}
		}

		entry := ActivityHistoryEntry{
			ActivityID:           rec.Request.ID,
			ParentID:             rec.Request.ParentID,
			AgentID:              rec.Request.AgentID,
			Objective:            rec.Request.Objective,
			DependsOn:            rec.Request.DependsOn,
			Status:               rec.Status,
			BudgetUsed:           consumed,
			Error:                rec.Error,
			Outputs:              outputs,
			EvidenceIDs:          evIDs,
			Metrics:              metrics,
			CapabilityProfile:    prof,
			StructuralArtifactID: structID,
			CreatedAt:            rec.Request.CreatedAt,
		}
		report.Activities = append(report.Activities, entry)

		// Review outputs
		if rec.Request.AgentID == "reviewer" && outputs != nil {
			approved, _ := outputs["approved"].(bool)
			comments, _ := outputs["review_comments"].(string)
			report.ReviewDecisions = append(report.ReviewDecisions, ReviewHistoryEntry{
				ActivityID: rec.Request.ID,
				ReviewerID: rec.Request.AgentID,
				Approved:   approved,
				Comments:   comments,
				Timestamp:  rec.Request.CreatedAt,
			})
		}
	}
	report.MetricsSummary = metricsAgg

	for agent := range agentMap {
		report.ParticipatingAgents = append(report.ParticipatingAgents, agent)
	}

	// Tool executions from IntentJournal
	if journal != nil {
		for _, rec := range subtasks {
			intents := journal.FindIntentsForActivity(rec.Request.ID)
			report.ToolExecutions = append(report.ToolExecutions, intents...)
		}
	}

	// Human interventions from store
	for _, dec := range store.ListAllDecisions() {
		for _, rec := range subtasks {
			if dec.ActivityID == rec.Request.ID {
				report.HumanInterventions = append(report.HumanInterventions, *dec)
			}
		}
	}

	// Promoted memories
	if memStore != nil {
		for _, m := range memStore.ListVerified() {
			for _, rec := range subtasks {
				if m.SourceActivityID == rec.Request.ID {
					report.PromotedConventions = append(report.PromotedConventions, *m)
				}
			}
		}
	}

	// Final outcome
	hasFailures := false
	var failErr string
	allDone := true
	hasApprovedReview := false

	for _, rec := range subtasks {
		if rec.Status == StatusFailed {
			hasFailures = true
			failErr = rec.Error
		}
		if rec.Status == StatusPending || rec.Status == StatusClaimed {
			allDone = false
		}
		if rec.Request.AgentID == "reviewer" && rec.Result != nil {
			if app, ok := rec.Result.Outputs["approved"].(bool); ok && app {
				hasApprovedReview = true
			}
		}
	}

	if hasFailures {
		report.FinalOutcome = fmt.Sprintf("Task failed: %s", failErr)
	} else if hasApprovedReview {
		report.FinalOutcome = "Task successfully implemented, verified natively, and reviewed."
	} else if rootRec.Request.Strategy == StrategyDirect && rootRec.Status == StatusCompleted {
		report.FinalOutcome = "Task successfully implemented, verified natively, and completed."
	} else if !allDone {
		report.FinalOutcome = "Task in progress."
	} else if rootRec.Status == StatusAwaitingHuman {
		report.FinalOutcome = "Task paused awaiting human engineer decision."
	} else {
		report.FinalOutcome = "Task completed."
	}

	return report, nil
}

// FormatHumanReadable produces a clean, professional audit trail.
func (r *TaskHistoryReport) FormatHumanReadable() string {
	var sb strings.Builder
	sb.WriteString("================================================================================\n")
	sb.WriteString(fmt.Sprintf("A4C Task Activity History: %s\n", r.TaskID))
	sb.WriteString("================================================================================\n")
	sb.WriteString(fmt.Sprintf("Original Objective:  %s\n", r.OriginalObjective))
	sb.WriteString(fmt.Sprintf("Status:              %s\n", strings.ToUpper(string(r.Status))))
	if r.Strategy != "" {
		sb.WriteString(fmt.Sprintf("Execution Strategy:  %s\n", strings.ToUpper(string(r.Strategy))))
	}
	if r.Branch != "" {
		sb.WriteString(fmt.Sprintf("Git Worktree Branch: %s\n", r.Branch))
	}
	sb.WriteString(fmt.Sprintf("Participating Agents: %s\n", strings.Join(r.ParticipatingAgents, ", ")))
	sb.WriteString(fmt.Sprintf("Budget:              %.0f allocated (%.0f tokens consumed)\n", r.AllocatedBudget, r.ConsumedBudget))
	if r.MetricsSummary != nil {
		extra := ""
		if r.MetricsSummary.StructuralQueriesCount > 0 || r.MetricsSummary.FileReadsCount > 0 {
			extra = fmt.Sprintf(" | Investigation: %d file reads, %d structural queries",
				r.MetricsSummary.FileReadsCount, r.MetricsSummary.StructuralQueriesCount)
		}
		overrunStr := ""
		if r.MetricsSummary.OverrunTokens > 0 {
			overrunStr = fmt.Sprintf(", %d overrun", r.MetricsSummary.OverrunTokens)
		}
		provWait := r.MetricsSummary.ProviderWaitDuration
		if provWait == 0 {
			provWait = r.MetricsSummary.ModelDuration
		}
		sb.WriteString(fmt.Sprintf("Execution Metrics:   Duration: wall-clock %v (provider wait: %v, model: %v, tools: %v, validation: %v) | Tokens: %d prompt, %d completion (%d total%s)%s\n",
			r.MetricsSummary.WallClockDuration.Round(time.Millisecond),
			provWait.Round(time.Millisecond),
			r.MetricsSummary.ModelDuration.Round(time.Millisecond),
			r.MetricsSummary.ToolDuration.Round(time.Millisecond),
			r.MetricsSummary.ValidationDuration.Round(time.Millisecond),
			r.MetricsSummary.PromptTokens,
			r.MetricsSummary.CompletionTokens,
			r.MetricsSummary.TotalTokens,
			overrunStr,
			extra,
		))
	}
	sb.WriteString(fmt.Sprintf("Created At:          %s\n\n", r.CreatedAt.Format(time.RFC3339)))


	sb.WriteString("--- Activity DAG & Transitions ---\n")
	for idx, act := range r.Activities {
		deps := "none"
		if len(act.DependsOn) > 0 {
			deps = strings.Join(act.DependsOn, ", ")
		}
		sb.WriteString(fmt.Sprintf("[%d] %s (%s)\n", idx+1, act.ActivityID, act.AgentID))
		sb.WriteString(fmt.Sprintf("    Objective:  %s\n", act.Objective))
		sb.WriteString(fmt.Sprintf("    Status:     %s\n", act.Status))
		sb.WriteString(fmt.Sprintf("    Depends On: %s\n", deps))
		if act.Error != "" {
			sb.WriteString(fmt.Sprintf("    Error:      %s\n", act.Error))
		}
		if len(act.EvidenceIDs) > 0 {
			sb.WriteString(fmt.Sprintf("    Evidence:   %s\n", strings.Join(act.EvidenceIDs, ", ")))
		}
	}

	if len(r.ReviewDecisions) > 0 {
		sb.WriteString("\n--- Peer Review Decisions ---\n")
		for _, rev := range r.ReviewDecisions {
			status := "APPROVED"
			if !rev.Approved {
				status = "REJECTED (Revision requested)"
			}
			sb.WriteString(fmt.Sprintf("- Reviewer [%s] on %s: %s\n", rev.ReviewerID, rev.ActivityID, status))
			sb.WriteString(fmt.Sprintf("  Comments: %s\n", rev.Comments))
		}
	}

	if len(r.HumanInterventions) > 0 {
		sb.WriteString("\n--- Human Interventions ---\n")
		for _, dec := range r.HumanInterventions {
			sb.WriteString(fmt.Sprintf("- Decision %s (Proposed by %s): Status=%s\n", dec.DecisionID, dec.ProposedBy, dec.Status))
			sb.WriteString(fmt.Sprintf("  Question:   %s\n", dec.Question))
			if dec.Resolution != "" {
				sb.WriteString(fmt.Sprintf("  Resolution: %s\n", dec.Resolution))
			}
		}
	} else {
		sb.WriteString("\n--- Human Interventions ---\nNone (Fully autonomous collaboration)\n")
	}

	if len(r.PromotedConventions) > 0 {
		sb.WriteString("\n--- Promoted Organizational Conventions ---\n")
		for _, m := range r.PromotedConventions {
			sb.WriteString(fmt.Sprintf("- [%s] %s (Verified by %s, rev %d)\n", m.Key, m.Content, m.Verifier, m.Revision))
		}
	}

	sb.WriteString("\n--- Final Outcome ---\n")
	sb.WriteString(fmt.Sprintf("%s\n", r.FinalOutcome))
	sb.WriteString("================================================================================\n")
	return sb.String()
}

// FormatInspect renders a compact inspection view of a task. structStore and workDir
// are optional; they are used to resolve structural artifact freshness. The workspace
// snapshot is computed at most once per call.
func (r *TaskHistoryReport) FormatInspect(structStore *DurableStructuralStore, workDir string) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Task:       %s\n", r.TaskID))
	sb.WriteString(fmt.Sprintf("Objective:  %s\n", r.OriginalObjective))
	sb.WriteString(fmt.Sprintf("Status:     %s\n", strings.ToUpper(string(r.Status))))
	if r.Strategy != "" {
		sb.WriteString(fmt.Sprintf("Strategy:   %s\n", r.Strategy))
	}
	if r.Branch != "" {
		sb.WriteString(fmt.Sprintf("Branch:     %s\n", r.Branch))
	}
	rootAgent := ""
	for _, a := range r.Activities {
		if a.ActivityID == r.TaskID {
			rootAgent = a.AgentID
		}
	}
	if rootAgent == "" {
		rootAgent = "unassigned"
	}
	sb.WriteString(fmt.Sprintf("Assigned:   %s\n", rootAgent))
	if len(r.ParticipatingAgents) > 0 {
		agents := append([]string(nil), r.ParticipatingAgents...)
		sort.Strings(agents)
		sb.WriteString(fmt.Sprintf("Agents:     %s\n", strings.Join(agents, ", ")))
	}
	sb.WriteString(fmt.Sprintf("Tokens:     %.0f allocated, %.0f consumed\n", r.AllocatedBudget, r.ConsumedBudget))
	if m := r.MetricsSummary; m != nil {
		overrun := ""
		if m.OverrunTokens > 0 {
			overrun = fmt.Sprintf(", %d overrun", m.OverrunTokens)
		}
		provWait := m.ProviderWaitDuration
		if provWait == 0 {
			provWait = m.ModelDuration
		}
		sb.WriteString(fmt.Sprintf("Durations:  wall-clock %v (provider wait: %v, model: %v, tools: %v, validation: %v)%s\n",
			m.WallClockDuration.Round(time.Millisecond),
			provWait.Round(time.Millisecond),
			m.ModelDuration.Round(time.Millisecond),
			m.ToolDuration.Round(time.Millisecond),
			m.ValidationDuration.Round(time.Millisecond),
			overrun))
	} else {
		sb.WriteString("Durations:  n/a\n")
	}

	var snap string
	snapDone := false
	currentSnapshot := func() string {
		if !snapDone {
			snapDone = true
			if workDir != "" {
				if s, err := NewWorkspaceSnapshotter().ComputeSnapshot(workDir); err == nil {
					snap = s
				}
			}
		}
		return snap
	}

	byParent := make(map[string][]ActivityHistoryEntry)
	known := make(map[string]bool)
	for _, a := range r.Activities {
		known[a.ActivityID] = true
	}
	var roots []ActivityHistoryEntry
	for _, a := range r.Activities {
		if a.ActivityID == r.TaskID || a.ParentID == "" || !known[a.ParentID] {
			roots = append(roots, a)
		} else {
			byParent[a.ParentID] = append(byParent[a.ParentID], a)
		}
	}

	sb.WriteString("\n--- Activity Tree ---\n")
	visited := make(map[string]bool)
	var render func(a ActivityHistoryEntry, depth int)
	render = func(a ActivityHistoryEntry, depth int) {
		if visited[a.ActivityID] {
			return
		}
		visited[a.ActivityID] = true
		indent := strings.Repeat("  ", depth)
		sb.WriteString(fmt.Sprintf("%s- %s [%s] agent=%s\n", indent, a.ActivityID, a.Status, a.AgentID))
		sb.WriteString(fmt.Sprintf("%s    Objective: %s\n", indent, a.Objective))
		if len(a.DependsOn) > 0 {
			sb.WriteString(fmt.Sprintf("%s    Depends on: %s\n", indent, strings.Join(a.DependsOn, ", ")))
		}
		if p := a.CapabilityProfile; p != nil {
			name := p.Name
			if name == "" {
				name = p.ID
			}
			sb.WriteString(fmt.Sprintf("%s    Capability profile: %s\n", indent, name))
			if p.Description != "" {
				sb.WriteString(fmt.Sprintf("%s      Description: %s\n", indent, p.Description))
			}
			sb.WriteString(fmt.Sprintf("%s      Allowed tools: %s\n", indent, strings.Join(p.AllowedTools, ", ")))
		}
		sb.WriteString(fmt.Sprintf("%s    Tokens consumed: %.0f\n", indent, a.BudgetUsed))
		if m := a.Metrics; m != nil {
			provWait := m.ProviderWaitDuration
			if provWait == 0 {
				provWait = m.ModelDuration
			}
			sb.WriteString(fmt.Sprintf("%s    Durations: wall-clock %v (provider wait: %v, tools: %v, validation: %v)\n", indent,
				m.WallClockDuration.Round(time.Millisecond),
				provWait.Round(time.Millisecond),
				m.ToolDuration.Round(time.Millisecond),
				m.ValidationDuration.Round(time.Millisecond)))
		}
		if len(a.EvidenceIDs) > 0 {
			sb.WriteString(fmt.Sprintf("%s    Validation evidence: %s\n", indent, strings.Join(a.EvidenceIDs, ", ")))
		}
		if a.StructuralArtifactID != "" {
			sb.WriteString(fmt.Sprintf("%s    Structural context: %s (%s)\n", indent, a.StructuralArtifactID,
				structuralFreshness(structStore, a.StructuralArtifactID, currentSnapshot)))
		}
		if a.Error != "" {
			sb.WriteString(fmt.Sprintf("%s    Error: %s\n", indent, a.Error))
		}
		for _, c := range byParent[a.ActivityID] {
			render(c, depth+1)
		}
	}
	for _, a := range roots {
		render(a, 0)
	}

	if len(r.ReviewDecisions) > 0 {
		sb.WriteString("\n--- Review Decisions ---\n")
		for _, rev := range r.ReviewDecisions {
			status := "APPROVED"
			if !rev.Approved {
				status = "REJECTED"
			}
			sb.WriteString(fmt.Sprintf("- %s on %s: %s\n", rev.ReviewerID, rev.ActivityID, status))
			if rev.Comments != "" {
				sb.WriteString(fmt.Sprintf("  Comments: %s\n", rev.Comments))
			}
		}
	}

	sb.WriteString("\n--- Human Decisions ---\n")
	if len(r.HumanInterventions) == 0 {
		sb.WriteString("None\n")
	}
	for _, d := range r.HumanInterventions {
		sb.WriteString(fmt.Sprintf("- %s [%s] proposed by %s: %s\n", d.DecisionID, d.Status, d.ProposedBy, d.Question))
		if d.Resolution != "" {
			sb.WriteString(fmt.Sprintf("  Resolution: %s\n", d.Resolution))
		}
	}
	return sb.String()
}

// structuralFreshness resolves CURRENT/STALE/UNKNOWN for an artifact. It compares the
// artifact's indexed snapshot with the current workspace snapshot when available and
// otherwise falls back to the recorded freshness.
func structuralFreshness(store *DurableStructuralStore, id string, currentSnapshot func() string) IndexFreshness {
	if store == nil {
		return FreshnessUnknown
	}
	art, err := store.Get(id)
	if err != nil || art == nil {
		return FreshnessUnknown
	}
	if art.IndexedSnapshot != "" {
		if cur := currentSnapshot(); cur != "" {
			if cur == art.IndexedSnapshot {
				return FreshnessCurrent
			}
			return FreshnessStale
		}
	}
	switch art.Freshness {
	case FreshnessCurrent, FreshnessStale:
		return art.Freshness
	}
	return FreshnessUnknown
}
