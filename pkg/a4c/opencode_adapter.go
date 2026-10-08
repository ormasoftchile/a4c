package a4c

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// OpenCodeProvider implements AgentProvider for OpenCode headless servers.
type OpenCodeProvider struct {
	mu           sync.Mutex
	endpoint     string
	defaultModel string
	serverMgr    *OpenCodeServerManager
	httpClient   *http.Client
}

// NewOpenCodeProvider creates an OpenCode provider pointing to an endpoint.
func NewOpenCodeProvider(endpoint, model string, mgr *OpenCodeServerManager) *OpenCodeProvider {
	if endpoint == "" {
		endpoint = "http://127.0.0.1:4096"
	}
	endpoint = strings.TrimRight(endpoint, "/")

	if model == "" {
		model = "opencode-go/mimo-v2.5"
	}

	return &OpenCodeProvider{
		endpoint:     endpoint,
		defaultModel: model,
		serverMgr:    mgr,
		httpClient:   &http.Client{Timeout: 120 * time.Second},
	}
}

// Close stops the managed server if applicable.
func (p *OpenCodeProvider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.serverMgr != nil && p.serverMgr.IsManaged() {
		return p.serverMgr.Stop()
	}
	return nil
}

// ParseModelIdentifier splits a model string like "opencode-go/mimo-v2.5" into providerID and modelID.
func ParseModelIdentifier(model string) (string, string) {
	if model == "" {
		return "opencode-go", "mimo-v2.5"
	}
	parts := strings.SplitN(model, "/", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	// Default to opencode-go if provider prefix is omitted
	return "opencode-go", parts[0]
}

// CreateSession initializes a new governed OpenCode session.
func (p *OpenCodeProvider) CreateSession(ctx context.Context, config AgentConfig) (AgentSession, error) {
	return p.buildSession(ctx, "", config)
}

// ResumeSession reconnects to an existing OpenCode session.
func (p *OpenCodeProvider) ResumeSession(ctx context.Context, sessionID string, config AgentConfig) (AgentSession, error) {
	return p.buildSession(ctx, sessionID, config)
}

func (p *OpenCodeProvider) buildSession(ctx context.Context, existingID string, config AgentConfig) (AgentSession, error) {
	modelStr := config.Model
	if modelStr == "" {
		modelStr = p.defaultModel
	}
	providerID, modelID := ParseModelIdentifier(modelStr)

	// Determine permission rules for the session
	permRules := p.buildPermissionRules(config)

	sessionID := existingID
	if sessionID == "" {
		// Create session on OpenCode server
		reqBody := map[string]any{
			"title": fmt.Sprintf("A4C Activity %s (%s)", config.AgentID, time.Now().Format(time.RFC3339)),
			"model": map[string]string{
				"providerID": providerID,
				"id":         modelID,
			},
			"permission": permRules,
		}

		bodyBytes, err := json.Marshal(reqBody)
		if err != nil {
			return nil, err
		}

		u, err := url.Parse(p.endpoint + "/session")
		if err != nil {
			return nil, err
		}
		if config.WorkspaceDir != "" {
			q := u.Query()
			q.Set("directory", config.WorkspaceDir)
			u.RawQuery = q.Encode()
		}

		httpReq, err := http.NewRequestWithContext(ctx, "POST", u.String(), bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")

		resp, err := p.httpClient.Do(httpReq)
		if err != nil {
			return nil, fmt.Errorf("opencode: create session: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			var errData any
			_ = json.NewDecoder(resp.Body).Decode(&errData)
			return nil, fmt.Errorf("opencode: create session returned status %d: %v", resp.StatusCode, errData)
		}

		var created struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
			return nil, fmt.Errorf("opencode: decode session response: %w", err)
		}
		sessionID = created.ID
	}

	adapter := &OpenCodeSessionAdapter{
		sessionID:  sessionID,
		provider:   p,
		config:     config,
		providerID: providerID,
		modelID:    modelID,
	}

	return adapter, nil
}

func (p *OpenCodeProvider) buildPermissionRules(config AgentConfig) []map[string]string {
	// Base P0 security boundary: bash and task are ALWAYS strictly denied
	rules := []map[string]string{
		{"permission": "bash", "pattern": "*", "action": "deny"},
		{"permission": "task", "pattern": "*", "action": "deny"},
		{"permission": "webfetch", "pattern": "*", "action": "deny"},
		{"permission": "websearch", "pattern": "*", "action": "deny"},
		{"permission": "question", "pattern": "*", "action": "deny"},
		{"permission": "skill", "pattern": "*", "action": "deny"},
	}

	isCoder := config.AgentID == "coder" || strings.Contains(config.AgentID, "coder")
	canMutate := false

	if config.Profile != nil {
		for _, t := range config.Profile.AllowedTools {
			if t == "governed_apply_patch" || t == "write" || t == "edit" {
				canMutate = true
			}
		}
	} else if isCoder {
		canMutate = true
	}

	if canMutate {
		rules = append(rules,
			map[string]string{"permission": "write", "pattern": "*", "action": "allow"},
			map[string]string{"permission": "edit", "pattern": "*", "action": "allow"},
		)
	} else {
		rules = append(rules,
			map[string]string{"permission": "write", "pattern": "*", "action": "deny"},
			map[string]string{"permission": "edit", "pattern": "*", "action": "deny"},
		)
	}

	// Read tools are safe and read-only
	rules = append(rules,
		map[string]string{"permission": "read", "pattern": "*", "action": "allow"},
		map[string]string{"permission": "glob", "pattern": "*", "action": "allow"},
		map[string]string{"permission": "grep", "pattern": "*", "action": "allow"},
	)

	return rules
}

// OpenCodeSessionAdapter implements AgentSession for an OpenCode session.
type OpenCodeSessionAdapter struct {
	mu                   sync.Mutex
	sessionID            string
	provider             *OpenCodeProvider
	config               AgentConfig
	providerID           string
	modelID              string
	usage                UsageInfo
	turnCount            int
	providerWaitDuration time.Duration
	toolDuration         time.Duration
	closed               bool
}

func (s *OpenCodeSessionAdapter) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

func (s *OpenCodeSessionAdapter) CheckBudget() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.config.Budget.Allocated > 0 && float64(s.usage.TotalTokens) >= s.config.Budget.Allocated {
		return fmt.Errorf("%w: session consumed %d tokens exceeds allocated %.0f",
			ErrBudgetExceeded, s.usage.TotalTokens, s.config.Budget.Allocated)
	}
	return nil
}

func (s *OpenCodeSessionAdapter) Usage() UsageInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage
}

