package mcp_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"

	a4cmcp "github.com/ormasoftchile/a4c/pkg/a4c/mcp"
	"github.com/ormasoftchile/a4c/pkg/a4c/knowledge"
)

// newKnowledgeClient builds an in-process MCP client over a server bound to a
// specific knowledge service and (optionally) a bound execution-context token.
func newKnowledgeClient(t *testing.T, ks *knowledge.Service, boundToken string) *client.Client {
	t.Helper()
	srv := a4cmcp.NewServerWithRole(nil, "coder")
	srv.RegisterKnowledge(ks, boundToken)
	cli, err := client.NewInProcessClient(srv.MCPServer())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := cli.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	initReq := mcp.InitializeRequest{Params: mcp.InitializeParams{
		ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
		ClientInfo:      mcp.Implementation{Name: "knowledge-test", Version: "1.0.0"},
	}}
	if _, err := cli.Initialize(ctx, initReq); err != nil {
		t.Fatal(err)
	}
	return cli
}

// setupKnowledgeHarness wires an in-process MCP client over an A4C Knowledge service.
type knowledgeHarness struct {
	client *client.Client
	ks     *knowledge.Service
	dir    string
}

func setupKnowledgeHarness(t *testing.T, enabled bool, boundToken string) *knowledgeHarness {
	t.Helper()
	dir := t.TempDir()
	if enabled {
		if err := knowledge.WriteConfig(dir, knowledge.Config{Enabled: true, Project: "skiff"}); err != nil {
			t.Fatal(err)
		}
	}
	var ks *knowledge.Service
	if enabled {
		var err error
		ks, err = knowledge.NewService(dir, knowledge.ActivityIsActive)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ks.Close() })
	}
	return &knowledgeHarness{client: newKnowledgeClient(t, ks, boundToken), ks: ks, dir: dir}
}

func callTool(t *testing.T, h *knowledgeHarness, name string, args map[string]any) (string, bool) {
	t.Helper()
	text, isErr, err := tryCallTool(h, name, args)
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	return text, isErr
}

// tryCallTool tolerates a "tool not found" transport error (which is the correct
// fail-closed outcome when the module is disabled and the tool is unregistered).
func tryCallTool(h *knowledgeHarness, name string, args map[string]any) (string, bool, error) {
	res, err := h.client.CallTool(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: name, Arguments: args},
	})
	if err != nil {
		return err.Error(), true, nil
	}
	if len(res.Content) == 0 {
		return "", res.IsError, nil
	}
	tc, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		return "", res.IsError, nil
	}
	return tc.Text, res.IsError, nil
}

// issueTokenFor mints a token via the harness store for a live activity.
func issueTokenFor(t *testing.T, h *knowledgeHarness, activityID string) string {
	t.Helper()
	tok, err := h.ks.IssueToken(knowledge.ExecContext{
		ProjectID: "skiff", TaskID: "T-1", ActivityID: activityID, Role: "coder", Host: "opencode",
	})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestKnowledgeMCP_DisabledNotAdvertised(t *testing.T) {
	h := setupKnowledgeHarness(t, false, "")
	ctx := context.Background()
	tools, err := h.client.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if strings.HasPrefix(tool.Name, "knowledge_") {
			t.Fatalf("disabled module must not advertise %s", tool.Name)
		}
	}
	// Direct call fails closed (tool is not even registered).
	_, isErr, _ := tryCallTool(h, "knowledge_record", map[string]any{"kind": "finding", "statement": "x", "context_token": "bogus"})
	if !isErr {
		t.Fatalf("disabled knowledge_record must fail closed")
	}
}

func TestKnowledgeMCP_RecordAndQuery(t *testing.T) {
	h := setupKnowledgeHarness(t, true, "")
	tok := issueTokenFor(t, h, "A-1")

	text, isErr := callTool(t, h, "knowledge_record", map[string]any{
		"kind": "hypothesis", "statement": "LRU reduces p99", "rationale": "cache locality",
		"investigation_id": "I-001", "client_key": "k1", "context_token": tok,
	})
	if isErr {
		t.Fatalf("record failed: %s", text)
	}
	var recRes knowledge.RecordResult
	if err := json.Unmarshal([]byte(text), &recRes); err != nil {
		t.Fatalf("record result not JSON: %s", text)
	}
	if recRes.Record.Support != knowledge.SupportUntested {
		t.Fatalf("hypothesis must start untested, got %q", recRes.Record.Support)
	}
	if recRes.Record.ActivityID != "A-1" || recRes.Record.Author != "coder" || recRes.Record.Host != "opencode" {
		t.Fatalf("provenance lost: %+v", recRes.Record)
	}
	if recRes.Record.InvestigationID != "I-001" {
		t.Fatalf("investigation lost: %+v", recRes.Record)
	}

	// Query returns it.
	qtext, qErr := callTool(t, h, "knowledge_query", map[string]any{
		"investigation_id": "I-001", "kinds": []string{"hypothesis"}, "context_token": tok,
	})
	if qErr {
		t.Fatalf("query failed: %s", qtext)
	}
	var qres knowledge.QueryResult
	if err := json.Unmarshal([]byte(qtext), &qres); err != nil {
		t.Fatalf("query result not JSON: %s", qtext)
	}
	if qres.Total != 1 || qres.Records[0].Statement != "LRU reduces p99" {
		t.Fatalf("query did not return the record: %+v", qres)
	}
}

