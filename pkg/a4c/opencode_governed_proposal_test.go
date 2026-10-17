package a4c_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	spike "github.com/ormasoftchile/a4c/pkg/a4c"
)

func TestGate2_OpenCodeProvider_Capabilities(t *testing.T) {
	opProv := spike.NewOpenCodeProvider("http://localhost:9999", "opencode-go/mimo-v2.5", nil)
	caps := opProv.Capabilities()

	if caps.GovernedCustomTools {
		t.Errorf("OpenCode should not claim GovernedCustomTools (native tools used via HTTP)")
	}
	if !caps.GovernedProposalMode {
		t.Errorf("OpenCode must support GovernedProposalMode")
	}
	if !caps.NativeToolRestrictions {
		t.Errorf("OpenCode must support NativeToolRestrictions")
	}
	if !caps.SessionResumption {
		t.Errorf("OpenCode must support SessionResumption")
	}
	if !caps.UsageReporting {
		t.Errorf("OpenCode must support UsageReporting")
	}

	fakeProv := spike.NewFakeProvider()
	fCaps := fakeProv.Capabilities()
	if !fCaps.GovernedCustomTools || !fCaps.GovernedProposalMode {
		t.Errorf("FakeProvider should support both custom tools and proposal mode")
	}
}

func TestGate2_GovernedPatch_SuccessForCoder(t *testing.T) {
	mock := newMockOpenCodeServer()
	defer mock.Close()

	dir := t.TempDir()
	journalDir := filepath.Join(dir, "journal")
	journal, err := spike.NewDurableIntentJournal(journalDir)
	if err != nil {
		t.Fatalf("create journal: %v", err)
	}

	gov := spike.NewPolicyGovernor([]string{"governed_read_file", "governed_apply_patch"})
	gov.SetAgentTools("coder", []string{"governed_read_file", "governed_apply_patch"})

	provider := spike.NewOpenCodeProvider(mock.server.URL, "opencode-go/mimo-v2.5", nil)
	defer provider.Close()

	targetFile := "pkg/calculator/calc.go"
	fileContent := "package calculator\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n"

	// Mock server returns a Governed Proposal Mode response
	mock.customMessageResp = map[string]any{
		"id":   "msg-test-1",
		"type": "assistant",
		"cost": 0.0001,
		"tokens": map[string]any{
			"input":     80,
			"output":    40,
			"reasoning": 0,
			"cache":     map[string]any{"read": 0, "write": 0},
		},
		"finish": "stop",
		"content": []map[string]any{
			{
				"type": "text",
				"text": "I have implemented the addition logic:\n\n" +
					"[PROPOSE_PATCH: path=\"pkg/calculator/calc.go\"]\n" +
					"```go\n" +
					fileContent +
					"```\n" +
					"[END_PATCH]\n\nAll tests should now pass.",
			},
		},
	}

	ctx := context.Background()
	config := spike.AgentConfig{
		AgentID:       "coder",
		WorkspaceDir:  dir,
		Governor:      gov,
		IntentJournal: journal,
	}

	sess, err := provider.CreateSession(ctx, config)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	defer sess.Close()

	msg, err := sess.Send(ctx, "Implement Add function")
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	if !strings.Contains(msg.Content, "[PROPOSE_PATCH:") {
		t.Errorf("expected message to contain patch proposal")
	}

	// 1. Verify file was written to disk at the exact relative workspace path
	writtenPath := filepath.Join(dir, targetFile)
	data, err := os.ReadFile(writtenPath)
	if err != nil {
		t.Fatalf("file was not written by Governed Proposal Mode: %v", err)
	}
	if string(data) != fileContent {
		t.Errorf("content mismatch: got %q, want %q", string(data), fileContent)
	}

	// 2. Verify pre-mutation intent was logged and committed in the durable journal
	intents := journal.FindIntentsForActivity("coder")
	if len(intents) != 1 {
		t.Fatalf("expected 1 intent recorded in journal, got %d", len(intents))
	}
	if intents[0].Action != "apply_patch" {
		t.Errorf("expected action 'apply_patch', got %q", intents[0].Action)
	}
	if intents[0].Target != targetFile {
		t.Errorf("expected target %q, got %q", targetFile, intents[0].Target)
	}
	if intents[0].Status != spike.IntentCommitted {
		t.Errorf("expected status IntentCommitted, got %v", intents[0].Status)
	}
}

