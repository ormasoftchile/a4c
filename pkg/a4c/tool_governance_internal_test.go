package a4c

import (
	"errors"
	"testing"
)

// TestToolGovernance_TerminalOpsExemptFromExplorationCap reproduces the
// historical defect: a reviewer exhausted its exploration allowance and then
// could not submit its verdict. Terminal lifecycle operations must remain
// usable after the exploration cap is hit.
func TestToolGovernance_TerminalOpsExemptFromExplorationCap(t *testing.T) {
	hits := 0
	e := newToolBudgetEnforcer(3, func(string) { hits++ })

	// Exhaust the exploration allowance exactly.
	for i := 0; i < 3; i++ {
		if err := e.check("governed_read_file"); err != nil {
			t.Fatalf("exploration call %d should succeed within cap: %v", i+1, err)
		}
	}
	// The next exploration call exceeds the cap.
	if err := e.check("governed_read_file"); err == nil {
		t.Fatalf("exploration call beyond cap must fail")
	} else if !IsToolLimit(err) {
		t.Fatalf("expected a tool-limit error, got %v", err)
	}
	if hits != 1 {
		t.Fatalf("expected 1 limit hit, got %d", hits)
	}

	// Terminal operations still succeed after exploration exhaustion.
	if err := e.check("governed_submit_review"); err != nil {
		t.Fatalf("submit_review must succeed after exploration exhaustion: %v", err)
	}
	if err := e.check("governed_propose_activity"); err != nil {
		t.Fatalf("propose_activity must succeed after exploration exhaustion: %v", err)
	}
	if err := e.check("governed_request_human_decision"); err != nil {
		t.Fatalf("request_human_decision must succeed after exploration exhaustion: %v", err)
	}
}

// TestToolGovernance_TerminalOpsBounded verifies terminal operations cannot
// become unlimited loops: they are capped separately.
func TestToolGovernance_TerminalOpsBounded(t *testing.T) {
	e := newToolBudgetEnforcer(100, nil)
	for i := 0; i < maxTerminalOps; i++ {
		if err := e.check("governed_submit_review"); err != nil {
			t.Fatalf("terminal call %d within cap should succeed: %v", i+1, err)
		}
	}
	if err := e.check("governed_submit_review"); err == nil {
		t.Fatalf("terminal call beyond cap must fail")
	} else {
		tl, ok := asToolLimit(err)
		if !ok || tl.Kind != "terminal" {
			t.Fatalf("expected a terminal tool-limit error, got %v", err)
		}
	}
	// The exploration allowance is untouched by terminal calls.
	if err := e.check("governed_read_file"); err != nil {
		t.Fatalf("exploration should be unaffected by terminal calls: %v", err)
	}
}

// TestToolGovernance_Classification pins the shared classification used by
// every host adapter so terminal semantics stay consistent.
func TestToolGovernance_Classification(t *testing.T) {
	cases := map[string]ToolClass{
		"governed_read_file":              ToolClassExploration,
		"governed_apply_patch":            ToolClassWork,
		"governed_propose_memory":         ToolClassWork,
		"governed_propose_activity":       ToolClassTerminal,
		"governed_submit_review":          ToolClassTerminal,
		"governed_request_human_decision": ToolClassTerminal,
		"governed_unknown_tool":           ToolClassWork, // fail-closed
	}
	for name, want := range cases {
		if got := ClassifyTool(name); got != want {
			t.Errorf("ClassifyTool(%q) = %q, want %q", name, got, want)
		}
	}
	if !IsTerminalTool("governed_submit_review") {
		t.Fatalf("submit_review must be a terminal tool")
	}
	if IsTerminalTool("governed_read_file") {
		t.Fatalf("read_file must not be terminal")
	}
}

// TestToolGovernance_ToolLimitErrorWrapsSentinel verifies existing errors.Is
// checks against ErrToolCallLimitExceeded keep working.
func TestToolGovernance_ToolLimitErrorWrapsSentinel(t *testing.T) {
	e := newToolBudgetEnforcer(1, nil)
	_ = e.check("governed_read_file")
	err := e.check("governed_read_file")
	if err == nil {
		t.Fatalf("expected tool limit error")
	}
	if !errors.Is(err, ErrToolCallLimitExceeded) {
		t.Fatalf("tool limit error must wrap ErrToolCallLimitExceeded")
	}
	if !IsToolLimit(err) {
		t.Fatalf("IsToolLimit must recognize the error")
	}
	if ToolLimitName(err) != "governed_read_file" {
		t.Fatalf("ToolLimitName = %q", ToolLimitName(err))
	}
}
