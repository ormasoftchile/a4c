package a4c_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	spike "github.com/ormasoftchile/a4c/pkg/a4c"
)

// TestMilestone8_01_CapabilityRegistryAndProfileResolution tests declarative capability profiles.
func TestMilestone8_01_CapabilityRegistryAndProfileResolution(t *testing.T) {
	reg := spike.NewCapabilityRegistry()
	gov := spike.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_propose_activity",
		"governed_submit_review", "governed_request_human_decision", "governed_propose_memory",
		"governed_structural_search_symbols", "governed_structural_context_for", "governed_structural_callers",
	})

	// Test Architecture Investigation profile
	leadProf := reg.ResolveProfile("lead", "Lead Software Architect", "Investigate dependency graph", nil, gov)
	if leadProf.ID != spike.ProfileArchitectureInvestigation {
		t.Fatalf("expected profile %q, got %q", spike.ProfileArchitectureInvestigation, leadProf.ID)
	}
	hasTool := func(tools []string, target string) bool {
		for _, tool := range tools {
			if tool == target {
				return true
			}
		}
		return false
	}
	if !hasTool(leadProf.AllowedTools, "governed_structural_callers") {
		t.Errorf("lead profile missing governed_structural_callers")
	}
	if hasTool(leadProf.AllowedTools, "governed_apply_patch") {
		t.Errorf("lead profile should NOT allow governed_apply_patch")
	}

	// Test Code Implementation profile
	coderProf := reg.ResolveProfile("coder", "Software Engineer", "Implement feature fix", nil, gov)
	if coderProf.ID != spike.ProfileCodeImplementation {
		t.Fatalf("expected profile %q, got %q", spike.ProfileCodeImplementation, coderProf.ID)
	}
	if !hasTool(coderProf.AllowedTools, "governed_apply_patch") {
		t.Errorf("coder profile missing governed_apply_patch")
	}
	if hasTool(coderProf.AllowedTools, "governed_submit_review") {
		t.Errorf("coder profile should NOT allow governed_submit_review")
	}
	if hasTool(coderProf.AllowedTools, "governed_structural_search_symbols") {
		t.Errorf("coder profile should NOT allow governed_structural_search_symbols by default")
	}

	// Test Independent Review profile
	revProf := reg.ResolveProfile("reviewer", "Independent Reviewer", "Verify implementation diff", nil, gov)
	if revProf.ID != spike.ProfileIndependentReview {
		t.Fatalf("expected profile %q, got %q", spike.ProfileIndependentReview, revProf.ID)
	}
	if !hasTool(revProf.AllowedTools, "governed_submit_review") {
		t.Errorf("reviewer profile missing governed_submit_review")
	}
	if hasTool(revProf.AllowedTools, "governed_apply_patch") {
		t.Errorf("reviewer profile must strictly prohibit governed_apply_patch")
	}

	// Test Governor restriction overrides (YAWR authority)
	strictGov := spike.NewPolicyGovernor([]string{"governed_read_file"})
	restrictedProf := reg.ResolveProfile("lead", "Lead", "Investigate", nil, strictGov)
	if len(restrictedProf.AllowedTools) != 1 || restrictedProf.AllowedTools[0] != "governed_read_file" {
		t.Errorf("expected strictly filtered tools under restrictive governor, got %v", restrictedProf.AllowedTools)
	}
}

