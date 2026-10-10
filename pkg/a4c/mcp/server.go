package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/ormasoftchile/a4c/pkg/a4c"
)

// Server wraps the mark3labs MCP server with A4C team service handlers.
type Server struct {
	mcpServer *server.MCPServer
	service   a4c.TeamService
	role      string
}

// NewServer initializes an MCP server exposing canonical A4C team tools.
func NewServer(service a4c.TeamService) *Server {
	return NewServerWithRole(service, "")
}

// NewServerWithRole initializes an MCP server filtered for a specific agent role boundary.
func NewServerWithRole(service a4c.TeamService, role string) *Server {
	s := &Server{
		mcpServer: server.NewMCPServer("a4c-team", "1.0.0"),
		service:   service,
		role:      role,
	}
	s.registerTools()
	return s
}

// MCPServer returns the underlying mark3labs MCPServer.
func (s *Server) MCPServer() *server.MCPServer {
	return s.mcpServer
}

// ServeStdio runs the server over standard input and output.
func (s *Server) ServeStdio() error {
	return server.ServeStdio(s.mcpServer)
}

func (s *Server) registerTools() {
	role := strings.ToLower(s.role)
	isLead := role == "lead" || role == "alice" || role == "a4c"
	isCoder := role == "coder" || role == "bob"
	isReviewer := role == "reviewer" || role == "charlie"

	addAgentTool := func(forLead, forCoder, forReviewer bool, tool mcp.Tool, handler server.ToolHandlerFunc) {
		if s.role == "" {
			// On un-roled MCP server, register any tool permitted for at least one agent role.
			// Human-only tools (forLead=false, forCoder=false, forReviewer=false) are never registered over MCP.
			if forLead || forCoder || forReviewer {
				s.mcpServer.AddTool(tool, handler)
			}
			return
		}
		// On roled MCP server, only register if permitted for the specific role.
		if (isLead && forLead) || (isCoder && forCoder) || (isReviewer && forReviewer) {
			s.mcpServer.AddTool(tool, handler)
		}
	}

	// 1. team_delegate (Lead only)
	addAgentTool(true, false, false,
		mcp.NewTool("team_delegate",
			mcp.WithDescription("Submit an engineering task to the autonomous personal software engineering team"),
			mcp.WithString("objective", mcp.Required(), mcp.Description("Objective, specification, or instruction for the engineering assignment")),
			mcp.WithNumber("budget_tokens", mcp.Description("Allocated token budget for execution (default: 50000)")),
			mcp.WithString("strategy", mcp.Description("Execution strategy: direct, reviewed, or team (default: team)"), mcp.Enum("direct", "reviewed", "team")),
			mcp.WithString("runtime", mcp.Description("Agent runtime provider: copilot, opencode, or fake"), mcp.Enum("copilot", "opencode", "fake")),
			mcp.WithString("model", mcp.Description("Model identifier (e.g. gpt-4o, claude-3-5-sonnet)")),
		),
		s.handleDelegate,
	)

	// 2. team_list (Lead only)
	addAgentTool(true, false, false,
		mcp.NewTool("team_list",
			mcp.WithDescription("List known engineering task assignments with their live lifecycle state and progress"),
			mcp.WithString("state", mcp.Description("Filter by lifecycle state (queued, running, awaiting_decision, awaiting_human_review, accepted, integrated, rejected, failed)")),
			mcp.WithString("runtime", mcp.Description("Filter by runtime provider (copilot, opencode, fake)")),
			mcp.WithInteger("limit", mcp.Description("Maximum number of tasks to return")),
		),
		s.handleList,
	)

	// 3. team_status (All agents)
	addAgentTool(true, true, true,
		mcp.NewTool("team_status",
			mcp.WithDescription("Inspect detailed live state, worktree branch, activity DAG, token metrics, and lifecycle facts for a task"),
			mcp.WithString("task_id", mcp.Required(), mcp.Description("The unique task identifier")),
		),
		s.handleStatus,
	)

	// 4. team_result (Lead, Reviewer)
	addAgentTool(true, false, true,
		mcp.NewTool("team_result",
			mcp.WithDescription("Retrieve final execution outputs, review verdict, reviewer comments, and validation evidence for a task"),
			mcp.WithString("task_id", mcp.Required(), mcp.Description("The unique task identifier")),
		),
		s.handleResult,
	)

	// 5. team_diff (All agents)
	addAgentTool(true, true, true,
		mcp.NewTool("team_diff",
			mcp.WithDescription("Inspect git diffs and file modifications made in the task worktree before approving"),
			mcp.WithString("task_id", mcp.Required(), mcp.Description("The unique task identifier")),
		),
		s.handleDiff,
	)

	// 6. team_feedback (Lead only)
	addAgentTool(true, false, false,
		mcp.NewTool("team_feedback",
			mcp.WithDescription("Provide human review feedback and request revisions from the engineering team"),
			mcp.WithString("task_id", mcp.Required(), mcp.Description("The unique task identifier")),
			mcp.WithString("feedback", mcp.Required(), mcp.Description("Human review critique or revision instructions")),
			mcp.WithString("target_agent", mcp.Description("Target agent to receive feedback (default: coder)")),
		),
		s.handleFeedback,
	)

	// NOTE: team_accept and team_integrate are STRICT human-only CLI boundaries.
	// They are NEVER exposed over MCP under any circumstances. Agents attempting to
	// invoke acceptance or integration over MCP fail closed by server definition.

	// 7. team_context (Lead only)
	addAgentTool(true, false, false,
		mcp.NewTool("team_context",
			mcp.WithDescription("Retrieve environmental context: repository branch, head commit, team roster, active tasks, and recent architectural memories."),
		),
		s.handleContext,
	)

	// 8. team_begin_task (Lead only)
	addAgentTool(true, false, false,
		mcp.NewTool("team_begin_task",
			mcp.WithDescription("Initialize an engineering task and allocate an isolated git worktree branch for host-native coordination without spawning an autonomous background LLM loop."),
			mcp.WithString("objective", mcp.Required(), mcp.Description("Objective, specification, or instruction for the engineering assignment")),
			mcp.WithNumber("budget_tokens", mcp.Description("Allocated token budget for execution (default: 50000)")),
			mcp.WithString("strategy", mcp.Description("Execution strategy: direct, reviewed, or team (default: team)"), mcp.Enum("direct", "reviewed", "team")),
			mcp.WithString("coordinator", mcp.Description("Coordinator agent identity (default: alice)")),
			mcp.WithString("runtime", mcp.Description("Agent runtime environment: copilot, opencode, or antigravity")),
		),
		s.handleBeginTask,
	)

	// 9. team_assign_activity (Lead only)
	addAgentTool(true, false, false,
		mcp.NewTool("team_assign_activity",
			mcp.WithDescription("Record assignment of a subtask to a specialist agent (Bob Coder, Charlie Reviewer) within the task's isolated worktree."),
			mcp.WithString("task_id", mcp.Required(), mcp.Description("The unique task identifier")),
			mcp.WithString("agent", mcp.Required(), mcp.Description("Target specialist agent (e.g. bob, charlie)")),
			mcp.WithString("role", mcp.Description("Role for the specialist: coder, reviewer, specialist")),
			mcp.WithString("objective", mcp.Required(), mcp.Description("Specific subtask assignment objective")),
			mcp.WithString("input", mcp.Description("Input parameters or context for the specialist")),
		),
		s.handleAssignActivity,
	)

	// 10. team_checkpoint (All agents)
	addAgentTool(true, true, true,
		mcp.NewTool("team_checkpoint",
			mcp.WithDescription("Persist an intermediate milestone, discovery, assumption, or snapshot SHA during host-native execution so progress survives host termination."),
			mcp.WithString("task_id", mcp.Required(), mcp.Description("The unique task identifier")),
			mcp.WithString("activity_id", mcp.Description("Optional activity identifier")),
			mcp.WithString("actor", mcp.Required(), mcp.Description("Reporting actor (e.g. alice, bob)")),
			mcp.WithString("summary", mcp.Required(), mcp.Description("Summary of progress, observations, or discoveries")),
			mcp.WithString("findings", mcp.Description("Semicolon-separated findings or discoveries")),
			mcp.WithString("assumptions", mcp.Description("Semicolon-separated key assumptions made")),
			mcp.WithString("next_steps", mcp.Description("Semicolon-separated next planned steps")),
		),
		s.handleCheckpoint,
	)

	// 11. team_complete_activity (Lead, Coder)
	addAgentTool(true, true, false,
		mcp.NewTool("team_complete_activity",
			mcp.WithDescription("Record completion of a specialist subtask with output summary, modified files, and validation evidence."),
			mcp.WithString("task_id", mcp.Required(), mcp.Description("The unique task identifier")),
			mcp.WithString("activity_id", mcp.Required(), mcp.Description("The unique activity identifier")),
			mcp.WithString("actor", mcp.Required(), mcp.Description("Completing actor (e.g. bob, charlie)")),
			mcp.WithString("summary", mcp.Required(), mcp.Description("Summary of implemented changes or results")),
			mcp.WithString("files_modified", mcp.Description("Comma-separated list of modified file paths")),
			mcp.WithString("validation_evidence", mcp.Description("Verification command output or test evidence")),
			mcp.WithBoolean("success", mcp.Description("Whether the activity succeeded (default: true)")),
		),
		s.handleCompleteActivity,
	)

	// 12. team_review_result (Reviewer only)
	addAgentTool(false, false, true,
		mcp.NewTool("team_review_result",
			mcp.WithDescription("Record an independent code review verdict (approved, rejected, revisions_requested). Verifies that the reviewer did not produce any uncommitted mutations."),
			mcp.WithString("task_id", mcp.Required(), mcp.Description("The unique task identifier")),
			mcp.WithString("reviewer", mcp.Description("Reviewer identity (default: charlie)")),
			mcp.WithString("verdict", mcp.Required(), mcp.Description("Review verdict"), mcp.Enum("approved", "rejected", "revisions_requested")),
			mcp.WithString("comments", mcp.Required(), mcp.Description("Detailed review comments and critique")),
			mcp.WithString("findings", mcp.Description("Semicolon-separated review findings or issues")),
			mcp.WithString("snapshot_sha", mcp.Description("Git snapshot SHA reviewed")),
		),
		s.handleReviewResult,
	)
}

