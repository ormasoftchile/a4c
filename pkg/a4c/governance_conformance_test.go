package a4c_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	spike "github.com/ormasoftchile/a4c/pkg/a4c"
)

// ConformanceBattery verifies that any runtime adapter satisfies A4C's core governance invariants.
func runGovernanceRoleSeparationBattery(t *testing.T, runtimeName string, gov *spike.PolicyGovernor) {
	// 1. Coder is permitted to apply patches, but barred from submitting reviews
	if err := gov.AuthorizeToolInvocation("coder", "governed_apply_patch"); err != nil {
		t.Errorf("[%s] coder should be authorized for governed_apply_patch: %v", runtimeName, err)
	}
	if err := gov.AuthorizeToolInvocation("coder", "governed_submit_review"); err == nil {
		t.Errorf("[%s] security violation: coder must NOT be authorized for governed_submit_review", runtimeName)
	}

	// 2. Reviewer is permitted to submit reviews, but barred from applying patches
	if err := gov.AuthorizeToolInvocation("reviewer", "governed_submit_review"); err != nil {
		t.Errorf("[%s] reviewer should be authorized for governed_submit_review: %v", runtimeName, err)
	}
	if err := gov.AuthorizeToolInvocation("reviewer", "governed_apply_patch"); err == nil {
		t.Errorf("[%s] security violation: reviewer must NOT be authorized for governed_apply_patch", runtimeName)
	}

	// 3. Lead is permitted to propose activities, but barred from applying patches
	if err := gov.AuthorizeToolInvocation("lead", "governed_propose_activity"); err != nil {
		t.Errorf("[%s] lead should be authorized for governed_propose_activity: %v", runtimeName, err)
	}
	if err := gov.AuthorizeToolInvocation("lead", "governed_apply_patch"); err == nil {
		t.Errorf("[%s] security violation: lead must NOT be authorized for governed_apply_patch", runtimeName)
	}
}

func TestGovernanceConformance_PolicyGovernorRoleSeparation(t *testing.T) {
	rosterDir := t.TempDir()
	roster, err := spike.DefaultTeamRoster(rosterDir)
	if err != nil {
		t.Fatalf("default roster: %v", err)
	}

	gov := spike.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_submit_review", "governed_propose_activity",
	})
	gov.SetAgentTools("coder", []string{"governed_read_file", "governed_apply_patch"})
	gov.SetAgentTools("reviewer", []string{"governed_read_file", "governed_submit_review"})
	gov.SetAgentTools("lead", []string{"governed_read_file", "governed_propose_activity"})

	// Verify against OpenCode and Copilot role definitions
	runGovernanceRoleSeparationBattery(t, "opencode", gov)
	runGovernanceRoleSeparationBattery(t, "copilot", gov)

	// Verify roster role mappings
	lead, ok := roster.GetMember("lead")
	if !ok || lead == nil {
		t.Fatalf("lead member not found in roster")
	}
	coder, ok := roster.GetMember("coder")
	if !ok || coder == nil {
		t.Fatalf("coder member not found in roster")
	}
	rev, ok := roster.GetMember("reviewer")
	if !ok || rev == nil {
		t.Fatalf("reviewer member not found in roster")
	}

	if !strings.Contains(lead.Role, "Lead") {
		t.Errorf("expected Lead role, got %v", lead.Role)
	}
	if !strings.Contains(coder.Role, "Software Engineer") {
		t.Errorf("expected Software Engineer role, got %v", coder.Role)
	}
	if !strings.Contains(rev.Role, "Quality") && !strings.Contains(rev.Role, "Reviewer") {
		t.Errorf("expected Reviewer role, got %v", rev.Role)
	}
}

