package a4c

import (
	"fmt"
	"strings"
)

// HandoffArtifact represents a structured, minimal context passed to an agent.
type HandoffArtifact struct {
	Objective          string
	Role               string
	AgentID            string
	Strategy           ExecutionStrategy
	ActiveWorkspace    string
	Diff               string
	ValidationEvidence string
	VerifiedMemories   string
	Inputs             map[string]any
	ProfileDescription string
	Checkpoint         *TaskCheckpoint
}

// HandoffBuilder builds concise, token-efficient prompt handoffs.
type HandoffBuilder struct {
	MaxDiffLines int
}

func NewHandoffBuilder() *HandoffBuilder {
	return &HandoffBuilder{
		MaxDiffLines: 150, // Truncate huge diffs to prevent token blowups
	}
}

// BuildPrompt creates a role-tailored, minimal prompt for an agent.
func (b *HandoffBuilder) BuildPrompt(artifact HandoffArtifact) string {
	if artifact.Checkpoint != nil {
		return b.BuildContinuationPrompt(*artifact.Checkpoint)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Objective: %s\n", artifact.Objective))
	sb.WriteString(fmt.Sprintf("Role: %s\n", artifact.Role))

	// In Direct mode, keep context ultra-lean: omit structural profile bloat and verbose conventions
	if artifact.Strategy != StrategyDirect {
		if artifact.ProfileDescription != "" {
			sb.WriteString(fmt.Sprintf("Active Capability Profile: %s\n", artifact.ProfileDescription))
		}
		if artifact.VerifiedMemories != "" {
			sb.WriteString(fmt.Sprintf("Team Conventions: %s\n", artifact.VerifiedMemories))
		}
	}

	if len(artifact.Inputs) > 0 {
		sb.WriteString(fmt.Sprintf("Inputs: %v\n", artifact.Inputs))
	}

	switch artifact.AgentID {
	case "lead", "alice":
		sb.WriteString("Instructions: You are the Lead Engineer. Identify the task, select capabilities, and delegate promptly using governed_propose_activity. Inspect only what is needed for a precise handoff; do not solve the task yourself. Each proposal's objective must be a concise, self-contained handoff: target files, exact contract, constraints, and verification — never your investigation transcript.\n")

	case "reviewer", "charlie":
		trimmedDiff := b.truncateDiff(artifact.Diff)
		sb.WriteString(fmt.Sprintf("Changes Under Review:\n%s\n", trimmedDiff))
		sb.WriteString(fmt.Sprintf("Native Evidence: %s\n", artifact.ValidationEvidence))
		sb.WriteString("Instructions: Review the implementation independently for correctness, edge cases, test coverage, and specification compliance against the objective and native test evidence. You are strictly read-only. Indicate your verdict with [REVIEW_APPROVED] or [REVIEW_REJECTED: reason] and your review findings.\n")

	default:
		// Specialist / Coder: use native tools directly in worktree
		sb.WriteString("Instructions: Implement the objective directly in the workspace using your native editing and test tools. Verify that all tests pass. Keep modifications clean, minimal, and focused on the objective.\n")
	}

	return sb.String()
}

// BuildContinuationPrompt constructs a compact resumption prompt from an authoritative task checkpoint.
// It explicitly distinguishes already-completed work from remaining obligations to prevent repeating work.
func (b *HandoffBuilder) BuildContinuationPrompt(chk TaskCheckpoint) string {
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

// BuildContinuationPrompt provides a package-level helper to generate continuation prompts.
func BuildContinuationPrompt(chk TaskCheckpoint) string {
	return NewHandoffBuilder().BuildContinuationPrompt(chk)
}

func (b *HandoffBuilder) truncateDiff(diff string) string {
	if diff == "" {
		return "No uncommitted git changes detected."
	}
	lines := strings.Split(diff, "\n")
	if len(lines) <= b.MaxDiffLines {
		return diff
	}
	return strings.Join(lines[:b.MaxDiffLines], "\n") + fmt.Sprintf("\n... [Diff truncated: %d more lines]", len(lines)-b.MaxDiffLines)
}