func (s *Server) handleDelegate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	objective, err := req.RequireString("objective")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("missing required parameter 'objective': %v", err)), nil
	}

	budget := req.GetFloat("budget_tokens", 50000.0)
	strategy := req.GetString("strategy", "team")
	runtime := req.GetString("runtime", "")
	model := req.GetString("model", "")

	ref, err := s.service.SubmitTask(ctx, a4c.SubmitTaskRequest{
		Objective:    objective,
		BudgetTokens: budget,
		Strategy:     a4c.ExecutionStrategy(strategy),
		Runtime:      runtime,
		Model:        model,
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("delegate task failed: %v", err)), nil
	}

	return jsonResult(ref)
}

func (s *Server) handleList(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	state := req.GetString("state", "")
	runtime := req.GetString("runtime", "")
	limit := req.GetInt("limit", 0)

	tasks, err := s.service.ListTasks(ctx, a4c.TaskFilter{
		State:   a4c.TaskLifecycleState(state),
		Runtime: runtime,
		Limit:   limit,
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("list tasks failed: %v", err)), nil
	}

	return jsonResult(tasks)
}

func (s *Server) handleStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("missing required parameter 'task_id': %v", err)), nil
	}

	details, err := s.service.GetTask(ctx, taskID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get task status failed: %v", err)), nil
	}

	return jsonResult(details)
}