// TestMilestone8_02_StructuralArtifactPersistenceAndProjections tests artifact creation,
// durable disk storage, and bounded role-specific projections.
func TestMilestone8_02_StructuralArtifactPersistenceAndProjections(t *testing.T) {
	tmpDir := t.TempDir()
	storeDir := filepath.Join(tmpDir, "structural")

	store, err := spike.NewDurableStructuralStore(storeDir)
	if err != nil {
		t.Fatalf("failed to create structural store: %v", err)
	}

	artifact := &spike.StructuralContextArtifact{
		ID:                "struct-task-123",
		RepositoryID:      "repo-yawr-runtime",
		WorkspaceSnapshot: "sha256-abc12345",
		IndexedSnapshot:   "sha256-abc12345",
		Freshness:         spike.FreshnessCurrent,
		RelevantFiles: []string{
			"runtime/pkg/evidence/evidence.go",
			"runtime/internal/evidence/attachment.go",
		},
		RelevantSymbols: []spike.StructuralSymbol{
			{Name: "EvidenceRecord", FilePath: "runtime/pkg/evidence/evidence.go", StartLine: 45, Signature: "type EvidenceRecord struct"},
			{Name: "Store", FilePath: "runtime/internal/evidence/attachment.go", StartLine: 82, Signature: "func (s *AttachmentStore) Store(ctx, r)"},
		},
		DependencyEdges: []spike.StructuralEdgeRecord{
			{CallerSymbol: "Collect", CallerFile: "runtime/internal/evidence/collector.go", CallerLine: 110, CalleeSymbol: "Store"},
			{CallerSymbol: "Dispatch", CallerFile: "runtime/internal/adapter/wire.go", CallerLine: 65, CalleeSymbol: "EvidenceRecord"},
		},
		Completeness: "complete",
		CreatedAt:    time.Now(),
	}

	// 1. Test persistence
	if err := store.Save(artifact); err != nil {
		t.Fatalf("failed to save structural artifact: %v", err)
	}

	loaded, err := store.Get("struct-task-123")
	if err != nil {
		t.Fatalf("failed to retrieve structural artifact: %v", err)
	}
	if loaded.ID != artifact.ID || loaded.Freshness != spike.FreshnessCurrent {
		t.Errorf("mismatched loaded artifact: %+v", loaded)
	}

	// 2. Test Coder projection
	coderProj := loaded.ProjectForCoder(25)
	if !strings.Contains(coderProj, "Target Files: runtime/pkg/evidence/evidence.go") {
		t.Errorf("coder projection missing target files: %s", coderProj)
	}
	if !strings.Contains(coderProj, "EvidenceRecord") || !strings.Contains(coderProj, "Collect") {
		t.Errorf("coder projection missing symbols or callers: %s", coderProj)
	}

	// 3. Test Reviewer projection
	revProj := loaded.ProjectForReviewer(25)
	if !strings.Contains(revProj, "Expected Call Sites to Audit:") {
		t.Errorf("reviewer projection missing call sites section: %s", revProj)
	}
	if !strings.Contains(revProj, "NOT semantic correctness") {
		t.Errorf("reviewer projection missing independent validation disclaimer: %s", revProj)
	}

	// 4. Test Stale projection warning
	loaded.Freshness = spike.FreshnessStale
	staleCoderProj := loaded.ProjectForCoder(25)
	if !strings.Contains(staleCoderProj, "WARNING: Workspace has modified content") {
		t.Errorf("stale coder projection missing stale warning: %s", staleCoderProj)
	}
}

type mockTestRunner struct{}

func (m *mockTestRunner) RunTests(ctx context.Context, dir string) (int, string, error) {
	return 0, "PASS\nok\n", nil
}

