// Acceptance tests for the interval-algebra task.
//
// This file is injected by the benchmark harness AFTER both products finish
// their runs, so neither product can see or specialize to these tests.
// Helper and test names are intentionally unusual to avoid collisions with
// test functions the agents may have written.
package sched

import "testing"

func accNormWant(t *testing.T, name string, got, want []Interval) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if len(got) != len(want) {
		t.Fatalf("%s: got %v, want %v", name, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: got %v, want %v", name, got, want)
		}
	}
}

func TestAcceptanceIntervalMerge(t *testing.T) {
	accNormWant(t, "empty", Merge(nil), nil)
	accNormWant(t, "unsorted with overlap", Merge([]Interval{{30, 60}, {0, 20}, {10, 15}}), []Interval{{0, 20}, {30, 60}})
	accNormWant(t, "touching merges", Merge([]Interval{{0, 10}, {10, 20}}), []Interval{{0, 20}})
	accNormWant(t, "chained touch after overlap", Merge([]Interval{{0, 10}, {8, 12}, {12, 20}}), []Interval{{0, 20}})
	accNormWant(t, "gap preserved", Merge([]Interval{{0, 10}, {11, 20}}), []Interval{{0, 10}, {11, 20}})
	accNormWant(t, "containment", Merge([]Interval{{0, 100}, {10, 20}}), []Interval{{0, 100}})
	accNormWant(t, "invalid ignored", Merge([]Interval{{50, 50}, {0, 10}, {1400, 1500}}), []Interval{{0, 10}})
	accNormWant(t, "all invalid", Merge([]Interval{{9, 9}, {10, 5}}), nil)
}

func TestAcceptanceIntervalTotal(t *testing.T) {
	if got := Total([]Interval{{0, 10}, {5, 15}}); got != 15 {
		t.Errorf("overlap counted once: got %d, want 15", got)
	}
	if got := Total([]Interval{{0, 10}, {10, 20}}); got != 20 {
		t.Errorf("touching: got %d, want 20", got)
	}
	if got := Total([]Interval{{0, 10}, {20, 30}}); got != 20 {
		t.Errorf("disjoint: got %d, want 20", got)
	}
	if got := Total([]Interval{{0, 100}, {10, 20}}); got != 100 {
		t.Errorf("containment: got %d, want 100", got)
	}
	if got := Total([]Interval{{900, 900}, {0, 10}}); got != 10 {
		t.Errorf("invalid ignored: got %d, want 10", got)
	}
	if got := Total(nil); got != 0 {
		t.Errorf("empty: got %d, want 0", got)
	}
}

func TestAcceptanceIntervalSubtract(t *testing.T) {
	accNormWant(t, "middle hole", Subtract(Interval{0, 100}, []Interval{{30, 40}}), []Interval{{0, 30}, {40, 100}})
	accNormWant(t, "full cover", Subtract(Interval{0, 50}, []Interval{{0, 100}}), nil)
	accNormWant(t, "no overlap", Subtract(Interval{0, 10}, []Interval{{100, 200}}), []Interval{{0, 10}})
	accNormWant(t, "clip both ends", Subtract(Interval{50, 100}, []Interval{{0, 60}, {90, 1000}}), []Interval{{60, 90}})
	accNormWant(t, "invalid base", Subtract(Interval{10, 10}, []Interval{{0, 5}}), nil)
	accNormWant(t, "invalid others ignored", Subtract(Interval{0, 10}, []Interval{{20, 10}, {3, 5}}), []Interval{{0, 3}, {5, 10}})
	accNormWant(t, "touching not subtracted", Subtract(Interval{0, 10}, []Interval{{10, 20}}), []Interval{{0, 10}})
}

func TestAcceptanceIntervalComplement(t *testing.T) {
	accNormWant(t, "middle gap", Complement([]Interval{{30, 40}}, Interval{0, 100}), []Interval{{0, 30}, {40, 100}})
	accNormWant(t, "multiple gaps", Complement([]Interval{{0, 10}, {5, 15}, {30, 35}}, Interval{0, 50}), []Interval{{15, 30}, {35, 50}})
	accNormWant(t, "no gaps", Complement([]Interval{{0, 50}}, Interval{0, 50}), nil)
	accNormWant(t, "edges", Complement([]Interval{{10, 20}}, Interval{0, 30}), []Interval{{0, 10}, {20, 30}})
	accNormWant(t, "empty ivs", Complement(nil, Interval{0, 10}), []Interval{{0, 10}})
	accNormWant(t, "invalid universe", Complement([]Interval{{0, 10}}, Interval{5, 5}), nil)
	accNormWant(t, "invalid ivs ignored", Complement([]Interval{{100, 100}}, Interval{0, 10}), []Interval{{0, 10}})
}
