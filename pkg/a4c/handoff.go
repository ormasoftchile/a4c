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
		sb.WriteString("Instructions: You are the Lead Engineer. Inspect the codebase if necessary and delegate implementation to a specialist using governed_propose_activity.\n")

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
func (b *HandoffBuilder) BuildContinuationPrompt(chk TaskCheckpoint) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Objective: %s\n", chk.Objective))
	if chk.WorktreeBranch != "" {
		sb.WriteString(fmt.Sprintf("Branch: %s\n", chk.WorktreeBranch))
	}
	sb.WriteString("\n--- CROSS-HOST CONTINUATION CHECKPOINT (Task Continuation Checkpoint) ---\n")
	if chk.SourceRuntime != "" && chk.TargetRuntime != "" {
		sb.WriteString(fmt.Sprintf("Transition: %s -> %s\n", chk.SourceRuntime, chk.TargetRuntime))
	}
	if len(chk.CompletedWork) > 0 {
		sb.WriteString("Completed Work:\n")
		for _, w := range chk.CompletedWork {
			sb.WriteString(fmt.Sprintf("- %s\n", w))
		}
	}
	if chk.GitStatusSummary != "" {
		sb.WriteString(fmt.Sprintf("Current Repository State:\n%s\n", chk.GitStatusSummary))
	}
	if len(chk.AcceptedDecisions) > 0 {
		sb.WriteString("Accepted Decisions & Constraints:\n")
		for _, d := range chk.AcceptedDecisions {
			sb.WriteString(fmt.Sprintf("- %s\n", d))
		}
	}
	if len(chk.RemainingObligations) > 0 {
		sb.WriteString("Remaining Obligations:\n")
		for _, o := range chk.RemainingObligations {
			sb.WriteString(fmt.Sprintf("- %s\n", o))
		}
	}
	if chk.ContinuationInstructions != "" {
		sb.WriteString(fmt.Sprintf("Continuation Guidance:\n%s\n", chk.ContinuationInstructions))
	}
	if chk.ValidationEvidence != nil && chk.ValidationEvidence.Valid {
		sb.WriteString(fmt.Sprintf("Prior Test Evidence: %s (Exit %d)\n", chk.ValidationEvidence.EvidenceID, chk.ValidationEvidence.ExitCode))
	}
	sb.WriteString("\nInstructions: Continue the implementation from the current worktree state. Do not redo completed work. Use your native tools to edit files and run tests. Ensure all remaining obligations are satisfied and tests pass.\n")
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
