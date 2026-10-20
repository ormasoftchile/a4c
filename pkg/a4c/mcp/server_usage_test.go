package mcp_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/ormasoftchile/a4c/pkg/a4c"
)

// Host-native usage telemetry flows through team_complete_activity's optional
// 'usage' parameter; completion without usage keeps working (Patch 1 contract).
func TestMCP_CompleteActivityOptionalUsageTelemetry(t *testing.T) {
	h := setupMCPHarness(t)
	defer h.client.Close()
	ctx := context.Background()

	beginRes, err := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "team_begin_task",
			Arguments: map[string]any{
				"objective":     "implement telemetry plumbing",
				"budget_tokens": 5000,
				"strategy":      "direct",
			},
		},
	})
	if err != nil || beginRes.IsError {
		t.Fatalf("team_begin_task failed: %v (%+v)", err, beginRes)
	}
	var begun a4c.BeginTaskResult
	if err := json.Unmarshal([]byte(beginRes.Content[0].(mcp.TextContent).Text), &begun); err != nil {
		t.Fatalf("parse begin result: %v", err)
	}

	usageJSON := `{"input_tokens":100,"cache_read_tokens":50,"cache_write_tokens":10,"output_tokens":20,"reasoning_tokens":5,"model_invocations":2,"cost_reported":0.5,"cost_unit":"ai_credits","model":"claude-opus-5.5","source":"copilot","scope":"session_including_subagents"}`
	compRes, err := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "team_complete_activity",
			Arguments: map[string]any{
				"task_id":     begun.TaskID,
				"activity_id": begun.TaskID,
				"actor":       "alice",
				"summary":     "implemented",
				"success":     true,
				"usage":       usageJSON,
			},
		},
	})
	if err != nil || compRes.IsError {
		t.Fatalf("team_complete_activity failed: %v (%+v)", err, compRes)
	}

	rec, err := h.store.Get(begun.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	m := rec.Result.Metrics
	if m == nil {
		t.Fatal("usage reported but no metrics recorded")
	}
	if m.PromptTokens != 100 || m.CacheReadTokens != 50 || m.CacheWriteTokens != 10 ||
		m.CompletionTokens != 20 || m.ReasoningTokens != 5 || m.ModelInvocations != 2 {
		t.Fatalf("metrics mismatch: %+v", m)
	}
	if m.UsageSource != "copilot" || m.UsageModel != "claude-opus-5.5" ||
		m.CostUnit != "ai_credits" || m.ReportedCost != 0.5 {
		t.Fatalf("provenance/units not preserved: %+v", m)
	}
	if m.UsageScope != "session_including_subagents" {
		t.Fatalf("usage scope not preserved: %q", m.UsageScope)
	}
	raw, ok := rec.Result.Outputs["host_usage"].(*a4c.HostUsage)
	if !ok {
		// Durable records round-trip through JSON: accept the decoded map form.
		if m, ok2 := rec.Result.Outputs["host_usage"].(map[string]any); !ok2 || m["cost_unit"] != "ai_credits" {
			t.Fatalf("raw host usage missing from durable record: %+v", rec.Result.Outputs["host_usage"])
		}
	} else if raw == nil || raw.CostUnit != "ai_credits" {
		t.Fatalf("raw host usage not preserved: %+v", raw)
	}

	// Malformed usage is rejected explicitly, never silently dropped.
	badRes, _ := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "team_complete_activity",
			Arguments: map[string]any{
				"task_id": begun.TaskID, "activity_id": begun.TaskID, "actor": "alice",
				"summary": "again", "success": true, "usage": "{ not json",
			},
		},
	})
	if badRes == nil || !badRes.IsError || !strings.Contains(badRes.Content[0].(mcp.TextContent).Text, "invalid 'usage' parameter") {
		t.Fatalf("malformed usage not rejected: %+v", badRes)
	}
}
