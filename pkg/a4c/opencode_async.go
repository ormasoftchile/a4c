package a4c

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// OpenCode long-running execution lifecycle constants.
//
// These separate the transport timeout (per HTTP control request) from the
// overall activity deadline and from stall detection, so a healthy activity
// performing a long build/test does not fail merely because one HTTP request
// lasted longer than a transport budget. The previous implementation used a
// single http.Client{Timeout: 300s} for a synchronous blocking POST, which
// capped the entire turn and failed legitimate long-running activities.
const (
	// ocTransportTimeout bounds a single control-plane HTTP request (submit,
	// wait-slice, message fetch, abort). It is a connection/response budget,
	// NOT a cap on total activity duration.
	ocTransportTimeout = 60 * time.Second
	// ocWaitSlice is how long each /wait poll may block before we re-check
	// cancellation, the activity deadline, and progress.
	ocWaitSlice = 20 * time.Second
	// ocStallTimeout is the maximum time the turn may make no observable
	// progress (no new assistant message) before it is declared stalled.
	ocStallTimeout = 15 * time.Minute
	// ocDefaultActivityDeadline bounds a single Send (one agent turn) when the
	// caller supplies no shorter context deadline.
	ocDefaultActivityDeadline = 45 * time.Minute
)

// ocMessageResponse mirrors a single OpenCode assistant message, returned both
// by the synchronous POST /message and by GET /session/{id}/message.
type ocMessageResponse struct {
	Info struct {
		ID      string  `json:"id"`
		Role    string  `json:"role"`
		Cost    float64 `json:"cost"`
		ModelID string  `json:"modelID"`
		// Finish is set by the host only when the turn has completed (e.g.
		// "stop"). An in-progress assistant message has an empty Finish, so
		// this is the authoritative completion signal — an early partial
		// message must not be mistaken for a finished turn.
		Finish string `json:"finish"`
		Tokens struct {
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

// isComplete reports whether the assistant message represents the FINISHED
// turn. A turn produces one assistant message per tool-call round with
// finish="tool-calls", and a final message with finish="stop". Only the final
// "stop" message is complete; returning on a "tool-calls" message would abandon
// the turn mid-flight and capture only a partial (often empty) planning
// message instead of the real result.
func (m *ocMessageResponse) isComplete() bool {
	if m.Info.Finish == "stop" {
		return true
	}
	for i := len(m.Parts) - 1; i >= 0; i-- {
		if m.Parts[i].Type == "step-finish" && m.Parts[i].Reason == "stop" {
			return true
		}
	}
	return false
}

// ocMessageList is the GET /session/{id}/message response.
type ocMessageList []ocMessageResponse

// doControl issues one control-plane HTTP request bounded by a transport
// timeout derived from the parent context. It never relies on a client-level
// Timeout, so a long /wait slice is governed by its own context budget.
func (s *OpenCodeSessionAdapter) doControl(ctx context.Context, method, url string, body []byte, transportTimeout time.Duration) (*http.Response, error) {
	reqCtx, cancel := context.WithTimeout(ctx, transportTimeout)
	defer cancel()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, url, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return s.provider.httpClient.Do(req)
}

// abortSession best-effort cancels a running turn on the host.
func (s *OpenCodeSessionAdapter) abortSession(endpoint, sessionID string) {
	url := fmt.Sprintf("%s/session/%s/abort", endpoint, sessionID)
	resp, err := s.doControl(context.Background(), http.MethodPost, url, nil, 30*time.Second)
	if err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

// sendTurn executes one agent turn using OpenCode's asynchronous lifecycle:
// submit via prompt_async (short transport budget), then poll /wait in slices
// while observing cancellation, an overall activity deadline, and stall
// detection, and finally fetch the completed assistant message via GET
// /message. This guarantees a healthy long-running turn is not killed by a
// transport timeout, while still bounding total duration and detecting stalls.
func (s *OpenCodeSessionAdapter) sendTurn(ctx context.Context, sessionID, endpoint string, payload []byte, modelID string) (ocMessageResponse, error) {
	var empty ocMessageResponse

	// Bound the overall activity if the caller supplied no deadline.
	actCtx := ctx
	var cancelAct context.CancelFunc
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		actCtx, cancelAct = context.WithTimeout(ctx, ocDefaultActivityDeadline)
		defer cancelAct()
	}

	// 1. Submit the turn asynchronously (short transport budget).
	submitURL := fmt.Sprintf("%s/session/%s/prompt_async", endpoint, sessionID)
	subResp, err := s.doControl(actCtx, http.MethodPost, submitURL, payload, ocTransportTimeout)
	if err != nil {
		return empty, s.classifySendErr(modelID, fmt.Errorf("opencode: async submit: %w", err))
	}
	_, _ = io.Copy(io.Discard, subResp.Body)
	_ = subResp.Body.Close()
	if subResp.StatusCode == http.StatusNotFound || subResp.StatusCode == http.StatusMethodNotAllowed {
		// Host does not support prompt_async: fall back to the synchronous
		// POST /message (still bounded by the activity deadline via actCtx,
		// not by a client-level transport cap).
		return s.sendTurnSync(actCtx, endpoint, sessionID, payload, modelID)
	}
	if subResp.StatusCode != http.StatusNoContent && subResp.StatusCode != http.StatusOK {
		return empty, s.classifySendErr(modelID, fmt.Errorf("opencode: async submit status %d", subResp.StatusCode))
	}

	// 2. Poll for completion.
	msgURL := fmt.Sprintf("%s/session/%s/message", endpoint, sessionID)
	waitURL := fmt.Sprintf("%s/api/session/%s/wait", endpoint, sessionID)

	var lastMsgID string
	lastProgress := time.Now()
	for {
		if err := actCtx.Err(); err != nil {
			s.abortSession(endpoint, sessionID)
			if err == context.Canceled {
				return empty, fmt.Errorf("opencode: turn canceled")
			}
			return empty, fmt.Errorf("opencode: activity deadline exceeded: %w", err)
		}

		// Wait a bounded slice; it returns when the session is idle or the
		// slice elapses, whichever comes first.
		wResp, werr := s.doControl(actCtx, http.MethodPost, waitURL, nil, ocWaitSlice)
		if werr == nil {
			_, _ = io.Copy(io.Discard, wResp.Body)
			_ = wResp.Body.Close()
		}

		// Check for a completed assistant message. A turn is complete only
		// when the host marks it finished (info.finish / step-finish); an
		// early partial message with empty text must not end the turn.
		msgResp, ferr := s.fetchLastAssistant(actCtx, msgURL)
		if ferr == nil && msgResp.Info.ID != "" && msgResp.Info.Role == "assistant" && msgResp.isComplete() {
			if msgResp.Info.ID != lastMsgID {
				lastMsgID = msgResp.Info.ID
				lastProgress = time.Now()
			}
			return msgResp, nil
		}
		// Progress signal: a new assistant message appearing means the turn
		// is alive even if not yet complete.
		if ferr == nil && msgResp.Info.ID != "" && msgResp.Info.ID != lastMsgID {
			lastMsgID = msgResp.Info.ID
			lastProgress = time.Now()
		}

		if time.Since(lastProgress) > ocStallTimeout {
			s.abortSession(endpoint, sessionID)
			return empty, fmt.Errorf("opencode: turn stalled: no assistant progress for %v", ocStallTimeout)
		}
	}
}

// sendTurnSync is the fallback for hosts that do not support prompt_async. It
// performs the synchronous POST /message, bounded by the activity deadline via
// the provided context (never by a client-level transport cap).
func (s *OpenCodeSessionAdapter) sendTurnSync(ctx context.Context, endpoint, sessionID string, payload []byte, modelID string) (ocMessageResponse, error) {
	var empty ocMessageResponse
	url := fmt.Sprintf("%s/session/%s/message", endpoint, sessionID)
	// The sync call IS the long-running turn, so it is bounded by the remaining
	// activity deadline (not a short transport budget). If no deadline is set,
	// fall back to the default activity deadline.
	transportBudget := ocDefaultActivityDeadline
	if dl, ok := ctx.Deadline(); ok {
		if rem := time.Until(dl); rem > 0 {
			transportBudget = rem
		} else {
			return empty, fmt.Errorf("opencode: activity deadline exceeded before submit")
		}
	}
	resp, err := s.doControl(ctx, http.MethodPost, url, payload, transportBudget)
	if err != nil {
		return empty, s.classifySendErr(modelID, fmt.Errorf("opencode: send request: %w", err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var errData any
		_ = json.NewDecoder(resp.Body).Decode(&errData)
		rawErr := fmt.Sprintf("status %d: %v", resp.StatusCode, errData)
		return empty, s.classifySendErr(modelID, fmt.Errorf("opencode: message request %s", rawErr))
	}
	var msgResp ocMessageResponse
	if err := json.NewDecoder(resp.Body).Decode(&msgResp); err != nil {
		return empty, fmt.Errorf("opencode: decode message response: %w", err)
	}
	return msgResp, nil
}

// fetchLastAssistant retrieves the most recent assistant message, if any.
func (s *OpenCodeSessionAdapter) fetchLastAssistant(ctx context.Context, msgURL string) (ocMessageResponse, error) {
	var empty ocMessageResponse
	resp, err := s.doControl(ctx, http.MethodGet, msgURL, nil, ocTransportTimeout)
	if err != nil {
		return empty, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return empty, fmt.Errorf("opencode: message list status %d", resp.StatusCode)
	}
	var list ocMessageList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return empty, err
	}
	for i := len(list) - 1; i >= 0; i-- {
		if list[i].Info.Role == "assistant" {
			return list[i], nil
		}
	}
	return empty, nil
}

// classifySendErr maps transport failures to provider errors, preserving the
// distinction between a transport failure and a host execution failure.
func (s *OpenCodeSessionAdapter) classifySendErr(modelID string, err error) error {
	if pe := ClassifyProviderError("opencode", modelID, err, ""); pe != nil {
		return pe
	}
	return err
}

// TransportCapSeconds reports the provider's client-level HTTP Timeout in
// seconds (0 means no global cap). It exists so tests can assert that no
// client-level timeout can prematurely kill a long-running turn — the root
// cause of the historical 300-second activity failure.
func (p *OpenCodeProvider) TransportCapSeconds() float64 {
	if p == nil || p.httpClient == nil {
		return 0
	}
	return p.httpClient.Timeout.Seconds()
}

// extractContent concatenates text parts of a message.
func (m *ocMessageResponse) extractContent() string {
	var sb strings.Builder
	for _, part := range m.Parts {
		if part.Type == "text" && part.Text != "" {
			sb.WriteString(part.Text)
		}
	}
	return sb.String()
}

// extractReviewVerdict is the structured, host-independent verdict protocol for
// OpenCode reviewers. OpenCode has no custom-tool registration API, so the
// governed verdict is carried in the reviewer's structured output and parsed
// deterministically here — never by a loose prose heuristic alone. It resolves,
// in priority order: an explicit [REVIEW_*] directive, a structured JSON
// {"verdict":...} object, then conservative standalone verdict words. It returns
// ok=false when no verdict can be established (a missing verdict is NOT a
// rejection).
func extractReviewVerdict(content string) (ReviewResult, bool) {
	rev := ReviewResult{Timestamp: time.Now()}
	// 1. Explicit directives (highest priority; also the Copilot/Antigravity protocol).
	if i := strings.Index(content, "[REVIEW_BLOCKED:"); i >= 0 {
		s := content[i+len("[REVIEW_BLOCKED:"):]
		if e := strings.Index(s, "]"); e >= 0 {
			rev.Verdict = ReviewVerdictVerificationBlocked
			rev.Approved = false
			rev.Comments = strings.TrimSpace(s[:e])
			return rev, true
		}
	}
	if i := strings.Index(content, "[REVIEW_REJECTED:"); i >= 0 {
		s := content[i+len("[REVIEW_REJECTED:"):]
		if e := strings.Index(s, "]"); e >= 0 {
			rev.Verdict = ReviewVerdictRejected
			rev.Approved = false
			rev.Comments = strings.TrimSpace(s[:e])
			return rev, true
		}
	}
	if strings.Contains(content, "[REVIEW_APPROVED]") {
		rev.Verdict = ReviewVerdictApproved
		rev.Approved = true
		rev.Comments = "Approved via structured verdict directive"
		return rev, true
	}
	// 2. Structured JSON verdict object.
	if i := strings.Index(content, "\"verdict\""); i >= 0 {
		// Extract the enclosing JSON object.
		start := strings.LastIndex(content[:i], "{")
		end := strings.Index(content[i:], "}")
		if start >= 0 && end >= 0 {
			obj := content[start : i+end+1]
			var parsed struct {
				Verdict   string   `json:"verdict"`
				Comments  string   `json:"comments"`
				Blockers  []string `json:"blockers"`
				Approved  bool     `json:"approved"`
			}
			if json.Unmarshal([]byte(obj), &parsed) == nil {
				switch strings.ToLower(strings.TrimSpace(parsed.Verdict)) {
				case "approved", "approve":
					rev.Verdict = ReviewVerdictApproved
					rev.Approved = true
					rev.Comments = parsed.Comments
					rev.Blockers = parsed.Blockers
					return rev, true
				case "rejected", "reject", "changes_required":
					rev.Verdict = ReviewVerdictRejected
					rev.Approved = false
					rev.Comments = parsed.Comments
					rev.Blockers = parsed.Blockers
					return rev, true
				case "verification_blocked", "blocked":
					rev.Verdict = ReviewVerdictVerificationBlocked
					rev.Approved = false
					rev.Comments = parsed.Comments
					return rev, true
				}
			}
		}
	}
	// 3. Conservative standalone verdict-word signals (last resort; only clear
	// review conclusions, matched as whole words to avoid prose false-positives).
	lower := strings.ToLower(content)
	verdictWord := func(word string) bool {
		return regexp.MustCompile(`\b` + regexp.QuoteMeta(word) + `\b`).MatchString(lower)
	}
	if verdictWord("approved") || verdictWord("lgtm") || verdictWord("ship it") {
		rev.Verdict = ReviewVerdictApproved
		rev.Approved = true
		rev.Comments = "Approved (structured verdict)"
		return rev, true
	}
	// "approve"/"reject"/"blocked" only count as a verdict when clearly framed
	// as the review conclusion (e.g. "verdict: approve", "I approve", "approve.").
	if m := regexp.MustCompile(`(?i)(verdict|review|approval)\s*[:\-]?\s*(approve|approved)\b`).FindStringSubmatch(content); m != nil {
		rev.Verdict = ReviewVerdictApproved
		rev.Approved = true
		rev.Comments = "Approved (structured verdict)"
		return rev, true
	}
	if m := regexp.MustCompile(`(?i)\bI\s+(approve|approve this)\b`).FindStringSubmatch(content); m != nil {
		rev.Verdict = ReviewVerdictApproved
		rev.Approved = true
		rev.Comments = "Approved (structured verdict)"
		return rev, true
	}
	if verdictWord("rejected") || verdictWord("reject") {
		rev.Verdict = ReviewVerdictRejected
		rev.Approved = false
		rev.Comments = "Rejected (structured verdict)"
		return rev, true
	}
	return rev, false
}
