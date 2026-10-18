package a4c

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// ResolveTask turns an optional task identifier into the task the operator most
// likely means.
//
// An explicit identifier is matched exactly and returned. When it is omitted,
// the most recently updated task matching one of the preferred states wins, so
// the common case — "resume whatever I was doing" — never requires
// copy-pasting an opaque identifier. With no preference the most recently
// updated task is used.
//
// Ambiguity resolves deterministically to the most recent match rather than
// being reported, because the operator asked for the obvious one. A missing
// explicit ID and an empty candidate set are both errors: this never silently
// no-ops.
func (s *TeamServiceImpl) ResolveTask(explicit string, prefer []TaskLifecycleState) (*TaskSummary, error) {
	summaries, err := s.ListTasks(context.Background(), TaskFilter{})
	if err != nil {
		return nil, err
	}

	if id := strings.TrimSpace(explicit); id != "" {
		for i := range summaries {
			if summaries[i].TaskID == id {
				return &summaries[i], nil
			}
		}
		return nil, ErrTaskNotFound
	}

	if len(summaries) == 0 {
		return nil, fmt.Errorf("no tasks in this workspace")
	}

	// Most recently updated first.
	sort.SliceStable(summaries, func(i, j int) bool {
		return summaries[i].UpdatedAt.After(summaries[j].UpdatedAt)
	})

	if len(prefer) == 0 {
		return &summaries[0], nil
	}

	for i := range summaries {
		for _, want := range prefer {
			if summaries[i].State == want {
				return &summaries[i], nil
			}
		}
	}

	states := make([]string, 0, len(prefer))
	for _, w := range prefer {
		states = append(states, string(w))
	}
	return nil, fmt.Errorf("no task currently in state %s", strings.Join(states, " or "))
}
