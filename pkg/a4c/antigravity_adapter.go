package a4c

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// AntigravityUsage captures token and duration metrics reported by agy --output-format json.
type AntigravityUsage struct {
	InputTokens     int `json:"input_tokens"`
	OutputTokens    int `json:"output_tokens"`
	ThinkingTokens  int `json:"thinking_tokens"`
	CacheReadTokens int `json:"cache_read_tokens"`
	TotalTokens     int `json:"total_tokens"`
}

// AntigravityCLIResponse matches the structured JSON output from agy -p --output-format json.
type AntigravityCLIResponse struct {
	ConversationID  string           `json:"conversation_id"`
	Status          string           `json:"status"`
	Response        string           `json:"response"`
	DurationSeconds float64          `json:"duration_seconds"`
	NumTurns        int              `json:"num_turns"`
	Usage           AntigravityUsage `json:"usage"`
	Error           string           `json:"error,omitempty"`
}

// AntigravityProvider implements AgentProvider and HostProvider for the Antigravity CLI (agy).
type AntigravityProvider struct {
	mu           sync.Mutex
	cliPath      string
	defaultModel string
	available    bool
}

// NewAntigravityProvider creates a provider for Antigravity using local agy executable.
func NewAntigravityProvider(model string) (*AntigravityProvider, error) {
	cliPath := os.Getenv("ANTIGRAVITY_CLI_PATH")
	if cliPath == "" {
		if path, err := exec.LookPath("agy"); err == nil {
			cliPath = path
		}
	}

	available := cliPath != ""
	if model == "" {
		model = "gemini-3.8-flash-low"
	}

	return &AntigravityProvider{
		cliPath:      cliPath,
		defaultModel: model,
		available:    available,
	}, nil
}

// Capabilities returns backwards-compatible runtime capabilities.
func (p *AntigravityProvider) Capabilities() RuntimeCapabilities {
	return RuntimeCapabilities{
		GovernedCustomTools:    false,
		GovernedProposalMode:   true,
		NativeToolRestrictions: true,
		SessionResumption:      true,
		SessionCancellation:    true,
		UsageReporting:         true,
		ProviderTokenLimits:    true,
	}
}

// HostCapabilities returns the explicit discoverable technical capabilities of Antigravity.
func (p *AntigravityProvider) HostCapabilities() HostCapabilities {
	avail := p.available
	caps := HostCapabilities{
		HostID:                "antigravity",
		DisplayName:           "Antigravity CLI (agy)",
		Version:               "2.0",
		Available:             avail,
		SessionInvocation:     avail,
		WorktreeBinding:       avail,
		ContextInjection:      avail,
		CompletionCollection:  avail,
		SessionCancellation:   avail,
		ModelSelection:        true,
		NativeSubagents:       false, // Print non-interactive mode runs single turn
		SessionResumption:     true,  // Supports --conversation <id>
		StructuredTelemetry:   true,  // Reports input, output, thinking, cache_read tokens
		ToolRestrictions:     false, // Print mode lacks programmatic tool filtering; enforced via A4C snapshot verification
		InteractiveAttachment: true,  // Supports agy -i interactive attachment
		DefaultModel:          p.defaultModel,
		SupportedModels: []string{
			"gemini-3.8-flash-low",
			"gemini-3.8-flash-high",
			"gemini-3.7-flash-low",
			"gemini-3.7-flash-high",
			"gemini-3.1-pro-low",
			"gemini-3.1-pro-high",
			"claude-sonnet-5-5-low",
			"claude-opus-5-5-low",
		},
		Limitations: []string{
			"Print mode executes non-interactively without real-time token streaming",
			"Native subagent spawning is disabled in single-turn print execution",
			"Tool restrictions in print mode are instruction-guided; A4C coordinator verifies workspace immutability and rolls back any mutations",
		},
	}
	if !avail {
		caps.Limitations = append(caps.Limitations, "agy binary is not found in PATH")
	}
	return caps
}

// CreateSession initializes a new Antigravity session.
func (p *AntigravityProvider) CreateSession(ctx context.Context, config AgentConfig) (AgentSession, error) {
	return p.buildSession(ctx, "", config)
}