func (s *Server) handleResult(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("missing required parameter 'task_id': %v", err)), nil
	}

	details, err := s.service.GetTask(ctx, taskID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get task result failed: %v", err)), nil
	}

	type ResultSummary struct {
		TaskID            string                 `json:"task_id"`
		Objective         string                 `json:"objective"`
		State             a4c.TaskLifecycleState `json:"state"`
		ReviewVerdict     string                 `json:"review_verdict,omitempty"`
		ReviewComments    string                 `json:"review_comments,omitempty"`
		CurrentSnapshotID string                 `json:"current_snapshot_id,omitempty"`
		EvidenceIDs       []string               `json:"evidence_ids,omitempty"`
		Metrics           *a4c.ExecutionMetrics  `json:"metrics,omitempty"`
		Accepted          bool                   `json:"accepted"`
		Integrated        bool                   `json:"integrated"`
	}

	summary := ResultSummary{
		TaskID:            details.TaskID,
		Objective:         details.Objective,
		State:             details.State,
		ReviewVerdict:     details.ReviewVerdict,
		ReviewComments:    details.ReviewComments,
		CurrentSnapshotID: details.CurrentSnapshotID,
		EvidenceIDs:       details.EvidenceIDs,
		Metrics:           details.Metrics,
	}
	if details.LifecycleFacts != nil {
		summary.Accepted = details.LifecycleFacts.Accepted
		summary.Integrated = details.LifecycleFacts.Integrated
	}

	return jsonResult(summary)
}

