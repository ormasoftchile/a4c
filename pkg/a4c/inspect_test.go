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

	out := r.FormatInspect(nil, "")

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
	out := r.FormatInspect(nil, "")
	if strings.Count(out, "- x [") > 1 {
		t.Errorf("x rendered more than once:\n%s", out)
	}
	if !strings.Contains(out, "Human Decisions ---\nNone") {
		t.Errorf("expected no human decisions:\n%s", out)
	}
}

func TestFormatInspectStructuralFreshness(t *testing.T) {
	r := &TaskHistoryReport{
		TaskID: "root",
		Activities: []ActivityHistoryEntry{
			{ActivityID: "root", AgentID: "lead", StructuralArtifactID: "art-1"},
		},
	}
	if out := r.FormatInspect(nil, ""); !strings.Contains(out, "art-1 (UNKNOWN)") {
		t.Errorf("nil store should give UNKNOWN:\n%s", out)
	}

	store, err := NewDurableStructuralStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if out := r.FormatInspect(store, ""); !strings.Contains(out, "art-1 (UNKNOWN)") {
		t.Errorf("missing artifact should give UNKNOWN:\n%s", out)
	}

	if err := store.Save(&StructuralContextArtifact{ID: "art-1", Freshness: FreshnessStale}); err != nil {
		t.Fatal(err)
	}
	if out := r.FormatInspect(store, ""); !strings.Contains(out, "art-1 (STALE)") {
		t.Errorf("recorded STALE should be reported:\n%s", out)
	}
}

func TestStructuralFreshnessSnapshotComparison(t *testing.T) {
	store, err := NewDurableStructuralStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&StructuralContextArtifact{ID: "same", IndexedSnapshot: "abc", Freshness: FreshnessUnknown}); err != nil {
		t.Fatal(err)
	}

	if got := structuralFreshness(store, "same", func() string { return "abc" }); got != FreshnessCurrent {
		t.Errorf("matching snapshot: got %s", got)
	}
	if got := structuralFreshness(store, "same", func() string { return "def" }); got != FreshnessStale {
		t.Errorf("differing snapshot: got %s", got)
	}
	if got := structuralFreshness(store, "same", func() string { return "" }); got != FreshnessUnknown {
		t.Errorf("unavailable snapshot: got %s", got)
	}
}