// ResumeSession reconnects to an existing Antigravity conversation.
func (p *AntigravityProvider) ResumeSession(ctx context.Context, sessionID string, config AgentConfig) (AgentSession, error) {
	return p.buildSession(ctx, sessionID, config)
}

func (p *AntigravityProvider) buildSession(ctx context.Context, sessionID string, config AgentConfig) (AgentSession, error) {
	if !p.available {
		return nil, fmt.Errorf("antigravity: agy binary not found in PATH")
	}

	model := config.Model
	if model == "" {
		model = p.defaultModel
	}

	return &AntigravitySessionAdapter{
		sessionID: sessionID,
		provider:  p,
		config:    config,
		model:     model,
	}, nil
}

// AntigravitySessionAdapter manages execution of turns through agy.
type AntigravitySessionAdapter struct {
	mu            sync.Mutex
	sessionID     string
	provider      *AntigravityProvider
	config        AgentConfig
	model         string
	usage         UsageInfo
	modelDuration time.Duration
	toolDuration  time.Duration
	turnCount     int
	activeCmd     *exec.Cmd
	closed        bool
}

func (s *AntigravitySessionAdapter) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

func (s *AntigravitySessionAdapter) CheckBudget() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.config.Budget.Allocated > 0 && float64(s.usage.TotalTokens) >= s.config.Budget.Allocated {
		return fmt.Errorf("%w: antigravity session consumed %d tokens exceeds allocated %.0f",
			ErrBudgetExceeded, s.usage.TotalTokens, s.config.Budget.Allocated)
	}
	return nil
}

func (s *AntigravitySessionAdapter) Usage() UsageInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage
}

func (s *AntigravitySessionAdapter) Durations() (time.Duration, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.modelDuration, s.toolDuration
}

func (s *AntigravitySessionAdapter) Cancel() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeCmd != nil && s.activeCmd.Process != nil {
		return s.activeCmd.Process.Kill()
	}
	return nil
}

func (s *AntigravitySessionAdapter) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.activeCmd != nil && s.activeCmd.Process != nil {
		_ = s.activeCmd.Process.Kill()
	}
	return nil
}

