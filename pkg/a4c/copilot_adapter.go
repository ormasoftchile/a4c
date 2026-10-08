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
	Path string `json:"path" jsonschema:"relative file path to read"`
}

type ApplyPatchParams struct {
	Path    string `json:"path" jsonschema:"relative file path to modify"`
	Content string `json:"content" jsonschema:"new file content to write"`
}

type ProposeActivityParams struct {
	ProposedAgentID string  `json:"proposed_agent_id" jsonschema:"the agent ID to delegate to: 'coder' or 'reviewer'"`
	Objective       string  `json:"objective" jsonschema:"the specific objective of the subtask"`
	BudgetTokens    float64 `json:"budget_tokens" jsonschema:"the requested budget allocation in tokens"`
	DependsOn       []string `json:"depends_on,omitempty" jsonschema:"optional list of activity IDs that must complete first"`
}

type SubmitReviewParams struct {
	Approved bool   `json:"approved" jsonschema:"true if changes are verified and satisfy the specification; false otherwise"`
	Comments string `json:"comments" jsonschema:"detailed review comments, findings, or reasons for rejection"`
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
	mu        sync.Mutex
	session   *copilot.Session
	sessionID string
	config    AgentConfig
	usage     UsageInfo
}

func (s *CopilotSessionAdapter) SessionID() string {
	return s.sessionID
}

func (s *CopilotSessionAdapter) Send(ctx context.Context, prompt string) (*AgentMessage, error) {
	s.mu.Lock()
	session := s.session
	s.mu.Unlock()

	if session == nil {
		return nil, fmt.Errorf("copilot: session is closed")
	}

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
				s.usage.PromptTokens += int(*data.InputTokens)
			}
			if data.OutputTokens != nil {
				s.usage.CompletionTokens += int(*data.OutputTokens)
			}
			s.usage.TotalTokens = s.usage.PromptTokens + s.usage.CompletionTokens
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

	content := responseContent.String()
	if strings.TrimSpace(content) == "" {
		return nil, ErrMalformedOutput
	}

	return &AgentMessage{
		Role:      "assistant",
		Content:   content,
		Timestamp: time.Now(),
	}, nil
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

func (p *CopilotProvider) CreateSession(ctx context.Context, config AgentConfig) (AgentSession, error) {
	return p.buildSession(ctx, "", config)
}

func (p *CopilotProvider) ResumeSession(ctx context.Context, sessionID string, config AgentConfig) (AgentSession, error) {
	return p.buildSession(ctx, sessionID, config)
}

func (p *CopilotProvider) buildSession(ctx context.Context, sessionID string, config AgentConfig) (AgentSession, error) {
	// 1. Build Governed Custom Tools
	readFileTool := copilot.DefineTool("governed_read_file", "Read file content from workspace",
		func(params ReadFileParams, inv copilot.ToolInvocation) (any, error) {
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
			data, err := os.ReadFile(target)
			if err != nil {
				return nil, err
			}
			return string(data), nil
		})

	applyPatchTool := copilot.DefineTool("governed_apply_patch", "Write or modify file in workspace",
		func(params ApplyPatchParams, inv copilot.ToolInvocation) (any, error) {
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
			if config.Governor != nil {
				if err := config.Governor.AuthorizeToolInvocation(config.AgentID, "governed_submit_review"); err != nil {
					return nil, err
				}
			}
			rev := ReviewResult{
				ReviewerID: config.AgentID,
				Approved:   params.Approved,
				Comments:   params.Comments,
				Timestamp:  time.Now(),
			}
			if config.OnSubmitReview != nil {
				config.OnSubmitReview(rev)
			}
			return fmt.Sprintf("Review recorded (Approved=%t): %s", params.Approved, params.Comments), nil
		})

	requestHumanDecisionTool := copilot.DefineTool("governed_request_human_decision", "Escalate a decision to the human engineer",
		func(params RequestHumanDecisionParams, inv copilot.ToolInvocation) (any, error) {
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

	tools := []copilot.Tool{
		readFileTool,
		applyPatchTool,
		proposeActivityTool,
		submitReviewTool,
		requestHumanDecisionTool,
		proposeMemoryTool,
	}

	// 2. Strict permission handler that fails closed
	permHandler := func(request copilot.PermissionRequest, invocation copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
		toolName := string(request.Kind())
		switch req := request.(type) {
		case *rpc.PermissionRequestCustomTool:
			toolName = req.ToolName
		case rpc.PermissionRequestCustomTool:
			toolName = req.ToolName
		}
		if config.Governor != nil {
			if err := config.Governor.AuthorizeToolInvocation(config.AgentID, toolName); err != nil {
				feedback := fmt.Sprintf("Policy rejected tool: %v", err)
				return &rpc.PermissionDecisionReject{Feedback: &feedback}, nil
			}
		}
		return &rpc.PermissionDecisionApproveOnce{}, nil
	}

	disableGit := false
	sessionConfig := &copilot.SessionConfig{
		SessionID: sessionID,
		Model:     p.model,
		Tools:     tools,
		AvailableTools: []string{
			"governed_read_file",
			"governed_apply_patch",
			"governed_propose_activity",
			"governed_submit_review",
			"governed_request_human_decision",
			"governed_propose_memory",
		},
		EnableHostGitOperations: &disableGit,
		OnPermissionRequest:     permHandler,
		SystemMessage: &copilot.SystemMessageConfig{
			Content: config.Charter,
		},
	}

	copilotSession, err := p.client.CreateSession(ctx, sessionConfig)
	if err != nil {
		return nil, fmt.Errorf("copilot: failed to create session: %w", err)
	}

	actualID := sessionID
	if actualID == "" {
		actualID = copilotSession.SessionID
	}

	return &CopilotSessionAdapter{
		session:   copilotSession,
		sessionID: actualID,
		config:    config,
	}, nil
}
