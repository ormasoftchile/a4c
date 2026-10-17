package a4c_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	spike "github.com/ormasoftchile/a4c/pkg/a4c"
)

// fakeScriptedRunner is a deterministic TestRunner that returns a scripted
// (exitCode, output) so tests never depend on a real toolchain.
type fakeScriptedRunner struct {
	exitCode int
	output   string
	calls    int
}

func (r *fakeScriptedRunner) RunTests(ctx context.Context, workspaceDir string) (int, string, error) {
	r.calls++
	return r.exitCode, r.output, nil
}

// reviewHarness bundles a coordinator wired to a fake provider plus the stores
// needed to drive and inspect review workflows deterministically.
type reviewHarness struct {
	coord     *spike.TeamCoordinator
	store     *spike.DurableActivityStore
	fake      *spike.FakeProvider
	runner    *fakeScriptedRunner
	workspace string
}

func newReviewHarness(t *testing.T, runner spike.TestRunner) *reviewHarness {
	t.Helper()
	dir := t.TempDir()
	workspace := filepath.Join(dir, "repo")
	_ = os.MkdirAll(workspace, 0o755)

	store, _ := spike.NewDurableActivityStore(filepath.Join(dir, "activities"))
	memory, _ := spike.NewDurableMemoryStore(filepath.Join(dir, "memory"))
	roster, _ := spike.DefaultTeamRoster(filepath.Join(dir, "roster"))
	gov := spike.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_propose_activity",
		"governed_submit_review", "governed_request_human_decision", "governed_propose_memory",
	})
	journal, _ := spike.NewDurableIntentJournal(filepath.Join(dir, "journal"))
	fake := spike.NewFakeProvider()

	if runner == nil {
		runner = &fakeScriptedRunner{exitCode: 0, output: "ok  \texample.com/calc\t0.01s\n"}
	}
	fr, _ := runner.(*fakeScriptedRunner)

	coord := spike.NewTeamCoordinator(workspace, store, roster, memory, gov, journal, runner, fake)
	return &reviewHarness{coord: coord, store: store, fake: fake, runner: fr, workspace: workspace}
}

// ---------------------------------------------------------------------------
// 1. Reviewer can inspect full changed files through bounded reads.
// ---------------------------------------------------------------------------
func TestReview_01_BoundedReadsInspectFullChangedFiles(t *testing.T) {
	// A large changed file (larger than one read window) must remain fully
	// inspectable through paginated bounded reads, and the truncation message
	// must direct pagination rather than implying missing content.
	const totalLines = 500
	var sb strings.Builder
	for i := 1; i <= totalLines; i++ {
		sb.WriteString(fmt.Sprintf("line %d: assert covered\n", i))
	}
	content := sb.String()

	// Single bounded read returns the first window plus a pagination directive.
	first := spike.SliceFileLines(content, 0, 0, 300)
	if !strings.Contains(first, "line 1:") || !strings.Contains(first, "line 300:") {
		t.Fatalf("first bounded read missing expected lines")
	}
	if strings.Contains(first, "line 301:") {
		t.Fatalf("first read should be bounded to 300 lines")
	}
	if !strings.Contains(first, "Specify start_line and end_line") {
		t.Fatalf("truncation must direct pagination, got: %q", first[len(first)-120:])
	}

	// A second, explicit page reaches the end of the file.
	second := spike.SliceFileLines(content, 301, totalLines, 300)
	if !strings.Contains(second, "line 301:") || !strings.Contains(second, fmt.Sprintf("line %d:", totalLines)) {
		t.Fatalf("paginated read must reach the final line; got tail %q", second[len(second)-80:])
	}
	if strings.Contains(second, "truncated") {
		t.Fatalf("explicit in-range page must not be truncated")
	}
}