func (s *Server) handleDiff(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("missing required parameter 'task_id': %v", err)), nil
	}

	diffRes, err := s.service.GetTaskDiff(ctx, taskID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get task diff failed: %v", err)), nil
	}

	return jsonResult(diffRes)
}

func (s *Server) handleFeedback(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("missing required parameter 'task_id': %v", err)), nil
	}
	feedback, err := req.RequireString("feedback")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("missing required parameter 'feedback': %v", err)), nil
	}
	targetAgent := req.GetString("target_agent", "coder")

	ref, err := s.service.SubmitFeedback(ctx, a4c.FeedbackRequest{
		TaskID:      taskID,
		Feedback:    feedback,
		TargetAgent: targetAgent,
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("submit feedback failed: %v", err)), nil
	}

	return jsonResult(ref)
}

func (s *Server) handleAccept(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("missing required parameter 'task_id': %v", err)), nil
	}
	comments := req.GetString("comments", "")
	token := req.GetString("confirmation_token", "")

	res, err := s.service.AcceptTask(ctx, a4c.AcceptTaskRequest{
		TaskID:            taskID,
		Comments:          comments,
		AcceptedBy:        "mcp_caller",
		ConfirmationToken: token,
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("accept task failed: %v", err)), nil
	}

	return jsonResult(res)
}

func (s *Server) handleIntegrate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("missing required parameter 'task_id': %v", err)), nil
	}

	// AI agents cannot integrate work over MCP; manual human execution via CLI is strictly required.
	return mcp.NewToolResultError(fmt.Sprintf("unauthorized: AI agents cannot integrate work into base workspace over MCP; manual execution via CLI ('a4c task integrate %s') is required", taskID)), nil
}

func (s *Server) handleContext(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	res, err := s.service.GetTeamContext(ctx)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get team context failed: %v", err)), nil
	}
	return jsonResult(res)
}

func (s *Server) handleBeginTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	objective, err := req.RequireString("objective")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("missing required parameter 'objective': %v", err)), nil
	}

	budget := req.GetFloat("budget_tokens", 50000.0)
	strategy := req.GetString("strategy", "team")
	coordinator := req.GetString("coordinator", "alice")
	runtime := req.GetString("runtime", "")

	res, err := s.service.BeginTask(ctx, a4c.BeginTaskRequest{
		Objective:    objective,
		BudgetTokens: budget,
		Strategy:     a4c.ExecutionStrategy(strategy),
		Coordinator:  coordinator,
		Runtime:      runtime,
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("begin task failed: %v", err)), nil
	}
	return jsonResult(res)
}

func (s *Server) handleAssignActivity(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("missing required parameter 'task_id': %v", err)), nil
	}
	agent, err := req.RequireString("agent")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("missing required parameter 'agent': %v", err)), nil
	}
	objective, err := req.RequireString("objective")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("missing required parameter 'objective': %v", err)), nil
	}

	role := req.GetString("role", "")
	input := req.GetString("input", "")

	res, err := s.service.AssignActivity(ctx, a4c.AssignActivityRequest{
		TaskID:    taskID,
		Agent:     agent,
		Role:      role,
		Objective: objective,
		Input:     input,
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("assign activity failed: %v", err)), nil
	}
	return jsonResult(res)
}

