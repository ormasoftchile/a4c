package a4c_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ormasoftchile/a4c/pkg/a4c"
)

func TestVersionIsStampedIntoBinary(t *testing.T) {
	if a4c.Version == "" {
		t.Fatal("a4c.Version is empty; the VERSION file is not embedded")
	}
	if _, err := a4c.BumpPatch(a4c.Version); err != nil {
		t.Fatalf("embedded version %q is not a bumpable major.minor.patch version: %v", a4c.Version, err)
	}
}

func TestBumpPatch(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "0.1.0", want: "0.1.1"},
		{in: "0.1.9", want: "0.1.10"},
		{in: "1.2.3", want: "1.2.4"},
		{in: " 2.0.0\n", want: "2.0.1"}, // file contents carry a trailing newline
		{in: "1.2", wantErr: true},
		{in: "1.2.3.4", wantErr: true},
		{in: "1.2.x", wantErr: true},
		{in: "v1.2.3", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tc := range cases {
		got, err := a4c.BumpPatch(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("BumpPatch(%q) = %q, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("BumpPatch(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("BumpPatch(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPendingVersionBump(t *testing.T) {
	dir := t.TempDir()

	// A workspace without a version file has nothing to bump.
	if _, _, ok := a4c.PendingVersionBump(dir); ok {
		t.Fatal("expected no pending bump in a workspace without a version file")
	}

	path := a4c.WorkspaceVersionFile(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("0.1.0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	from, to, ok := a4c.PendingVersionBump(dir)
	if !ok || from != "0.1.0" || to != "0.1.1" {
		t.Fatalf("PendingVersionBump = (%q, %q, %v), want (0.1.0, 0.1.1, true)", from, to, ok)
	}

	// Unparseable content is not silently passed through.
	if err := os.WriteFile(path, []byte("nightly\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := a4c.PendingVersionBump(dir); ok {
		t.Fatal("expected no pending bump for an unparseable version")
	}
}

func TestVersionFilePathLocation(t *testing.T) {
	got := filepath.ToSlash(a4c.VersionFilePath)
	if got != "pkg/a4c/VERSION" {
		t.Fatalf("VersionFilePath = %q, want pkg/a4c/VERSION", got)
	}
}

func TestNextReleaseRules(t *testing.T) {
	cases := []struct {
		name         string
		current      string
		recordedFrom string
		recordedTo   string
		want         string
		wantApply    bool
	}{
		{
			name: "recorded bump applies as approved",
			// The acceptance recorded 0.1.0 -> 0.1.1; integration must land it.
			current: "0.1.0", recordedFrom: "0.1.0", recordedTo: "0.1.1",
			want: "0.1.1", wantApply: true,
		},
		{
			name: "retried integration does not bump twice",
			// The tree already carries the bump the acceptance recorded.
			current: "0.1.1", recordedFrom: "0.1.0", recordedTo: "0.1.1",
			want: "0.1.1", wantApply: false,
		},
		{
			name: "the change itself moved the version: recompute from the tree",
			// The approved change shipped 0.2.0; the release rule advances
			// whatever is actually being shipped, not a stale baseline.
			current: "0.2.0", recordedFrom: "0.1.0", recordedTo: "0.1.1",
			want: "0.2.1", wantApply: true,
		},
		{
			name: "no recorded bump still advances the patch",
			// A task accepted before this rule existed.
			current: "1.0.0", recordedFrom: "", recordedTo: "",
			want: "1.0.1", wantApply: true,
		},
	}
	for _, tc := range cases {
		got, apply := a4c.NextRelease(tc.current, tc.recordedFrom, tc.recordedTo)
		if got != tc.want || apply != tc.wantApply {
			t.Errorf("%s: NextRelease(%q, %q, %q) = (%q, %v), want (%q, %v)",
				tc.name, tc.current, tc.recordedFrom, tc.recordedTo, got, apply, tc.want, tc.wantApply)
		}
	}
}

// The full release rule: an accepted change records the patch bump, and
// integration lands it as its own commit on the task branch.
func TestReleaseRule_AcceptRecordsBumpIntegrateAppliesIt(t *testing.T) {
	h := setupServiceTestHarness(t)
	ctx := context.Background()

	// The workspace carries a release version.
	versionPath := a4c.WorkspaceVersionFile(h.repoDir)
	if err := os.MkdirAll(filepath.Dir(versionPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(versionPath, []byte("0.1.0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGitCmd(t, h.repoDir, "add", "pkg/a4c/VERSION")
	runGitCmd(t, h.repoDir, "commit", "-m", "chore: add version file")

	ref, err := h.service.SubmitTask(ctx, a4c.SubmitTaskRequest{
		Objective: "Add helper utility",
		Strategy:  a4c.StrategyReviewed,
	})
	if err != nil {
		t.Fatalf("SubmitTask failed: %v", err)
	}

	wtRec, err := h.wtMgr.GetWorktree(ref.TaskID)
	if err != nil {
		t.Fatalf("GetWorktree failed: %v", err)
	}

	helperFile := filepath.Join(wtRec.WorktreePath, "helper.go")
	if err := os.WriteFile(helperFile, []byte("package main\n\nfunc Helper() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGitCmd(t, wtRec.WorktreePath, "add", "helper.go")
	runGitCmd(t, wtRec.WorktreePath, "commit", "-m", "feat: add helper")

	_ = h.store.Complete(ref.TaskID, a4c.ActivityResult{ID: ref.TaskID, Status: a4c.StatusCompleted})
	reviewerID := ref.TaskID + "-rev"
	_ = h.store.Complete(reviewerID, a4c.ActivityResult{
		ID: reviewerID, Status: a4c.StatusCompleted,
		Outputs: map[string]any{"approved": true, "review_comments": "LGTM"},
	})

	accRes, err := h.service.AcceptTask(ctx, a4c.AcceptTaskRequest{
		TaskID: ref.TaskID, AcceptedBy: "human_cli", IsInteractive: true,
	})
	if err != nil {
		t.Fatalf("AcceptTask failed: %v", err)
	}
	if accRes.VersionBumpTo != "0.1.1" {
		t.Fatalf("accept recorded version bump %q, want 0.1.1", accRes.VersionBumpTo)
	}

	// Acceptance itself leaves the checkout untouched.
	data, err := os.ReadFile(versionPath)
	if err != nil || strings.TrimSpace(string(data)) != "0.1.0" {
		t.Fatalf("acceptance modified the checkout version: %q (%v)", data, err)
	}

	intRes, err := h.service.IntegrateTask(ctx, a4c.IntegrateTaskRequest{TaskID: ref.TaskID})
	if err != nil {
		t.Fatalf("IntegrateTask failed: %v", err)
	}
	if intRes.Version != "0.1.1" {
		t.Fatalf("integration landed version %q, want 0.1.1", intRes.Version)
	}

	data, err = os.ReadFile(versionPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != "0.1.1" {
		t.Fatalf("workspace version = %q, want 0.1.1", got)
	}

	// The bump is a commit of its own, so the approved change and the
	// release rule stay separately inspectable.
	log := runGitCmd(t, h.repoDir, "log", "--pretty=%s")
	if !strings.Contains(log, "chore(release): bump version to 0.1.1") {
		t.Fatalf("expected a release bump commit, got log:\n%s", log)
	}
	if !strings.Contains(log, "feat: add helper") {
		t.Fatalf("expected the approved change in the merged history, got log:\n%s", log)
	}
	if _, err := os.Stat(filepath.Join(h.repoDir, "helper.go")); err != nil {
		t.Fatalf("approved change was not integrated: %v", err)
	}
}

// A workspace without a version file must integrate untouched: the release
// rule is opt-in via carrying a version, not a surprise for every project.
func TestReleaseRule_NoVersionFileIntegrationUnchanged(t *testing.T) {
	h := setupServiceTestHarness(t)
	ctx := context.Background()

	ref, err := h.service.SubmitTask(ctx, a4c.SubmitTaskRequest{Objective: "noop", Strategy: a4c.StrategyDirect})
	if err != nil {
		t.Fatalf("SubmitTask failed: %v", err)
	}
	wtRec, err := h.wtMgr.GetWorktree(ref.TaskID)
	if err != nil {
		t.Fatalf("GetWorktree failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wtRec.WorktreePath, "helper.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGitCmd(t, wtRec.WorktreePath, "add", "helper.go")
	runGitCmd(t, wtRec.WorktreePath, "commit", "-m", "feat: helper")

	_ = h.store.Complete(ref.TaskID, a4c.ActivityResult{ID: ref.TaskID, Status: a4c.StatusCompleted})

	accRes, err := h.service.AcceptTask(ctx, a4c.AcceptTaskRequest{TaskID: ref.TaskID, AcceptedBy: "human_cli", IsInteractive: true})
	if err != nil {
		t.Fatalf("AcceptTask failed: %v", err)
	}
	if accRes.VersionBumpTo != "" {
		t.Fatalf("accept recorded a version bump %q in a workspace without a version file", accRes.VersionBumpTo)
	}

	intRes, err := h.service.IntegrateTask(ctx, a4c.IntegrateTaskRequest{TaskID: ref.TaskID})
	if err != nil {
		t.Fatalf("IntegrateTask failed: %v", err)
	}
	if intRes.Version != "" {
		t.Fatalf("integration reported version %q in a workspace without a version file", intRes.Version)
	}
	if _, err := os.Stat(a4c.WorkspaceVersionFile(h.repoDir)); !os.IsNotExist(err) {
		t.Fatalf("integration created a version file where none existed (err=%v)", err)
	}
}
