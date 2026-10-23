package a4c_test

import (
	"encoding/json"
	"strings"
	"testing"

	spike "github.com/ormasoftchile/a4c/pkg/a4c"
)

// TestContinuation_CheckpointSurvivesRestart is the critical fix: a checkpoint
// injected into an activity's Inputs must survive a JSON persistence round-trip
// and still produce a continuation prompt after a coordinator restart. Before
// the fix, Inputs["checkpoint"] round-tripped as map[string]any and the type
// assertion silently dropped it, rebuilding the task from its initial prompt.
func TestContinuation_CheckpointSurvivesRestart(t *testing.T) {
	chk := spike.TaskCheckpoint{
		TaskID:               "task-1",
		Objective:            "Implement the feature",
		WorktreePath:         "/tmp/wt",
		ModifiedFiles:        []string{"src/a.c"},
		CompletedWork:        []string{"investigated parser"},
		RemainingObligations: []string{"add tests"},
		SourceRuntime:        "copilot",
		TargetRuntime:        "opencode",
	}
	// Simulate the durable round-trip: *TaskCheckpoint -> JSON -> map[string]any.
	inputs := map[string]any{"checkpoint": chk}
	b, err := json.Marshal(inputs)
	if err != nil {
		t.Fatal(err)
	}
	var restored map[string]any
	if err := json.Unmarshal(b, &restored); err != nil {
		t.Fatal(err)
	}
	// The coordinator's rehydration must recover the typed checkpoint.
	recovered := spike.CheckpointFromInput(restored["checkpoint"])
	if recovered == nil {
		t.Fatalf("checkpoint must be rehydrated from JSON round-trip, got nil")
	}
	if recovered.TaskID != "task-1" || recovered.SourceRuntime != "copilot" || recovered.TargetRuntime != "opencode" {
		t.Fatalf("rehydrated checkpoint lost identity: %+v", recovered)
	}
	if len(recovered.CompletedWork) != 1 || recovered.CompletedWork[0] != "investigated parser" {
		t.Fatalf("rehydrated checkpoint lost completed work: %+v", recovered.CompletedWork)
	}
	// The continuation prompt must reflect durable state, not the initial prompt.
	prompt := spike.BuildContinuationPrompt(*recovered)
	if !strings.Contains(prompt, "investigated parser") || !strings.Contains(prompt, "add tests") {
		t.Fatalf("continuation prompt must include completed work and remaining obligations:\n%s", prompt)
	}
	if !strings.Contains(prompt, "copilot -> opencode") {
		t.Fatalf("continuation prompt must show host transition:\n%s", prompt)
	}
	// A typed value passes through unchanged.
	if got := spike.CheckpointFromInput(&chk); got == nil || got.TaskID != "task-1" {
		t.Fatalf("typed checkpoint must pass through, got %+v", got)
	}
	// nil / empty is nil.
	if got := spike.CheckpointFromInput(nil); got != nil {
		t.Fatalf("nil input must yield nil checkpoint")
	}
}

// TestContinuation_SameHostSessionResume: an interrupted activity with a saved
// session reference is resumed on the SAME host via ResumeSession (preserving
// session identity), not recreated with a fresh session.


// TestContinuation_CrashRecoverySurfacesInterrupted: a claimed activity after a
// crash (no resumable same-host session) is surfaced as interrupted/recovery
// needed — never falsely completed and never blindly replayed.


// resumeObservingProvider wraps a provider and records ResumeSession calls.


func TestContinuation_PromptPreservesState(t *testing.T) {
	chk := spike.TaskCheckpoint{
		TaskID:                  "task-1",
		Objective:               "fix the parser",
		WorktreeBranch:          "a4c/task-1",
		ModifiedFiles:           []string{"parser.go"},
		NewFiles:                []string{"parser_test.go"},
		CompletedWork:           []string{"lexer done"},
		RemainingObligations:    []string{"parser + tests"},
		ContinuationInstructions: "start with parser.go",
	}
	prompt := spike.BuildContinuationPrompt(chk)
	for _, want := range []string{"fix the parser", "parser.go", "lexer done", "parser + tests", "start with parser.go"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("continuation prompt missing %q: %s", want, prompt)
		}
	}
}
