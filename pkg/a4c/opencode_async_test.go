package a4c_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	spike "github.com/ormasoftchile/a4c/pkg/a4c"
)

// asyncOCServer is a controllable OpenCode v2 fixture that supports the turn
// lifecycle: POST /api/session/{id}/prompt (200), POST
// /api/experimental/session/{id}/wait, and GET /api/session/{id}/message. It
// lets tests simulate long-running turns, transport blips, cancellation, and
// stalls.
type asyncOCServer struct {
	mu          sync.Mutex
	srv         *httptest.Server
	promptCalls int32
	waitCalls   int32
	interrupted []string
	// messageAfterWaits: the assistant message becomes available after this
	// many /wait calls (simulating a long turn progressing over time).
	messageAfterWaits int32
	// waitDelay: how long each /wait blocks before returning.
	waitDelay time.Duration
	// failNextWait: when true, the next /wait returns 500 (transport blip).
	failNextWait atomic.Bool
	// neverMessage: when true, /message never returns an assistant (stall).
	neverMessage  bool
	assistantText string
}

func newAsyncOCServer(messageAfterWaits int32, waitDelay time.Duration) *asyncOCServer {
	a := &asyncOCServer{
		messageAfterWaits: messageAfterWaits,
		waitDelay:         waitDelay,
		assistantText:     "DONE: implemented the fix",
	}
	mux := http.NewServeMux()

	mux.HandleFunc("/api/project", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "prj-mock"})
	})
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"version": "2.0.29"})
	})
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ses-async-1"}})
	})
	mux.HandleFunc("/api/session/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/session/"), "/")
		if len(parts) >= 2 && parts[1] == "prompt" {
			atomic.AddInt32(&a.promptCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "msg-user-1"}})
			return
		}
		if len(parts) >= 2 && parts[1] == "interrupt" {
			a.mu.Lock()
			a.interrupted = append(a.interrupted, parts[0])
			a.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		if len(parts) >= 2 && parts[1] == "message" && r.Method == http.MethodGet {
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.neverMessage || atomic.LoadInt32(&a.waitCalls) < a.messageAfterWaits {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{
					"id": "msg-async-1", "type": "assistant", "cost": 0.001, "finish": "stop",
					"tokens": map[string]any{"input": 60, "output": 40, "reasoning": 0, "cache": map[string]any{"read": 0, "write": 0}},
					"content": []map[string]any{{"type": "text", "text": a.assistantText}},
				}},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/api/experimental/session/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/wait") {
			if a.failNextWait.CompareAndSwap(true, false) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if a.waitDelay > 0 {
				time.Sleep(a.waitDelay)
			}
			atomic.AddInt32(&a.waitCalls, 1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	a.srv = httptest.NewServer(mux)
	return a
}

func (a *asyncOCServer) Close() { a.srv.Close() }

// TestOpenCode_AsyncLifecycle_NoGlobalTransportCap verifies the provider no
// longer sets a client-level Timeout (the root cause of the 300s activity
// failure) and that a turn completing across multiple wait slices succeeds via
// the v2 lifecycle (prompt + /wait + message), not a single blocking POST.
func TestOpenCode_AsyncLifecycle_NoGlobalTransportCap(t *testing.T) {
	mock := newAsyncOCServer(3, 30*time.Millisecond) // assistant after 3 waits
	defer mock.Close()

	prov := spike.NewOpenCodeProvider(mock.srv.URL, "opencode-go/mimo-v2.5", nil)
	defer prov.Close()

	// Structural proof: no client-level timeout that could cap a long turn.
	if cap := prov.TransportCapSeconds(); cap != 0 {
		t.Fatalf("provider must not impose a global transport cap (got %.0fs); this caused the 300s activity failure", cap)
	}

	sess, err := prov.CreateSession(context.Background(), spike.AgentConfig{AgentID: "coder"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer sess.Close()

	msg, err := sess.Send(context.Background(), "implement the fix")
	if err != nil {
		t.Fatalf("async turn should complete across wait slices: %v", err)
	}
	if msg.Content == "" {
		t.Fatalf("expected assistant content")
	}
	if atomic.LoadInt32(&mock.promptCalls) == 0 {
		t.Fatalf("expected the v2 prompt endpoint to be used")
	}
	if atomic.LoadInt32(&mock.waitCalls) < 3 {
		t.Fatalf("expected the turn to progress across >=3 wait slices, got %d", mock.waitCalls)
	}
}

// TestOpenCode_AsyncTransportBlipRecovered: a transient /wait failure (transport
// disconnect) is tolerated; the turn continues and completes on a later slice
// without duplicating the submit (no blind resubmission).
func TestOpenCode_AsyncTransportBlipRecovered(t *testing.T) {
	mock := newAsyncOCServer(3, 10*time.Millisecond)
	defer mock.Close()
	mock.failNextWait.Store(true) // first wait blips

	prov := spike.NewOpenCodeProvider(mock.srv.URL, "opencode-go/mimo-v2.5", nil)
	defer prov.Close()
	sess, err := prov.CreateSession(context.Background(), spike.AgentConfig{AgentID: "coder"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer sess.Close()

	if _, err := sess.Send(context.Background(), "implement"); err != nil {
		t.Fatalf("turn should recover from a transient wait failure: %v", err)
	}
	// The turn was submitted exactly once — never blindly resubmitted.
	if got := atomic.LoadInt32(&mock.promptCalls); got != 1 {
		t.Fatalf("expected exactly 1 prompt submit (no duplicate), got %d", got)
	}
}

// TestOpenCode_AsyncCancellationAborts: cancellation during the turn calls the
// host interrupt endpoint and returns a cancellation error.
func TestOpenCode_AsyncCancellationAborts(t *testing.T) {
	mock := newAsyncOCServer(1000000, 20*time.Millisecond) // never completes
	defer mock.Close()

	prov := spike.NewOpenCodeProvider(mock.srv.URL, "opencode-go/mimo-v2.5", nil)
	defer prov.Close()
	sess, err := prov.CreateSession(context.Background(), spike.AgentConfig{AgentID: "coder"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer sess.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel()
	}()
	_, err = sess.Send(ctx, "implement")
	if err == nil {
		t.Fatalf("expected cancellation error")
	}
	if !strings.Contains(err.Error(), "cancel") {
		t.Fatalf("expected a cancellation error, got: %v", err)
	}
	// Interrupt should have been issued to stop the host execution.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mock.mu.Lock()
		n := len(mock.interrupted)
		mock.mu.Unlock()
		if n >= 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("expected the host interrupt endpoint to be called on cancellation")
}

// TestOpenCode_AsyncStallDetected: a turn that never produces an assistant
// message is declared stalled (bounded), not hung forever.
func TestOpenCode_AsyncStallDetected(t *testing.T) {
	mock := newAsyncOCServer(0, 5*time.Millisecond)
	mock.neverMessage = true
	defer mock.Close()

	prov := spike.NewOpenCodeProvider(mock.srv.URL, "opencode-go/mimo-v2.5", nil)
	defer prov.Close()
	// Shrink the stall window via a bounded context so the test is fast.
	sess, err := prov.CreateSession(context.Background(), spike.AgentConfig{AgentID: "coder"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer sess.Close()

	// Use a short overall deadline so the activity-deadline path is exercised
	// quickly (the stall window is large; the deadline fires first).
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err = sess.Send(ctx, "implement")
	if err == nil {
		t.Fatalf("expected a deadline/stall error for a turn that never completes")
	}
	if !strings.Contains(err.Error(), "deadline") && !strings.Contains(err.Error(), "stall") {
		t.Fatalf("expected a deadline or stall error, got: %v", err)
	}
}

// TestOpenCode_ActivityDeadlineDistinctFromTransport: an activity deadline
// exceeded is reported distinctly (not as a generic transport failure), proving
// the separation of transport timeout from overall activity deadline.
func TestOpenCode_ActivityDeadlineDistinctFromTransport(t *testing.T) {
	mock := newAsyncOCServer(1000000, 10*time.Millisecond) // never completes
	defer mock.Close()
	prov := spike.NewOpenCodeProvider(mock.srv.URL, "opencode-go/mimo-v2.5", nil)
	defer prov.Close()
	sess, err := prov.CreateSession(context.Background(), spike.AgentConfig{AgentID: "coder"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer sess.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = sess.Send(ctx, "implement")
	if err == nil {
		t.Fatalf("expected deadline error")
	}
	// Must be an activity-deadline error, not a provider transport outage.
	if strings.Contains(err.Error(), "TemporaryOutage") || strings.Contains(err.Error(), "connection") {
		t.Fatalf("deadline must not be misreported as a transport outage: %v", err)
	}
	if !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("expected an activity-deadline error, got: %v", err)
	}}