func (s *Server) handleCheckpoint(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("missing required parameter 'task_id': %v", err)), nil
	}
	actor, err := req.RequireString("actor")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("missing required parameter 'actor': %v", err)), nil
	}
	summary, err := req.RequireString("summary")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("missing required parameter 'summary': %v", err)), nil
	}

	activityID := req.GetString("activity_id", "")
	findingsStr := req.GetString("findings", "")
	assumptionsStr := req.GetString("assumptions", "")
	nextStepsStr := req.GetString("next_steps", "")

	var findings, assumptions, nextSteps []string
	if findingsStr != "" {
		for _, f := range splitClean(findingsStr, ";") {
			findings = append(findings, f)
		}
	}
	if assumptionsStr != "" {
		for _, a := range splitClean(assumptionsStr, ";") {
			assumptions = append(assumptions, a)
		}
	}
	if nextStepsStr != "" {
		for _, n := range splitClean(nextStepsStr, ";") {
			nextSteps = append(nextSteps, n)
		}
	}

	res, err := s.service.RecordCheckpoint(ctx, a4c.CheckpointRequest{
		TaskID:      taskID,
		ActivityID:  activityID,
		Actor:       actor,
		Summary:     summary,
		Findings:    findings,
		Assumptions: assumptions,
		NextSteps:   nextSteps,
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("record checkpoint failed: %v", err)), nil
	}
	return jsonResult(res)
}

func (s *Server) handleCompleteActivity(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("missing required parameter 'task_id': %v", err)), nil
	}
	activityID, err := req.RequireString("activity_id")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("missing required parameter 'activity_id': %v", err)), nil
	}
	actor, err := req.RequireString("actor")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("missing required parameter 'actor': %v", err)), nil
	}
	summary, err := req.RequireString("summary")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("missing required parameter 'summary': %v", err)), nil
	}

	filesModifiedStr := req.GetString("files_modified", "")
	var filesModified []string
	if filesModifiedStr != "" {
		filesModified = splitClean(filesModifiedStr, ",")
	}

	evidence := req.GetString("validation_evidence", "")
	success := req.GetBool("success", true)

	res, err := s.service.CompleteActivity(ctx, a4c.CompleteActivityRequest{
		TaskID:             taskID,
		ActivityID:         activityID,
		Actor:              actor,
		Summary:            summary,
		FilesModified:      filesModified,
		ValidationEvidence: evidence,
		Success:            success,
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("complete activity failed: %v", err)), nil
	}
	return jsonResult(res)
}

func (s *Server) handleReviewResult(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("missing required parameter 'task_id': %v", err)), nil
	}
	verdict, err := req.RequireString("verdict")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("missing required parameter 'verdict': %v", err)), nil
	}
	comments, err := req.RequireString("comments")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("missing required parameter 'comments': %v", err)), nil
	}

	reviewer := req.GetString("reviewer", "charlie")
	snapshotSHA := req.GetString("snapshot_sha", "")
	findingsStr := req.GetString("findings", "")
	var findings []string
	if findingsStr != "" {
		findings = splitClean(findingsStr, ";")
	}

	res, err := s.service.RecordReview(ctx, a4c.RecordReviewRequest{
		TaskID:      taskID,
		Reviewer:    reviewer,
		Verdict:     verdict,
		Comments:    comments,
		Findings:    findings,
		SnapshotSHA: snapshotSHA,
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("record review failed: %v", err)), nil
	}
	return jsonResult(res)
}

func splitClean(s, sep string) []string {
	var res []string
	for _, part := range strings.Split(s, sep) {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			res = append(res, trimmed)
		}
	}
	return res
}

func jsonResult(data any) (*mcp.CallToolResult, error) {
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("json serialization error: %v", err)), nil
	}
	return mcp.NewToolResultText(string(b)), nil
}


