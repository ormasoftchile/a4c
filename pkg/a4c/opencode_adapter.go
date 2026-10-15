package a4c

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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
		httpClient:   &http.Client{Timeout: 300 * time.Second},
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

// Capabilities returns the explicit governance and technical guarantees supported by OpenCode.
func (p *OpenCodeProvider) Capabilities() RuntimeCapabilities {
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

// isServerReachable performs a lightweight probe against the OpenCode server endpoint.
func (p *OpenCodeProvider) isServerReachable() bool {
	probeClient := &http.Client{Timeout: 800 * time.Millisecond}
	resp, err := probeClient.Get(p.endpoint + "/project")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 500
}

// HostCapabilities returns the explicit discoverable technical capabilities of OpenCode.
func (p *OpenCodeProvider) HostCapabilities() HostCapabilities {
	reachable := p.isServerReachable()
	caps := HostCapabilities{
		HostID:                "opencode",
		DisplayName:           "OpenCode Headless Server",
		Version:               "1.18.35",
		Available:             reachable,
		SessionInvocation:     reachable,
		WorktreeBinding:       reachable,
		ContextInjection:      reachable,
		CompletionCollection:  reachable,
		SessionCancellation:   reachable,
		ModelSelection:        true,
		NativeSubagents:       false,
		SessionResumption:     true,
		StructuredTelemetry:   true,
		ToolRestrictions:     true,
		InteractiveAttachment: false,
		DefaultModel:          p.defaultModel,
		SupportedModels: []string{
			"opencode-go/mimo-v2.5",
			"opencode-go/mimo-v2",
		},
		Limitations: []string{
			"Requires running headless server (opencode serve)",
			"Native subagents are not exposed through public headless HTTP API",
		},
	}
	if !reachable {
		caps.Limitations = append(caps.Limitations, fmt.Sprintf("OpenCode server at %s is not reachable", p.endpoint))
	}
	return caps
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
	if config.WorkspaceDir != "" {
		if absDir, err := filepath.Abs(config.WorkspaceDir); err == nil {
			config.WorkspaceDir = absDir
		}
	}

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

func isMutatingSpecialist(agentID string) bool {
	id := strings.ToLower(agentID)
	if id == "lead" || id == "alice" || id == "reviewer" || id == "charlie" || strings.Contains(id, "review") {
		return false
	}
	return true
}

func (p *OpenCodeProvider) buildPermissionRules(config AgentConfig) []map[string]string {
	if !isMutatingSpecialist(config.AgentID) {
		// Non-mutating roles (lead, reviewer): strictly read-only session
		return []map[string]string{
			{"permission": "bash", "pattern": "*", "action": "deny"},
			{"permission": "task", "pattern": "*", "action": "deny"},
			{"permission": "write", "pattern": "*", "action": "deny"},
			{"permission": "edit", "pattern": "*", "action": "deny"},
			{"permission": "patch", "pattern": "*", "action": "deny"},
			{"permission": "webfetch", "pattern": "*", "action": "deny"},
			{"permission": "websearch", "pattern": "*", "action": "deny"},
			{"permission": "question", "pattern": "*", "action": "deny"},
			{"permission": "skill", "pattern": "*", "action": "deny"},
			{"permission": "read", "pattern": "*", "action": "allow"},
			{"permission": "glob", "pattern": "*", "action": "allow"},
			{"permission": "grep", "pattern": "*", "action": "allow"},
		}
	}

	// Specialists and implementers: allow native tools inside isolated worktree
	return []map[string]string{
		{"permission": "bash", "pattern": "*", "action": "allow"},
		{"permission": "task", "pattern": "*", "action": "deny"},
		{"permission": "write", "pattern": "*", "action": "allow"},
		{"permission": "edit", "pattern": "*", "action": "allow"},
		{"permission": "patch", "pattern": "*", "action": "allow"},
		{"permission": "webfetch", "pattern": "*", "action": "deny"},
		{"permission": "websearch", "pattern": "*", "action": "deny"},
		{"permission": "question", "pattern": "*", "action": "deny"},
		{"permission": "skill", "pattern": "*", "action": "deny"},
		{"permission": "read", "pattern": "*", "action": "allow"},
		{"permission": "glob", "pattern": "*", "action": "allow"},
		{"permission": "grep", "pattern": "*", "action": "allow"},
	}
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
	toolCallCount        int
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

	isReviewer := strings.EqualFold(s.config.AgentID, "reviewer") ||
		strings.EqualFold(s.config.AgentID, "charlie") ||
		strings.Contains(strings.ToLower(s.config.AgentID), "review")
	mutating := isMutatingSpecialist(s.config.AgentID)

	// Specialists & coders use native editing and bash tools directly inside the isolated worktree.
	// Non-mutating roles (lead, reviewer) are strictly read-only.
	toolsMap := map[string]bool{
		"bash":      mutating,
		"task":      false,
		"write":     mutating,
		"edit":      mutating,
		"patch":     mutating,
		"webfetch":  false,
		"websearch": false,
		"question":  false,
		"skill":     false,
		"read":      true,
		"glob":      true,
		"grep":      true,
	}

	promptText := prompt
	if isReviewer {
		promptText += "\n\n[INSTRUCTION]: Inspect the changes strictly read-only. If approving, include [REVIEW_APPROVED] in your response. If rejecting, include [REVIEW_REJECTED: reason]."
	} else if strings.EqualFold(s.config.AgentID, "lead") || strings.EqualFold(s.config.AgentID, "alice") {
		promptText += "\n\n[INSTRUCTION]: Delegate tasks using a JSON block: ```json\n{\"proposals\": [{\"proposal_id\": \"...\", \"proposed_agent_id\": \"coder\", \"objective\": \"...\", \"budget_tokens\": 30000}]}\n```\nPropose budgets that cover full expected context overhead; the runtime raises proposals below the viable minimum and rejects proposals the parent cannot afford."
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
		if pe := ClassifyProviderError("opencode", modelID, err, ""); pe != nil {
			return nil, pe
		}
		return nil, fmt.Errorf("opencode: send request: %w", err)
	}
	defer resp.Body.Close()

	s.mu.Lock()
	s.providerWaitDuration += waitDur
	s.mu.Unlock()

	if resp.StatusCode != http.StatusOK {
		var errData any
		_ = json.NewDecoder(resp.Body).Decode(&errData)
		rawErr := fmt.Sprintf("status %d: %v", resp.StatusCode, errData)
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, &ProviderError{
				Kind:        ProviderFailureRateLimited,
				HostID:      "opencode",
				Model:       modelID,
				Message:     rawErr,
				Retryable:   true,
				OriginalErr: errors.New(rawErr),
			}
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, &ProviderError{
				Kind:        ProviderFailureAuthInvalid,
				HostID:      "opencode",
				Model:       modelID,
				Message:     rawErr,
				Retryable:   false,
				OriginalErr: errors.New(rawErr),
			}
		}
		if resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusBadGateway {
			return nil, &ProviderError{
				Kind:        ProviderFailureTemporaryOutage,
				HostID:      "opencode",
				Model:       modelID,
				Message:     rawErr,
				Retryable:   true,
				OriginalErr: errors.New(rawErr),
			}
		}
		if pe := ClassifyProviderError("opencode", modelID, errors.New(rawErr), rawErr); pe != nil {
			return nil, pe
		}
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
	in := msgResp.Info.Tokens.Input
	out := msgResp.Info.Tokens.Output
	reasoning := msgResp.Info.Tokens.Reasoning
	cached := msgResp.Info.Tokens.Cache.Read
	totalTok := msgResp.Info.Tokens.Total

	// Fallback to parts-level tokens if info.tokens was empty
	if totalTok == 0 && in == 0 && out == 0 {
		for _, p := range msgResp.Parts {
			in += p.Tokens.Input
			out += p.Tokens.Output
			reasoning += p.Tokens.Reasoning
			totalTok += p.Tokens.Total
		}
	}
	if totalTok == 0 && (in > 0 || out > 0) {
		totalTok = in + out
	}

	s.usage.PromptTokens += in
	s.usage.CompletionTokens += out
	s.usage.TotalTokens += totalTok
	s.usage.ReasoningTokens += reasoning
	s.usage.CachedTokens += cached
	s.usage.ReportedCostUSD += msgResp.Info.Cost

	total := s.usage.TotalTokens
	alloc := s.config.Budget.Allocated
	if alloc > 0 && float64(total) > alloc {
		s.usage.OverrunTokens = total - int(alloc)
	}
	s.mu.Unlock()

	msg := &AgentMessage{
		Role:      "assistant",
		Content:   content,
		Timestamp: time.Now(),
	}

	// Apply governed patches in Governed Proposal Mode
	if err := s.applyGovernedPatches(content); err != nil {
		return msg, err
	}

	// Parse fallback review or proposal patterns from output
	s.parseStructuredOutputs(content)

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

// GovernedPatch represents a code modification proposal emitted by an agent.
type GovernedPatch struct {
	Path    string
	Content string
}

// extractGovernedPatches extracts all [PROPOSE_PATCH: path="..."] blocks from model output.
func extractGovernedPatches(content string) []GovernedPatch {
	var patches []GovernedPatch
	marker := "[PROPOSE_PATCH:"
	pos := 0
	for {
		idx := strings.Index(content[pos:], marker)
		if idx == -1 {
			break
		}
		patchStart := pos + idx
		headerEnd := strings.Index(content[patchStart:], "]")
		if headerEnd == -1 {
			break
		}
		header := content[patchStart : patchStart+headerEnd]

		// Parse path="..." or file="..."
		path := ""
		if pIdx := strings.Index(header, "path="); pIdx != -1 {
			rawPath := header[pIdx+5:]
			rawPath = strings.TrimLeft(rawPath, " \"'")
			if endQ := strings.IndexAny(rawPath, "\"' ]"); endQ != -1 {
				path = rawPath[:endQ]
			} else {
				path = rawPath
			}
		} else if fIdx := strings.Index(header, "file="); fIdx != -1 {
			rawPath := header[fIdx+5:]
			rawPath = strings.TrimLeft(rawPath, " \"'")
			if endQ := strings.IndexAny(rawPath, "\"' ]"); endQ != -1 {
				path = rawPath[:endQ]
			} else {
				path = rawPath
			}
		}

		bodyStart := patchStart + headerEnd + 1
		// Look for [END_PATCH]
		bodyEnd := -1
		if endIdx := strings.Index(content[bodyStart:], "[END_PATCH]"); endIdx != -1 {
			bodyEnd = bodyStart + endIdx
			pos = bodyEnd + len("[END_PATCH]")
		} else {
			// Look for next [PROPOSE_PATCH:
			if nextIdx := strings.Index(content[bodyStart:], marker); nextIdx != -1 {
				bodyEnd = bodyStart + nextIdx
				pos = bodyEnd
			} else {
				bodyEnd = len(content)
				pos = bodyEnd
			}
		}

		body := strings.TrimSpace(content[bodyStart:bodyEnd])
		// Strip markdown code fences if present: ```lang ... ```
		if strings.HasPrefix(body, "```") {
			if firstLineEnd := strings.Index(body, "\n"); firstLineEnd != -1 {
				body = body[firstLineEnd+1:]
			} else {
				body = strings.TrimPrefix(body, "```")
			}
			if lastFence := strings.LastIndex(body, "```"); lastFence != -1 {
				body = body[:lastFence]
			}
			body = strings.TrimRight(body, " \t\r")
		}

		if path != "" && body != "" {
			patches = append(patches, GovernedPatch{
				Path:    path,
				Content: body,
			})
		}
	}
	return patches
}

func (s *OpenCodeSessionAdapter) applyGovernedPatches(content string) error {
	patches := extractGovernedPatches(content)
	if len(patches) == 0 {
		return nil
	}

	for _, patch := range patches {
		cleanPath := filepath.Clean(patch.Path)
		if strings.HasPrefix(cleanPath, "..") || filepath.IsAbs(cleanPath) {
			return fmt.Errorf("access denied: path outside workspace: %s", patch.Path)
		}

		// PolicyGovernor pre-invocation authorization
		if s.config.Governor != nil {
			if err := s.config.Governor.AuthorizeToolInvocation(s.config.AgentID, "governed_apply_patch"); err != nil {
				return fmt.Errorf("governance authorization denied for %s on %s: %w", s.config.AgentID, cleanPath, err)
			}
		}

		s.mu.Lock()
		s.toolCallCount++
		calls := s.toolCallCount
		maxCalls := s.config.MaxToolCalls
		if s.config.Limits != nil && s.config.Limits.MaxToolCalls > 0 && maxCalls == 0 {
			maxCalls = s.config.Limits.MaxToolCalls
		}
		s.mu.Unlock()

		if maxCalls > 0 && calls > maxCalls {
			return fmt.Errorf("%w: maximum %d calls allowed per session", ErrToolCallLimitExceeded, maxCalls)
		}

		tStart := time.Now()
		hasher := sha256.New()
		hasher.Write([]byte(patch.Content))
		payloadHash := fmt.Sprintf("%x", hasher.Sum(nil))
		intentID := fmt.Sprintf("intent-patch-%d-%s", time.Now().UnixNano(), payloadHash[:8])

		if s.config.IntentJournal != nil {
			_ = s.config.IntentJournal.RecordIntent(IntentRecord{
				IntentID:    intentID,
				ActivityID:  s.config.AgentID,
				Action:      "apply_patch",
				Target:      cleanPath,
				PayloadHash: payloadHash,
			})
		}

		if s.config.WorkspaceDir != "" {
			target := filepath.Join(s.config.WorkspaceDir, cleanPath)
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return fmt.Errorf("failed to create directory for patch %s: %w", cleanPath, err)
			}
			if err := os.WriteFile(target, []byte(patch.Content), 0644); err != nil {
				return fmt.Errorf("failed to write file for patch %s: %w", cleanPath, err)
			}
		}

		if s.config.IntentJournal != nil {
			_ = s.config.IntentJournal.RecordOutcome(intentID, "committed-"+cleanPath)
		}

		s.mu.Lock()
		s.toolDuration += time.Since(tStart)
		s.mu.Unlock()

		if s.config.OnRecordMetric != nil {
			s.config.OnRecordMetric("governed_apply_patch", 1)
		}
	}

	return nil
}