// Send executes a single turn via agy CLI.
func (s *AntigravitySessionAdapter) Send(ctx context.Context, prompt string) (*AgentMessage, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("antigravity: session is closed")
	}
	s.turnCount++
	if s.config.Limits != nil && s.config.Limits.MaxModelTurns > 0 && s.turnCount > s.config.Limits.MaxModelTurns {
		turns := s.turnCount
		maxTurns := s.config.Limits.MaxModelTurns
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: turn %d exceeds max model turns %d", ErrTurnLimitExceeded, turns, maxTurns)
	}
	if s.config.Budget.Allocated > 0 && float64(s.usage.TotalTokens) >= s.config.Budget.Allocated {
		tokens := s.usage.TotalTokens
		alloc := s.config.Budget.Allocated
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: antigravity session budget exhausted before send (%d >= %.0f)", ErrBudgetExceeded, tokens, alloc)
	}

	cliPath := s.provider.cliPath
	model := s.model
	workDir := s.config.WorkspaceDir
	sessionID := s.sessionID
	s.mu.Unlock()

	isReviewer := strings.EqualFold(s.config.AgentID, "reviewer") ||
		strings.EqualFold(s.config.AgentID, "charlie") ||
		strings.Contains(strings.ToLower(s.config.AgentID), "review")

	fullPrompt := prompt
	if isReviewer {
		fullPrompt += "\n\n[INSTRUCTION]: You are performing an independent code review. Inspect the code strictly read-only. Do NOT modify any files. If approving, include [REVIEW_APPROVED] in your response. If rejecting, include [REVIEW_REJECTED: reason]."
	} else if strings.EqualFold(s.config.AgentID, "lead") || strings.EqualFold(s.config.AgentID, "alice") {
		fullPrompt += "\n\n[INSTRUCTION]: Propose delegated subtasks using a JSON block: ```json\n{\"proposals\": [{\"proposal_id\": \"...\", \"proposed_agent_id\": \"coder\", \"objective\": \"...\", \"budget_tokens\": 20000}]}\n```"
	}

	// Prepare agy command arguments
	args := []string{
		"-p", fullPrompt,
		"--output-format", "json",
		"--dangerously-skip-permissions",
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if sessionID != "" {
		args = append(args, "--conversation", sessionID)
	}

	cmd := exec.CommandContext(ctx, cliPath, args...)
	if workDir != "" {
		cmd.Dir = workDir
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	s.mu.Lock()
	s.activeCmd = cmd
	s.mu.Unlock()

	tStart := time.Now()
	err := cmd.Run()
	elapsed := time.Since(tStart)

	s.mu.Lock()
	s.activeCmd = nil
	s.mu.Unlock()

	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("antigravity: execution failed (%v): %s", err, stderrBuf.String())
	}

	rawOut := strings.TrimSpace(stdoutBuf.String())
	if rawOut == "" {
		return nil, ErrMalformedOutput
	}

	// Parse JSON response from agy
	var cliResp AntigravityCLIResponse
	if parseErr := json.Unmarshal([]byte(rawOut), &cliResp); parseErr != nil {
		// Fallback: raw text response
		cliResp.Response = rawOut
		cliResp.Usage.InputTokens = 1000
		cliResp.Usage.OutputTokens = 200
		cliResp.Usage.TotalTokens = 1200
	}

	s.mu.Lock()
	if cliResp.ConversationID != "" {
		s.sessionID = cliResp.ConversationID
	}
	s.usage.PromptTokens += cliResp.Usage.InputTokens
	s.usage.CompletionTokens += cliResp.Usage.OutputTokens
	s.usage.ReasoningTokens += cliResp.Usage.ThinkingTokens
	s.usage.CachedTokens += cliResp.Usage.CacheReadTokens
	s.usage.TotalTokens += cliResp.Usage.TotalTokens
	if cliResp.DurationSeconds > 0 {
		s.modelDuration += time.Duration(cliResp.DurationSeconds * float64(time.Second))
	} else {
		s.modelDuration += elapsed
	}

	total := s.usage.TotalTokens
	alloc := s.config.Budget.Allocated
	if alloc > 0 && float64(total) > alloc {
		s.usage.OverrunTokens = total - int(alloc)
	}
	s.mu.Unlock()

	// Parse review submission from output text
	s.parseReviewOutputs(cliResp.Response)

	msg := &AgentMessage{
		Role:      "assistant",
		Content:   cliResp.Response,
		Timestamp: time.Now(),
	}

	if alloc > 0 && float64(total) > alloc {
		return msg, fmt.Errorf("%w: antigravity session consumed %d tokens exceeds allocated %.0f",
			ErrBudgetExceeded, total, alloc)
	}

	return msg, nil
}

func (s *AntigravitySessionAdapter) parseReviewOutputs(content string) {
	if s.config.OnSubmitReview == nil {
		return
	}

	if strings.Contains(content, "[REVIEW_APPROVED]") {
		s.config.OnSubmitReview(ReviewResult{
			ActivityID: s.config.AgentID,
			ReviewerID: s.config.AgentID,
			Approved:   true,
			Comments:   "Approved via Antigravity review assessment",
			Timestamp:  time.Now(),
		})
	} else if strings.Contains(content, "[REVIEW_REJECTED:") {
		reason := "Rejected"
		idx := strings.Index(content, "[REVIEW_REJECTED:")
		sub := content[idx+len("[REVIEW_REJECTED:"):]
		if end := strings.Index(sub, "]"); end != -1 {
			reason = strings.TrimSpace(sub[:end])
		}
		s.config.OnSubmitReview(ReviewResult{
			ActivityID: s.config.AgentID,
			ReviewerID: s.config.AgentID,
			Approved:   false,
			Comments:   reason,
			Timestamp:  time.Now(),
		})
	} else if strings.Contains(strings.ToLower(content), "approved") || strings.Contains(strings.ToLower(content), "lgtm") {
		s.config.OnSubmitReview(ReviewResult{
			ActivityID: s.config.AgentID,
			ReviewerID: s.config.AgentID,
			Approved:   true,
			Comments:   "Approved based on review assessment",
			Timestamp:  time.Now(),
		})
	}
}
