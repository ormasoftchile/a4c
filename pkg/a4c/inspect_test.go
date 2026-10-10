package a4c

import (
	"strings"
	"testing"
)

func TestFormatInspectTreeOrphanAndCycle(t *testing.T) {
	r := &TaskHistoryReport{
		TaskID:              "root",
		OriginalObjective:   "do it",
		Status:              StatusCompleted,
		Branch:              "a4c/root",
		ParticipatingAgents: []string{"coder", "lead"},
		Activities: []ActivityHistoryEntry{
			{ActivityID: "root", AgentID: "lead", Objective: "root obj"},
			{ActivityID: "child", ParentID: "root", AgentID: "coder", Objective: "child obj", DependsOn: []string{"other"}},
			{ActivityID: "orphan", ParentID: "missing", AgentID: "reviewer", Objective: "orphan obj"},
			{ActivityID: "cycA", ParentID: "cycB", AgentID: "coder", Objective: "a"},
			{ActivityID: "cycB", ParentID: "cycA", AgentID: "coder", Objective: "b"},
		},
		ReviewDecisions: []ReviewHistoryEntry{{ActivityID: "orphan", ReviewerID: "reviewer", Approved: true, Comments: "ok"}},
	}

	out := r.FormatInspect("")

	for _, want := range []string{
		"Assigned:   lead", "Branch:     a4c/root", "Agents:     coder, lead",
		"- root [", "  - child [", "Depends on: other",
		"- orphan [", "--- Review Decisions ---", "APPROVED",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\n- child [") {
		t.Errorf("child must be nested under root:\n%s", out)
	}
	if strings.Count(out, "- orphan [") != 1 {
		t.Errorf("orphan should render exactly once:\n%s", out)
	}
}

func TestFormatInspectSelfCycleTerminates(t *testing.T) {
	r := &TaskHistoryReport{
		TaskID: "root",
		Activities: []ActivityHistoryEntry{
			{ActivityID: "root", AgentID: "lead"},
			{ActivityID: "x", ParentID: "x", AgentID: "coder"},
		},
	}
	out := r.FormatInspect("")
	if strings.Count(out, "- x [") > 1 {
		t.Errorf("x rendered more than once:\n%s", out)
	}
	if !strings.Contains(out, "Human Decisions ---\nNone") {
		t.Errorf("expected no human decisions:\n%s", out)
	}
}
