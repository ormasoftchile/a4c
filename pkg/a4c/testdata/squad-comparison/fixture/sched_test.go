package sched

import "testing"

func TestIsValid(t *testing.T) {
	cases := []struct {
		iv   Interval
		want bool
	}{
		{Interval{0, 1440}, true},
		{Interval{60, 90}, true},
		{Interval{10, 10}, false},
		{Interval{20, 10}, false},
		{Interval{-1, 10}, false},
		{Interval{0, 1441}, false},
	}
	for _, c := range cases {
		if got := c.iv.IsValid(); got != c.want {
			t.Errorf("Interval{%d,%d}.IsValid() = %v, want %v", c.iv.From, c.iv.To, got, c.want)
		}
	}
}

func TestOverlaps(t *testing.T) {
	cases := []struct {
		a, b Interval
		want bool
	}{
		{Interval{0, 10}, Interval{5, 15}, true},
		{Interval{0, 10}, Interval{10, 20}, false}, // touching is not overlapping
		{Interval{0, 10}, Interval{11, 20}, false},
		{Interval{0, 100}, Interval{10, 20}, true}, // containment
	}
	for _, c := range cases {
		if got := c.a.Overlaps(c.b); got != c.want {
			t.Errorf("%v.Overlaps(%v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestSortIntervals(t *testing.T) {
	got := SortIntervals([]Interval{{30, 40}, {0, 10}, {0, 5}})
	want := []Interval{{0, 5}, {0, 10}, {30, 40}}
	if len(got) != len(want) {
		t.Fatalf("SortIntervals returned %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("SortIntervals returned %v, want %v", got, want)
		}
	}
}
