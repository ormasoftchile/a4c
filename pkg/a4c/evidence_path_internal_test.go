package a4c

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestEvidencePath_TwoWorkspacesReviewerCanReadLog reproduces the exact defect
// from the benchmark: the reviewer was told to read a .a4c/evidence log that
// did not exist in its disposable workspace. Here, evidence produced in a
// SEPARATE producer workspace is materialized into the reviewer's workspace and
// the reviewer-side path must resolve and read back the full log.
func TestEvidencePath_TwoWorkspacesReviewerCanReadLog(t *testing.T) {
	// Producer workspace (coder's worktree).
	producer := t.TempDir()
	if err := os.MkdirAll(filepath.Join(producer, ".a4c", "evidence"), 0o700); err != nil {
		t.Fatal(err)
	}
	fullLog := "=== RUN TestCypher\n--- PASS: TestCypher (0.00s)\nPASS\nok  \texample.com/c\t0.02s\n"
	producerLog := filepath.Join(producer, ".a4c", "evidence", "evi-1.log")
	if err := os.WriteFile(producerLog, []byte(fullLog), 0o600); err != nil {
		t.Fatal(err)
	}

	// Separate reviewer workspace (disposable copy — no .a4c present).
	reviewer := t.TempDir()

	evidence := &ValidationEvidence{
		EvidenceID:          "evi-1",
		Command:             []string{"make", "test"},
		ExitCode:            0,
		Valid:               true,
		TestsExecuted:       2,
		LogPath:             filepath.Join(".a4c", "evidence", "evi-1.log"), // producer-relative
		WorkspaceDir:        producer,
		OutputSummary:       "PASS",
		WorkspaceSnapshotID: "snap",
		CompletedAt:         time.Now(),
	}

	c := &TeamCoordinator{}
	bundle := c.materializeReviewEvidence(reviewer, &ActivityRequest{Objective: "obj"},
		"diff --git a/x b/x", []string{"x.c"}, nil, nil, evidence, "VALID")

	if bundle == nil {
		t.Fatalf("expected a bundle")
	}
	if !bundle.LogAvailable {
		t.Fatalf("reviewer must be able to read the materialized log; missing=%v", bundle.Missing)
	}
	// The referenced path is review-workspace-relative and must resolve there.
	if filepath.IsAbs(bundle.LogPath) {
		t.Fatalf("bundle log path must be review-workspace-relative, got absolute %q", bundle.LogPath)
	}
	if strings.HasPrefix(filepath.ToSlash(bundle.LogPath), ".a4c/") {
		t.Fatalf("bundle log path must not be a producer .a4c path, got %q", bundle.LogPath)
	}
	abs := filepath.Join(reviewer, bundle.LogPath)
	data, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("REVIEWER READ FAILED (the historical defect): %v", err)
	}
	if !strings.Contains(string(data), "TestCypher") {
		t.Fatalf("materialized log missing full output: %q", string(data))
	}
}

// TestEvidencePath_MissingLogReportedExplicitly: when the producer log cannot
// be read, the bundle reports it explicitly instead of handing an unresolvable
// path to the reviewer.
func TestEvidencePath_MissingLogReportedExplicitly(t *testing.T) {
	producer := t.TempDir() // no log written here
	reviewer := t.TempDir()

	evidence := &ValidationEvidence{
		EvidenceID:   "evi-missing",
		Command:      []string{"make", "test"},
		ExitCode:     0,
		Valid:        true,
		LogPath:      filepath.Join(".a4c", "evidence", "does-not-exist.log"),
		WorkspaceDir: producer,
		CompletedAt:  time.Now(),
	}
	c := &TeamCoordinator{}
	bundle := c.materializeReviewEvidence(reviewer, &ActivityRequest{Objective: "obj"},
		"", nil, nil, nil, evidence, "VALID")

	if bundle.LogAvailable {
		t.Fatalf("a missing producer log must not be reported available")
	}
	if len(bundle.Missing) == 0 {
		t.Fatalf("missing evidence must be reported explicitly")
	}
}

// TestEvidencePath_ChangedFilesIndexListsTests: the changed-files index includes
// test names and line counts so a reviewer can establish coverage without
// paging whole files.
func TestEvidencePath_ChangedFilesIndexListsTests(t *testing.T) {
	ws := t.TempDir()
	testSrc := "package c\n\nfunc TestAlpha(t *testing.T) {}\nfunc TestBeta(t *testing.T) {}\n"
	if err := os.WriteFile(filepath.Join(ws, "test_cypher.c"), []byte(testSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	summary := buildChangedFilesSummary(ws, []string{"test_cypher.c"})
	if !strings.Contains(summary, "test_cypher.c") {
		t.Fatalf("index must list the file: %q", summary)
	}
	if !strings.Contains(summary, "TestAlpha") || !strings.Contains(summary, "TestBeta") {
		t.Fatalf("index must list test names so the reviewer need not page the file: %q", summary)
	}
	if !strings.Contains(summary, "lines)") {
		t.Fatalf("index must include a line count: %q", summary)
	}
}

// TestEvidencePath_InternalA4CFilesExcludedFromDeliverable: .a4c operational
// state is never part of the changed-file list or deliverable.
func TestEvidencePath_InternalA4CFilesExcludedFromDeliverable(t *testing.T) {
	files := []string{
		"src/cypher/cypher.c",
		".a4c/evidence/evi-1.log",
		".a4c/budgets/b.json",
		"tests/test_cypher.c",
	}
	out := excludeA4CInternal(files)
	for _, f := range out {
		if isA4CInternal(f) {
			t.Fatalf(".a4c internal path leaked into deliverable: %s", f)
		}
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 source files, got %v", out)
	}
}