// ---------------------------------------------------------------------------
// 2. Existing test results and logs are accessible.
// ---------------------------------------------------------------------------
func TestReview_02_DurableTestLogsAccessible(t *testing.T) {
	ws := t.TempDir()
	out := "=== RUN   TestCypher\n--- PASS: TestCypher (0.00s)\nPASS\nok  \texample.com/c\t0.02s\n"
	logPath := spike.WriteDurableTestLog(ws, "evi-test-1", out)
	if logPath == "" {
		t.Fatalf("expected durable log path, got empty")
	}
	abs := filepath.Join(ws, logPath)
	data, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("durable log not readable: %v", err)
	}
	if !strings.Contains(string(data), "TestCypher") {
		t.Fatalf("durable log missing test output: %q", string(data))
	}
}

// ---------------------------------------------------------------------------
// Vacuous-evidence detection (foundation for tests 3-5).
// ---------------------------------------------------------------------------
func TestReview_VacuousEvidenceDetection(t *testing.T) {
	// `go test` on a repo with no Go test files: proves nothing.
	_, vacuous := spike.AnalyzeGoTestOutput("?   \tgithub.com/DeusData/codebase-memory-mcp\t[no test files]\n")
	if !vacuous {
		t.Fatalf("[no test files] output must be detected as vacuous")
	}
	// Empty output proves nothing.
	_, vacuous = spike.AnalyzeGoTestOutput("")
	if !vacuous {
		t.Fatalf("empty output must be detected as vacuous")
	}
	// A real passing run is not vacuous.
	n, vacuous := spike.AnalyzeGoTestOutput("ok  \texample.com/calc\t0.01s\n")
	if vacuous || n == 0 {
		t.Fatalf("real ok output must be non-vacuous with tests counted, got n=%d vacuous=%t", n, vacuous)
	}
	// Verbose per-test lines are counted precisely.
	n, vacuous = spike.AnalyzeGoTestOutput("--- PASS: TestA (0.00s)\n--- PASS: TestB (0.00s)\n")
	if vacuous || n != 2 {
		t.Fatalf("expected 2 verbose tests non-vacuous, got n=%d vacuous=%t", n, vacuous)
	}
	// An opaque but non-empty custom-runner "PASS" message must NOT be
	// misclassified as vacuous (this is what fakePassTestRunner emits).
	_, vacuous = spike.AnalyzeGoTestOutput("PASS: All tests passed")
	if vacuous {
		t.Fatalf("opaque custom-runner PASS output must not be treated as vacuous")
	}
}

// ---------------------------------------------------------------------------
// 3. Missing evidence produces verification-blocked, not changes-required.
// ---------------------------------------------------------------------------
func TestReview_03_MissingEvidenceIsVerificationBlocked(t *testing.T) {
	h := newReviewHarness(t, &fakeScriptedRunner{exitCode: 0, output: ""})

	// A completed coder with NO validation evidence.
	_ = h.store.Enqueue(spike.ActivityRequest{ID: "c1", AgentID: "coder", Objective: "impl", CreatedAt: time.Now()})
	_ = h.store.Complete("c1", spike.ActivityResult{
		ID: "c1", Status: spike.StatusCompleted,
		Outputs: map[string]any{"summary": "implemented"},
	})

	// Reviewer approves, but there is no evidence.
	h.fake.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		session.Config().OnSubmitReview(spike.ReviewResult{
			Verdict: spike.ReviewVerdictApproved, Approved: true, Comments: "LGTM", Timestamp: time.Now(),
		})
		return &spike.AgentMessage{Content: "[REVIEW_APPROVED]"}, nil
	}
	_ = h.store.Enqueue(spike.ActivityRequest{
		ID: "r1", AgentID: "reviewer", Objective: "review", DependsOn: []string{"c1"}, CreatedAt: time.Now(),
	})

	res, err := h.coord.Step(context.Background())
	if err != nil {
		t.Fatalf("step failed: %v", err)
	}
	approved, _ := res.Outputs["approved"].(bool)
	if approved {
		t.Fatalf("approval without evidence must not stand")
	}
	verdict, _ := res.Outputs["verdict"].(string)
	if verdict != string(spike.ReviewVerdictVerificationBlocked) {
		t.Fatalf("expected verification_blocked, got %q", verdict)
	}
	// It must NOT be reported as a code rejection requiring changes.
	if fr, _ := res.Outputs["failure_reason"].(string); strings.Contains(fr, "revision") {
		t.Fatalf("verification gap must not be framed as a revision, got %q", fr)
	}
	// No coder revision may be scheduled.
	if _, err := h.store.Get("r1-revision-1"); err == nil {
		t.Fatalf("verification-blocked must not schedule a coding revision")
	}
}

