package mcp_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/ormasoftchile/a4c/pkg/a4c"
)

// TestMCP_CompleteActivity_KnowledgeAttestation proves the knowledge
// disposition flows through the real MCP tool surface into durable state.
func TestMCP_CompleteActivity_KnowledgeAttestation(t *testing.T) {
	h := setupMCPHarness(t)
	ctx := context.Background()

	beginRes, err := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "team_begin_task",
			Arguments: map[string]any{
				"objective": "mcp knowledge attestation",
				"strategy":  "direct",
				"actor":     "alice",
			},
		},
	})
	if err != nil || beginRes.IsError {
		t.Fatalf("team_begin_task: %v %+v", err, beginRes)
	}
	var begin struct {
		TaskID string `json:"task_id"`
	}
	if err := decodeMCPResult(beginRes, &begin); err != nil || begin.TaskID == "" {
		t.Fatalf("decode begin: %v %+v", err, beginRes.Content)
	}

	claimJSON, _ := json.Marshal(map[string]any{
		"significant": false,
		"reason":      "No significant reusable knowledge",
	})
	compRes, err := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "team_complete_activity",
			Arguments: map[string]any{
				"task_id":     begin.TaskID,
				"activity_id": begin.TaskID,
				"actor":       "coder",
				"summary":     "routine change",
				"success":     true,
				"knowledge":   string(claimJSON),
			},
		},
	})
	if err != nil || compRes.IsError {
		t.Fatalf("team_complete_activity: %v %+v", err, compRes)
	}
	var comp a4c.HostActivityResult
	if err := decodeMCPResult(compRes, &comp); err != nil {
		t.Fatalf("decode completion: %v", err)
	}
	if comp.Knowledge == nil || comp.Knowledge.Status != "assessed" || comp.Knowledge.Significant {
		t.Fatalf("knowledge outcome through MCP = %+v", comp.Knowledge)
	}
}

// TestMCP_CompleteActivity_KnowledgeMissingPathIsPending proves a claim for a
// document that does not exist is recorded as pending, never as success.
func TestMCP_CompleteActivity_KnowledgeMissingPathIsPending(t *testing.T) {
	h := setupMCPHarness(t)
	ctx := context.Background()

	beginRes, err := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "team_begin_task",
			Arguments: map[string]any{
				"objective": "mcp knowledge missing path",
				"strategy":  "direct",
			},
		},
	})
	if err != nil || beginRes.IsError {
		t.Fatalf("team_begin_task: %v %+v", err, beginRes)
	}
	var begin struct {
		TaskID string `json:"task_id"`
	}
	if err := decodeMCPResult(beginRes, &begin); err != nil {
		t.Fatal(err)
	}

	claimJSON, _ := json.Marshal(map[string]any{
		"significant": true,
		"paths":       []string{"findings/never-written.md"},
	})
	compRes, err := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "team_complete_activity",
			Arguments: map[string]any{
				"task_id":     begin.TaskID,
				"activity_id": begin.TaskID,
				"actor":       "coder",
				"summary":     "done",
				"success":     true,
				"knowledge":   string(claimJSON),
			},
		},
	})
	if err != nil || compRes.IsError {
		t.Fatalf("team_complete_activity: %v %+v", err, compRes)
	}
	var comp a4c.HostActivityResult
	if err := decodeMCPResult(compRes, &comp); err != nil {
		t.Fatal(err)
	}
	if comp.Knowledge == nil || comp.Knowledge.Status != "pending" {
		t.Fatalf("missing claimed document must be pending, got %+v", comp.Knowledge)
	}
}

// TestMCP_CompleteActivity_KnowledgeInvalidJSONFailsClosed proves a malformed
// knowledge parameter is rejected explicitly rather than silently dropped.
func TestMCP_CompleteActivity_KnowledgeInvalidJSONFailsClosed(t *testing.T) {
	h := setupMCPHarness(t)
	ctx := context.Background()

	compRes, err := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "team_complete_activity",
			Arguments: map[string]any{
				"task_id":     "task-x",
				"activity_id": "task-x",
				"actor":       "coder",
				"summary":     "done",
				"knowledge":   "{not-json",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !compRes.IsError {
		t.Fatal("invalid knowledge JSON must produce an error result")
	}
	text := mcpResultText(compRes)
	if !strings.Contains(text, "knowledge") {
		t.Fatalf("error should mention the knowledge parameter: %q", text)
	}
}

func decodeMCPResult(res *mcp.CallToolResult, out any) error {
	if res == nil || len(res.Content) == 0 {
		return errNoContent
	}
	text := mcpResultText(res)
	return json.Unmarshal([]byte(text), out)
}

func mcpResultText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

type noContentError struct{}

func (noContentError) Error() string { return "mcp: empty result content" }

var errNoContent = noContentError{}
