package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/ormasoftchile/a4c/pkg/a4c"
	"github.com/ormasoftchile/a4c/pkg/a4c/governed"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

// Server wraps the mark3labs MCP server with A4C team service handlers.
type Server struct {
	mcpServer *server.MCPServer
	service   a4c.TeamService
	governed  *governed.Manager
	role      string
}

// NewServer initializes an MCP server exposing canonical A4C team tools and YAWR-governed execution tools.
func NewServer(service a4c.TeamService) *Server {
	return NewServerWithRole(service, "")
}

// NewServerWithRole initializes an MCP server filtered for a specific agent role boundary.
func NewServerWithRole(service a4c.TeamService, role string) *Server {
	govMgr, _ := governed.NewManagerWithRole(service, "", role)
	s := &Server{
		mcpServer: server.NewMCPServer("a4c-team", "1.0.0"),
		service:   service,
		governed:  govMgr,
		role:      role,
	}
	s.registerTools()
	return s
}

// NewServerWithGoverned allows supplying an explicit governed Manager.
func NewServerWithGoverned(service a4c.TeamService, gov *governed.Manager) *Server {
	s := &Server{
		mcpServer: server.NewMCPServer("a4c-team", "1.0.0"),
		service:   service,
		governed:  gov,
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

	// 13. governed_read_file (All agents)
	addAgentTool(true, true, true,
		mcp.NewTool("governed_read_file",
			mcp.WithDescription("Read an authorized file strictly inside the task's isolated worktree using the embedded YAWR execution engine."),
			mcp.WithString("task_id", mcp.Required(), mcp.Description("The unique task identifier")),
			mcp.WithString("path", mcp.Required(), mcp.Description("Relative file path within the task worktree")),
		),
		s.handleGovernedReadFile,
	)

	// 14. governed_apply_patch (Coder only)
	addAgentTool(false, true, false,
		mcp.NewTool("governed_apply_patch",
			mcp.WithDescription("Apply a snapshot-bound unified patch strictly inside the task worktree via YAWR's governed execution kernel. Recorded in durable dispatch store with replay detection."),
			mcp.WithString("task_id", mcp.Required(), mcp.Description("The unique task identifier")),
			mcp.WithString("patch", mcp.Required(), mcp.Description("Unified diff patch content to apply")),
			mcp.WithString("expected_snapshot", mcp.Description("Expected git commit SHA prior to mutation")),
			mcp.WithString("idempotency_key", mcp.Description("Optional idempotency key for deduplication")),
		),
		s.handleGovernedApplyPatch,
	)

	// 15. governed_git_inspect (Lead, Reviewer)
	addAgentTool(true, false, true,
		mcp.NewTool("governed_git_inspect",
			mcp.WithDescription("Inspect git status, diff, log, or commit SHA strictly inside the task worktree using the embedded YAWR engine."),
			mcp.WithString("task_id", mcp.Required(), mcp.Description("The unique task identifier")),
			mcp.WithString("subcommand", mcp.Required(), mcp.Description("Subcommand: diff, status, log, rev-parse"), mcp.Enum("diff", "status", "log", "rev-parse")),
			mcp.WithString("target", mcp.Description("Optional commit, branch, or file target")),
		),
		s.handleGovernedGitInspect,
	)

	// 16. governed_run_tests (Coder only)
	addAgentTool(false, true, false,
		mcp.NewTool("governed_run_tests",
			mcp.WithDescription("Execute bounded go test procedures in the task worktree and collect structured validation evidence via YAWR."),
			mcp.WithString("task_id", mcp.Required(), mcp.Description("The unique task identifier")),
			mcp.WithString("package", mcp.Description("Go package path to test (default: ./...)")),
			mcp.WithBoolean("race", mcp.Description("Enable Go data race detector (default: true)")),
			mcp.WithString("idempotency_key", mcp.Description("Optional idempotency key for deduplication")),
		),
		s.handleGovernedRunTests,
	)

	// 17. governed_structural_search_symbols (All agents)
	addAgentTool(true, true, true,
		mcp.NewTool("governed_structural_search_symbols",
			mcp.WithDescription("Search code symbols (functions, structs, methods, interfaces) across repository or task worktree via CodeTopo structural index."),
			mcp.WithString("query", mcp.Required(), mcp.Description("Symbol name query or search term")),
			mcp.WithString("task_id", mcp.Description("Optional task identifier to query the task worktree's isolated index")),
		),
		s.handleGovernedStructuralSearchSymbols,
	)

	// 18. governed_structural_context_for (All agents)
	addAgentTool(true, true, true,
		mcp.NewTool("governed_structural_context_for",
			mcp.WithDescription("Retrieve rich structural context (definition, signature, source snippet, container, callers, and sibling members) for a code symbol."),
			mcp.WithString("symbol", mcp.Description("Name of the symbol (e.g. 'Cache', 'Clear')")),
			mcp.WithInteger("node_id", mcp.Description("Optional exact node_id in the CodeTopo graph")),
			mcp.WithString("task_id", mcp.Description("Optional task identifier to query the task worktree's isolated index")),
		),
		s.handleGovernedStructuralContextFor,
	)

	// 19. governed_structural_callers (All agents)
	addAgentTool(true, true, true,
		mcp.NewTool("governed_structural_callers",
			mcp.WithDescription("Retrieve callers for a symbol by node_id using CodeTopo structural intelligence."),
			mcp.WithInteger("node_id", mcp.Required(), mcp.Description("The unique node_id of the target symbol")),
			mcp.WithString("task_id", mcp.Description("Optional task identifier to query the task worktree's isolated index")),
		),
		s.handleGovernedStructuralCallers,
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

func (s *Server) handleGovernedReadFile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.governed == nil {
		return mcp.NewToolResultError("governed tool manager is not initialized"), nil
	}
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("missing required parameter 'task_id': %v", err)), nil
	}
	path, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("missing required parameter 'path': %v", err)), nil
	}

	res, err := s.governed.ReadFile(ctx, taskID, path)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("governed read_file failed: %v", err)), nil
	}
	if res.Status == tool.InvocationStatusDenied {
		return mcp.NewToolResultError(fmt.Sprintf("governed read_file denied: %s", res.ErrorMessage)), nil
	}
	if res.Status == tool.InvocationStatusFailed {
		return mcp.NewToolResultError(fmt.Sprintf("governed read_file failed: %s\n%s", res.ErrorMessage, res.Stderr)), nil
	}

	return jsonResult(map[string]any{
		"status":         res.Status,
		"classification": res.Classification,
		"content":        res.Stdout,
		"duration_ms":    res.DurationMs,
	})
}

