package a4c

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Checkpoint and continuation support: A4C records what was done and what
// remains; the host resumes execution. Recovery evidence, not orchestration -
// no agent is selected, spawned, or supervised here.

func BuildContinuationPrompt(chk TaskCheckpoint) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Objective: %s\n", chk.Objective))
	if chk.WorktreeBranch != "" {
		sb.WriteString(fmt.Sprintf("Branch: %s\n", chk.WorktreeBranch))
	}
	sb.WriteString("\n--- CROSS-HOST CONTINUATION CHECKPOINT (Task Continuation Checkpoint: DO NOT RE-IMPLEMENT EXISTING WORK) ---\n")
	if chk.SourceRuntime != "" && chk.TargetRuntime != "" {
		sb.WriteString(fmt.Sprintf("Host Transition: %s -> %s\n", chk.SourceRuntime, chk.TargetRuntime))
	}
	if chk.FailureReason != "" || chk.InterruptionMessage != "" {
		reason := chk.FailureReason
		if chk.InterruptionMessage != "" {
			reason = fmt.Sprintf("%s (%s)", reason, chk.InterruptionMessage)
		}
		sb.WriteString(fmt.Sprintf("Interruption Context: %s\n", reason))
	}

	sb.WriteString("\n[ALREADY DONE]:\n")
	if len(chk.ModifiedFiles) > 0 || len(chk.NewFiles) > 0 {
		sb.WriteString("- Existing repository changes preserved in worktree:\n")
		for _, m := range chk.ModifiedFiles {
			sb.WriteString(fmt.Sprintf("  * [MODIFIED]: %s\n", m))
		}
		for _, n := range chk.NewFiles {
			sb.WriteString(fmt.Sprintf("  * [NEW FILE]: %s\n", n))
		}
	} else if chk.GitStatusSummary != "" {
		sb.WriteString(fmt.Sprintf("- Current Git Status:\n%s\n", chk.GitStatusSummary))
	}
	if len(chk.CompletedWork) > 0 {
		sb.WriteString("- Completed milestones & activities:\n")
		for _, w := range chk.CompletedWork {
			sb.WriteString(fmt.Sprintf("  * %s\n", w))
		}
	}
	if len(chk.AcceptedDecisions) > 0 {
		sb.WriteString("- Accepted architectural decisions & constraints:\n")
		for _, d := range chk.AcceptedDecisions {
			sb.WriteString(fmt.Sprintf("  * %s\n", d))
		}
	}
	if chk.ValidationEvidence != nil && chk.ValidationEvidence.Valid {
		sb.WriteString(fmt.Sprintf("- Prior passing test evidence: %s (ExitCode %d)\n", chk.ValidationEvidence.EvidenceID, chk.ValidationEvidence.ExitCode))
	}

	sb.WriteString("\n[STILL NEEDED]:\n")
	if len(chk.RemainingObligations) > 0 {
		sb.WriteString("- Remaining obligations & unfinished functionality:\n")
		for _, o := range chk.RemainingObligations {
			sb.WriteString(fmt.Sprintf("  * %s\n", o))
		}
	} else {
		sb.WriteString(fmt.Sprintf("- Complete remaining implementation for: %s\n", chk.Objective))
	}
	if chk.ContinuationInstructions != "" {
		sb.WriteString(fmt.Sprintf("- Continuation guidance from developer:\n  %s\n", chk.ContinuationInstructions))
	}

	sb.WriteString("\n[CRITICAL CONTINUATION INSTRUCTIONS]:\n")
	sb.WriteString("1. Inspect the existing codebase and files in the worktree before modifying anything.\n")
	sb.WriteString("2. Build incrementally upon the existing changes. Do NOT delete, discard, or re-implement already completed work.\n")
	sb.WriteString("3. Complete the remaining functionality, run native tests, and ensure all tests pass.\n")

	return sb.String()
}

func CheckpointFromInput(v any) *TaskCheckpoint {
	switch c := v.(type) {
	case nil:
		return nil
	case *TaskCheckpoint:
		return c
	case TaskCheckpoint:
		return &c
	case map[string]any:
		b, err := json.Marshal(c)
		if err != nil {
			return nil
		}
		var chk TaskCheckpoint
		if err := json.Unmarshal(b, &chk); err != nil {
			return nil
		}
		if chk.TaskID == "" && chk.Objective == "" {
			return nil
		}
		return &chk
	default:
		return nil
	}
}
