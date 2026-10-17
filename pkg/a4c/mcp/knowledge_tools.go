package mcp

import (
	"context"
	"fmt"
	"os"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/ormasoftchile/a4c/pkg/a4c/knowledge"
)

// RegisterKnowledge wires the knowledge MCP tools into the server. It is a no-op
// when the module is disabled (ks == nil), so a disabled installation neither
// advertises nor exposes knowledge capabilities.
//
// boundToken, when non-empty, is the execution-context token the server was
// launched with (Copilot per-session MCP env). It takes precedence over any
// model-supplied context_token argument, because the host bound it to this
// session's activity. When empty (workspace-scoped MCP transports such as
// OpenCode and Antigravity), the caller must supply the per-activity token that
// the host adapter injected into its session.
func (s *Server) RegisterKnowledge(ks *knowledge.Service, boundToken string) {
	if ks == nil {
		return
	}
	s.knowledge = ks
	s.knowledgeToken = boundToken
	// Local diagnostic on stderr (stdout is the MCP protocol stream). Never
	// prints the token itself — only whether one is bound.
	fmt.Fprintf(os.Stderr, "[a4c-knowledge] enabled project=%s db=%s bound_token=%v\n",
		ks.Project(), ks.Path(), boundToken != "")

	s.mcpServer.AddTool(
		mcp.NewTool("knowledge_record",
			mcp.WithDescription("Record a structured engineering knowledge item (hypothesis, experiment, evidence, finding, decision, alternative, lesson, or question) linked to this A4C activity. Support is never set at record time."),
			mcp.WithString("kind", mcp.Required(), mcp.Description("Record kind"),
				mcp.Enum("hypothesis", "experiment", "evidence", "finding", "decision", "alternative", "lesson", "question")),
			mcp.WithString("statement", mcp.Required(), mcp.Description("The claim or content"), mcp.MaxLength(2000)),
			mcp.WithString("rationale", mcp.Description("Explicit rationale / reasoning"), mcp.MaxLength(4000)),
			mcp.WithArray("evidence", mcp.Description("Evidence references: [{kind,artifact_id,hash,locator}]"), mcp.WithStringItems()),
			mcp.WithString("investigation_id", mcp.Description("Investigation this belongs to")),
			mcp.WithString("client_key", mcp.Description("Idempotency key; retries with the same key return the original record"), mcp.MaxLength(128)),
			mcp.WithBoolean("publish", mcp.Description("Publish immediately (default false = draft)")),
			mcp.WithString("context_token", mcp.Description("Execution-context token (required unless the server was launched with one)")),
		),
		s.handleKnowledgeRecord,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("knowledge_query",
			mcp.WithDescription("Retrieve prior engineering knowledge with filtering and bounded output. Provenance-rich; not a transcript dump."),
			mcp.WithString("investigation_id", mcp.Description("Scope to one investigation")),
			mcp.WithArray("kinds", mcp.Description("Filter by kind"), mcp.WithStringItems()),
			mcp.WithArray("supports", mcp.Description("Filter by support state"), mcp.WithStringItems()),
			mcp.WithArray("status", mcp.Description("Filter by status"), mcp.WithStringItems()),
			mcp.WithString("text", mcp.Description("Substring match on statement/rationale")),
			mcp.WithString("rel", mcp.Description("Only records related via this relation")),
			mcp.WithNumber("limit", mcp.Description("Page size (1-50, default 10)")),
			mcp.WithNumber("offset", mcp.Description("Page offset")),
			mcp.WithString("context_token", mcp.Description("Execution-context token (required unless the server was launched with one)")),
		),
		s.handleKnowledgeQuery,
	)
}

// resolveKnowledgeContext authenticates the execution context for a knowledge
// call, preferring the server-bound token over a model-supplied one.
func (s *Server) resolveKnowledgeContext(req mcp.CallToolRequest) (*knowledge.ExecContext, error) {
	if s.knowledge == nil {
		return nil, knowledge.ErrDisabled
	}
	token := s.knowledgeToken
	if token == "" {
		token = req.GetString("context_token", "")
	}
	if token == "" {
		return nil, knowledge.ErrNoContext
	}
	return s.knowledge.ValidateToken(token)
}

func (s *Server) handleKnowledgeRecord(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ec, err := s.resolveKnowledgeContext(req)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("knowledge_record rejected: %v", err)), nil
	}
	in := knowledge.RecordInput{
		Kind:            knowledge.Kind(req.GetString("kind", "")),
		Statement:       req.GetString("statement", ""),
		Rationale:       req.GetString("rationale", ""),
		InvestigationID: req.GetString("investigation_id", ""),
		ClientKey:       req.GetString("client_key", ""),
		Publish:         req.GetBool("publish", false),
	}
	if raw, ok := req.GetArguments()["evidence"]; ok {
		in.Evidence = parseEvidenceRefs(raw)
	}
	res, err := s.knowledge.Record(ec, in)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("knowledge_record failed: %v", err)), nil
	}
	return jsonResult(res)
}

func (s *Server) handleKnowledgeQuery(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ec, err := s.resolveKnowledgeContext(req)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("knowledge_query rejected: %v", err)), nil
	}
	_ = ec
	in := knowledge.QueryInput{
		InvestigationID: req.GetString("investigation_id", ""),
		Text:            req.GetString("text", ""),
		Rel:             req.GetString("rel", ""),
		Limit:           req.GetInt("limit", 10),
		Offset:          req.GetInt("offset", 0),
	}
	for _, k := range req.GetStringSlice("kinds", nil) {
		in.Kinds = append(in.Kinds, knowledge.Kind(k))
	}
	for _, sp := range req.GetStringSlice("supports", nil) {
		in.Supports = append(in.Supports, knowledge.Support(sp))
	}
	for _, st := range req.GetStringSlice("status", nil) {
		in.Statuses = append(in.Statuses, knowledge.Status(st))
	}
	res, err := s.knowledge.Query(in)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("knowledge_query failed: %v", err)), nil
	}
	return jsonResult(res)
}

// parseEvidenceRefs converts the loosely-typed evidence array argument into
// EvidenceRef values. Unknown shapes are skipped with no panic.
func parseEvidenceRefs(raw any) []knowledge.EvidenceRef {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	var out []knowledge.EvidenceRef
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		ev := knowledge.EvidenceRef{
			Kind:       str(m["kind"]),
			ArtifactID: str(m["artifact_id"]),
			Hash:       str(m["hash"]),
			Locator:    str(m["locator"]),
			Validity:   str(m["validity"]),
		}
		if ev.Kind == "" {
			ev.Kind = "external"
		}
		out = append(out, ev)
	}
	return out
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
