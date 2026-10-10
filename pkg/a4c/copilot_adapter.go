package a4c

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

type ReadFileParams struct {
	Path      string `json:"path" jsonschema:"relative file path to read"`
	StartLine int    `json:"start_line,omitempty" jsonschema:"optional 1-based start line number to begin reading from"`
	EndLine   int    `json:"end_line,omitempty" jsonschema:"optional 1-based end line number to stop reading at"`
}

type ApplyPatchParams struct {
	Path    string `json:"path" jsonschema:"relative file path to modify"`
	Content string `json:"content" jsonschema:"new file content to write"`
}

type ProposeActivityParams struct {
	ProposedAgentID string   `json:"proposed_agent_id" jsonschema:"the agent ID to delegate to: 'coder' or 'reviewer'"`
	Objective       string   `json:"objective" jsonschema:"the specific objective of the subtask"`
	BudgetTokens    float64  `json:"budget_tokens" jsonschema:"the requested budget allocation in tokens"`
	DependsOn       []string `json:"depends_on,omitempty" jsonschema:"optional list of activity IDs that must complete first"`
}

type SubmitReviewParams struct {
	Approved bool     `json:"approved" jsonschema:"true if changes are verified and satisfy the specification; false otherwise"`
	Comments string   `json:"comments" jsonschema:"detailed review comments, findings, or reasons for rejection"`
	Blockers []string `json:"blockers,omitempty" jsonschema:"optional list of specific blocking defects found during review"`
}

type RequestHumanDecisionParams struct {
	Question string `json:"question" jsonschema:"the decision or clarification needed from the human engineer"`
	Reason   string `json:"reason" jsonschema:"the architectural or policy reason requiring human guidance"`
}

type ProposeMemoryParams struct {
	Key     string `json:"key" jsonschema:"unique key for the convention or architectural constraint"`
	Content string `json:"content" jsonschema:"the constraint or convention description"`
}

type CopilotSessionAdapter struct {
	mu             sync.Mutex
	session        *copilot.Session
	sessionID      string
	config         AgentConfig
	usage          UsageInfo
	budgetExceeded bool
	modelDuration  time.Duration
	toolDuration   time.Duration
	toolCallCount  int
	turnCount      int
}

func (s *CopilotSessionAdapter) SessionID() string {
	return s.sessionID
}

func (s *CopilotSessionAdapter) CheckBudget() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.config.Budget.Allocated > 0 && float64(s.usage.TotalTokens) >= s.config.Budget.Allocated {
		return fmt.Errorf("%w: session consumed %d tokens exceeds allocated %.0f",
			ErrBudgetExceeded, s.usage.TotalTokens, s.config.Budget.Allocated)
	}
	return nil
}

func (s *CopilotSessionAdapter) recordToolDuration(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.toolDuration += d
}

func (s *CopilotSessionAdapter) Durations() (time.Duration, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.modelDuration, s.toolDuration
}

