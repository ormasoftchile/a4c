package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/ormasoftchile/a4c/pkg/a4c"
)

// Server wraps the mark3labs MCP server with A4C team service handlers.
type Server struct {
	mcpServer *server.MCPServer
	service   a4c.TeamService
}

// NewServer initializes an MCP server exposing the 8 canonical A4C team tools.
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
	// 1. team_delegate
	s.mcpServer.AddTool(
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

	// 2. team_list
	s.mcpServer.AddTool(
		mcp.NewTool("team_list",
			mcp.WithDescription("List known engineering task assignments with their live lifecycle state and progress"),
			mcp.WithString("state", mcp.Description("Filter by lifecycle state (queued, running, awaiting_decision, awaiting_human_review, accepted, integrated, rejected, failed)")),
			mcp.WithString("runtime", mcp.Description("Filter by runtime provider (copilot, opencode, fake)")),
			mcp.WithInteger("limit", mcp.Description("Maximum number of tasks to return")),
		),
		s.handleList,
	)

	// 3. team_status
	s.mcpServer.AddTool(
		mcp.NewTool("team_status",
			mcp.WithDescription("Inspect detailed live state, worktree branch, activity DAG, token metrics, and lifecycle facts for a task"),
			mcp.WithString("task_id", mcp.Required(), mcp.Description("The unique task identifier")),
		),
		s.handleStatus,
	)

	// 4. team_result
	s.mcpServer.AddTool(
		mcp.NewTool("team_result",
			mcp.WithDescription("Retrieve final execution outputs, review verdict, reviewer comments, and validation evidence for a task"),
			mcp.WithString("task_id", mcp.Required(), mcp.Description("The unique task identifier")),
		),
		s.handleResult,
	)

	// 5. team_diff
	s.mcpServer.AddTool(
		mcp.NewTool("team_diff",
			mcp.WithDescription("Inspect git diffs and file modifications made in the task worktree before approving"),
			mcp.WithString("task_id", mcp.Required(), mcp.Description("The unique task identifier")),
		),
		s.handleDiff,
	)

	// 6. team_feedback
	s.mcpServer.AddTool(
		mcp.NewTool("team_feedback",
			mcp.WithDescription("Provide human review feedback and request revisions from the engineering team"),
			mcp.WithString("task_id", mcp.Required(), mcp.Description("The unique task identifier")),
			mcp.WithString("feedback", mcp.Required(), mcp.Description("Human review critique or revision instructions")),
			mcp.WithString("target_agent", mcp.Description("Target agent to receive feedback (default: coder)")),
		),
		s.handleFeedback,
	)

	// 7. team_accept
	s.mcpServer.AddTool(
		mcp.NewTool("team_accept",
			mcp.WithDescription("Record explicit human approval of an exact reviewed snapshot. Note: does NOT modify the working checkout."),
			mcp.WithString("task_id", mcp.Required(), mcp.Description("The unique task identifier")),
			mcp.WithString("comments", mcp.Description("Approval comments or sign-off notes")),
		),
		s.handleAccept,
	)

	// 8. team_integrate
	s.mcpServer.AddTool(
		mcp.NewTool("team_integrate",
			mcp.WithDescription("Integrate approved changes from an accepted task into the base repository working checkout. Verifies snapshot identity and clean working tree before merging."),
			mcp.WithString("task_id", mcp.Required(), mcp.Description("The unique task identifier")),
		),
		s.handleIntegrate,
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

	res, err := s.service.AcceptTask(ctx, a4c.AcceptTaskRequest{
		TaskID:   taskID,
		Comments: comments,
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

	res, err := s.service.IntegrateTask(ctx, a4c.IntegrateTaskRequest{
		TaskID: taskID,
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("integrate task failed: %v", err)), nil
	}

	return jsonResult(res)
}

func jsonResult(data any) (*mcp.CallToolResult, error) {
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("json serialization error: %v", err)), nil
	}
	return mcp.NewToolResultText(string(b)), nil
}