// TestMilestone8_03_CoordinatorCapabilityEnforcementAndHandoff verifies that TeamCoordinator
// injects capability profiles and structural projections into the agent loop.
func TestMilestone8_03_CoordinatorCapabilityEnforcementAndHandoff(t *testing.T) {
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "activities")
	rosterDir := filepath.Join(dir, "roster")
	memDir := filepath.Join(dir, "memory")
	journalDir := filepath.Join(dir, "journal")
	structDir := filepath.Join(dir, "structural")

	store, _ := spike.NewDurableActivityStore(storeDir)
	roster, _ := spike.DefaultTeamRoster(rosterDir)
	memory, _ := spike.NewDurableMemoryStore(memDir)
	journal, _ := spike.NewDurableIntentJournal(journalDir)
	structStore, _ := spike.NewDurableStructuralStore(structDir)

	gov := spike.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_propose_activity",
		"governed_submit_review", "governed_request_human_decision", "governed_propose_memory",
		"governed_structural_search_symbols", "governed_structural_context_for", "governed_structural_callers",
	})

	fakeProv := spike.NewFakeProvider()
	var capturedCoderPrompt string
	var capturedReviewerPrompt string
	var capturedCoderTools []string
	var capturedReviewerTools []string

	fakeProv.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		agentID := session.Config().AgentID
		switch agentID {
		case "lead":
			// Lead publishes an activity proposal to coder and reviewer
			session.Config().OnProposeActivity(spike.ActivityProposal{
				ProposalID:      "sub-coder-1",
				ProposedAgentID: "coder",
				Objective:       "Implement diagnostic inspector",
				RequestedBudget: spike.Budget{Allocated: 20000},
			})
			session.Config().OnProposeActivity(spike.ActivityProposal{
				ProposalID:      "sub-reviewer-1",
				ProposedAgentID: "reviewer",
				Objective:       "Verify diagnostics output natively",
				RequestedBudget: spike.Budget{Allocated: 10000},
				DependsOn:       []string{"sub-coder-1"},
			})
		case "coder":
			capturedCoderPrompt = prompt
			capturedCoderTools = session.Config().AllowedTools
		case "reviewer":
			capturedReviewerPrompt = prompt
			capturedReviewerTools = session.Config().AllowedTools
			session.Config().OnSubmitReview(spike.ReviewResult{
				Approved:  true,
				Comments:  "Diagnostics pass all verification checks",
				Timestamp: time.Now(),
			})
		}
		return &spike.AgentMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("[%s] done", agentID),
		}, nil
	}

	runner := &mockTestRunner{}
	coord := spike.NewTeamCoordinator(dir, store, roster, memory, gov, journal, runner, fakeProv)
	coord.SetStructuralStore(structStore)

	// Pre-seed a structural context artifact
	testArt := &spike.StructuralContextArtifact{
		ID:                "test-art-diag",
		RepositoryID:      "task-root",
		WorkspaceSnapshot: "snap-123",
		IndexedSnapshot:   "snap-123",
		Freshness:         spike.FreshnessCurrent,
		RelevantFiles:     []string{"cmd/a4c/main.go", "pkg/a4c/history.go"},
		RelevantSymbols: []spike.StructuralSymbol{
			{Name: "BuildTaskHistory", FilePath: "pkg/a4c/history.go", StartLine: 50},
		},
		DependencyEdges: []spike.StructuralEdgeRecord{
			{CallerSymbol: "runTaskHistory", CallerFile: "cmd/a4c/main.go", CallerLine: 260, CalleeSymbol: "BuildTaskHistory"},
		},
		CreatedAt: time.Now(),
	}
	coord.SetStructuralArtifact(testArt)

	taskReq, err := coord.SubmitTask("Build diagnostics feature", spike.Budget{Allocated: 50000}, nil)
	if err != nil {
		t.Fatalf("task submission failed: %v", err)
	}

	ctx := context.Background()
	results, err := coord.RunLoop(ctx, 5)
	if err != nil {
		t.Fatalf("RunLoop failed: %v", err)
	}

	if len(results) < 3 {
		t.Fatalf("expected at least 3 completed activities (lead, coder, reviewer), got %d", len(results))
	}

	// 1. Verify capability scoping on Coder
	hasTool := func(tools []string, target string) bool {
		for _, tool := range tools {
			if tool == target {
				return true
			}
		}
		return false
	}
	if !hasTool(capturedCoderTools, "governed_apply_patch") {
		t.Errorf("coder was missing governed_apply_patch in allowed tools: %v", capturedCoderTools)
	}
	if hasTool(capturedCoderTools, "governed_structural_search_symbols") {
		t.Errorf("coder received structural search tools unnecessarily: %v", capturedCoderTools)
	}
	if hasTool(capturedCoderTools, "governed_submit_review") {
		t.Errorf("coder received governed_submit_review: %v", capturedCoderTools)
	}

	// 2. Verify capability scoping on Reviewer
	if !hasTool(capturedReviewerTools, "governed_submit_review") {
		t.Errorf("reviewer was missing governed_submit_review: %v", capturedReviewerTools)
	}
	if hasTool(capturedReviewerTools, "governed_apply_patch") {
		t.Errorf("reviewer received governed_apply_patch: %v", capturedReviewerTools)
	}

	// 3. Verify that Coder received bounded structural projection in prompt
	if !strings.Contains(capturedCoderPrompt, "Shared Structural Context:") {
		t.Errorf("coder prompt did not contain shared structural context:\n%s", capturedCoderPrompt)
	}
	if !strings.Contains(capturedCoderPrompt, "BuildTaskHistory") {
		t.Errorf("coder prompt missing projected symbol BuildTaskHistory:\n%s", capturedCoderPrompt)
	}

	// 4. Verify that Reviewer received independent review projection
	if !strings.Contains(capturedReviewerPrompt, "Structural Impact Scope") {
		t.Errorf("reviewer prompt missing structural impact projection:\n%s", capturedReviewerPrompt)
	}
	if !strings.Contains(capturedReviewerPrompt, "NOT semantic correctness") {
		t.Errorf("reviewer prompt missing correctness advisory:\n%s", capturedReviewerPrompt)
	}

	// 5. Verify durable history record includes capability profile
	report, err := spike.BuildTaskHistory(taskReq.ID, store, journal, nil, memory)
	if err != nil {
		t.Fatalf("failed to build task history: %v", err)
	}
	for _, act := range report.Activities {
		if act.CapabilityProfile == nil {
			t.Errorf("activity %s missing recorded CapabilityProfile in history", act.ActivityID)
		}
	}
}

