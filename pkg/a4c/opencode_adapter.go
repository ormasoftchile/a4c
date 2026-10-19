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
	password     string
}

// NewOpenCodeProvider creates an OpenCode provider pointing to an endpoint.
func NewOpenCodeProvider(endpoint, model string, mgr *OpenCodeServerManager) *OpenCodeProvider {
	// An explicit endpoint wins; otherwise discover the running shared service
	// rather than assuming a fixed port, which on a real machine is often some
	// other process entirely.
	discoveredPassword := ""
	if endpoint == "" {
		if url, password, ok := DiscoverOpenCodeService(); ok {
			endpoint, discoveredPassword = url, password
		} else {
			endpoint = "http://127.0.0.1:4096"
		}
	}
	endpoint = strings.TrimRight(endpoint, "/")

	if model == "" {
		model = "opencode-go/mimo-v2.5"
	}

	// The v2 server requires HTTP Basic auth. Precedence: a managed server pins
	// its own password; otherwise the registration's, then the environment.
	password := os.Getenv("OPENCODE_SERVER_PASSWORD")
	if discoveredPassword != "" && password == "" {
		password = discoveredPassword
	}
	if mgr != nil && mgr.Password() != "" {
		password = mgr.Password()
	}

	return &OpenCodeProvider{
		endpoint:     endpoint,
		defaultModel: model,
		serverMgr:    mgr,
		password:     password,
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
	req, err := http.NewRequest(http.MethodGet, p.endpoint+"/api/project", nil)
	if err != nil {
		return false
	}
	setBasicAuth(req, p.password)
	resp, err := probeClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 500
}

// fetchServerVersion queries GET /api/info for the running server version.
func (p *OpenCodeProvider) fetchServerVersion() string {
	client := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequest(http.MethodGet, p.endpoint+"/api/info", nil)
	if err != nil {
		return "unknown"
	}
	setBasicAuth(req, p.password)
	resp, err := client.Do(req)
	if err != nil {
		return "unknown"
	}
	defer resp.Body.Close()
	var info struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil || info.Version == "" {
		return "unknown"
	}
	return info.Version
}

// HostCapabilities returns the explicit discoverable technical capabilities of OpenCode.
func (p *OpenCodeProvider) HostCapabilities() HostCapabilities {
	reachable := p.isServerReachable()
	version := "unknown"
	if reachable {
		version = p.fetchServerVersion()
	}
	caps := HostCapabilities{
		HostID:                "opencode",
		DisplayName:           "OpenCode Server",
		Version:               version,
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
			"Requires a running OpenCode v2 server (opencode serve or the shared background service)",
			"Host-native subagents are not driven by the headless worker",
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
		// Create session on the OpenCode v2 server. The permission ruleset and
		// the workspace location are session-scoped in v2 and travel in the
		// create body; unknown keys are rejected by the v2 schema.
		reqBody := map[string]any{
			"title": fmt.Sprintf("A4C Activity %s (%s)", config.AgentID, time.Now().Format(time.RFC3339)),
			"model": map[string]string{
				"providerID": providerID,
				"id":         modelID,
			},
			"permissions": permRules,
		}
		if config.WorkspaceDir != "" {
			reqBody["location"] = map[string]string{"directory": config.WorkspaceDir}
		}

		bodyBytes, err := json.Marshal(reqBody)
		if err != nil {
			return nil, err
		}

		u, err := url.Parse(p.endpoint + "/api/session")
		if err != nil {
			return nil, err
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
		setBasicAuth(httpReq, p.password)

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
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
			return nil, fmt.Errorf("opencode: decode session response: %w", err)
		}
		if created.Data.ID == "" {
			return nil, errors.New("opencode: session create returned no session id")
		}
		sessionID = created.Data.ID
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

// buildPermissionRules translates A4C's governance intent into an OpenCode v2
// permission ruleset (action/resource/effect). Rules are last-match-wins, so
// the catch-all comes first and specific rules follow. The session-scoped
// ruleset is the v2 enforcement mechanism for review isolation — v2 rejects
// unknown keys on session creation and has no per-message tools override.
//
// A headless session can never answer an "ask" approval (it would deadlock
// the turn waiting for a human who is not attached), so every rule resolves
// to allow or deny: the catch-all guarantees unlisted actions (MCP tools, new
// host actions) never fall through to ask.
func (p *OpenCodeProvider) buildPermissionRules(config AgentConfig) []map[string]string {
	if !isMutatingSpecialist(config.AgentID) {
		// Non-mutating roles (lead, reviewer): strictly read-only session.
		return []map[string]string{
			{"action": "*", "resource": "*", "effect": "deny"},
			{"action": "read", "resource": "*", "effect": "allow"},
			{"action": "glob", "resource": "*", "effect": "allow"},
			{"action": "grep", "resource": "*", "effect": "allow"},
			{"action": "read", "resource": "*.env", "effect": "deny"},
			{"action": "read", "resource": "*.env.*", "effect": "deny"},
			{"action": "shell", "resource": "*", "effect": "deny"},
			{"action": "subagent", "resource": "*", "effect": "deny"},
			{"action": "edit", "resource": "*", "effect": "deny"},
			{"action": "webfetch", "resource": "*", "effect": "deny"},
			{"action": "websearch", "resource": "*", "effect": "deny"},
			{"action": "question", "resource": "*", "effect": "deny"},
			{"action": "skill", "resource": "*", "effect": "deny"},
		}
	}

	// Specialists and implementers: allow native tools inside the isolated
	// worktree. Sensitive env-file reads stay denied (last match wins, so the
	// read catch-all precedes the env denies).
	return []map[string]string{
		{"action": "*", "resource": "*", "effect": "allow"},
		{"action": "subagent", "resource": "*", "effect": "deny"},
		{"action": "webfetch", "resource": "*", "effect": "deny"},
		{"action": "websearch", "resource": "*", "effect": "deny"},
		{"action": "question", "resource": "*", "effect": "deny"},
		{"action": "skill", "resource": "*", "effect": "deny"},
		{"action": "edit", "resource": "*", "effect": "allow"},
		{"action": "shell", "resource": "*", "effect": "allow"},
		{"action": "read", "resource": "*", "effect": "allow"},
		{"action": "glob", "resource": "*", "effect": "allow"},
		{"action": "grep", "resource": "*", "effect": "allow"},
		{"action": "read", "resource": "*.env", "effect": "deny"},
		{"action": "read", "resource": "*.env.*", "effect": "deny"},
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
	req, err := http.NewRequestWithContext(reqCtx, "POST", fmt.Sprintf("%s/api/session/%s/interrupt", endpoint, sessionID), nil)
	if err != nil {
		return err
	}
	setBasicAuth(req, s.provider.password)

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
	modelID := s.modelID
	s.mu.Unlock()

	isReviewer := strings.EqualFold(s.config.AgentID, "reviewer") ||
		strings.EqualFold(s.config.AgentID, "charlie") ||
		strings.Contains(strings.ToLower(s.config.AgentID), "review")

	// v2 sends a text-only prompt; tool access, model, and workspace are all
	// session-scoped (set at session creation). The charter, if any, is folded
	// into the prompt text because v2 prompts have no per-message system field.
	promptText := prompt
	if s.config.Charter != "" {
		promptText = s.config.Charter + "\n\n" + promptText
	}
	if isReviewer {
		promptText += "\n\n[INSTRUCTION]: Inspect the changes strictly read-only. If approving, include [REVIEW_APPROVED] in your response. If you find a concrete implementation defect, include [REVIEW_REJECTED: reason]. If the implementation looks correct but you cannot verify because evidence or capabilities are missing, include [REVIEW_BLOCKED: reason] -- this is NOT a rejection and will not trigger code changes."
	} else if strings.EqualFold(s.config.AgentID, "lead") || strings.EqualFold(s.config.AgentID, "alice") {
		promptText += "\n\n[INSTRUCTION]: Delegate tasks using a JSON block: ```json\n{\"proposals\": [{\"proposal_id\": \"...\", \"proposed_agent_id\": \"coder\", \"objective\": \"...\", \"budget_tokens\": 30000}]}\n```\nPropose budgets that cover full expected context overhead; the runtime raises proposals below the viable minimum and rejects proposals the parent cannot afford."
	}

	reqPayload := map[string]any{
		"text": promptText,
	}

	payloadBytes, err := json.Marshal(reqPayload)
	if err != nil {
		return nil, err
	}

	// Execute the turn via OpenCode's v2 lifecycle (prompt submit + /wait
	// polling + message fetch). This separates the transport timeout from the
	// overall activity deadline so a healthy long-running build is not killed
	// by an HTTP timeout, while still bounding total duration.
	tStart := time.Now()
	msgResp, turnErr := s.sendTurn(ctx, sessionID, endpoint, payloadBytes, modelID)
	waitDur := time.Since(tStart)

	s.mu.Lock()
	s.providerWaitDuration += waitDur
	s.mu.Unlock()

	if turnErr != nil {
		return nil, turnErr
	}

	// Extract text content from the assistant message's content items.
	content := msgResp.extractContent()

	s.mu.Lock()
	in := msgResp.Tokens.Input
	out := msgResp.Tokens.Output
	reasoning := msgResp.Tokens.Reasoning
	cached := msgResp.Tokens.Cache.Read
	// v2 reports no aggregate token total; the turn total is input + output.
	totalTok := in + out

	s.usage.PromptTokens += in
	s.usage.CompletionTokens += out
	s.usage.TotalTokens += totalTok
	s.usage.ReasoningTokens += reasoning
	s.usage.CachedTokens += cached
	s.usage.ReportedCostUSD += msgResp.Cost

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
				Target:      filepath.ToSlash(cleanPath),
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
			_ = s.config.IntentJournal.RecordOutcome(intentID, "committed-"+filepath.ToSlash(cleanPath))
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
