package a4c

import "strings"

// ToolClass classifies a governed tool by purpose for budget enforcement.
type ToolClass string

const (
	// ToolClassExploration gathers information (read/search). Counted against
	// the exploration tool-call allowance.
	ToolClassExploration ToolClass = "exploration"
	// ToolClassWork performs the activity's substantive work (patch, memory).
	// Counted against the exploration allowance.
	ToolClassWork ToolClass = "work"
	// ToolClassTerminal drives lifecycle completion or escalation (delegate,
	// submit a verdict, ask a human). These are the operations required to
	// FINISH an activity, so they are exempt from the exploration call cap:
	// a reviewer that exhausted its read budget must still be able to submit
	// its verdict, and a lead must still be able to delegate. They remain
	// subject to permission checks and token budgets, and are bounded by a
	// separate small terminal-op cap so they cannot become unlimited loops.
	ToolClassTerminal ToolClass = "terminal"
)

// ClassifyTool returns the budget class of a governed tool by its name.
// This is the single source of truth shared by every host adapter so the
// terminal-operation semantics stay consistent across hosts.
func ClassifyTool(name string) ToolClass {
	switch name {
	case "governed_propose_activity", "governed_submit_review", "governed_request_human_decision":
		return ToolClassTerminal
	case "governed_read_file":
		return ToolClassExploration
	default:
		// governed_apply_patch, governed_propose_memory, and anything unknown
		// are counted as work so unknown tools cannot silently bypass the cap.
		return ToolClassWork
	}
}

// IsTerminalTool reports whether a tool is a lifecycle-control operation that
// must remain usable after the exploration allowance is exhausted.
func IsTerminalTool(name string) bool {
	return ClassifyTool(name) == ToolClassTerminal
}

// maxTerminalOps bounds how many terminal-control operations a single session
// may perform. This keeps terminal tools from becoming unlimited model-execution
// loops while still guaranteeing a reviewer can always submit its verdict.
const maxTerminalOps = 8

// toolBudgetEnforcer applies the shared exploration/terminal tool-call budget
// policy at a single boundary. Adapters embed it so semantics stay consistent.
type toolBudgetEnforcer struct {
	maxExploration int
	exploration    int
	terminal       int
	onLimitHit     func(tool string)
}

// newToolBudgetEnforcer creates an enforcer with the exploration cap.
func newToolBudgetEnforcer(maxExploration int, onLimitHit func(string)) *toolBudgetEnforcer {
	if maxExploration <= 0 {
		maxExploration = 10
	}
	return &toolBudgetEnforcer{maxExploration: maxExploration, onLimitHit: onLimitHit}
}

// check applies the budget policy for one invocation of toolName. Terminal
// tools are exempt from the exploration cap but bounded by maxTerminalOps.
// Exploration/work tools consume the exploration allowance.
func (e *toolBudgetEnforcer) check(toolName string) error {
	class := ClassifyTool(toolName)
	if class == ToolClassTerminal {
		e.terminal++
		if e.terminal > maxTerminalOps {
			if e.onLimitHit != nil {
				e.onLimitHit(toolName)
			}
			return &ToolLimitError{
				Tool:       toolName,
				Kind:       "terminal",
				MaxAllowed: maxTerminalOps,
			}
		}
		return nil
	}
	e.exploration++
	if e.exploration > e.maxExploration {
		if e.onLimitHit != nil {
			e.onLimitHit(toolName)
		}
		return &ToolLimitError{
			Tool:       toolName,
			Kind:       "exploration",
			MaxAllowed: e.maxExploration,
		}
	}
	return nil
}

// ToolLimitError distinguishes a tool-budget rejection from other failures so
// callers can tell an operational limit from a genuine work outcome.
type ToolLimitError struct {
	Tool       string
	Kind       string // "exploration" or "terminal"
	MaxAllowed int
}

func (e *ToolLimitError) Error() string {
	return "tool limit: " + e.Tool + " exceeded " + e.Kind + " allowance (max " +
		itoa(e.MaxAllowed) + " per session)"
}

// Unwrap exposes ErrToolCallLimitExceeded so existing errors.Is checks keep
// working, while IsToolLimit still distinguishes the budget class.
func (e *ToolLimitError) Unwrap() error { return ErrToolCallLimitExceeded }

// IsToolLimit reports whether err is a tool-budget rejection.
func IsToolLimit(err error) bool {
	_, ok := asToolLimit(err)
	return ok
}

func asToolLimit(err error) (*ToolLimitError, bool) {
	for err != nil {
		if tl, ok := err.(*ToolLimitError); ok {
			return tl, true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return nil, false
		}
		err = u.Unwrap()
	}
	return nil, false
}

// ToolLimitName returns the tool name for a tool-limit error, or "".
func ToolLimitName(err error) string {
	if tl, ok := asToolLimit(err); ok {
		return tl.Tool
	}
	return ""
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// normalizeToolName trims and lowercases a tool name for classification.
func normalizeToolName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}