func (s *CopilotSessionAdapter) Send(ctx context.Context, prompt string) (*AgentMessage, error) {
	s.mu.Lock()
	s.turnCount++
	if s.config.Limits != nil && s.config.Limits.MaxModelTurns > 0 && s.turnCount > s.config.Limits.MaxModelTurns {
		turns := s.turnCount
		maxTurns := s.config.Limits.MaxModelTurns
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: turn %d exceeds max model turns %d", ErrTurnLimitExceeded, turns, maxTurns)
	}
	if s.config.Budget.Allocated > 0 && float64(s.usage.TotalTokens) >= s.config.Budget.Allocated {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: session budget exhausted before send", ErrBudgetExceeded)
	}
	session := s.session
	toolBefore := s.toolDuration
	s.mu.Unlock()

	if session == nil {
		return nil, fmt.Errorf("copilot: session is closed")
	}

	startSend := time.Now()
	done := make(chan bool, 1)
	errChan := make(chan error, 1)
	var responseContent strings.Builder

	// Attach temporary listener for completion
	unsubscribe := session.On(func(event copilot.SessionEvent) {
		switch data := event.Data.(type) {
		case *copilot.AssistantMessageData:
			responseContent.WriteString(data.Content)
		case *copilot.AssistantUsageData:
			s.mu.Lock()
			if data.InputTokens != nil {
				inTok := int(*data.InputTokens)
				s.usage.PromptTokens += inTok
				if inTok > s.usage.PeakContextTokens {
					s.usage.PeakContextTokens = inTok
				}
			}
			if data.OutputTokens != nil {
				s.usage.CompletionTokens += int(*data.OutputTokens)
			}
			if data.CacheReadTokens != nil {
				s.usage.CachedTokens += int(*data.CacheReadTokens)
			}
			if data.ReasoningTokens != nil {
				s.usage.ReasoningTokens += int(*data.ReasoningTokens)
			}
			if data.Cost != nil {
				s.usage.ReportedCostUSD += *data.Cost
			}
			s.usage.TotalTokens = s.usage.PromptTokens + s.usage.CompletionTokens
			if s.config.Budget.Allocated > 0 && float64(s.usage.TotalTokens) > s.config.Budget.Allocated {
				s.budgetExceeded = true
				s.usage.OverrunTokens = s.usage.TotalTokens - int(s.config.Budget.Allocated)
			}
			s.mu.Unlock()
		case *copilot.SessionIdleData:
			select {
			case done <- true:
			default:
			}
		case *copilot.SessionErrorData:
			select {
			case errChan <- fmt.Errorf("session error: %s", data.Message):
			default:
			}
		}
	})
	defer unsubscribe()

	_, err := session.Send(ctx, copilot.MessageOptions{
		Prompt: prompt,
	})
	if err != nil {
		return nil, fmt.Errorf("copilot: send error: %w", err)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case err := <-errChan:
		return nil, err
	case <-done:
	}

	totalSend := time.Since(startSend)
	s.mu.Lock()
	toolDuringSend := s.toolDuration - toolBefore
	modelSend := totalSend - toolDuringSend
	if modelSend < 0 {
		modelSend = 0
	}
	s.modelDuration += modelSend
	exceeded := s.budgetExceeded
	total := s.usage.TotalTokens
	alloc := s.config.Budget.Allocated
	s.mu.Unlock()

	content := responseContent.String()
	if strings.TrimSpace(content) == "" {
		return nil, ErrMalformedOutput
	}

	msg := &AgentMessage{
		Role:      "assistant",
		Content:   content,
		Timestamp: time.Now(),
	}

	if alloc > 0 && (exceeded || float64(total) > alloc) {
		return msg, fmt.Errorf("%w: session consumed %d tokens exceeds allocated %.0f", ErrBudgetExceeded, total, alloc)
	}

	return msg, nil
}

func (s *CopilotSessionAdapter) Cancel() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session != nil {
		return s.session.Abort(context.Background())
	}
	return nil
}

func (s *CopilotSessionAdapter) Usage() UsageInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage
}

func (s *CopilotSessionAdapter) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session != nil {
		err := s.session.Disconnect()
		s.session = nil
		return err
	}
	return nil
}

type CopilotProvider struct {
	mu     sync.Mutex
	client *copilot.Client
	model  string
}

func NewCopilotProvider(model string) (*CopilotProvider, error) {
	cliPath := os.Getenv("COPILOT_CLI_PATH")
	if cliPath == "" {
		if path, err := exec.LookPath("copilot"); err == nil {
			cliPath = path
		}
	}

	opts := &copilot.ClientOptions{
		LogLevel: "error",
	}
	if cliPath != "" {
		opts.Connection = copilot.StdioConnection{
			Path: cliPath,
		}
	}

	client := copilot.NewClient(opts)
	if err := client.Start(context.Background()); err != nil {
		return nil, fmt.Errorf("copilot: failed to start client: %w", err)
	}

	if model == "" {
		models, err := client.ListModels(context.Background())
		if err == nil && len(models) > 0 {
			model = models[0].ID
		} else {
			model = "auto"
		}
	}

	return &CopilotProvider{
		client: client,
		model:  model,
	}, nil
}

