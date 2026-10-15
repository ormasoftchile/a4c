// Package sched provides single-day minute-interval utilities.
//
// Intervals are half-open minute ranges [From, To) within one day:
// 0 <= From < To <= 1440.
package sched

import "sort"

// Interval is a half-open minute range [From, To) within a single day.
// From and To are minutes since midnight: 0 <= From < To <= 1440.
type Interval struct {
	From int
	To   int
}

// IsValid reports whether iv is a well-formed interval.
func (iv Interval) IsValid() bool {
	return iv.From >= 0 && iv.From < iv.To && iv.To <= 1440
}

// Overlaps reports whether two valid intervals share any minute.
func (iv Interval) Overlaps(other Interval) bool {
	return iv.From < other.To && other.From < iv.To
}

// SortIntervals returns a new slice sorted by From (then To) ascending.
func SortIntervals(ivs []Interval) []Interval {
	out := make([]Interval, len(ivs))
	copy(out, ivs)
	sort.Slice(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].To < out[j].To
	})
	return out
}