func (s *OpenCodeSessionAdapter) Durations() (time.Duration, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.providerWaitDuration, s.toolDuration
}

func (s *OpenCodeSessionAdapter) Cancel() error {
	s.mu.Lock()
	sessionID := s.sessionID
	endpoint := s.provider.endpoint
	httpClient := s.provider.httpClient
	s.mu.Unlock()

	if sessionID == "" {
		return nil
	}

	req, err := http.NewRequest("POST", fmt.Sprintf("%s/session/%s/abort", endpoint, sessionID), nil)
	if err != nil {
		return err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

func (s *OpenCodeSessionAdapter) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// Send executes a model turn against OpenCode.
func (s *OpenCodeSessionAdapter) Send(ctx context.Context, prompt string) (*AgentMessage, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("opencode: session closed")
	}

	s.turnCount++
	if s.config.Limits != nil && s.config.Limits.MaxModelTurns > 0 && s.turnCount > s.config.Limits.MaxModelTurns {
		turns := s.turnCount
		maxTurns := s.config.Limits.MaxModelTurns
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: turn %d exceeds maximum %d", ErrTurnLimitExceeded, turns, maxTurns)
	}

	// Pre-invocation admission check
	if s.config.Budget.Allocated > 0 && float64(s.usage.TotalTokens) >= s.config.Budget.Allocated {
		tokens := s.usage.TotalTokens
		alloc := s.config.Budget.Allocated
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: session consumed %d tokens exceeds allocated %.0f", ErrBudgetExceeded, tokens, alloc)
	}

	sessionID := s.sessionID
	endpoint := s.provider.endpoint
	httpClient := s.provider.httpClient
	providerID := s.providerID
	modelID := s.modelID
	s.mu.Unlock()

	// Tool toggles: strictly deny bash, task, and unauthorized tools
	toolsMap := map[string]bool{
		"bash":      false,
		"task":      false,
		"patch":     false,
		"webfetch":  false,
		"websearch": false,
		"question":  false,
		"skill":     false,
		"read":      true,
		"glob":      true,
		"grep":      true,
	}

	isCoder := s.config.AgentID == "coder" || strings.Contains(s.config.AgentID, "coder")
	if isCoder {
		toolsMap["write"] = true
		toolsMap["edit"] = true
	} else {
		toolsMap["write"] = false
		toolsMap["edit"] = false
	}

	promptText := prompt
	if s.config.AgentID == "reviewer" {
		promptText += "\n\n[INSTRUCTION]: If approving the changes, include [REVIEW_APPROVED] in your response. If rejecting, include [REVIEW_REJECTED: reason]."
	}
	if s.config.AgentID == "lead" {
		promptText += "\n\n[INSTRUCTION]: Delegate tasks using a JSON block: ```json\n{\"proposals\": [{\"proposal_id\": \"...\", \"proposed_agent_id\": \"coder\", \"objective\": \"...\", \"budget_tokens\": 20000}]}\n```"
	}

	reqPayload := map[string]any{
		"tools": toolsMap,
		"model": map[string]string{
			"providerID": providerID,
			"modelID":    modelID,
		},
		"parts": []map[string]string{
			{
				"type": "text",
				"text": promptText,
			},
		},
	}
	if s.config.Charter != "" {
		reqPayload["system"] = s.config.Charter
	}

	payloadBytes, err := json.Marshal(reqPayload)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/session/%s/message", endpoint, sessionID),
		bytes.NewReader(payloadBytes))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	tStart := time.Now()
	resp, err := httpClient.Do(httpReq)
	waitDur := time.Since(tStart)

	if err != nil {
		return nil, fmt.Errorf("opencode: send request: %w", err)
	}
	defer resp.Body.Close()

	s.mu.Lock()
	s.providerWaitDuration += waitDur
	s.mu.Unlock()

	if resp.StatusCode != http.StatusOK {
		var errData any
		_ = json.NewDecoder(resp.Body).Decode(&errData)
		return nil, fmt.Errorf("opencode: message request status %d: %v", resp.StatusCode, errData)
	}

	var msgResp struct {
		Info struct {
			ID      string  `json:"id"`
			Role    string  `json:"role"`
			Cost    float64 `json:"cost"`
			ModelID string  `json:"modelID"`
			Tokens  struct {
				Total     int `json:"total"`
				Input     int `json:"input"`
				Output    int `json:"output"`
				Reasoning int `json:"reasoning"`
				Cache     struct {
					Read  int `json:"read"`
					Write int `json:"write"`
				} `json:"cache"`
			} `json:"tokens"`
		} `json:"info"`
		Parts []struct {
			Type   string `json:"type"`
			Text   string `json:"text,omitempty"`
			Reason string `json:"reason,omitempty"`
			Tokens struct {
				Total     int `json:"total"`
				Input     int `json:"input"`
				Output    int `json:"output"`
				Reasoning int `json:"reasoning"`
			} `json:"tokens,omitempty"`
			Cost float64 `json:"cost,omitempty"`
		} `json:"parts"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&msgResp); err != nil {
		return nil, fmt.Errorf("opencode: decode message response: %w", err)
	}

	// Extract text content from parts
	var sb strings.Builder
	for _, part := range msgResp.Parts {
		if part.Type == "text" && part.Text != "" {
			sb.WriteString(part.Text)
		}
	}
	content := sb.String()

	s.mu.Lock()
	s.usage.PromptTokens += msgResp.Info.Tokens.Input
	s.usage.CompletionTokens += msgResp.Info.Tokens.Output
	s.usage.TotalTokens += msgResp.Info.Tokens.Total
	s.usage.ReasoningTokens += msgResp.Info.Tokens.Reasoning
	s.usage.CachedTokens += msgResp.Info.Tokens.Cache.Read
	s.usage.ReportedCostUSD += msgResp.Info.Cost

	total := s.usage.TotalTokens
	alloc := s.config.Budget.Allocated
	if alloc > 0 && float64(total) > alloc {
		s.usage.OverrunTokens = total - int(alloc)
	}
	s.mu.Unlock()

	// Parse fallback review or proposal patterns from output
	s.parseStructuredOutputs(content)

	msg := &AgentMessage{
		Role:      "assistant",
		Content:   content,
		Timestamp: time.Now(),
	}

	// Post-invocation budget check
	if alloc > 0 && float64(total) > alloc {
		return msg, fmt.Errorf("%w: opencode session consumed %d tokens exceeds allocated %.0f",
			ErrBudgetExceeded, total, alloc)
	}

	return msg, nil
}

func (s *OpenCodeSessionAdapter) parseStructuredOutputs(content string) {
	// Parse review submissions: [SUBMIT_REVIEW: approved=true/false, comments="..."]
	if s.config.OnSubmitReview != nil {
		if strings.Contains(content, "[SUBMIT_REVIEW:") {
			approved := strings.Contains(content, "approved=true")
			comments := "Review completed"
			if idx := strings.Index(content, "comments="); idx != -1 {
				sub := content[idx+len("comments="):]
				if end := strings.Index(sub, "]"); end != -1 {
					comments = strings.Trim(strings.TrimSpace(sub[:end]), "\"")
				}
			}
			s.config.OnSubmitReview(ReviewResult{
				ActivityID: s.config.AgentID,
				ReviewerID: s.config.AgentID,
				Approved:   approved,
				Comments:   comments,
				Timestamp:  time.Now(),
			})
		} else if strings.Contains(content, "[REVIEW_APPROVED]") {
			s.config.OnSubmitReview(ReviewResult{
				ActivityID: s.config.AgentID,
				ReviewerID: s.config.AgentID,
				Approved:   true,
				Comments:   "Approved via model directive",
				Timestamp:  time.Now(),
			})
		} else if strings.Contains(content, "[REVIEW_REJECTED:") {
			reason := "Rejected"
			idx := strings.Index(content, "[REVIEW_REJECTED:")
			sub := content[idx+len("[REVIEW_REJECTED:"): ]
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
		} else if strings.Contains(strings.ToLower(content), "reject") || strings.Contains(strings.ToLower(content), "revision needed") {
			s.config.OnSubmitReview(ReviewResult{
				ActivityID: s.config.AgentID,
				ReviewerID: s.config.AgentID,
				Approved:   false,
				Comments:   "Rejected based on review assessment",
				Timestamp:  time.Now(),
			})
		}
	}

	// Parse activity proposals: ```json { "proposals": [...] } ```
	if s.config.OnProposeActivity != nil && strings.Contains(content, "```json") {
		start := strings.Index(content, "```json") + 7
		end := strings.Index(content[start:], "```")
		if end != -1 {
			jsonBlock := strings.TrimSpace(content[start : start+end])
			var parsed struct {
				Proposals []ActivityProposal `json:"proposals"`
			}
			if err := json.Unmarshal([]byte(jsonBlock), &parsed); err == nil {
				for _, prop := range parsed.Proposals {
					s.config.OnProposeActivity(prop)
				}
			}
		}
	}

	// Parse memory proposals: [PROPOSE_MEMORY: key="...", content="..."]
	if s.config.OnProposeMemory != nil && strings.Contains(content, "[PROPOSE_MEMORY:") {
		idx := strings.Index(content, "[PROPOSE_MEMORY:")
		sub := content[idx+len("[PROPOSE_MEMORY:"): ]
		if end := strings.Index(sub, "]"); end != -1 {
			line := sub[:end]
			key := ""
			val := ""
			if kIdx := strings.Index(line, "key="); kIdx != -1 {
				kSub := line[kIdx+4:]
				if comma := strings.Index(kSub, ","); comma != -1 {
					key = strings.Trim(strings.TrimSpace(kSub[:comma]), "\"")
				}
			}
			if cIdx := strings.Index(line, "content="); cIdx != -1 {
				val = strings.Trim(strings.TrimSpace(line[cIdx+8:]), "\"")
			}
			if key != "" && val != "" {
				s.config.OnProposeMemory(key, val)
			}
		}
	}
}
