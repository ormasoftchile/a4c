package a4c_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ormasoftchile/a4c/pkg/a4c"
)

// ---------------------------------------------------------------------------
// R4: interruption / recovery — abandoned work is terminal, never successful
// ---------------------------------------------------------------------------

func TestRecovery_InterruptedRootAbandonsChildren(t *testing.T) {
	h := setupServiceTestHarness(t)
	task := liBegin(t, h, a4c.StrategyReviewed)
	liAddChild(t, h, task.TaskID, "orphan-review", "reviewer")
	liAddChild(t, h, task.TaskID, "orphan-coder", "bob")

	if err := h.store.InterruptWithResult(task.TaskID, a4c.ActivityResult{
		ID:     task.TaskID,
		Status: a4c.StatusInterrupted,
		Outputs: map[string]any{
			"interruption_reason":  "temporary_outage",
			"interruption_message": "coordinator: no agent provider configured",
		},
	}, errors.New("coordinator: no agent provider configured for runtime")); err != nil {
		t.Fatalf("interrupt root: %v", err)
	}

	// Children of the interrupted root are abandoned: terminal but not
	// successful, with an explicit reason (historical evidence preserved).
	for _, id := range []string{"orphan-review", "orphan-coder"} {
		rec, err := h.store.Get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if rec.Status != a4c.StatusFailed {
			t.Fatalf("%s status = %s, want failed (abandoned)", id, rec.Status)
		}
		if !strings.Contains(rec.Error, "abandoned:") {
			t.Fatalf("%s error = %q, want explicit abandonment reason", id, rec.Error)
		}
		if rec.Result != nil {
			t.Fatalf("%s must not carry a fabricated result: %+v", id, rec.Result)
		}
	}

	// Obsolete work cannot silently complete later.
	if err := h.store.Complete("orphan-review", a4c.ActivityResult{ID: "orphan-review", Status: a4c.StatusCompleted}); !errors.Is(err, a4c.ErrActivityTerminal) {
		t.Fatalf("completing abandoned work: err = %v, want ErrActivityTerminal", err)
	}

	// Repeated recovery is safe and idempotent.
	before, _ := h.store.Get("orphan-coder")
	_ = h.store.InterruptWithResult(task.TaskID, a4c.ActivityResult{ID: task.TaskID, Status: a4c.StatusInterrupted}, errors.New("again"))
	after, _ := h.store.Get("orphan-coder")
	if before.Status != after.Status || before.Error != after.Error {
		t.Fatalf("recovery not idempotent: %+v -> %+v", before, after)
	}

	// Acceptance remains blocked; nothing was marked successful.
	requireFinalizationRejected(t, liAccept(h, task.TaskID), "accept after interruption")
}

func TestRecovery_RejectAbandonsChildren(t *testing.T) {
	h := setupServiceTestHarness(t)
	task := liBegin(t, h, a4c.StrategyTeam)
	liAddChild(t, h, task.TaskID, "rejected-child", "bob")

	if _, err := h.service.RejectTask(context.Background(), a4c.RejectTaskRequest{
		TaskID: task.TaskID, Reason: "wrong approach",
	}); err != nil {
		t.Fatalf("RejectTask: %v", err)
	}
	rec, err := h.store.Get("rejected-child")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != a4c.StatusFailed || !strings.Contains(rec.Error, "abandoned:") {
		t.Fatalf("child after reject = %s (%q), want failed/abandoned", rec.Status, rec.Error)
	}
}

// ---------------------------------------------------------------------------
// R8: optional host usage telemetry — present vs absent vs partial
// ---------------------------------------------------------------------------

func intPtr(v int) *int          { return &v }
func floatPtr(v float64) *float64 { return &v }

