package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureOutput runs f while capturing stdout and stderr.
func captureOutput(t *testing.T, f func()) (string, string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = wOut, wErr
	defer func() {
		os.Stdout, os.Stderr = oldOut, oldErr
	}()
	f()
	_ = wOut.Close()
	_ = wErr.Close()
	outBuf := make([]byte, 64<<10)
	errBuf := make([]byte, 64<<10)
	nOut, _ := rOut.Read(outBuf)
	nErr, _ := rErr.Read(errBuf)
	return string(outBuf[:nOut]), string(errBuf[:nErr])
}

func noTaskState(t *testing.T, dir string) {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(dir, ".a4c", "activities", "task-*.json"))
	if len(matches) != 0 {
		t.Fatalf("task state was created: %v", matches)
	}
}

// CLI help must never create a task: `a4c task start --help` used to create a
// task whose objective was "--help" (Stage 1 finding R10).
func TestTaskHelpNeverCreatesTask(t *testing.T) {
	for _, sub := range []string{"submit", "start", "adopt", "run", "accept", "integrate", "reject"} {
		dir := t.TempDir()
		out, errOut := captureOutput(t, func() {
			runTask([]string{"--dir", dir, sub, "--help"})
		})
		if !strings.Contains(out, "Usage: a4c task") {
			t.Fatalf("%s --help printed no usage: stdout=%q stderr=%q", sub, out, errOut)
		}
		noTaskState(t, dir)
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Fatalf("%s --help created files in the workspace: %v", sub, entries)
		}
	}
}

// Flag-like values must never become objectives.
func TestTaskFlagLikeObjectiveRejected(t *testing.T) {
	dir := t.TempDir()
	captureOutput(t, func() { runInit([]string{dir}) })

	for _, tc := range []struct{ sub, flag string }{
		{"submit", "--oops"},
		{"start", "--help-me"},
		{"adopt", "-x"},
	} {
		_, errOut := captureOutput(t, func() {
			runTask([]string{"--dir", dir, tc.sub, tc.flag})
		})
		if !strings.Contains(errOut, "refusing flag-like objective") {
			t.Fatalf("%s %q: expected refusal, got stderr=%q", tc.sub, tc.flag, errOut)
		}
		noTaskState(t, dir)
	}
}

// Unsupported options must be explicit errors, not silently dropped values.
func TestTaskUnsupportedOptionRejected(t *testing.T) {
	dir := t.TempDir()
	captureOutput(t, func() { runInit([]string{dir}) })

	_, errOut := captureOutput(t, func() {
		runTask([]string{"--dir", dir, "submit", "implement the feature", "--bogus-option"})
	})
	if !strings.Contains(errOut, "unsupported argument") {
		t.Fatalf("expected explicit unsupported-option error, got stderr=%q", errOut)
	}
	noTaskState(t, dir)
}

// Legitimate objectives keep working unchanged.
func TestTaskValidObjectiveStillCreatesTask(t *testing.T) {
	dir := t.TempDir()
	captureOutput(t, func() { runInit([]string{dir}) })

	out, _ := captureOutput(t, func() {
		runTask([]string{"--dir", dir, "submit", "implement the feature", "5000", "--strategy", "direct"})
	})
	if !strings.Contains(out, "Task enqueued: task-") {
		t.Fatalf("expected task creation, got stdout=%q", out)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, ".a4c", "activities", "task-*.json"))
	if len(matches) != 1 {
		t.Fatalf("expected exactly one task record, got %v", matches)
	}
}