func TestKnowledgeMCP_BoundTokenPreferredOverArgument(t *testing.T) {
	// Server launched with a bound token (Copilot per-session env). A model
	// supplying a bogus context_token must NOT override the bound one.
	dir := t.TempDir()
	if err := knowledge.WriteConfig(dir, knowledge.Config{Enabled: true, Project: "skiff"}); err != nil {
		t.Fatal(err)
	}
	ks, err := knowledge.NewService(dir, knowledge.ActivityIsActive)
	if err != nil {
		t.Fatal(err)
	}
	defer ks.Close()
	tok, err := ks.IssueToken(knowledge.ExecContext{ProjectID: "skiff", TaskID: "T-1", ActivityID: "A-1", Role: "coder"})
	if err != nil {
		t.Fatal(err)
	}
	// Server bound to the SAME store and token.
	bound := &knowledgeHarness{client: newKnowledgeClient(t, ks, tok), ks: ks, dir: dir}
	text, isErr := callTool(t, bound, "knowledge_record", map[string]any{
		"kind": "finding", "statement": "bound works", "context_token": "TOTALLY-BOGUS",
	})
	if isErr {
		t.Fatalf("bound-token record should succeed despite bogus arg: %s", text)
	}
	var recRes knowledge.RecordResult
	_ = json.Unmarshal([]byte(text), &recRes)
	if recRes.Record.ActivityID != "A-1" {
		t.Fatalf("bound token must attribute to A-1, got %q", recRes.Record.ActivityID)
	}
}

func TestKnowledgeMCP_ForgedTokenRejected(t *testing.T) {
	h := setupKnowledgeHarness(t, true, "")
	text, isErr := callTool(t, h, "knowledge_record", map[string]any{
		"kind": "finding", "statement": "x", "context_token": "forged.deadbeef",
	})
	if !isErr {
		t.Fatalf("forged token must be rejected")
	}
	if !strings.Contains(text, "signature") && !strings.Contains(text, "context") {
		t.Fatalf("rejection should mention the context failure: %s", text)
	}
}