// ---------------------------------------------------------------------------
// 4. Verification-blocked does not consume MaxRevisions.
// ---------------------------------------------------------------------------
func TestReview_04_VerificationBlockedDoesNotConsumeMaxRevisions(t *testing.T) {
	h := newReviewHarness(t, &fakeScriptedRunner{exitCode: 0, output: ""})
	h.coord.MaxRevisions = 1

	// Coder with no evidence.
	_ = h.store.Enqueue(spike.ActivityRequest{ID: "c1", AgentID: "coder", Objective: "impl", CreatedAt: time.Now()})
	_ = h.store.Complete("c1", spike.ActivityResult{ID: "c1", Status: spike.StatusCompleted, Outputs: map[string]any{"summary": "ok"}})

	// Reviewer explicitly reports verification-blocked.
	h.fake.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		session.Config().OnSubmitReview(spike.ReviewResult{
			Verdict: spike.ReviewVerdictVerificationBlocked, Approved: false,
			Comments: "cannot verify, no command access", Timestamp: time.Now(),
		})
		return &spike.AgentMessage{Content: "[REVIEW_BLOCKED: cannot verify]"}, nil
	}
	_ = h.store.Enqueue(spike.ActivityRequest{ID: "r1", AgentID: "reviewer", Objective: "review", DependsOn: []string{"c1"}, CreatedAt: time.Now()})

	res, err := h.coord.Step(context.Background())
	if err != nil {
		t.Fatalf("step failed: %v", err)
	}
	verdict, _ := res.Outputs["verdict"].(string)
	if verdict != string(spike.ReviewVerdictVerificationBlocked) {
		t.Fatalf("expected verification_blocked, got %q", verdict)
	}
	// A verification retry (not a coding revision) is scheduled, and it does
	// not depend on nor consume the coder revision budget.
	if _, err := h.store.Get("r1-revision-1"); err == nil {
		t.Fatalf("verification-blocked must not enqueue a coding revision")
	}
}

// ---------------------------------------------------------------------------
// 5. Evidence collection enables a subsequent review without a coding revision.
// ---------------------------------------------------------------------------
func TestReview_05_EvidenceCollectionRetriesReviewNotCoder(t *testing.T) {
	// Runner that first yields vacuous evidence, then a real passing run.
	runner := &fakeScriptedRunner{exitCode: 0, output: "?   \texample.com/c\t[no test files]\n"}
	h := newReviewHarness(t, runner)

	// Coder produced only vacuous evidence (the observed failure mode).
	_ = h.store.Enqueue(spike.ActivityRequest{ID: "c1", AgentID: "coder", Objective: "impl", CreatedAt: time.Now()})
	_ = h.store.Complete("c1", spike.ActivityResult{
		ID: "c1", Status: spike.StatusCompleted,
		Outputs: map[string]any{
			"summary": "implemented",
			"validation_evidence": spike.ValidationEvidence{
				EvidenceID: "evi-c1", WorkspaceSnapshotID: "snap", Command: []string{"go", "test"},
				ExitCode: 0, Valid: true, Vacuous: true, TestsExecuted: 0, CompletedAt: time.Now(),
			},
		},
	})

	h.fake.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		// Reviewer keeps reporting it cannot verify the vacuous evidence.
		session.Config().OnSubmitReview(spike.ReviewResult{
			Verdict: spike.ReviewVerdictVerificationBlocked, Approved: false,
			Comments: "evidence ran no tests; cannot verify", Timestamp: time.Now(),
		})
		return &spike.AgentMessage{Content: "[REVIEW_BLOCKED: no tests ran]"}, nil
	}
	_ = h.store.Enqueue(spike.ActivityRequest{ID: "r1", AgentID: "reviewer", Objective: "review", DependsOn: []string{"c1"}, CreatedAt: time.Now()})

	res, err := h.coord.Step(context.Background())
	if err != nil {
		t.Fatalf("step failed: %v", err)
	}
	if collected, _ := res.Outputs["evidence_collected"].(bool); !collected {
		t.Fatalf("expected evidence to be collected, outputs=%+v", res.Outputs)
	}
	// A verification retry is enqueued; it must NOT be a coder revision.
	if _, err := h.store.Get("r1-revision-1"); err == nil {
		t.Fatalf("evidence retry must not be a coding revision")
	}
	// The verification activity exists and depends on the review.
	rec, err := h.store.Get("r1-verify-1")
	if err != nil || rec == nil {
		t.Fatalf("expected a verification retry activity, got err=%v", err)
	}
	if rec.Request.AgentID != "reviewer" {
		t.Fatalf("verification retry must target the reviewer, got %q", rec.Request.AgentID)
	}
}

