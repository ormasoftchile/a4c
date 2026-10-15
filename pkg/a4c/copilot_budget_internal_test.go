package a4c

import (
	"errors"
	"testing"
)

// TestCopilotAdapter_BudgetViolationAbortsSessionOnce verifies that a budget
// violation during an in-flight action terminates the session exactly once:
// the model cannot keep issuing operations that governance will keep
// rejecting (the benchmark's 84k-token rejection loop).
func TestCopilotAdapter_BudgetViolationAbortsSessionOnce(t *testing.T) {
	aborts := 0
	s := &CopilotSessionAdapter{
		config:  AgentConfig{Budget: Budget{Allocated: 100}},
		abortFn: func() { aborts++ },
	}
	// Billed consumption already exceeds the allocation.
	s.usage = UsageInfo{PromptTokens: 500, CompletionTokens: 0, TotalTokens: 500, BilledTokens: 500}

	err := s.checkActionBudget("tool call")
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("expected ErrBudgetExceeded, got: %v", err)
	}
	// Repeated impossible calls keep failing fast and never re-abort.
	for i := 0; i < 5; i++ {
		if err := s.checkActionBudget("tool call"); !errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("repeat %d: expected ErrBudgetExceeded, got: %v", i, err)
		}
	}
	if aborts != 1 {
		t.Fatalf("expected exactly one session abort, got %d", aborts)
	}
	if !s.budgetExceeded {
		t.Fatal("expected budgetExceeded flag set")
	}
}

// TestCopilotAdapter_CheckBudgetUsesBilledTokens verifies that cache reads
// (re-served context) do not consume budget: a session whose total input is
// dominated by cache reads stays within a billed-consistent allocation.
func TestCopilotAdapter_CheckBudgetUsesBilledTokens(t *testing.T) {
	s := &CopilotSessionAdapter{
		config: AgentConfig{Budget: Budget{Allocated: 25000}},
	}
	// Mirrors the benchmark's direct-run coder profile: 91.6k total input of
	// which 75.8k were cache reads; billed = uncached + output = 19.9k.
	s.usage = UsageInfo{
		PromptTokens:     91589,
		CachedTokens:     75776,
		CompletionTokens: 4098,
		TotalTokens:      95687,
		BilledTokens:     19911,
	}
	if err := s.CheckBudget(); err != nil {
		t.Fatalf("cache reads must not trip the budget: %v", err)
	}

	// Once billed consumption crosses the allocation, the budget trips.
	s.usage.BilledTokens = 25001
	if err := s.CheckBudget(); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("expected ErrBudgetExceeded at billed 25001, got: %v", err)
	}
}