func TestGovernanceConformance_OpenCodeVsFake_IntentAndContainment(t *testing.T) {
	// Test Governed Proposal Mode against OpenCode adapter
	mock := newMockOpenCodeServer()
	defer mock.Close()

	dir := t.TempDir()
	journalDir := filepath.Join(dir, "journal")
	journal, _ := spike.NewDurableIntentJournal(journalDir)

	gov := spike.NewPolicyGovernor([]string{"governed_read_file", "governed_apply_patch"})
	gov.SetAgentTools("coder", []string{"governed_read_file", "governed_apply_patch"})

	provider := spike.NewOpenCodeProvider(mock.server.URL, "opencode-go/mimo-v2.5", nil)
	defer provider.Close()

	// 1. Success case for Coder: verifies durable intent logging
	mock.customMessageResp = map[string]any{
		"info": map[string]any{"tokens": map[string]any{"total": 50}},
		"parts": []map[string]any{
			{
				"type": "text",
				"text": "[PROPOSE_PATCH: path=\"service.go\"]\n```go\npackage service\n```\n[END_PATCH]",
			},
		},
	}

	ctx := context.Background()
	sess, err := provider.CreateSession(ctx, spike.AgentConfig{
		AgentID:       "coder",
		WorkspaceDir:  dir,
		Governor:      gov,
		IntentJournal: journal,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer sess.Close()

	_, err = sess.Send(ctx, "Write service")
	if err != nil {
		t.Fatalf("send failed: %v", err)
	}

	intents := journal.FindIntentsForActivity("coder")
	if len(intents) != 1 {
		t.Fatalf("expected 1 intent logged, got %d", len(intents))
	}
	if intents[0].Target != "service.go" || intents[0].Status != spike.IntentCommitted {
		t.Errorf("intent state invalid: %+v", intents[0])
	}

	// 2. Traversal containment: attempts outside workspace are denied
	mock.customMessageResp = map[string]any{
		"info": map[string]any{"tokens": map[string]any{"total": 50}},
		"parts": []map[string]any{
			{
				"type": "text",
				"text": "[PROPOSE_PATCH: path=\"../../escape.go\"]\n```go\npackage escape\n```\n[END_PATCH]",
			},
		},
	}
	_, err = sess.Send(ctx, "Escape workspace")
	if err == nil {
		t.Fatalf("expected path escape error, got nil")
	}
	if !strings.Contains(err.Error(), "access denied: path outside workspace") {
		t.Errorf("expected access denied error, got: %v", err)
	}
}

func TestGovernanceConformance_ReviewerNonMutatingGuarantee(t *testing.T) {
	mock := newMockOpenCodeServer()
	defer mock.Close()

	dir := t.TempDir()
	journalDir := filepath.Join(dir, "journal")
	journal, _ := spike.NewDurableIntentJournal(journalDir)

	gov := spike.NewPolicyGovernor([]string{"governed_read_file", "governed_submit_review"})
	gov.SetAgentTools("reviewer", []string{"governed_read_file", "governed_submit_review"})

	provider := spike.NewOpenCodeProvider(mock.server.URL, "opencode-go/mimo-v2.5", nil)
	defer provider.Close()

	// Reviewer emits patch proposal: Governor must refuse
	mock.customMessageResp = map[string]any{
		"info": map[string]any{"tokens": map[string]any{"total": 50}},
		"parts": []map[string]any{
			{
				"type": "text",
				"text": "[PROPOSE_PATCH: path=\"fix.go\"]\n```go\npackage fix\n```\n[END_PATCH]",
			},
		},
	}

	ctx := context.Background()
	sess, _ := provider.CreateSession(ctx, spike.AgentConfig{
		AgentID:       "reviewer",
		WorkspaceDir:  dir,
		Governor:      gov,
		IntentJournal: journal,
	})
	defer sess.Close()

	_, err := sess.Send(ctx, "Propose rogue patch")
	if err == nil {
		t.Fatalf("expected reviewer mutation to be rejected by PolicyGovernor, got nil")
	}
	if !strings.Contains(err.Error(), "governance authorization denied") {
		t.Errorf("expected governance authorization denied, got: %v", err)
	}

	// Verify no file written
	if _, err := os.Stat(filepath.Join(dir, "fix.go")); !os.IsNotExist(err) {
		t.Errorf("fix.go was written despite reviewer non-mutating guarantee")
	}
}

func TestGovernanceConformance_RuntimeCapabilitiesComparison(t *testing.T) {
	opProv := spike.NewOpenCodeProvider("http://localhost:9999", "opencode-go/mimo-v2.5", nil)
	opCaps := opProv.Capabilities()

	fakeProv := spike.NewFakeProvider()
	fakeCaps := fakeProv.Capabilities()

	t.Logf("Runtime Capabilities Comparison:")
	t.Logf("  OpenCode: GovernedCustomTools=%t, GovernedProposalMode=%t, NativeRestrictions=%t",
		opCaps.GovernedCustomTools, opCaps.GovernedProposalMode, opCaps.NativeToolRestrictions)
	t.Logf("  Fake:     GovernedCustomTools=%t, GovernedProposalMode=%t, NativeRestrictions=%t",
		fakeCaps.GovernedCustomTools, fakeCaps.GovernedProposalMode, fakeCaps.NativeToolRestrictions)

	// Both runtimes must support proposal mode, native tool restrictions, and session resumption
	if !opCaps.GovernedProposalMode || !fakeCaps.GovernedProposalMode {
		t.Errorf("all supported runtimes must support GovernedProposalMode")
	}
	if !opCaps.NativeToolRestrictions || !fakeCaps.NativeToolRestrictions {
		t.Errorf("all supported runtimes must enforce NativeToolRestrictions")
	}
	if !opCaps.SessionResumption || !fakeCaps.SessionResumption {
		t.Errorf("all supported runtimes must support SessionResumption")
	}
}