// ---------------------------------------------------------------------------
// 6. Repeated verification-blocked outcomes terminate safely.
// ---------------------------------------------------------------------------
func TestReview_06_RepeatedVerificationBlockedTerminatesSafely(t *testing.T) {
	h := newReviewHarness(t, &fakeScriptedRunner{exitCode: 0, output: ""})
	h.coord.MaxRevisions = 2

	_ = h.store.Enqueue(spike.ActivityRequest{ID: "c1", AgentID: "coder", Objective: "impl", CreatedAt: time.Now()})
	_ = h.store.Complete("c1", spike.ActivityResult{ID: "c1", Status: spike.StatusCompleted, Outputs: map[string]any{"summary": "ok"}})

	// Reviewer ALWAYS reports verification-blocked, regardless of evidence.
	blockedCount := 0
	h.fake.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		blockedCount++
		session.Config().OnSubmitReview(spike.ReviewResult{
			Verdict: spike.ReviewVerdictVerificationBlocked, Approved: false,
			Comments: "cannot verify", Timestamp: time.Now(),
		})
		return &spike.AgentMessage{Content: "[REVIEW_BLOCKED: cannot verify]"}, nil
	}
	_ = h.store.Enqueue(spike.ActivityRequest{ID: "r1", AgentID: "reviewer", Objective: "review", DependsOn: []string{"c1"}, CreatedAt: time.Now()})

	// Bounded loop; must terminate rather than spin.
	results, err := h.coord.RunLoop(context.Background(), 12)
	if err != nil {
		t.Fatalf("run loop error: %v", err)
	}
	if len(results) == 0 {
		t.Fatalf("expected results")
	}

	// No coding revision may ever have been scheduled.
	for _, id := range []string{"r1-revision-1", "r1-revision-2"} {
		if _, err := h.store.Get(id); err == nil {
			t.Fatalf("repeated verification-blocked must never schedule coding revision %s", id)
		}
	}

	// The terminal state must be an explicit verification-blocked diagnostic,
	// not revision_limit_exhausted.
	last := results[len(results)-1]
	fr, _ := last.Outputs["failure_reason"].(string)
	if fr == "revision_limit_exhausted" {
		t.Fatalf("verification-blocked must not terminate as revision_limit_exhausted")
	}
	if !strings.Contains(fr, "verification_blocked") && last.Status != spike.StatusAwaitingHuman {
		t.Fatalf("expected terminal verification_blocked diagnostic or awaiting_human, got failure_reason=%q status=%s", fr, last.Status)
	}
	// It must not loop unboundedly.
	if blockedCount > 6 {
		t.Fatalf("review loop was not bounded; blocked reviews=%d", blockedCount)
	}
}

