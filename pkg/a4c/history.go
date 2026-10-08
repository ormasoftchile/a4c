package a4c

import (
	"fmt"
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
	Outputs      map[string]any `json:"outputs,omitempty"`
	EvidenceIDs  []string       `json:"evidence_ids,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
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

	for _, rec := range subtasks {
		if rec.Request.AgentID != "" {
			agentMap[rec.Request.AgentID] = true
		}

		consumed := 0.0
		var outputs map[string]any
		var evIDs []string

		if rec.Result != nil {
			consumed = rec.Result.Consumed.Consumed
			outputs = rec.Result.Outputs
			evIDs = rec.Result.EvidenceIDs
			report.ConsumedBudget += consumed
			report.EvidenceIDs = append(report.EvidenceIDs, evIDs...)
		}

		entry := ActivityHistoryEntry{
			ActivityID:  rec.Request.ID,
			ParentID:    rec.Request.ParentID,
			AgentID:     rec.Request.AgentID,
			Objective:   rec.Request.Objective,
			DependsOn:   rec.Request.DependsOn,
			Status:      rec.Status,
			BudgetUsed:  consumed,
			Error:       rec.Error,
			Outputs:     outputs,
			EvidenceIDs: evIDs,
			CreatedAt:   rec.Request.CreatedAt,
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
	if r.Branch != "" {
		sb.WriteString(fmt.Sprintf("Git Worktree Branch: %s\n", r.Branch))
	}
	sb.WriteString(fmt.Sprintf("Participating Agents: %s\n", strings.Join(r.ParticipatingAgents, ", ")))
	sb.WriteString(fmt.Sprintf("Budget:              %.0f allocated (%.0f tokens consumed)\n", r.AllocatedBudget, r.ConsumedBudget))
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