func (s *Server) handleGovernedApplyPatch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.governed == nil {
		return mcp.NewToolResultError("governed tool manager is not initialized"), nil
	}
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("missing required parameter 'task_id': %v", err)), nil
	}
	patch, err := req.RequireString("patch")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("missing required parameter 'patch': %v", err)), nil
	}
	expectedSnapshot := req.GetString("expected_snapshot", "")
	idempotencyKey := req.GetString("idempotency_key", "")

	res, err := s.governed.ApplyPatch(ctx, taskID, patch, expectedSnapshot, idempotencyKey)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("governed apply_patch failed: %v", err)), nil
	}
	if res.Status == tool.InvocationStatusDenied {
		return mcp.NewToolResultError(fmt.Sprintf("governed apply_patch denied: %s", res.ErrorMessage)), nil
	}
	if res.Status == tool.InvocationStatusFailed {
		return mcp.NewToolResultError(fmt.Sprintf("governed apply_patch failed: %s\n%s", res.ErrorMessage, res.Stderr)), nil
	}

	return jsonResult(map[string]any{
		"status":          res.Status,
		"classification":  res.Classification,
		"occurrence_id":   res.OccurrenceID,
		"idempotency_key": res.IdempotencyKey,
		"replayed":        res.Replayed,
		"stdout":          res.Stdout,
		"duration_ms":     res.DurationMs,
		"output":          res.Output,
	})
}

func (s *Server) handleGovernedGitInspect(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.governed == nil {
		return mcp.NewToolResultError("governed tool manager is not initialized"), nil
	}
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("missing required parameter 'task_id': %v", err)), nil
	}
	subcommand, err := req.RequireString("subcommand")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("missing required parameter 'subcommand': %v", err)), nil
	}
	target := req.GetString("target", "")

	res, err := s.governed.GitInspect(ctx, taskID, subcommand, target)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("governed git_inspect failed: %v", err)), nil
	}
	if res.Status == tool.InvocationStatusDenied {
		return mcp.NewToolResultError(fmt.Sprintf("governed git_inspect denied: %s", res.ErrorMessage)), nil
	}
	if res.Status == tool.InvocationStatusFailed {
		return mcp.NewToolResultError(fmt.Sprintf("governed git_inspect failed: %s\n%s", res.ErrorMessage, res.Stderr)), nil
	}

	return jsonResult(map[string]any{
		"status":         res.Status,
		"classification": res.Classification,
		"stdout":         res.Stdout,
		"duration_ms":    res.DurationMs,
	})
}