func TestGate2_GovernedPatch_DeniedForReviewer(t *testing.T) {
	mock := newMockOpenCodeServer()
	defer mock.Close()

	dir := t.TempDir()
	journalDir := filepath.Join(dir, "journal")
	journal, _ := spike.NewDurableIntentJournal(journalDir)

	gov := spike.NewPolicyGovernor([]string{"governed_read_file", "governed_apply_patch"})
	// Reviewer only has read and submit review permissions, NEVER apply patch
	gov.SetAgentTools("reviewer", []string{"governed_read_file", "governed_submit_review"})

	provider := spike.NewOpenCodeProvider(mock.server.URL, "opencode-go/mimo-v2.5", nil)
	defer provider.Close()

	targetFile := "pkg/calculator/calc.go"

	// Mock server returns a patch proposed by a reviewer
	mock.customMessageResp = map[string]any{
		"id":   "msg-test-2",
		"type": "assistant",
		"tokens": map[string]any{
			"input":     25,
			"output":    25,
			"reasoning": 0,
			"cache":     map[string]any{"read": 0, "write": 0},
		},
		"finish": "stop",
		"content": []map[string]any{
			{
				"type": "text",
				"text": "[PROPOSE_PATCH: path=\"pkg/calculator/calc.go\"]\n```go\npackage calculator\n```\n[END_PATCH]",
			},
		},
	}

	ctx := context.Background()
	config := spike.AgentConfig{
		AgentID:       "reviewer",
		WorkspaceDir:  dir,
		Governor:      gov,
		IntentJournal: journal,
	}

	sess, err := provider.CreateSession(ctx, config)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	defer sess.Close()

	_, err = sess.Send(ctx, "Review the code")
	if err == nil {
		t.Fatalf("expected governance error when reviewer attempts patch mutation, got nil")
	}

	if !strings.Contains(err.Error(), "governance authorization denied") {
		t.Errorf("expected governance authorization denied error, got: %v", err)
	}

	// Verify file was NOT written to disk
	writtenPath := filepath.Join(dir, targetFile)
	if _, err := os.Stat(writtenPath); !os.IsNotExist(err) {
		t.Errorf("security violation: file %s was created despite reviewer denial", writtenPath)
	}
}

func TestGate2_GovernedPatch_PathTraversalRejected(t *testing.T) {
	mock := newMockOpenCodeServer()
	defer mock.Close()

	dir := t.TempDir()
	journalDir := filepath.Join(dir, "journal")
	journal, _ := spike.NewDurableIntentJournal(journalDir)

	gov := spike.NewPolicyGovernor([]string{"governed_read_file", "governed_apply_patch"})
	gov.SetAgentTools("coder", []string{"governed_read_file", "governed_apply_patch"})

	provider := spike.NewOpenCodeProvider(mock.server.URL, "opencode-go/mimo-v2.5", nil)
	defer provider.Close()

	// Propose a patch targeting a path outside the workspace
	mock.customMessageResp = map[string]any{
		"id":   "msg-test-3",
		"type": "assistant",
		"tokens": map[string]any{
			"input":     25,
			"output":    25,
			"reasoning": 0,
			"cache":     map[string]any{"read": 0, "write": 0},
		},
		"finish": "stop",
		"content": []map[string]any{
			{
				"type": "text",
				"text": "[PROPOSE_PATCH: path=\"../../escaped_secret.txt\"]\n```\nescaped payload\n```\n[END_PATCH]",
			},
		},
	}

	ctx := context.Background()
	config := spike.AgentConfig{
		AgentID:       "coder",
		WorkspaceDir:  dir,
		Governor:      gov,
		IntentJournal: journal,
	}

	sess, err := provider.CreateSession(ctx, config)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	defer sess.Close()

	_, err = sess.Send(ctx, "Write secret outside")
	if err == nil {
		t.Fatalf("expected error on path traversal attempt, got nil")
	}

	if !strings.Contains(err.Error(), "access denied: path outside workspace") {
		t.Errorf("expected path outside workspace error, got: %v", err)
	}
}

func TestGate2_GovernedPatch_ToolCallLimitEnforced(t *testing.T) {
	mock := newMockOpenCodeServer()
	defer mock.Close()

	dir := t.TempDir()
	journalDir := filepath.Join(dir, "journal")
	journal, _ := spike.NewDurableIntentJournal(journalDir)

	gov := spike.NewPolicyGovernor([]string{"governed_read_file", "governed_apply_patch"})
	gov.SetAgentTools("coder", []string{"governed_read_file", "governed_apply_patch"})

	provider := spike.NewOpenCodeProvider(mock.server.URL, "opencode-go/mimo-v2.5", nil)
	defer provider.Close()

	// Propose two patches when limit is 1
	mock.customMessageResp = map[string]any{
		"id":   "msg-test-4",
		"type": "assistant",
		"tokens": map[string]any{
			"input":     25,
			"output":    25,
			"reasoning": 0,
			"cache":     map[string]any{"read": 0, "write": 0},
		},
		"finish": "stop",
		"content": []map[string]any{
			{
				"type": "text",
				"text": "[PROPOSE_PATCH: path=\"file1.txt\"]\n```\n1\n```\n[END_PATCH]\n\n" +
					"[PROPOSE_PATCH: path=\"file2.txt\"]\n```\n2\n```\n[END_PATCH]",
			},
		},
	}

	ctx := context.Background()
	config := spike.AgentConfig{
		AgentID:       "coder",
		WorkspaceDir:  dir,
		Governor:      gov,
		IntentJournal: journal,
		MaxToolCalls:  1, // Only 1 call permitted
	}

	sess, err := provider.CreateSession(ctx, config)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	defer sess.Close()

	_, err = sess.Send(ctx, "Modify two files")
	if err == nil {
		t.Fatalf("expected tool call limit error, got nil")
	}

	if !strings.Contains(err.Error(), "maximum 1 calls allowed") {
		t.Errorf("expected tool call limit error, got: %v", err)
	}
}
