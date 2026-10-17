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
		// No client-level Timeout: it would cap EVERY request, including a
		// legitimate long-running /wait. Per-request transport budgets are
		// applied via context in doControl instead, and the overall turn is
		// bounded by the activity deadline in sendTurn.
		httpClient: &http.Client{},
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

		// Bound session creation with a transport timeout (the shared client
		// has no global Timeout, so per-request contexts govern control calls).
		reqCtx, cancelReq := context.WithTimeout(ctx, ocTransportTimeout)
		httpReq, err := http.NewRequestWithContext(reqCtx, "POST", u.String(), bytes.NewReader(bodyBytes))
		if err != nil {
			cancelReq()
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")

		resp, err := p.httpClient.Do(httpReq)
		cancelReq()
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

	reqCtx, cancelReq := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelReq()
	req, err := http.NewRequestWithContext(reqCtx, "POST", fmt.Sprintf("%s/session/%s/abort", endpoint, sessionID), nil)
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
		promptText += "\n\n[INSTRUCTION]: Inspect the changes strictly read-only. If approving, include [REVIEW_APPROVED] in your response. If you find a concrete implementation defect, include [REVIEW_REJECTED: reason]. If the implementation looks correct but you cannot verify because evidence or capabilities are missing, include [REVIEW_BLOCKED: reason] -- this is NOT a rejection and will not trigger code changes."
	} else if strings.EqualFold(s.config.AgentID, "lead") || strings.EqualFold(s.config.AgentID, "alice") {
		promptText += "\n\n[INSTRUCTION]: Delegate tasks using a JSON block: ```json\n{\"proposals\": [{\"proposal_id\": \"...\", \"proposed_agent_id\": \"coder\", \"objective\": \"...\", \"budget_tokens\": 30000}]}\n```\nPropose budgets that cover full expected context overhead; the runtime raises proposals below the viable minimum and rejects proposals the parent cannot afford."
	}

	// A4C Knowledge: this host's MCP transport cannot bind a tool call to an
	// activity by itself, so the coordinator-issued execution-context token is
	// injected into the session and the model relays it as context_token. This
	// is weaker than Copilot's per-session MCP env binding and is documented as
	// such; the HMAC still authenticates the token and a completed activity's
	// token fails its StatusClaimed check.
	if s.config.KnowledgeToken != "" {
		promptText += "\n\n[KNOWLEDGE]: The A4C Knowledge MCP server exposes knowledge_record and knowledge_query. Record significant engineering understanding (hypothesis, finding, decision, experiment result) via knowledge_record, passing this execution-context token as context_token: " + s.config.KnowledgeToken + "\nRetrieve prior knowledge via knowledge_query. Do not record routine implementation steps."
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

	// Execute the turn via OpenCode's asynchronous lifecycle (prompt_async +
	// /wait polling + message fetch). This separates the transport timeout
	// from the overall activity deadline so a healthy long-running build is
	// not killed by an HTTP timeout, while still bounding total duration.
	tStart := time.Now()
	msgResp, turnErr := s.sendTurn(ctx, sessionID, endpoint, payloadBytes, modelID)
	waitDur := time.Since(tStart)

	s.mu.Lock()
	s.providerWaitDuration += waitDur
	s.mu.Unlock()

	if turnErr != nil {
		return nil, turnErr
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
	// Structured, host-independent review verdict protocol. OpenCode has no
	// custom-tool registration API, so the governed verdict is carried in the
	// reviewer's structured output and parsed deterministically by
	// extractReviewVerdict (directives, then JSON, then conservative verdict
	// words). A missing verdict is never treated as a rejection.
	if s.config.OnSubmitReview != nil {
		if rev, ok := extractReviewVerdict(content); ok {
			rev.ActivityID = s.config.AgentID
			rev.ReviewerID = s.config.AgentID
			s.config.OnSubmitReview(rev)
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