func (p *CopilotProvider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil {
		return p.client.Stop()
	}
	return nil
}

func (p *CopilotProvider) Capabilities() RuntimeCapabilities {
	return RuntimeCapabilities{
		GovernedCustomTools:    true,
		GovernedProposalMode:   false,
		NativeToolRestrictions: true,
		SessionResumption:      true,
		SessionCancellation:    true,
		UsageReporting:         true,
		ProviderTokenLimits:    true,
	}
}

// HostCapabilities returns the explicit discoverable technical capabilities of GitHub Copilot CLI.
func (p *CopilotProvider) HostCapabilities() HostCapabilities {
	return HostCapabilities{
		HostID:                "copilot",
		DisplayName:           "GitHub Copilot CLI",
		Version:               "1.0.95",
		Available:             true,
		SessionInvocation:     true,
		WorktreeBinding:       true,
		ContextInjection:      true,
		CompletionCollection:  true,
		SessionCancellation:   true,
		ModelSelection:        true,
		NativeSubagents:       false,
		SessionResumption:     true,
		StructuredTelemetry:   true,
		ToolRestrictions:     true,
		InteractiveAttachment: true,
		DefaultModel:          p.model,
		SupportedModels: []string{
			"claude-opus-4.8",
			"gpt-4o",
			"claude-3.5-sonnet",
			"auto",
		},
		Limitations: []string{
			"CLI stdio protocol requires active GitHub account authentication",
			"Native subagent spawning is not exposed via automation SDK session",
		},
	}
}

func (p *CopilotProvider) CreateSession(ctx context.Context, config AgentConfig) (AgentSession, error) {
	return p.buildSession(ctx, "", config)
}

func (p *CopilotProvider) ResumeSession(ctx context.Context, sessionID string, config AgentConfig) (AgentSession, error) {
	return p.buildSession(ctx, sessionID, config)
}

