package a4c

import (
	"encoding/json"
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

	// Review-evidence bundle. Populated only for reviewer activities so a
	// reviewer can verify without re-deriving what the coder already
	// established, and without needing unrestricted command execution.
	ReviewEvidenceDir   string
	FilesChanged        []string
	CoderHandoff        *CoderHandoff
	PriorFindings       []ReviewFinding
	DiffHygieneWarnings []string
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

		if len(artifact.FilesChanged) > 0 {
			sb.WriteString(fmt.Sprintf("Files Changed (%d):\n", len(artifact.FilesChanged)))
			for _, f := range artifact.FilesChanged {
				sb.WriteString(fmt.Sprintf("  - %s\n", f))
			}
		}

		if artifact.CoderHandoff != nil {
			h := artifact.CoderHandoff
			sb.WriteString("\nCoder Completion Handoff (already established by the implementer; do not re-derive):\n")
			if h.BehaviorSummary != "" {
				sb.WriteString(fmt.Sprintf("  Behavior: %s\n", h.BehaviorSummary))
			}
			if len(h.TestsAdded) > 0 {
				sb.WriteString(fmt.Sprintf("  Tests added: %s\n", strings.Join(h.TestsAdded, ", ")))
			}
			if len(h.TestsUpdated) > 0 {
				sb.WriteString(fmt.Sprintf("  Tests updated: %s\n", strings.Join(h.TestsUpdated, ", ")))
			}
			if len(h.VerificationCmds) > 0 {
				sb.WriteString(fmt.Sprintf("  Verification commands executed: %s\n", strings.Join(h.VerificationCmds, "; ")))
			}
			if len(h.TestExitCodes) > 0 {
				codes := make([]string, len(h.TestExitCodes))
				for i, c := range h.TestExitCodes {
					codes[i] = fmt.Sprintf("%d", c)
				}
				sb.WriteString(fmt.Sprintf("  Test exit codes: %s\n", strings.Join(codes, ", ")))
			}
			if len(h.KnownLimitations) > 0 {
				sb.WriteString(fmt.Sprintf("  Known limitations declared by coder: %s\n", strings.Join(h.KnownLimitations, "; ")))
			}
		}

		if len(artifact.PriorFindings) > 0 {
			sb.WriteString("\nPrior Review Findings (from earlier rounds; do not re-derive):\n")
			for _, f := range artifact.PriorFindings {
				loc := ""
				if f.Path != "" {
					loc = fmt.Sprintf(" (%s)", f.Path)
				}
				sb.WriteString(fmt.Sprintf("  [%s]%s %s\n", f.Kind, loc, f.Message))
			}
		}

		if len(artifact.DiffHygieneWarnings) > 0 {
			sb.WriteString("\nDiff Hygiene Warnings (unexpected/unrelated files flagged automatically; assess relevance, do not blindly reject):\n")
			for _, w := range artifact.DiffHygieneWarnings {
				sb.WriteString(fmt.Sprintf("  - %s\n", w))
			}
		}

		if artifact.ReviewEvidenceDir != "" {
			sb.WriteString(fmt.Sprintf(
				"\nEvidence Bundle (read-only, inside YOUR workspace under %s/):\n"+
					"  - %s/diff.patch           full baseline-to-candidate source diff\n"+
					"  - %s/changed-files.txt    each changed file with line count and test names + line numbers\n"+
					"  - %s/test-output.log      the COMPLETE verification log (read this for full test output)\n"+
					"  - %s/evidence.json        verification command, exit code, tests executed, vacuous flag\n"+
					"  - %s/coder-handoff.json   structured implementer summary\n"+
					"  - %s/prior-reviews.json   earlier findings and coder responses\n"+
					"To establish that tests exist, read changed-files.txt (it lists test names and line numbers) rather than paging whole test files. To inspect a specific region of a large file, use ranged reads (start_line/end_line). You may also use any read-only search tool available to you.\n",
				artifact.ReviewEvidenceDir, artifact.ReviewEvidenceDir, artifact.ReviewEvidenceDir,
				artifact.ReviewEvidenceDir, artifact.ReviewEvidenceDir, artifact.ReviewEvidenceDir,
				artifact.ReviewEvidenceDir))
		}

		sb.WriteString("Instructions: Review the implementation independently for correctness, edge cases, test coverage, and specification compliance against the objective and the evidence above. You are strictly read-only. You may not run commands; consume the recorded evidence and logs instead.\n")
		sb.WriteString("Verdicts: If you find a concrete implementation defect, respond [REVIEW_REJECTED: reason] with specific code/behavior. If the implementation looks correct but you CANNOT establish verification because evidence or capabilities are missing, respond [REVIEW_BLOCKED: reason] -- this is NOT a rejection and will not trigger code changes. If verified and defect-free, respond [REVIEW_APPROVED].\n")

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

// checkpointFromInput rehydrates a TaskCheckpoint from an activity's Inputs
// map. A checkpoint stored in-memory is a *TaskCheckpoint, but after the
// activity record round-trips through JSON persistence it becomes a
// map[string]any. Without this rehydration, a restarted coordinator silently
// loses the continuation framing (the type assertion fails) and the resumed
// activity is rebuilt from its initial prompt instead of durable state.
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
