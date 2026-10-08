package a4c

import (
	"fmt"
	"strings"
)

// HandoffArtifact represents a structured, minimal context passed to an agent.
type HandoffArtifact struct {
	Objective            string
	Role                 string
	AgentID              string
	ActiveWorkspace      string
	Diff                 string
	ValidationEvidence   string
	VerifiedMemories     string
	Inputs               map[string]any
	StructuralProjection string
	ProfileDescription   string
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
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Objective: %s\n", artifact.Objective))
	sb.WriteString(fmt.Sprintf("Role: %s\n", artifact.Role))

	if artifact.ProfileDescription != "" {
		sb.WriteString(fmt.Sprintf("Active Capability Profile: %s\n", artifact.ProfileDescription))
	}

	if len(artifact.Inputs) > 0 {
		sb.WriteString(fmt.Sprintf("Inputs: %v\n", artifact.Inputs))
	}

	if artifact.VerifiedMemories != "" {
		sb.WriteString(fmt.Sprintf("Team Conventions: %s\n", artifact.VerifiedMemories))
	}

	if artifact.StructuralProjection != "" {
		sb.WriteString(fmt.Sprintf("Shared Structural Context:\n%s\n", artifact.StructuralProjection))
	}

	switch artifact.AgentID {
	case "lead":
		sb.WriteString("Instructions: You are the Lead Engineer. Inspect the codebase if necessary and delegate implementation to 'coder' and verification to 'reviewer' using governed_propose_activity.\n")

	case "coder":
		sb.WriteString("Instructions: Implement the objective. Read code with governed_read_file and apply edits with governed_apply_patch. Keep modifications scoped directly to the objective.\n")

	case "reviewer":
		trimmedDiff := b.truncateDiff(artifact.Diff)
		sb.WriteString(fmt.Sprintf("Changes Under Review:\n%s\n", trimmedDiff))
		sb.WriteString(fmt.Sprintf("Native Evidence: %s\n", artifact.ValidationEvidence))
		sb.WriteString("Instructions: Review the implementation independently for correctness, edge cases, test coverage, and specification compliance against the objective and native test evidence. You may approve, request revisions, or reject using governed_submit_review.\n")

	default:
		sb.WriteString("Instructions: Execute the assigned objective using authorized governed tools.\n")
	}

	return sb.String()
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