func (p *CopilotProvider) buildSession(ctx context.Context, sessionID string, config AgentConfig) (AgentSession, error) {
	// 0. Determine allowed tools for this activity session
	allowedSet := make(map[string]bool)
	if len(config.AllowedTools) > 0 {
		for _, t := range config.AllowedTools {
			allowedSet[t] = true
		}
	} else if config.Profile != nil && len(config.Profile.AllowedTools) > 0 {
		for _, t := range config.Profile.AllowedTools {
			allowedSet[t] = true
		}
	} else {
		// Fallback: consult Governor or default set
		for _, t := range []string{
			"governed_read_file", "governed_apply_patch", "governed_propose_activity",
			"governed_submit_review", "governed_request_human_decision", "governed_propose_memory",
		} {
			if config.Governor == nil || config.Governor.AuthorizeToolInvocation(config.AgentID, t) == nil {
				allowedSet[t] = true
			}
		}
	}

	// 1. Build Governed Custom Tools with in-flight budget enforcement and duration tracking
	adapter := &CopilotSessionAdapter{
		sessionID: sessionID,
		config:    config,
	}

	maxToolCalls := config.MaxToolCalls
	if config.Limits != nil && config.Limits.MaxToolCalls > 0 {
		maxToolCalls = config.Limits.MaxToolCalls
	}
	if maxToolCalls <= 0 {
		maxToolCalls = 10
	}

	checkToolLimits := func() error {
		if err := adapter.CheckBudget(); err != nil {
			return err
		}
		adapter.mu.Lock()
		adapter.toolCallCount++
		calls := adapter.toolCallCount
		adapter.mu.Unlock()
		if calls > maxToolCalls {
			return fmt.Errorf("%w: maximum %d calls allowed per session", ErrToolCallLimitExceeded, maxToolCalls)
		}
		return nil
	}

	readFileTool := copilot.DefineTool("governed_read_file", "Read file content from workspace",
		func(params ReadFileParams, inv copilot.ToolInvocation) (any, error) {
			tStart := time.Now()
			defer func() { adapter.recordToolDuration(time.Since(tStart)) }()

			if err := checkToolLimits(); err != nil {
				return nil, err
			}
			if config.Governor != nil {
				if err := config.Governor.AuthorizeToolInvocation(config.AgentID, "governed_read_file"); err != nil {
					return nil, err
				}
			}
			cleanPath := filepath.Clean(params.Path)
			if strings.HasPrefix(cleanPath, "..") {
				return nil, fmt.Errorf("access denied: path outside workspace")
			}
			target := filepath.Join(config.WorkspaceDir, cleanPath)
			if config.OnRecordMetric != nil {
				config.OnRecordMetric("file_reads", 1)
			}
			data, err := os.ReadFile(target)
			if err != nil {
				return nil, err
			}

			return SliceFileLines(string(data), params.StartLine, params.EndLine, config.MaxReadFileLines), nil
		})

	applyPatchTool := copilot.DefineTool("governed_apply_patch", "Write or modify file in workspace",
		func(params ApplyPatchParams, inv copilot.ToolInvocation) (any, error) {
			tStart := time.Now()
			defer func() { adapter.recordToolDuration(time.Since(tStart)) }()

			if err := checkToolLimits(); err != nil {
				return nil, err
			}
			if config.Governor != nil {
				if err := config.Governor.AuthorizeToolInvocation(config.AgentID, "governed_apply_patch"); err != nil {
					return nil, err
				}
			}
			cleanPath := filepath.Clean(params.Path)
			if strings.HasPrefix(cleanPath, "..") {
				return nil, fmt.Errorf("access denied: path outside workspace")
			}
			target := filepath.Join(config.WorkspaceDir, cleanPath)

			// Record intent prior to mutation
			if config.IntentJournal != nil {
				_ = config.IntentJournal.RecordIntent(IntentRecord{
					IntentID:   fmt.Sprintf("intent-patch-%d", time.Now().UnixNano()),
					ActivityID: "activity-current",
					Action:     "apply_patch",
					Target:     cleanPath,
				})
			}

			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(target, []byte(params.Content), 0644); err != nil {
				return nil, err
			}
			return fmt.Sprintf("Successfully modified %s (%d bytes)", cleanPath, len(params.Content)), nil
		})

	proposeActivityTool := copilot.DefineTool("governed_propose_activity", "Propose a delegated subtask to another team member",
		func(params ProposeActivityParams, inv copilot.ToolInvocation) (any, error) {
			tStart := time.Now()
			defer func() { adapter.recordToolDuration(time.Since(tStart)) }()

			if err := checkToolLimits(); err != nil {
				return nil, err
			}
			if config.Governor != nil {
				if err := config.Governor.AuthorizeToolInvocation(config.AgentID, "governed_propose_activity"); err != nil {
					return nil, err
				}
			}
			prop := ActivityProposal{
				ProposedAgentID: params.ProposedAgentID,
				Objective:       params.Objective,
				RequestedBudget: Budget{Allocated: params.BudgetTokens},
				DependsOn:       params.DependsOn,
			}
			if config.OnProposeActivity != nil {
				config.OnProposeActivity(prop)
			}
			return fmt.Sprintf("Activity proposal for %s recorded: %s", params.ProposedAgentID, params.Objective), nil
		})

	submitReviewTool := copilot.DefineTool("governed_submit_review", "Submit peer review evaluation on code changes",
		func(params SubmitReviewParams, inv copilot.ToolInvocation) (any, error) {
			tStart := time.Now()
			defer func() { adapter.recordToolDuration(time.Since(tStart)) }()

			if err := checkToolLimits(); err != nil {
				return nil, err
			}
			if config.Governor != nil {
				if err := config.Governor.AuthorizeToolInvocation(config.AgentID, "governed_submit_review"); err != nil {
					return nil, err
				}
			}
			rev := ReviewResult{
				ReviewerID: config.AgentID,
				Approved:   params.Approved,
				Comments:   params.Comments,
				Blockers:   params.Blockers,
				Timestamp:  time.Now(),
			}
			if config.OnSubmitReview != nil {
				config.OnSubmitReview(rev)
			}
			return fmt.Sprintf("Review recorded (Approved=%t): %s", params.Approved, params.Comments), nil
		})

	requestHumanDecisionTool := copilot.DefineTool("governed_request_human_decision", "Escalate a decision to the human engineer",
		func(params RequestHumanDecisionParams, inv copilot.ToolInvocation) (any, error) {
			tStart := time.Now()
			defer func() { adapter.recordToolDuration(time.Since(tStart)) }()

			if err := checkToolLimits(); err != nil {
				return nil, err
			}
			if config.Governor != nil {
				if err := config.Governor.AuthorizeToolInvocation(config.AgentID, "governed_request_human_decision"); err != nil {
					return nil, err
				}
			}
			req := HumanDecisionRequest{
				DecisionID: fmt.Sprintf("dec-%d", time.Now().UnixNano()),
				ProposedBy: config.AgentID,
				Question:   params.Question,
				Context: map[string]any{
					"reason": params.Reason,
				},
				Status:    HumanDecisionPending,
				CreatedAt: time.Now(),
			}
			if config.OnRequestHumanDecision != nil {
				config.OnRequestHumanDecision(req)
			}
			return fmt.Sprintf("Decision request submitted to human: %s", params.Question), nil
		})

	proposeMemoryTool := copilot.DefineTool("governed_propose_memory", "Propose an architectural constraint or team convention",
		func(params ProposeMemoryParams, inv copilot.ToolInvocation) (any, error) {
			tStart := time.Now()
			defer func() { adapter.recordToolDuration(time.Since(tStart)) }()

			if err := checkToolLimits(); err != nil {
				return nil, err
			}
			if config.Governor != nil {
				if err := config.Governor.AuthorizeToolInvocation(config.AgentID, "governed_propose_memory"); err != nil {
					return nil, err
				}
			}
			if config.OnProposeMemory != nil {
				config.OnProposeMemory(params.Key, params.Content)
			}
			return fmt.Sprintf("Convention proposed: [%s] %s", params.Key, params.Content), nil
		})

	mutating := isMutatingSpecialist(config.AgentID)

	// 2. Strict permission handler that fails closed
	permHandler := func(request copilot.PermissionRequest, invocation copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
		if err := adapter.CheckBudget(); err != nil {
			feedback := fmt.Sprintf("Budget exceeded: %v", err)
			return &rpc.PermissionDecisionReject{Feedback: &feedback}, nil
		}
		toolName := string(request.Kind())
		switch req := request.(type) {
		case *rpc.PermissionRequestCustomTool:
			toolName = req.ToolName
		case rpc.PermissionRequestCustomTool:
			toolName = req.ToolName
		}

		if !mutating {
			// Reviewer is strictly read-only: deny mutation
			if toolName == "write" || toolName == "edit" || toolName == "patch" || toolName == "governed_apply_patch" {
				feedback := fmt.Sprintf("Reviewer boundary invariant: tool %q is mutating and prohibited for reviewer", toolName)
				return &rpc.PermissionDecisionReject{Feedback: &feedback}, nil
			}
			return &rpc.PermissionDecisionApproveOnce{}, nil
		}

		// Mutating specialist in isolated worktree: permit native tools
		if config.Governor != nil {
			if err := config.Governor.AuthorizeToolInvocation(config.AgentID, toolName); err != nil {
				if strings.HasPrefix(toolName, "governed_") {
					feedback := fmt.Sprintf("Policy rejected tool: %v", err)
					return &rpc.PermissionDecisionReject{Feedback: &feedback}, nil
				}
			}
		}
		return &rpc.PermissionDecisionApproveOnce{}, nil
	}

	// 3. Assemble ONLY capability-scoped tools
	var tools []copilot.Tool
	var availableTools []string

	if allowedSet["governed_read_file"] {
		tools = append(tools, readFileTool)
		availableTools = append(availableTools, "governed_read_file")
	}
	if allowedSet["governed_apply_patch"] {
		tools = append(tools, applyPatchTool)
		availableTools = append(availableTools, "governed_apply_patch")
	}
	if allowedSet["governed_propose_activity"] {
		tools = append(tools, proposeActivityTool)
		availableTools = append(availableTools, "governed_propose_activity")
	}
	if allowedSet["governed_submit_review"] {
		tools = append(tools, submitReviewTool)
		availableTools = append(availableTools, "governed_submit_review")
	}
	if allowedSet["governed_request_human_decision"] {
		tools = append(tools, requestHumanDecisionTool)
		availableTools = append(availableTools, "governed_request_human_decision")
	}
	if allowedSet["governed_propose_memory"] {
		tools = append(tools, proposeMemoryTool)
		availableTools = append(availableTools, "governed_propose_memory")
	}

	modelToUse := p.model
	if config.Model != "" {
		modelToUse = config.Model
	}

	disableGit := false
	var excludedTools []string
	if !mutating {
		excludedTools = []string{"write", "edit", "patch", "bash", "shell"}
	}

	sessionConfig := &copilot.SessionConfig{
		SessionID:               sessionID,
		Model:                   modelToUse,
		WorkingDirectory:        config.WorkspaceDir,
		Tools:                   tools,
		ExcludedTools:          excludedTools,
		EnableHostGitOperations: &disableGit,
		OnPermissionRequest:     permHandler,
		SystemMessage: &copilot.SystemMessageConfig{
			Content: config.Charter,
		},
	}
	if !mutating {
		sessionConfig.AvailableTools = availableTools
	}

	copilotSession, err := p.client.CreateSession(ctx, sessionConfig)
	if err != nil {
		return nil, fmt.Errorf("copilot: failed to create session: %w", err)
	}

	actualID := sessionID
	if actualID == "" {
		actualID = copilotSession.SessionID
	}

	adapter.session = copilotSession
	adapter.sessionID = actualID
	return adapter, nil
}