func (s *Server) handleGovernedRunTests(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.governed == nil {
		return mcp.NewToolResultError("governed tool manager is not initialized"), nil
	}
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("missing required parameter 'task_id': %v", err)), nil
	}
	pkg := req.GetString("package", "./...")
	race := req.GetBool("race", true)
	idempotencyKey := req.GetString("idempotency_key", "")

	res, err := s.governed.RunTests(ctx, taskID, pkg, race, idempotencyKey)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("governed run_tests failed: %v", err)), nil
	}
	if res.Status == tool.InvocationStatusDenied {
		return mcp.NewToolResultError(fmt.Sprintf("governed run_tests denied: %s", res.ErrorMessage)), nil
	}
	if res.Status == tool.InvocationStatusFailed {
		return mcp.NewToolResultError(fmt.Sprintf("governed run_tests failed: %s\n%s", res.ErrorMessage, res.Stderr)), nil
	}

	return jsonResult(map[string]any{
		"status":          res.Status,
		"classification":  res.Classification,
		"occurrence_id":   res.OccurrenceID,
		"replayed":        res.Replayed,
		"stdout":          res.Stdout,
		"exit_code":       res.ExitCode,
		"duration_ms":     res.DurationMs,
		"output":          res.Output,
	})
}

func (s *Server) getStructuralProvider(taskID string) (*a4c.CodeTopoProvider, error) {
	root := s.service.WorkspaceDir()
	dbPath := filepath.Join(root, ".codetopo", "index.sqlite")

	if taskID != "" {
		if wtPath, err := s.service.GetWorktreePath(taskID); err == nil && wtPath != "" {
			wtDB := filepath.Join(wtPath, ".codetopo", "index.sqlite")
			if _, statErr := os.Stat(wtDB); statErr == nil {
				root = wtPath
				dbPath = wtDB
			}
		}
	}

	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("codetopo index not found at %s; ensure repository is indexed", dbPath)
	}

	return a4c.NewCodeTopoProvider(root, dbPath)
}

func (s *Server) handleGovernedStructuralSearchSymbols(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query, err := req.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("missing required parameter 'query': %v", err)), nil
	}
	taskID := req.GetString("task_id", "")

	provider, err := s.getStructuralProvider(taskID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("structural provider unavailable: %v", err)), nil
	}

	packet, err := provider.SearchSymbols(ctx, query)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("structural symbol search failed: %v", err)), nil
	}

	return jsonResult(packet)
}

func (s *Server) handleGovernedStructuralContextFor(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	symbol := req.GetString("symbol", "")
	nodeID := req.GetInt("node_id", 0)
	taskID := req.GetString("task_id", "")

	if symbol == "" && nodeID == 0 {
		return mcp.NewToolResultError("either 'symbol' or 'node_id' must be provided"), nil
	}

	provider, err := s.getStructuralProvider(taskID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("structural provider unavailable: %v", err)), nil
	}

	var packet *a4c.StructuralContextPacket
	if nodeID > 0 {
		packet, err = provider.GetContextByNodeID(ctx, nodeID)
	} else {
		packet, err = provider.GetContextFor(ctx, symbol)
	}

	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("structural context query failed: %v", err)), nil
	}

	return jsonResult(packet)
}

func (s *Server) handleGovernedStructuralCallers(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	nodeID := req.GetInt("node_id", 0)
	if nodeID == 0 {
		return mcp.NewToolResultError("missing required parameter 'node_id' (or node_id must be > 0)"), nil
	}
	taskID := req.GetString("task_id", "")

	provider, err := s.getStructuralProvider(taskID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("structural provider unavailable: %v", err)), nil
	}

	packet, err := provider.GetCallers(ctx, nodeID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("structural callers query failed: %v", err)), nil
	}

	return jsonResult(packet)
}


