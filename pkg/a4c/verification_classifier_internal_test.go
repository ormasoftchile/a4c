package a4c

import "testing"

// TestVerification_CSuiteOutputNotMisreadAsVacuous pins the real native C
// test-runner output format observed in the benchmark, ensuring a passing C
// suite is never classified vacuous (a production defect the live run exposed:
// the C suite printed "5726 passed" but was misread as vacuous).
func TestVerification_CSuiteOutputNotMisreadAsVacuous(t *testing.T) {
	cOutput := "cd /repo && build/c/test-runner\n\n" +
		"  codebase-memory-mcp  C test suite\n\n=== arena ===\n" +
		"  arena_init_default                                     PASS\n" +
		"  arena_init_sized                                       PASS\n\n" +
		"PASS\n\n  5726 passed\n"
	testsExecuted, vacuous := classifyVerification("make", 0, cOutput)
	if vacuous {
		t.Fatalf("a passing C suite (5726 passed) must NOT be vacuous")
	}
	if testsExecuted == 0 {
		t.Fatalf("expected the C suite to count as executed")
	}
	// A failing C suite is a genuine failure, not vacuous.
	failOutput := "  some_test   FAIL\n\nFAIL\n\n3 passed, 1 failed\n"
	if _, vacuousFail := classifyVerification("make", 1, failOutput); vacuousFail {
		t.Fatalf("a failing C suite is not vacuous")
	}
	// A make that truly runs no tests (empty output) is vacuous.
	if _, vacuousEmpty := classifyVerification("make", 0, ""); !vacuousEmpty {
		t.Fatalf("empty make output must be vacuous")
	}
	// An opaque non-empty output with no test markers is fail-closed vacuous.
	if _, vacuousOpaque := classifyVerification("make", 0, "some build output\nlinking...\n"); !vacuousOpaque {
		t.Fatalf("opaque make output with no test markers must be vacuous (fail-closed)")
	}
	// Regression: a huge build log containing incidental "tests=0 tests_file=0"
	// pipeline stats must NOT override a suite that clearly passed (this was a
	// live production defect: 5728 passed was misread vacuous).
	logWithStats := "level=info msg=pass.done pass=tests tests=0 tests_file=0\n" +
		"  arena_init_default    PASS\n\nPASS\n\n  5728 passed\n"
	if n, vac := classifyVerification("make", 0, logWithStats); vac || n == 0 {
		t.Fatalf("positive PASS evidence must win over incidental tests=0 stats, got n=%d vac=%t", n, vac)
	}
	// Conversely, ONLY "tests=0" stats with no passing evidence is vacuous.
	if _, vac := classifyVerification("make", 0, "level=info msg=pass.done pass=tests tests=0 tests_file=0\n"); !vac {
		t.Fatalf("output with only tests=0 stats and no pass evidence must be vacuous")
	}
}

// TestVerification_GoClassifierStillCorrect: the Go-specific analyzer is
// unaffected by the C marker changes.
func TestVerification_GoClassifierStillCorrect(t *testing.T) {
	if n, vac := classifyVerification("go", 0, "?   \tpkg\t[no test files]\n"); !vac || n != 0 {
		t.Fatalf("[no test files] must stay vacuous, got n=%d vac=%t", n, vac)
	}
	if n, vac := classifyVerification("go", 0, "ok  \tpkg\t0.1s\n"); vac || n == 0 {
		t.Fatalf("ok output must stay non-vacuous, got n=%d vac=%t", n, vac)
	}
}