func TestMetrics_HostUsagePersistsAndReports(t *testing.T) {
	h := setupServiceTestHarness(t)
	task := liBegin(t, h, a4c.StrategyDirect)
	if _, err := h.service.CompleteActivity(context.Background(), a4c.CompleteActivityRequest{
		TaskID:     task.TaskID,
		ActivityID: task.TaskID,
		Actor:      "alice",
		Summary:    "implemented",
		Success:    true,
		Usage: &a4c.HostUsage{
			InputTokens:      intPtr(100),
			CacheReadTokens:  intPtr(50),
			CacheWriteTokens: intPtr(10),
			OutputTokens:     intPtr(20),
			ReasoningTokens:  intPtr(5),
			ModelInvocations: intPtr(3),
			CostReported:     floatPtr(0.25),
			CostUnit:         "usd",
			Model:            "test-model",
			Source:           "opencode",
		},
	}); err != nil {
		t.Fatalf("CompleteActivity: %v", err)
	}

	rec, err := h.store.Get(task.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	m := rec.Result.Metrics
	if m == nil {
		t.Fatal("metrics missing despite reported usage")
	}
	if m.PromptTokens != 100 || m.CacheReadTokens != 50 || m.CacheWriteTokens != 10 ||
		m.CompletionTokens != 20 || m.ReasoningTokens != 5 || m.ModelInvocations != 3 {
		t.Fatalf("metrics mismatch: %+v", m)
	}
	if m.CachedTokens != 50 {
		t.Fatalf("legacy cached alias = %d, want cache-read 50", m.CachedTokens)
	}
	if m.TotalTokens != 180 {
		t.Fatalf("gross total = %d, want 180 (documented sum of reported categories)", m.TotalTokens)
	}
	// Provenance and accounting boundaries preserved, never converted.
	if m.UsageSource != "opencode" || m.UsageModel != "test-model" || m.CostUnit != "usd" || m.ReportedCost != 0.25 {
		t.Fatalf("provenance lost: %+v", m)
	}
	for _, f := range []string{"input_tokens", "cache_read_tokens", "cache_write_tokens", "output_tokens", "reasoning_tokens", "model_invocations", "cost_reported"} {
		found := false
		for _, r := range m.ReportedFields {
			if r == f {
				found = true
			}
		}
		if !found {
			t.Fatalf("reported field %q missing from %v", f, m.ReportedFields)
		}
	}
	// Raw host report preserved verbatim alongside derived metrics. Durable
	// records round-trip through JSON, so the value may be the original struct
	// or its decoded map form.
	switch raw := rec.Result.Outputs["host_usage"].(type) {
	case *a4c.HostUsage:
		if raw == nil || raw.InputTokens == nil || *raw.InputTokens != 100 {
			t.Fatalf("raw host usage not preserved: %+v", raw)
		}
	case map[string]any:
		if int(raw["input_tokens"].(float64)) != 100 || raw["cost_unit"] != "usd" {
			t.Fatalf("raw host usage not preserved: %+v", raw)
		}
	default:
		t.Fatalf("raw host usage missing from durable record: %+v", rec.Result.Outputs["host_usage"])
	}
}

func TestMetrics_MissingUsageIsAbsentNotZero(t *testing.T) {
	h := setupServiceTestHarness(t)
	task := liBegin(t, h, a4c.StrategyDirect)
	if _, err := h.service.CompleteActivity(context.Background(), a4c.CompleteActivityRequest{
		TaskID: task.TaskID, ActivityID: task.TaskID, Actor: "alice", Summary: "done", Success: true,
	}); err != nil {
		t.Fatalf("completion without usage must work: %v", err)
	}
	rec, _ := h.store.Get(task.TaskID)
	if rec.Result.Metrics != nil {
		t.Fatalf("unreported usage must be absent, not measured zeros: %+v", rec.Result.Metrics)
	}
	if _, ok := rec.Result.Outputs["host_usage"]; ok {
		t.Fatalf("no host_usage output expected when nothing was reported")
	}
}

func TestMetrics_UnreportedFieldsStayUnknown(t *testing.T) {
	h := setupServiceTestHarness(t)
	task := liBegin(t, h, a4c.StrategyDirect)
	if _, err := h.service.CompleteActivity(context.Background(), a4c.CompleteActivityRequest{
		TaskID: task.TaskID, ActivityID: task.TaskID, Actor: "alice", Summary: "done", Success: true,
		Usage: &a4c.HostUsage{
			InputTokens:  intPtr(7),
			OutputTokens: intPtr(9),
			CostUnit:     "unpriced", // Antigravity-style: no cost counter exists
			Source:       "antigravity",
		},
	}); err != nil {
		t.Fatalf("CompleteActivity: %v", err)
	}
	rec, _ := h.store.Get(task.TaskID)
	m := rec.Result.Metrics
	if len(m.ReportedFields) != 2 {
		t.Fatalf("only two fields were reported, got %v", m.ReportedFields)
	}
	for _, f := range m.ReportedFields {
		if f != "input_tokens" && f != "output_tokens" {
			t.Fatalf("unexpected reported field %q", f)
		}
	}
	if m.CostUnit != "unpriced" {
		t.Fatalf("cost unit not preserved: %q", m.CostUnit)
	}
	// Zero cache/cost values are NOT claims of measured zeros: they are absent
	// from ReportedFields, which is what reporting consumers must key on.
	if m.CacheReadTokens != 0 || m.ReportedCost != 0 {
		t.Fatalf("unreported values should stay zero-valued but unlisted: %+v", m)
	}
}

// ---------------------------------------------------------------------------
// §3/§8: generated host instructions match real capabilities and contracts
// ---------------------------------------------------------------------------

var validTaskSubs = map[string]bool{
	"submit": true, "start": true, "run": true, "resume": true, "adopt": true,
	"escalate": true, "checkpoint": true, "list": true, "history": true,
	"inspect": true, "diff": true, "feedback": true, "accept": true,
	"integrate": true, "reject": true, "decide": true, "worker": true,
	"loop": true, "mode": true,
}

func TestRosterExport_CharterMatchesServiceContract(t *testing.T) {
	dir := t.TempDir()
	if err := a4c.ExportCopilotAgents(dir); err != nil {
		t.Fatalf("export copilot: %v", err)
	}
	if err := a4c.ExportOpenCodeAgents(dir); err != nil {
		t.Fatalf("export opencode: %v", err)
	}
	if err := a4c.ExportAntigravityCustomization(dir); err != nil {
		t.Fatalf("export antigravity: %v", err)
	}

	read := func(rel string) string {
		data, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		return string(data)
	}
	docs := map[string]string{
		"copilot":     read(filepath.Join(".github", "agents", "a4c.agent.md")),
		"opencode":    read(filepath.Join(".opencode", "agents", "a4c.md")),
		"antigravity": read(filepath.Join(".agents", "skills", "a4c", "SKILL.md")),
	}

	subRe := regexp.MustCompile(`a4c task ([a-z][a-z-]*)`)
	for name, body := range docs {
		lower := strings.ToLower(body)
		if !strings.Contains(lower, "root activity") || !strings.Contains(lower, "team_complete_activity") {
			t.Errorf("%s: root-activity completion requirement missing", name)
		}
		if !strings.Contains(lower, "delegation is prohibited") {
			t.Errorf("%s: direct-mode delegation prohibition missing (service rejects delegation)", name)
		}
		if !strings.Contains(lower, "'--help' is help, not an objective") {
			t.Errorf("%s: flag-like-objective warning missing", name)
		}
		if !strings.Contains(lower, "review approval never") {
			t.Errorf("%s: review-approval-never-completes rule missing", name)
		}
		for _, m := range subRe.FindAllStringSubmatch(body, -1) {
			if !validTaskSubs[m[1]] {
				t.Errorf("%s: teaches unknown command 'a4c task %s'", name, m[1])
			}
		}
	}

	// Host-specific invocation surfaces must name the tools as each host
	// actually exposes them (no discovery, no custom MCP clients).
	if !strings.Contains(docs["copilot"], "a4c-team_begin_task") {
		t.Errorf("copilot charter does not name the runtime tool form a4c-team_*")
	}
	// The whitelist must name the host's real shell tools: Windows Copilot
	// exposes "powershell", not "bash" (Stage 2 finding F5 — an agent that
	// whitelists only "bash" has no shell at all and cannot run verification).
	for _, shell := range []string{"bash", "powershell"} {
		if !strings.Contains(docs["copilot"], `"`+shell+`"`) {
			t.Errorf("copilot charter tools list misses host shell tool %q", shell)
		}
	}
	if !strings.Contains(docs["opencode"], "tools.a4c.team_begin_task") {
		t.Errorf("opencode charter does not teach the code-execution call form")
	}
	if !strings.Contains(docs["antigravity"], "agy mcp add a4c a4c mcp") {
		t.Errorf("antigravity skill does not document the supported MCP registration path")
	}
	if !strings.Contains(docs["antigravity"], "never build your own MCP client") {
		t.Errorf("antigravity skill does not forbid improvised MCP clients (Stage 1 R9)")
	}
}

func TestRegisterAntigravityMCP_WithoutAgyIsNoop(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if err := a4c.RegisterAntigravityMCP(); err != nil {
		t.Fatalf("registration without agy must be a no-op, got %v", err)
	}
}
