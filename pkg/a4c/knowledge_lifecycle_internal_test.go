package a4c

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type kbHarness struct {
	repoDir string
	store   *DurableActivityStore
	svc     *TeamServiceImpl
	wtMgr   *WorktreeManager
	kbDir   string
}

func setupKBHarness(t *testing.T) *kbHarness {
	t.Helper()
	root := setKBRoot(t)
	repoDir := t.TempDir()
	runKBGit(t, repoDir, "init")
	runKBGit(t, repoDir, "config", "user.name", "KB Tester")
	runKBGit(t, repoDir, "config", "user.email", "kb@example.com")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# repo\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runKBGit(t, repoDir, "add", "README.md")
	runKBGit(t, repoDir, "commit", "-m", "init")

	dotDir := filepath.Join(repoDir, ".a4c")
	store, err := NewDurableActivityStore(filepath.Join(dotDir, "activities"))
	if err != nil {
		t.Fatal(err)
	}
	lifecycleStore, err := NewDurableLifecycleStore(filepath.Join(dotDir, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	memory, err := NewDurableMemoryStore(filepath.Join(dotDir, "memory"))
	if err != nil {
		t.Fatal(err)
	}
	journal, err := NewDurableIntentJournal(filepath.Join(dotDir, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	wtMgr, err := NewWorktreeManager(repoDir, filepath.Join(dotDir, "worktrees"))
	if err != nil {
		t.Fatal(err)
	}
	gov := NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_propose_memory",
	})
	svc := NewTeamService(repoDir, store, lifecycleStore, gov, memory, journal, wtMgr)

	kbDir, err := ResolveKnowledgeDir(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	_ = root
	return &kbHarness{repoDir: repoDir, store: store, svc: svc, wtMgr: wtMgr, kbDir: kbDir}
}

func (h *kbHarness) beginDirect(t *testing.T, objective string) string {
	t.Helper()
	res, err := h.svc.BeginTask(context.Background(), BeginTaskRequest{
		Objective: objective,
	})
	if err != nil {
		t.Fatalf("BeginTask: %v", err)
	}
	return res.TaskID
}

func TestKnowledgeLifecycle_AttestationNoDocument(t *testing.T) {
	h := setupKBHarness(t)
	taskID := h.beginDirect(t, "routine fix")

	res, err := h.svc.CompleteActivity(context.Background(), CompleteActivityRequest{
		TaskID: taskID, ActivityID: taskID, Actor: "coder", Summary: "fixed typo", Success: true,
		Knowledge: &KnowledgeClaim{Significant: false, Reason: "No significant reusable knowledge"},
	})
	if err != nil {
		t.Fatalf("CompleteActivity: %v", err)
	}
	if res.Knowledge == nil || res.Knowledge.Status != "assessed" || res.Knowledge.Significant {
		t.Fatalf("outcome = %+v, want assessed/not-significant", res.Knowledge)
	}

	details, err := h.svc.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	cov := details.KnowledgeCoverage
	if cov == nil || !cov.AssessmentRecorded || cov.AssessmentMissing || cov.AssessmentSignificant {
		t.Fatalf("coverage = %+v", cov)
	}

	// No documents created for an attestation.
	if _, err := os.Stat(h.kbDir); !os.IsNotExist(err) {
		entries, _ := os.ReadDir(h.kbDir)
		t.Fatalf("attestation must not create KB files, found %v", entries)
	}

	acc, err := h.svc.AcceptTask(context.Background(), AcceptTaskRequest{
		TaskID: taskID, AcceptedBy: "human_test", ConfirmationToken: "tok",
	})
	if err != nil {
		t.Fatalf("AcceptTask: %v", err)
	}
	for _, w := range acc.Warnings {
		if strings.Contains(w, "knowledge") {
			t.Fatalf("unexpected knowledge warning on assessed task: %q", w)
		}
	}
}

func TestKnowledgeLifecycle_VerifiedPathsClaim(t *testing.T) {
	h := setupKBHarness(t)
	doc := "# Finding\n\nThe pool exhausts under burst load.\n"
	if _, err := WriteKnowledgeFile(h.kbDir, "findings/2026-10-17-pool.md", doc); err != nil {
		t.Fatal(err)
	}
	taskID := h.beginDirect(t, "investigate pool exhaustion")

	res, err := h.svc.CompleteActivity(context.Background(), CompleteActivityRequest{
		TaskID: taskID, ActivityID: taskID, Actor: "coder", Summary: "found root cause", Success: true,
		Knowledge: &KnowledgeClaim{Significant: true, Paths: []string{"findings/2026-10-17-pool.md"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Knowledge == nil || res.Knowledge.Status != "verified" || !res.Knowledge.TaskLevel {
		t.Fatalf("outcome = %+v", res.Knowledge)
	}

	acc, err := h.svc.AcceptTask(context.Background(), AcceptTaskRequest{
		TaskID: taskID, AcceptedBy: "human_test", ConfirmationToken: "tok",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range acc.Warnings {
		if strings.Contains(w, "knowledge") {
			t.Fatalf("verified assessment must not warn: %q", w)
		}
	}
}

func TestKnowledgeLifecycle_MissingClaimIsPendingAndVisible(t *testing.T) {
	h := setupKBHarness(t)
	taskID := h.beginDirect(t, "claim a document that was never written")

	res, err := h.svc.CompleteActivity(context.Background(), CompleteActivityRequest{
		TaskID: taskID, ActivityID: taskID, Actor: "coder", Summary: "done", Success: true,
		Knowledge: &KnowledgeClaim{Significant: true, Paths: []string{"findings/never-written.md"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Knowledge == nil || res.Knowledge.Status != "pending" {
		t.Fatalf("unverified claim must be pending, got %+v", res.Knowledge)
	}
	// Completion itself still succeeded.
	rec, err := h.store.Get(taskID)
	if err != nil || rec.Status != StatusCompleted {
		t.Fatalf("completion must succeed despite pending knowledge: %v %+v", err, rec)
	}

	details, _ := h.svc.GetTask(context.Background(), taskID)
	if details.KnowledgeCoverage == nil || details.KnowledgeCoverage.AssessmentStatus != "pending" {
		t.Fatalf("coverage = %+v", details.KnowledgeCoverage)
	}

	acc, err := h.svc.AcceptTask(context.Background(), AcceptTaskRequest{
		TaskID: taskID, AcceptedBy: "human_test", ConfirmationToken: "tok",
	})
	if err != nil {
		t.Fatalf("acceptance must not be blocked: %v", err)
	}
	found := false
	for _, w := range acc.Warnings {
		if strings.Contains(w, "knowledge") {
			found = true
		}
	}
	if !found {
		t.Fatalf("pending preservation must surface a warning, got %v", acc.Warnings)
	}
}

func TestKnowledgeLifecycle_MissingAssessmentWarns(t *testing.T) {
	h := setupKBHarness(t)
	taskID := h.beginDirect(t, "no disposition at all")
	if _, err := h.svc.CompleteActivity(context.Background(), CompleteActivityRequest{
		TaskID: taskID, ActivityID: taskID, Actor: "coder", Summary: "done", Success: true,
	}); err != nil {
		t.Fatal(err)
	}

	acc, err := h.svc.AcceptTask(context.Background(), AcceptTaskRequest{
		TaskID: taskID, AcceptedBy: "human_test", ConfirmationToken: "tok",
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range acc.Warnings {
		if strings.Contains(w, "no knowledge assessment") {
			found = true
		}
	}
	if !found {
		t.Fatalf("absent disposition must warn, got %v", acc.Warnings)
	}
}

func TestKnowledgeLifecycle_InlineContentFallback(t *testing.T) {
	h := setupKBHarness(t)
	taskID := h.beginDirect(t, "host cannot write outside workspace")
	content := "# Experiment\n\nRejected approach X: fails under concurrency.\n"

	res, err := h.svc.CompleteActivity(context.Background(), CompleteActivityRequest{
		TaskID: taskID, ActivityID: taskID, Actor: "coder", Summary: "done", Success: true,
		Knowledge: &KnowledgeClaim{
			Significant: true,
			FileName:    "experiments/2026-10-17-approach-x.md",
			Content:     content,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Knowledge == nil || res.Knowledge.Status != "verified" {
		t.Fatalf("inline fallback outcome = %+v", res.Knowledge)
	}
	written, err := os.ReadFile(filepath.Join(h.kbDir, "experiments", "2026-10-17-approach-x.md"))
	if err != nil || string(written) != content {
		t.Fatalf("written content = %q (%v)", written, err)
	}

	// A second inline write to the same name must refuse and stay pending.
	res2, err := h.svc.CompleteActivity(context.Background(), CompleteActivityRequest{
		TaskID: taskID, ActivityID: taskID, Actor: "coder", Summary: "again", Success: true,
		Knowledge: &KnowledgeClaim{
			Significant: true,
			FileName:    "experiments/2026-10-17-approach-x.md",
			Content:     "# Replacement\n",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Knowledge == nil || res2.Knowledge.Status != "pending" || !strings.Contains(res2.Knowledge.Reason, "overwrite") {
		t.Fatalf("overwrite refusal outcome = %+v", res2.Knowledge)
	}
	unchanged, _ := os.ReadFile(filepath.Join(h.kbDir, "experiments", "2026-10-17-approach-x.md"))
	if string(unchanged) != content {
		t.Fatalf("existing document was replaced: %q", unchanged)
	}
}

func TestKnowledgeLifecycle_AmendPreservesCompletionEvidence(t *testing.T) {
	h := setupKBHarness(t)
	taskID := h.beginDirect(t, "reviewed-mode amendment")

	// Initial completion with evidence and a candidate claim.
	if _, err := WriteKnowledgeFile(h.kbDir, "findings/coder-candidate.md", "# Candidate\n\nCoder's preliminary reading.\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.CompleteActivity(context.Background(), CompleteActivityRequest{
		TaskID: taskID, ActivityID: taskID, Actor: "coder", Summary: "implemented feature",
		ValidationEvidence: "ev-42", Success: true,
		FilesModified: []string{"feature.go"},
		Knowledge:     &KnowledgeClaim{Significant: true, Paths: []string{"findings/coder-candidate.md"}},
	}); err != nil {
		t.Fatal(err)
	}

	// Reviewer's finding invalidates the coder's interpretation; lead amends
	// the task-level assessment after review.
	if _, err := WriteKnowledgeFile(h.kbDir, "findings/final-conclusion.md", "# Finding\n\nCorrected after review: the candidate misread the failure mode.\n"); err != nil {
		t.Fatal(err)
	}
	amend, err := h.svc.CompleteActivity(context.Background(), CompleteActivityRequest{
		TaskID: taskID, ActivityID: taskID, Actor: "coder", Summary: "", Success: true,
		Knowledge: &KnowledgeClaim{Significant: true, Paths: []string{"findings/final-conclusion.md"}},
	})
	if err != nil {
		t.Fatalf("amendment: %v", err)
	}
	if amend.Knowledge == nil || amend.Knowledge.Status != "verified" || !amend.Knowledge.TaskLevel {
		t.Fatalf("amendment outcome = %+v", amend.Knowledge)
	}

	rec, err := h.store.Get(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != StatusCompleted {
		t.Fatalf("status changed by amendment: %s", rec.Status)
	}
	if ev, _ := rec.Result.Outputs["evidence"].(string); ev != "ev-42" {
		t.Fatalf("completion evidence was overwritten: %q", ev)
	}
	if sum, _ := rec.Result.Outputs["summary"].(string); sum != "implemented feature" {
		t.Fatalf("summary was overwritten: %q", sum)
	}
	files, _ := rec.Result.Outputs["files_modified"].([]string)
	if len(files) != 1 || files[0] != "feature.go" {
		// JSON round-trip may produce []any; accept either shape.
		if arr, ok := rec.Result.Outputs["files_modified"].([]any); !ok || len(arr) != 1 {
			t.Fatalf("files_modified was overwritten: %#v", rec.Result.Outputs["files_modified"])
		}
	}
	// The candidate document still exists — observations are never erased.
	if _, err := os.Stat(filepath.Join(h.kbDir, "findings", "coder-candidate.md")); err != nil {
		t.Fatalf("candidate document must be preserved: %v", err)
	}

	// Unauthorized actors cannot amend.
	if _, err := h.svc.CompleteActivity(context.Background(), CompleteActivityRequest{
		TaskID: taskID, ActivityID: taskID, Actor: "charlie", Summary: "", Success: true,
		Knowledge: &KnowledgeClaim{Significant: false, Reason: "x"},
	}); err == nil {
		t.Fatal("reviewer must not amend the task-level assessment")
	}
}

func TestKnowledgeLifecycle_KBSurvivesWorktreeAndInit(t *testing.T) {
	h := setupKBHarness(t)
	doc := "# Durable finding\n\nMust survive cleanup.\n"
	abs, err := WriteKnowledgeFile(h.kbDir, "findings/durable.md", doc)
	if err != nil {
		t.Fatal(err)
	}

	taskID := h.beginDirect(t, "cleanup survivor")
	ctx := context.Background()
	rec, err := h.wtMgr.AcquireWorktree(ctx, taskID, "host")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.wtMgr.RemoveWorktree(ctx, taskID, true); err != nil {
		t.Fatalf("remove worktree: %v", err)
	}
	_ = rec

	// Simulate 'a4c init' re-running directory creation.
	dotDir := filepath.Join(h.repoDir, ".a4c")
	for _, d := range []string{
		dotDir,
		filepath.Join(dotDir, "activities"),
		filepath.Join(dotDir, "worktrees"),
	} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}

	got, err := os.ReadFile(abs)
	if err != nil || string(got) != doc {
		t.Fatalf("KB document did not survive cleanup: %q (%v)", got, err)
	}
}

func TestKnowledgeLifecycle_TeamContextExposesKBDir(t *testing.T) {
	h := setupKBHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tc, err := h.svc.GetTeamContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if tc.KnowledgeDir != h.kbDir {
		t.Fatalf("team_context knowledge_dir = %q, want %q", tc.KnowledgeDir, h.kbDir)
	}
}
