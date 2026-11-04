package a4c

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCopilotProcessRunner_EmptyPromptErrors(t *testing.T) {
	runner := &CopilotProcessRunner{copilotBin: "/bin/echo"}
	_, err := runner.Run(context.Background(), ProcessRunRequest{
		Prompt: "",
	})
	if err == nil {
		t.Fatal("expected error for empty prompt, got nil")
	}
}

func TestCopilotProcessRunner_ExpectedOutputFileValidation(t *testing.T) {
	tempDir := t.TempDir()
	expectedFile := filepath.Join(tempDir, "output.json")

	// Mock binary using echo that exits 0 but does NOT create the file
	runner := &CopilotProcessRunner{copilotBin: "/bin/echo"}
	_, err := runner.Run(context.Background(), ProcessRunRequest{
		Prompt:             "echo skipped",
		ExpectedOutputFile: expectedFile,
		Timeout:            5 * time.Second,
	})
	if err == nil {
		t.Fatal("expected error when expected file was not created, got nil")
	}
	if !strings.Contains(err.Error(), "was not created") {
		t.Errorf("unexpected error: %v", err)
	}

	// Now create the file and test with mock runner
	if err := os.WriteFile(expectedFile, []byte(`{"status":"ok"}`), 0644); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	// Mock binary using echo that exits 0
	runner = &CopilotProcessRunner{copilotBin: "/bin/echo"}
	res, err := runner.Run(context.Background(), ProcessRunRequest{
		Prompt:             "hello",
		ExpectedOutputFile: expectedFile,
		Timeout:            5 * time.Second,
	})
	if err != nil {
		t.Fatalf("expected success when file exists, got: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("res.ExitCode = %d, want 0", res.ExitCode)
	}
}

func TestCopilotProcessRunner_StreamingOutput(t *testing.T) {
	var liveBuf bytes.Buffer
	runner := &CopilotProcessRunner{copilotBin: "/bin/echo"}
	res, err := runner.Run(context.Background(), ProcessRunRequest{
		Prompt:  "streaming-test",
		Prefix:  "[squad] ",
		Stdout:  &liveBuf,
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if !strings.Contains(res.Stdout, "-p") || !strings.Contains(res.Stdout, "streaming-test") {
		t.Errorf("unexpected stdout: %q", res.Stdout)
	}
	if !strings.Contains(liveBuf.String(), "[squad]") {
		t.Errorf("liveBuf missing prefix: %q", liveBuf.String())
	}
}
