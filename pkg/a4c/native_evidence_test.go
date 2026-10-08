package a4c_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	spike "github.com/ormasoftchile/a4c/pkg/a4c"
)

func TestNativeGoTestRunnerProducesSnapshotBoundEvidence(t *testing.T) {
	workspace := t.TempDir()
	files := map[string]string{
		"go.mod":      "module example.com/evidence\n\ngo 1.22\n",
		"calc.go":     "package calc\n\nfunc Add(a, b int) int { return a + b }\n",
		"calc_test.go": "package calc\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) { if Add(2, 3) != 5 { t.Fatal(\"Add returned the wrong result\") } }\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte(content), 0644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	runner := &spike.NativeGoTestRunner{}
	exitCode, output, err := runner.RunTests(context.Background(), workspace)
	if err != nil {
		t.Fatalf("native Go test runner failed: %v\n%s", err, output)
	}
	if exitCode != 0 {
		t.Fatalf("native Go tests failed with exit code %d:\n%s", exitCode, output)
	}

	snapshotter := spike.NewWorkspaceSnapshotter()
	snapshotID, err := snapshotter.ComputeSnapshot(workspace)
	if err != nil {
		t.Fatalf("compute validated workspace snapshot: %v", err)
	}
	evidence := spike.ValidationEvidence{
		EvidenceID:          "native-test-evidence",
		WorkspaceSnapshotID: snapshotID,
		ExitCode:            exitCode,
		Valid:               true,
	}
	if err := snapshotter.ValidateAgainstEvidence(workspace, evidence); err != nil {
		t.Fatalf("native test evidence should validate for the unchanged workspace: %v", err)
	}

	if err := os.WriteFile(filepath.Join(workspace, "calc.go"), []byte("package calc\n\nfunc Add(a, b int) int { return a - b }\n"), 0644); err != nil {
		t.Fatalf("tamper with validated source: %v", err)
	}
	if err := snapshotter.ValidateAgainstEvidence(workspace, evidence); err == nil {
		t.Fatal("native test evidence must be rejected after the validated workspace changes")
	}
}
