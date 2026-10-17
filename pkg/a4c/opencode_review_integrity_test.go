package a4c

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOpenCodeReviewVerdict_Directive: explicit directives take priority.
func TestOpenCodeReviewVerdict_Directive(t *testing.T) {
	if rev, ok := extractReviewVerdict("Looks fine. [REVIEW_APPROVED]"); !ok || rev.Verdict != ReviewVerdictApproved || !rev.Approved {
		t.Fatalf("expected approved directive, got %+v ok=%v", rev, ok)
	}
	if rev, ok := extractReviewVerdict("Bug at line 10. [REVIEW_REJECTED: divide-by-zero]"); !ok || rev.Verdict != ReviewVerdictRejected || rev.Comments != "divide-by-zero" {
		t.Fatalf("expected rejected directive, got %+v", rev)
	}
	if rev, ok := extractReviewVerdict("Cannot verify. [REVIEW_BLOCKED: no evidence]"); !ok || rev.Verdict != ReviewVerdictVerificationBlocked {
		t.Fatalf("expected verification_blocked, got %+v", rev)
	}
}

// TestOpenCodeReviewVerdict_JSON: structured JSON verdict object.
func TestOpenCodeReviewVerdict_JSON(t *testing.T) {
	content := `Here is my verdict: {"verdict":"approved","comments":"clean fix","blockers":[]}`
	if rev, ok := extractReviewVerdict(content); !ok || rev.Verdict != ReviewVerdictApproved || rev.Comments != "clean fix" {
		t.Fatalf("expected JSON approved, got %+v ok=%v", rev, ok)
	}
	content = `{"verdict":"rejected","comments":"missing null check","blockers":["null deref"]}`
	if rev, ok := extractReviewVerdict(content); !ok || rev.Verdict != ReviewVerdictRejected || len(rev.Blockers) != 1 {
		t.Fatalf("expected JSON rejected with blocker, got %+v", rev)
	}
	if rev, ok := extractReviewVerdict(`{"verdict":"verification_blocked","comments":"no logs"}`); !ok || rev.Verdict != ReviewVerdictVerificationBlocked {
		t.Fatalf("expected JSON blocked, got %+v", rev)
	}
}

// TestOpenCodeReviewVerdict_ConservativeWords: standalone verdict words the
// models actually emit (e.g. "Review verdict: approve") are recognized, but a
// missing verdict is NOT treated as rejection.
func TestOpenCodeReviewVerdict_ConservativeWords(t *testing.T) {
	// The observed MiMo output.
	if rev, ok := extractReviewVerdict("The file is valid. Review verdict: approve."); !ok || rev.Verdict != ReviewVerdictApproved {
		t.Fatalf("expected approve (framed verdict), got %+v ok=%v", rev, ok)
	}
	if rev, ok := extractReviewVerdict("I approve this change; no defects found. LGTM."); !ok || rev.Verdict != ReviewVerdictApproved {
		t.Fatalf("expected approved (lgtm/I approve), got %+v", rev)
	}
	if rev, ok := extractReviewVerdict("There is a real bug here, rejecting. reject"); !ok || rev.Verdict != ReviewVerdictRejected {
		t.Fatalf("expected rejected, got %+v", rev)
	}
	// Missing / ambiguous verdict: must NOT become a rejection.
	if _, ok := extractReviewVerdict("I inspected the files and read the diff carefully."); ok {
		t.Fatalf("a verdict-less response must not produce a verdict (missing != rejection)")
	}
	if _, ok := extractReviewVerdict(""); ok {
		t.Fatalf("empty content must not produce a verdict")
	}
}

// TestOpenCodeReviewVerdict_NoFalseApproval: prose containing 'approve' in an
// unrelated context must not be misread as an approval.
func TestOpenCodeReviewVerdict_NoFalseApproval(t *testing.T) {
	// "approachable" contains "approach", not a standalone approve verdict.
	if _, ok := extractReviewVerdict("The code is approachable and readable, though I have not decided yet."); ok {
		t.Fatalf("non-verdict prose must not be treated as a verdict")
	}
}

// TestWorkspaceSnapshot_IgnoresBuildArtifactsButCatchesSource: generated build/
// outputs do not change the snapshot (so legitimate C builds do not trip the
// integrity check), while unauthorized source modifications still do.
func TestWorkspaceSnapshot_IgnoresBuildArtifactsButCatchesSource(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte(content), 0o644)
	}
	write("src/main.c", "int main(){}\n")
	write("build/obj.o", "binary-garbage\n")

	snap := NewWorkspaceSnapshotter()
	before, err := snap.ComputeSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}

	// A legitimate build emits a new artifact under build/ — snapshot unchanged.
	write("build/test-runner", "new-binary\n")
	after, err := snap.ComputeSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("generated build artifacts must not change the workspace snapshot")
	}
	if err := snap.ValidateIntegrity(before, after); err != nil {
		t.Fatalf("a legitimate build must not trip integrity: %v", err)
	}

	// An unauthorized SOURCE modification must still fail integrity.
	write("src/main.c", "int main(){ /* tampered */ }\n")
	afterSrc, err := snap.ComputeSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if before == afterSrc {
		t.Fatalf("an unauthorized source change must change the snapshot")
	}
	if err := snap.ValidateIntegrity(before, afterSrc); err == nil {
		t.Fatalf("an unauthorized source modification must fail integrity validation")
	}
}
