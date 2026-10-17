package a4c

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

// OpenCode v2 long-running execution lifecycle constants.
//
// These separate the transport timeout (per HTTP control request) from the
// overall activity deadline and from stall detection, so a healthy activity
// performing a long build/test does not fail merely because one HTTP request
// lasted longer than a transport budget. The previous implementation used a
// single http.Client{Timeout: 300s} for a synchronous blocking POST, which
// capped the entire turn and failed legitimate long-running activities.
const (
	// ocTransportTimeout bounds a single control-plane HTTP request (submit,
	// wait-slice, message fetch, interrupt). It is a connection/response
	// budget, NOT a cap on total activity duration.
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
	// ocFallbackPollInterval is the sleep between message polls when the
	// experimental /wait endpoint is unavailable on the server.
	ocFallbackPollInterval = 2 * time.Second
)

// ocMessageResponse mirrors a single OpenCode v2 assistant message returned by
// GET /api/session/{id}/message. v2 messages are a discriminated union keyed by
// Type; only "assistant" messages carry content, a finish state, and tokens.
type ocMessageResponse struct {
	ID     string  `json:"id"`
	Type   string  `json:"type"`
	Finish string  `json:"finish"`
	Cost   float64 `json:"cost"`
	Tokens struct {
		Input     int `json:"input"`
		Output    int `json:"output"`
		Reasoning int `json:"reasoning"`
		Cache     struct {
			Read  int `json:"read"`
			Write int `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Error *struct {
		Name string `json:"name"`
	} `json:"error,omitempty"`
}

// isComplete reports whether the assistant message represents a TERMINAL turn
// state. A streaming or in-progress message has an empty Finish; a tool-round
// message has finish="tool-calls". Only finish="stop" (success) and
// finish="error" (host-side failure) are terminal; returning on a
// "tool-calls" message would abandon the turn mid-flight.
func (m *ocMessageResponse) isComplete() bool {
	if m.Type != "assistant" {
		return false
	}
	return m.Finish == "stop" || m.Finish == "error"
}

// ocMessageList is the GET /api/session/{id}/message response envelope.
type ocMessageList struct {
	Data []ocMessageResponse `json:"data"`
}

// doControl issues one control-plane HTTP request bounded by a transport
// timeout derived from the parent context, returning the status code and the
// fully-read body. The request context must not be canceled until the body
// has been consumed, so the read happens inside doControl — cancelling at
// return (the previous pattern) raced body reads against the context and
// failed intermittently with "context canceled" on real servers.
func (s *OpenCodeSessionAdapter) doControl(ctx context.Context, method, url string, body []byte, transportTimeout time.Duration) (int, []byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, transportTimeout)
	defer cancel()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, url, rdr)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	setBasicAuth(req, s.provider.password)
	resp, err := s.provider.httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, data, nil
}

// interruptSession best-effort cancels a running turn on the host.
func (s *OpenCodeSessionAdapter) interruptSession(endpoint, sessionID string) {
	url := fmt.Sprintf("%s/api/session/%s/interrupt", endpoint, sessionID)
	_, _, _ = s.doControl(context.Background(), http.MethodPost, url, nil, 30*time.Second)
}

// sendTurn executes one agent turn using the OpenCode v2 lifecycle: submit via
// POST /api/session/{id}/prompt (short transport budget), then poll the
// experimental /wait endpoint in slices while observing cancellation, an
// overall activity deadline, and stall detection, and finally fetch the
// completed assistant message via GET /api/session/{id}/message. This
// guarantees a healthy long-running turn is not killed by a transport timeout,
// while still bounding total duration and detecting stalls.
func (s *OpenCodeSessionAdapter) sendTurn(ctx context.Context, sessionID, endpoint string, payload []byte, modelID string) (ocMessageResponse, error) {
	var empty ocMessageResponse

	// Bound the overall activity if the caller supplied no deadline.
	actCtx := ctx
	var cancelAct context.CancelFunc
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		actCtx, cancelAct = context.WithTimeout(ctx, ocDefaultActivityDeadline)
		defer cancelAct()
	}

	// 1. Submit the prompt (short transport budget; the turn itself runs
	// asynchronously on the server and is observed via /wait + /message).
	submitURL := fmt.Sprintf("%s/api/session/%s/prompt", endpoint, sessionID)
	subStatus, _, err := s.doControl(actCtx, http.MethodPost, submitURL, payload, ocTransportTimeout)
	if err != nil {
		return empty, s.classifySendErr(modelID, fmt.Errorf("opencode: prompt submit: %w", err))
	}
	if os.Getenv("A4C_OC_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[a4c-oc-debug] prompt submit status=%d payloadBytes=%d\n", subStatus, len(payload))
	}
	if subStatus == http.StatusConflict {
		return empty, s.classifySendErr(modelID, errors.New("opencode: session busy (a turn is already running)"))
	}
	if subStatus != http.StatusOK && subStatus != http.StatusNoContent {
		return empty, s.classifySendErr(modelID, fmt.Errorf("opencode: prompt submit status %d", subStatus))
	}

	// 2. Poll for completion. The message list is requested in ascending
	// order and filtered to assistant messages so the end-scan below always
	// sees the newest assistant message regardless of server default order.
	msgURL := fmt.Sprintf("%s/api/session/%s/message?order=asc&type=assistant", endpoint, sessionID)
	waitURL := fmt.Sprintf("%s/api/experimental/session/%s/wait", endpoint, sessionID)
	waitSupported := true
	ocDebug := os.Getenv("A4C_OC_DEBUG") != ""

	var lastMsgID string
	lastProgress := time.Now()
	for {
		if err := actCtx.Err(); err != nil {
			s.interruptSession(endpoint, sessionID)
			if ocDebug {
				fmt.Fprintf(os.Stderr, "[a4c-oc-debug] actCtx done: %v (lastMsgID=%q)\n", err, lastMsgID)
			}
			if err == context.Canceled {
				return empty, fmt.Errorf("opencode: turn canceled")
			}
			return empty, fmt.Errorf("opencode: activity deadline exceeded: %w", err)
		}

		if waitSupported {
			// Wait a bounded slice; it returns when the session is idle or the
			// slice elapses, whichever comes first.
			wStatus, _, werr := s.doControl(actCtx, http.MethodPost, waitURL, nil, ocWaitSlice)
			if werr == nil {
				if ocDebug {
					fmt.Fprintf(os.Stderr, "[a4c-oc-debug] wait status=%d\n", wStatus)
				}
				if wStatus == http.StatusNotFound || wStatus == http.StatusMethodNotAllowed {
					// Experimental /wait endpoint unavailable on this server:
					// fall back to bounded sleep-polling (still bounded by
					// actCtx).
					waitSupported = false
				}
			}
		} else {
			select {
			case <-actCtx.Done():
				// Handled at loop top.
			case <-time.After(ocFallbackPollInterval):
			}
		}

		// Check for a completed assistant message. A turn is complete only
		// when the host marks it finished (finish=stop/error); an early
		// streaming message with empty text must not end the turn.
		msgResp, ferr := s.fetchLastAssistant(actCtx, msgURL)
		if os.Getenv("A4C_OC_DEBUG") != "" {
			fmt.Fprintf(os.Stderr, "[a4c-oc-debug] fetch err=%v id=%q type=%q finish=%q textLen=%d\n", ferr, msgResp.ID, msgResp.Type, msgResp.Finish, len(msgResp.extractContent()))
		}
		if ferr == nil && msgResp.ID != "" && msgResp.isComplete() {
			if msgResp.Finish == "error" {
				s.interruptSession(endpoint, sessionID)
				name := "unknown"
				if msgResp.Error != nil && msgResp.Error.Name != "" {
					name = msgResp.Error.Name
				}
				return empty, fmt.Errorf("opencode: turn failed on host: %s", name)
			}
			if msgResp.ID != lastMsgID {
				lastMsgID = msgResp.ID
				lastProgress = time.Now()
			}
			return msgResp, nil
		}
		// Progress signal: a new assistant message appearing means the turn
		// is alive even if not yet complete.
		if ferr == nil && msgResp.ID != "" && msgResp.ID != lastMsgID {
			lastMsgID = msgResp.ID
			lastProgress = time.Now()
		}

		if time.Since(lastProgress) > ocStallTimeout {
			s.interruptSession(endpoint, sessionID)
			return empty, fmt.Errorf("opencode: turn stalled: no assistant progress for %v", ocStallTimeout)
		}
	}
}

// fetchLastAssistant retrieves the most recent assistant message, if any.
func (s *OpenCodeSessionAdapter) fetchLastAssistant(ctx context.Context, msgURL string) (ocMessageResponse, error) {
	var empty ocMessageResponse
	status, data, err := s.doControl(ctx, http.MethodGet, msgURL, nil, ocTransportTimeout)
	if err != nil {
		return empty, err
	}
	if status != http.StatusOK {
		return empty, fmt.Errorf("opencode: message list status %d", status)
	}
	var list ocMessageList
	if err := json.Unmarshal(data, &list); err != nil {
		return empty, err
	}
	for i := len(list.Data) - 1; i >= 0; i-- {
		if list.Data[i].Type == "assistant" {
			return list.Data[i], nil
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

// extractContent concatenates text content items of an assistant message.
func (m *ocMessageResponse) extractContent() string {
	var sb strings.Builder
	for _, part := range m.Content {
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
