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
}

// NewServer initializes an MCP server exposing the A4C task-governance tools.
func NewServer(service a4c.TeamService) *Server {
	s := &Server{
		mcpServer: server.NewMCPServer("a4c-team", "1.0.0"),
		service:   service,
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
	// One execution model, one tool surface: every registered tool is available
	// to the host-native agent. Retired orchestration tools are neither
	// registered nor advertised.
	addTool := func(tool mcp.Tool, handler server.ToolHandlerFunc) {
		s.mcpServer.AddTool(tool, handler)
	}
	// 2. team_list (Lead only)
	addTool(
		mcp.NewTool("team_list",
			mcp.WithDescription("List known engineering task assignments with their live lifecycle state and progress"),
			mcp.WithString("state", mcp.Description("Filter by lifecycle state (queued, running, awaiting_decision, awaiting_human_review, accepted, integrated, rejected, failed)")),
			mcp.WithInteger("limit", mcp.Description("Maximum number of tasks to return")),
		),
		s.handleList,
	)

	// 3. team_status (All agents)
	addTool(
		mcp.NewTool("team_status",
			mcp.WithDescription("Inspect detailed live state, worktree branch, activity DAG, token metrics, and lifecycle facts for a task"),
			mcp.WithString("task_id", mcp.Required(), mcp.Description("The unique task identifier")),
		),
		s.handleStatus,
	)

	// 4. team_result (Lead, Reviewer)
	addTool(
		mcp.NewTool("team_result",
			mcp.WithDescription("Retrieve final execution outputs, review verdict, reviewer comments, and validation evidence for a task"),
			mcp.WithString("task_id", mcp.Required(), mcp.Description("The unique task identifier")),
		),
		s.handleResult,
	)

	// 5. team_diff (All agents)
	addTool(
		mcp.NewTool("team_diff",
			mcp.WithDescription("Inspect git diffs and file modifications made in the task worktree before approving"),
			mcp.WithString("task_id", mcp.Required(), mcp.Description("The unique task identifier")),
		),
		s.handleDiff,
	)

	// 6. team_feedback (Lead only)
	addTool(
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
	addTool(
		mcp.NewTool("team_context",
			mcp.WithDescription("Retrieve environmental context: repository branch, head commit, team roster, active tasks, and recent architectural memories."),
		),
		s.handleContext,
	)

	// 8. team_begin_task (Lead only)
	addTool(
		mcp.NewTool("team_begin_task",
			mcp.WithDescription("Initialize an engineering task and allocate an isolated git worktree branch for host-native coordination without spawning an autonomous background LLM loop."),
			mcp.WithString("objective", mcp.Required(), mcp.Description("Objective, specification, or instruction for the engineering assignment")),
			mcp.WithNumber("budget_tokens", mcp.Description("Allocated token budget for execution (default: 50000)")),
		),
		s.handleBeginTask,
	)
	// 10. team_checkpoint (All agents)
	addTool(
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
	addTool(
		mcp.NewTool("team_complete_activity",
			mcp.WithDescription("Record completion of a specialist subtask with output summary, modified files, and validation evidence."),
			mcp.WithString("task_id", mcp.Required(), mcp.Description("The unique task identifier")),
			mcp.WithString("activity_id", mcp.Required(), mcp.Description("The unique activity identifier")),
			mcp.WithString("actor", mcp.Required(), mcp.Description("Completing actor (e.g. bob, charlie)")),
			mcp.WithString("summary", mcp.Required(), mcp.Description("Summary of implemented changes or results")),
			mcp.WithString("files_modified", mcp.Description("Comma-separated list of modified file paths")),
			mcp.WithString("validation_evidence", mcp.Description("Verification command output or test evidence")),
			mcp.WithBoolean("success", mcp.Description("Whether the activity succeeded (default: true)")),
			mcp.WithString("usage", mcp.Description("Optional provider-reported usage as JSON: {\"input_tokens\":N,\"cache_read_tokens\":N,\"cache_write_tokens\":N,\"output_tokens\":N,\"reasoning_tokens\":N,\"model_invocations\":N,\"cost_reported\":N,\"cost_unit\":\"usd|ai_credits|unpriced\",\"model\":\"...\",\"source\":\"copilot|opencode|antigravity\",\"scope\":\"activity|session_including_subagents\"}. Report only what your host telemetry actually provides and omit unknown fields; units are preserved as reported and never converted.")),
		),
		s.handleCompleteActivity,
	)

	// 13. team_apply_patch (Coder only) — governed, journaled file write.
	// Strict-governance profiles deny native edit/write/patch and route every
	// mutation through this tool so writes are policy-checked, worktree-
	// contained, and intent-journaled. Non-mutating roles are excluded both
	// here (role-filtered registration) and again in the service.
	addTool(
		mcp.NewTool("team_apply_patch",
			mcp.WithDescription("Apply a governed, journaled file write inside the task worktree. Enforces worktree containment, mutating-role boundary, and policy authorization; journals the intent before and after the write."),
			mcp.WithString("task_id", mcp.Required(), mcp.Description("The unique task identifier whose worktree receives the write")),
			mcp.WithString("path", mcp.Required(), mcp.Description("Worktree-relative file path to write. Absolute paths and traversal are rejected.")),
			mcp.WithString("content", mcp.Required(), mcp.Description("Full file content to write (whole-file, atomic)")),
			mcp.WithString("agent", mcp.Description("Writing agent identity (default: coder)")),
		),
		s.handleApplyPatch,
	)

	// 14. team_resume (Lead & Coder) — continue an existing task in its worktree.
	addTool(
		mcp.NewTool("team_resume",
			mcp.WithDescription("Resume an existing task inside its existing worktree, reporting the work already done and what remains. Use this INSTEAD of team_begin_task whenever prior work exists: work lives in the task worktree, not the main checkout, so a partially-completed task looks like an empty repository if you only inspect the checkout."),
			mcp.WithString("task_id", mcp.Description("Task to resume. Omitted: the most recent task that still needs attention.")),
			mcp.WithString("instructions", mcp.Description("Optional guidance for the resumed work")),
		),
		s.handleResume,
	)
}



func (s *Server) handleList(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	state := req.GetString("state", "")
	limit := req.GetInt("limit", 0)

	tasks, err := s.service.ListTasks(ctx, a4c.TaskFilter{
		State: a4c.TaskLifecycleState(state),
		Limit: limit,
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
	// Legacy compatibility: a supplied strategy must be "direct". Retired
	// values fail explicitly instead of being silently reinterpreted.
	strategy := req.GetString("strategy", "")
	if strategy != "" && strategy != "direct" {
		return mcp.NewToolResultError(fmt.Sprintf(
			"strategy %q is retired: A4C no longer manages teams or AI reviewers; begin the task without a strategy (direct is the only execution model)", strategy)), nil
	}

	res, err := s.service.BeginTask(ctx, a4c.BeginTaskRequest{
		Objective:    objective,
		BudgetTokens: budget,
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("begin task failed: %v", err)), nil
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

	var knowledge *a4c.KnowledgeClaim
	if raw := strings.TrimSpace(req.GetString("knowledge", "")); raw != "" {
		var claim a4c.KnowledgeClaim
		if err := json.Unmarshal([]byte(raw), &claim); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid 'knowledge' parameter: %v", err)), nil
		}
		knowledge = &claim
	}

	var usage *a4c.HostUsage
	if raw := strings.TrimSpace(req.GetString("usage", "")); raw != "" {
		var u a4c.HostUsage
		if err := json.Unmarshal([]byte(raw), &u); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid 'usage' parameter: %v", err)), nil
		}
		usage = &u
	}

	res, err := s.service.CompleteActivity(ctx, a4c.CompleteActivityRequest{
		TaskID:             taskID,
		ActivityID:         activityID,
		Actor:              actor,
		Summary:            summary,
		FilesModified:      filesModified,
		ValidationEvidence: evidence,
		Success:            success,
		Knowledge:          knowledge,
		Usage:              usage,
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("complete activity failed: %v", err)), nil
	}
	return jsonResult(res)
}

// handleApplyPatch performs a governed, journaled file write inside a task
// worktree on behalf of a mutating specialist. It is the interactive
// counterpart of the headless governed_apply_patch tool.
func (s *Server) handleApplyPatch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("missing required parameter 'task_id': %v", err)), nil
	}
	path, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("missing required parameter 'path': %v", err)), nil
	}
	content, err := req.RequireString("content")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("missing required parameter 'content': %v", err)), nil
	}
	agent := req.GetString("agent", "coder")

	res, err := s.service.ApplyWorkspaceFile(ctx, a4c.ApplyWorkspaceFileRequest{
		TaskID:  taskID,
		AgentID: agent,
		Path:    path,
		Content: content,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return jsonResult(res)
}

// handleResume continues an existing task inside its existing worktree. It is
// the interactive counterpart of 'a4c task resume', which until now was the
// ONLY resumption path — so a host-native agent asked to resume could only ever
// call team_begin_task and therefore reported existing work as a fresh start.
func (s *Server) handleResume(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID := req.GetString("task_id", "")
	if strings.TrimSpace(taskID) == "" {
		// Same resolution the CLI uses: whatever most needs attention.
		resolved, err := s.service.ResolveTask("", []a4c.TaskLifecycleState{
			a4c.TaskStateInterrupted, a4c.TaskStateRunning,
			a4c.TaskStateFailed, a4c.TaskStateQueued,
			a4c.TaskStateAwaitingHumanReview, a4c.TaskStateAwaitingDecision,
			a4c.TaskStateRevisionRequested,
		})
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("no task to resume: %v", err)), nil
		}
		taskID = resolved.TaskID
	}

	chk, err := s.service.ResumeTaskOnHost(ctx, a4c.ResumeTaskRequest{
		TaskID:       taskID,
		Instructions: req.GetString("instructions", ""),
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	// The checkpoint is the whole point: it names the worktree the work lives
	// in and what is already done, so the caller continues rather than restarts.
	return jsonResult(chk)
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