// ---------------------------------------------------------------------------
// 7. A genuine code defect triggers a coding revision.
// ---------------------------------------------------------------------------
func TestReview_07_GenuineDefectTriggersCodingRevision(t *testing.T) {
	h := newReviewHarness(t, &fakeScriptedRunner{exitCode: 0, output: "ok  \texample.com/calc\t0.01s\n"})

	// Coder with REAL (non-vacuous) passing evidence.
	_ = h.store.Enqueue(spike.ActivityRequest{ID: "c1", AgentID: "coder", Objective: "impl", CreatedAt: time.Now()})
	_ = h.store.Complete("c1", spike.ActivityResult{
		ID: "c1", Status: spike.StatusCompleted,
		Outputs: map[string]any{
			"summary": "implemented",
			"validation_evidence": spike.ValidationEvidence{
				EvidenceID: "evi-c1", WorkspaceSnapshotID: "snap", Command: []string{"go", "test"},
				ExitCode: 0, Valid: true, Vacuous: false, TestsExecuted: 3, CompletedAt: time.Now(),
			},
		},
	})

	// Reviewer finds a concrete defect.
	h.fake.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		session.Config().OnSubmitReview(spike.ReviewResult{
			Verdict: spike.ReviewVerdictRejected, Approved: false,
			Comments: "calc.go:12 Div panics on divide-by-zero", Blockers: []string{"divide-by-zero panic"},
			Timestamp: time.Now(),
		})
		return &spike.AgentMessage{Content: "[REVIEW_REJECTED: divide-by-zero]"}, nil
	}
	_ = h.store.Enqueue(spike.ActivityRequest{ID: "r1", AgentID: "reviewer", Objective: "review", DependsOn: []string{"c1"}, CreatedAt: time.Now()})

	res, err := h.coord.Step(context.Background())
	if err != nil {
		t.Fatalf("step failed: %v", err)
	}
	verdict, _ := res.Outputs["verdict"].(string)
	if verdict != string(spike.ReviewVerdictRejected) {
		t.Fatalf("a concrete defect must be a rejection, got %q", verdict)
	}
	// A coding revision must be scheduled.
	if _, err := h.store.Get("r1-revision-1"); err != nil {
		t.Fatalf("genuine defect must schedule a coding revision: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 8. A corrected defect can be approved.
// ---------------------------------------------------------------------------
func TestReview_08_CorrectedDefectCanBeApproved(t *testing.T) {
	h := newReviewHarness(t, &fakeScriptedRunner{exitCode: 0, output: "ok  \texample.com/calc\t0.01s\n"})

	_ = h.store.Enqueue(spike.ActivityRequest{ID: "c1", AgentID: "coder", Objective: "impl", CreatedAt: time.Now()})
	_ = h.store.Complete("c1", spike.ActivityResult{
		ID: "c1", Status: spike.StatusCompleted,
		Outputs: map[string]any{
			"summary": "implemented",
			"validation_evidence": spike.ValidationEvidence{
				EvidenceID: "evi-c1", WorkspaceSnapshotID: "snap", Command: []string{"go", "test"},
				ExitCode: 0, Valid: true, TestsExecuted: 3, CompletedAt: time.Now(),
			},
		},
	})

	review := 0
	h.fake.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		agentID := session.Config().AgentID
		switch agentID {
		case "coder":
			// The revision re-runs and produces fresh valid evidence.
			_ = os.WriteFile(filepath.Join(h.workspace, "calc.go"), []byte("package calc\nfunc Div(a,b int) int { if b==0 { return 0 }; return a/b }\n"), 0o644)
			return &spike.AgentMessage{Content: "fixed"}, nil
		case "reviewer":
			review++
			if review == 1 {
				session.Config().OnSubmitReview(spike.ReviewResult{
					Verdict: spike.ReviewVerdictRejected, Approved: false,
					Comments: "divide-by-zero panic", Timestamp: time.Now(),
				})
				return &spike.AgentMessage{Content: "[REVIEW_REJECTED: divide-by-zero]"}, nil
			}
			session.Config().OnSubmitReview(spike.ReviewResult{
				Verdict: spike.ReviewVerdictApproved, Approved: true,
				Comments: "approved after fix", Timestamp: time.Now(),
			})
			return &spike.AgentMessage{Content: "[REVIEW_APPROVED]"}, nil
		}
		return &spike.AgentMessage{Content: "ok"}, nil
	}
	_ = h.store.Enqueue(spike.ActivityRequest{ID: "r1", AgentID: "reviewer", Objective: "review", DependsOn: []string{"c1"}, CreatedAt: time.Now()})

	results, err := h.coord.RunLoop(context.Background(), 10)
	if err != nil {
		t.Fatalf("run loop error: %v", err)
	}
	last := results[len(results)-1]
	if approved, ok := last.Outputs["approved"].(bool); !ok || !approved {
		t.Fatalf("corrected defect must be approvable, got outputs=%+v", last.Outputs)
	}
}

// ---------------------------------------------------------------------------
// 9. Unrelated doc.go and go.mod additions are surfaced for review.
// ---------------------------------------------------------------------------
func TestReview_09_UnrelatedFilesSurfacedForReview(t *testing.T) {
	// The exact junk the benchmark observed: doc.go + go.mod added to a C repo.
	files := []string{"src/cypher/cypher.c", "src/cypher/cypher.h", "tests/test_cypher.c", "doc.go", "go.mod"}
	warnings := spike.FlagDiffHygiene(files, "Support property-to-property comparisons in Cypher WHERE clauses", false)

	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "go.mod") {
		t.Fatalf("go.mod must be flagged in a non-Go repo, warnings=%v", warnings)
	}
	if !strings.Contains(joined, "doc.go") {
		t.Fatalf("doc.go must be flagged in a non-Go repo, warnings=%v", warnings)
	}
	// Legitimate task files must NOT be flagged.
	for _, legit := range []string{"src/cypher/cypher.c", "tests/test_cypher.c"} {
		for _, w := range warnings {
			if strings.HasPrefix(w, legit+":") {
				t.Fatalf("legitimate file %s must not be flagged, got %q", legit, w)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 10. Review remains read-only.
// ---------------------------------------------------------------------------
func TestReview_10_RemainsReadOnly(t *testing.T) {
	reg := spike.NewCapabilityRegistry()
	gov := spike.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_propose_activity",
		"governed_submit_review", "governed_request_human_decision", "governed_propose_memory",
	})
	profile := reg.ResolveProfile("reviewer", "Code Reviewer & Quality Engineer", "review the change", nil, gov)

	for _, tool := range profile.AllowedTools {
		if tool == "governed_apply_patch" {
			t.Fatalf("reviewer must never hold mutation authority, tools=%v", profile.AllowedTools)
		}
	}
	// Reviewer must retain read + submit-review.
	hasRead, hasSubmit := false, false
	for _, tool := range profile.AllowedTools {
		if tool == "governed_read_file" {
			hasRead = true
		}
		if tool == "governed_submit_review" {
			hasSubmit = true
		}
	}
	if !hasRead || !hasSubmit {
		t.Fatalf("reviewer must retain read+submit tools, got %v", profile.AllowedTools)
	}
}

// ---------------------------------------------------------------------------
// 11. Existing coordination and budget-governance invariants remain green.
// ---------------------------------------------------------------------------
func TestReview_11_CoordinationBudgetInvariantsPreserved(t *testing.T) {
	// Reviewer profile carries a larger read budget but MUST NOT silently
	// zero out MaxRevisions (the latent bug the fix corrects).
	reg := spike.NewCapabilityRegistry()
	gov := spike.NewPolicyGovernor([]string{"governed_read_file", "governed_submit_review"})
	profile := reg.ResolveProfile("reviewer", "Code Reviewer & Quality Engineer", "review", nil, gov)
	if profile.Limits == nil {
		t.Fatalf("reviewer profile should carry explicit read limits")
	}
	if profile.Limits.MaxToolCalls <= 10 {
		t.Fatalf("reviewer read budget must exceed the coder default of 10, got %d", profile.Limits.MaxToolCalls)
	}
	if profile.Limits.MaxReadFileLines < 300 {
		t.Fatalf("reviewer must be able to page large files, got %d", profile.Limits.MaxReadFileLines)
	}
	// The profile must not force MaxRevisions to zero.
	if profile.Limits.MaxRevisions != 0 {
		t.Fatalf("reviewer profile should not pin MaxRevisions, got %d", profile.Limits.MaxRevisions)
	}
}