// TestMilestone8_04_CapabilityIsolationAndFailClosed verifies that capability limits
// cannot be bypassed by session reuse or ambient calls.
func TestMilestone8_04_CapabilityIsolationAndFailClosed(t *testing.T) {
	reg := spike.NewCapabilityRegistry()
	gov := spike.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_submit_review",
	})
	gov.SetAgentTools("coder", []string{"governed_read_file", "governed_apply_patch"})
	gov.SetAgentTools("reviewer", []string{"governed_read_file", "governed_submit_review"})

	// 1. Coder profile cannot invoke governed_submit_review
	coderProf := reg.ResolveProfile("coder", "Software Engineer", "Write code", nil, gov)
	for _, tool := range coderProf.AllowedTools {
		if tool == "governed_submit_review" {
			t.Fatalf("coder profile illegally contains governed_submit_review")
		}
	}
	if err := gov.AuthorizeToolInvocation("coder", "governed_submit_review"); err == nil {
		t.Fatalf("expected governor to reject coder invoking governed_submit_review")
	}

	// 2. Reviewer profile cannot invoke governed_apply_patch
	revProf := reg.ResolveProfile("reviewer", "Reviewer", "Audit diff", nil, gov)
	for _, tool := range revProf.AllowedTools {
		if tool == "governed_apply_patch" {
			t.Fatalf("reviewer profile illegally contains governed_apply_patch")
		}
	}
	if err := gov.AuthorizeToolInvocation("reviewer", "governed_apply_patch"); err == nil {
		t.Fatalf("expected governor to reject reviewer invoking governed_apply_patch")
	}

	// 3. Unauthorized ambient tool invocation fails closed
	if err := gov.AuthorizeToolInvocation("coder", "unregistered_dangerous_tool"); err == nil {
		t.Fatalf("expected governor to reject unregistered tool")
	}
}

// TestMilestone8_05_StructuralContextTruncationBounds verifies that structural projections
// enforce maximum line limits to prevent prompt blowups.
func TestMilestone8_05_StructuralContextTruncationBounds(t *testing.T) {
	var symbols []spike.StructuralSymbol
	var edges []spike.StructuralEdgeRecord

	for i := 0; i < 50; i++ {
		symbols = append(symbols, spike.StructuralSymbol{
			Name:      fmt.Sprintf("Symbol%d", i),
			FilePath:  fmt.Sprintf("pkg/sub/file%d.go", i),
			StartLine: i * 10,
			Signature: fmt.Sprintf("func Symbol%d()", i),
		})
		edges = append(edges, spike.StructuralEdgeRecord{
			CallerSymbol: fmt.Sprintf("Caller%d", i),
			CallerFile:   fmt.Sprintf("pkg/caller/file%d.go", i),
			CallerLine:   i * 5,
			CalleeSymbol: fmt.Sprintf("Symbol%d", i),
		})
	}

	artifact := &spike.StructuralContextArtifact{
		ID:              "large-art",
		Freshness:       spike.FreshnessCurrent,
		RelevantSymbols: symbols,
		DependencyEdges: edges,
		RelevantFiles:   []string{"pkg/sub/file1.go", "pkg/sub/file2.go"},
	}

	// Test coder projection truncation
	coderProj := artifact.ProjectForCoder(15)
	coderLines := strings.Split(coderProj, "\n")
	if len(coderLines) > 17 { // 15 lines + truncation note
		t.Errorf("coder projection exceeded max line limit: got %d lines", len(coderLines))
	}
	if !strings.Contains(coderProj, "truncated") {
		t.Errorf("expected truncation indicator in coder projection")
	}

	// Test reviewer projection truncation
	revProj := artifact.ProjectForReviewer(12)
	revLines := strings.Split(revProj, "\n")
	if len(revLines) > 14 {
		t.Errorf("reviewer projection exceeded max line limit: got %d lines", len(revLines))
	}
	if !strings.Contains(revProj, "truncated") {
		t.Errorf("expected truncation indicator in reviewer projection")
	}
}