func TestKnowledgeMCP_CompletedActivityRejected(t *testing.T) {
	h := setupKnowledgeHarness(t, true, "")
	// Token for an activity that is NOT currently claimed (simulates a delayed
	// call arriving after the activity completed).
	tok, err := h.ks.IssueToken(knowledge.ExecContext{
		ProjectID: "skiff", TaskID: "T-1", ActivityID: "A-COMPLETED", Role: "coder",
	})
	if err != nil {
		t.Fatal(err)
	}
	// ActivityIsActive returns true only for non-empty IDs; simulate a completed
	// activity by using a validator that reports it inactive via a fresh service.
	// Here we validate against a service whose validator reports inactive.
	ks2, err := knowledge.NewService(h.dir, func(string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	defer ks2.Close()
	if _, err := ks2.ValidateToken(tok); err == nil {
		t.Fatalf("a token for a non-claimed activity must be rejected")
	}
}

func TestKnowledgeMCP_QuerySurvivesReopen(t *testing.T) {
	// Durability: a record written by one service instance is visible after the
	// store is reopened (process restart).
	h := setupKnowledgeHarness(t, true, "")
	tok := issueTokenFor(t, h, "A-1")
	_, isErr := callTool(t, h, "knowledge_record", map[string]any{
		"kind": "finding", "statement": "durable", "client_key": "k1", "context_token": tok,
	})
	if isErr {
		t.Fatal("record failed")
	}
	// Reopen the store (simulate restart) and query.
	ks2, err := knowledge.NewService(h.dir, knowledge.ActivityIsActive)
	if err != nil {
		t.Fatal(err)
	}
	defer ks2.Close()
	res, err := ks2.Query(knowledge.QueryInput{Text: "durable"})
	if err != nil || res.Total != 1 {
		t.Fatalf("record must survive reopen, got total=%d err=%v", res.Total, err)
	}
}

// TestKnowledgeMCP_CrossHostRetrieval simulates the three A4C hosts as three
// separate MCP server processes over ONE shared workspace DB. Copilot records a
// hypothesis; OpenCode retrieves it and records a related finding; Antigravity
// retrieves both. Provenance must remain unchanged.
func TestKnowledgeMCP_CrossHostRetrieval(t *testing.T) {
	dir := t.TempDir()
	if err := knowledge.WriteConfig(dir, knowledge.Config{Enabled: true, Project: "skiff"}); err != nil {
		t.Fatal(err)
	}
	// THREE separate Service instances over the SAME on-disk DB model three
	// separate `a4c mcp` processes (one per host). Each has its own SQLite
	// connection, exercising real cross-process visibility.
	newSvc := func() *knowledge.Service {
		ks, err := knowledge.NewService(dir, knowledge.ActivityIsActive)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ks.Close() })
		return ks
	}
	ksCopilot, ksOpenCode, ksAnti := newSvc(), newSvc(), newSvc()

	tokFor := func(ks *knowledge.Service, host, activity string) string {
		tok, err := ks.IssueToken(knowledge.ExecContext{
			ProjectID: "skiff", TaskID: "T-1", ActivityID: activity, Role: "coder", Host: host,
		})
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}

	// Copilot process: record a hypothesis.
	copilot := &knowledgeHarness{client: newKnowledgeClient(t, ksCopilot, tokFor(ksCopilot, "copilot", "A-copilot")), ks: ksCopilot, dir: dir}
	text, isErr := callTool(t, copilot, "knowledge_record", map[string]any{
		"kind": "hypothesis", "statement": "LRU reduces p99 read latency.",
		"investigation_id": "I-001", "client_key": "cop-1",
	})
	if isErr {
		t.Fatalf("copilot record failed: %s", text)
	}
	var hyp knowledge.RecordResult
	_ = json.Unmarshal([]byte(text), &hyp)

	// OpenCode process: retrieve the hypothesis, then record a related finding.
	opencode := &knowledgeHarness{client: newKnowledgeClient(t, ksOpenCode, tokFor(ksOpenCode, "opencode", "A-opencode")), ks: ksOpenCode, dir: dir}
	qtext, qErr := callTool(t, opencode, "knowledge_query", map[string]any{
		"investigation_id": "I-001", "kinds": []string{"hypothesis"},
	})
	if qErr {
		t.Fatalf("opencode query failed: %s", qtext)
	}
	var qres knowledge.QueryResult
	_ = json.Unmarshal([]byte(qtext), &qres)
	if qres.Total != 1 || qres.Records[0].ID != hyp.Record.ID {
		t.Fatalf("opencode must retrieve the copilot record unchanged, got %+v", qres)
	}
	if qres.Records[0].Host != "copilot" || qres.Records[0].ActivityID != "A-copilot" {
		t.Fatalf("provenance must be unchanged across hosts: %+v", qres.Records[0])
	}
	ftext, fErr := callTool(t, opencode, "knowledge_record", map[string]any{
		"kind": "finding", "statement": "LRU improved p99 by 12% at 1M keys.",
		"investigation_id": "I-001", "client_key": "oc-1",
	})
	if fErr {
		t.Fatalf("opencode record failed: %s", ftext)
	}
	var finding knowledge.RecordResult
	_ = json.Unmarshal([]byte(ftext), &finding)
	if finding.Record.Host != "opencode" || finding.Record.ActivityID != "A-opencode" {
		t.Fatalf("finding provenance wrong: %+v", finding.Record)
	}

	// Antigravity process: retrieve both.
	antigravity := &knowledgeHarness{client: newKnowledgeClient(t, ksAnti, tokFor(ksAnti, "antigravity", "A-agy")), ks: ksAnti, dir: dir}
	all, aErr := callTool(t, antigravity, "knowledge_query", map[string]any{"investigation_id": "I-001"})
	if aErr {
		t.Fatalf("antigravity query failed: %s", all)
	}
	var ares knowledge.QueryResult
	_ = json.Unmarshal([]byte(all), &ares)
	if ares.Total != 2 {
		t.Fatalf("antigravity must retrieve both records, got %d", ares.Total)
	}
	hosts := map[string]bool{}
	for _, r := range ares.Records {
		hosts[r.Host] = true
	}
	if !hosts["copilot"] || !hosts["opencode"] {
		t.Fatalf("cross-host records not both visible: %+v", ares.Records)
	}
	// Original records unchanged.
	got, err := ksCopilot.Store().Get(hyp.Record.ID)
	if err != nil || got.ActivityID != "A-copilot" || got.Host != "copilot" {
		t.Fatalf("original record must be unchanged: %+v err=%v", got, err)
	}
}