// SliceFileLines applies line bounds and truncation protection to file content.
func SliceFileLines(content string, startLine, endLine, maxLines int) string {
	if maxLines <= 0 {
		maxLines = 300
	}
	lines := strings.Split(content, "\n")
	if startLine > 0 || endLine > 0 {
		if startLine < 1 {
			startLine = 1
		}
		if endLine < 1 || endLine > len(lines) {
			endLine = len(lines)
		}
		if startLine > len(lines) {
			return fmt.Sprintf("start_line %d exceeds total file lines (%d)", startLine, len(lines))
		}
		count := endLine - startLine + 1
		if count > maxLines {
			endLine = startLine + maxLines - 1
			sub := lines[startLine-1 : endLine]
			return strings.Join(sub, "\n") + fmt.Sprintf("\n... [Output truncated to %d lines. Total lines requested: %d. Total file lines: %d]", maxLines, count, len(lines))
		}
		sub := lines[startLine-1 : endLine]
		return strings.Join(sub, "\n")
	}

	if len(lines) > maxLines {
		sub := lines[:maxLines]
		return strings.Join(sub, "\n") + fmt.Sprintf("\n... [File truncated: showing first %d lines of %d total lines. Specify start_line and end_line to inspect specific ranges]", maxLines, len(lines))
	}
	return content
}
