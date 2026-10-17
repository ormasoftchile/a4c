package knowledge

import (
	"fmt"
	"strings"
	"time"
)

// RenderMarkdown converts one legacy record to readable Markdown, preserving
// the original statement, rationale, epistemic state, and evidence. Support
// values are presented as recorded observations, never as agent-authored
// promotion targets.
func RenderMarkdown(rec KnowledgeRecord) string {
	var b strings.Builder
	b.WriteString("# Legacy record: " + rec.ID + "\n\n")
	fmt.Fprintf(&b, "- Kind: %s\n", rec.Kind)
	fmt.Fprintf(&b, "- Support at export (was never set at write time by agents): %s\n", rec.Support)
	fmt.Fprintf(&b, "- Status: %s\n", rec.Status)
	fmt.Fprintf(&b, "- Integrity: %s\n", rec.Integrity)
	if rec.Author != "" {
		fmt.Fprintf(&b, "- Author: %s\n", rec.Author)
	}
	if rec.Host != "" {
		fmt.Fprintf(&b, "- Host: %s\n", rec.Host)
	}
	if rec.TaskID != "" {
		fmt.Fprintf(&b, "- Task: %s\n", rec.TaskID)
	}
	if !rec.CreatedAt.IsZero() {
		fmt.Fprintf(&b, "- Created: %s\n", rec.CreatedAt.Format(time.RFC3339))
	}
	if rec.Supersedes != "" {
		fmt.Fprintf(&b, "- Supersedes: %s\n", rec.Supersedes)
	}
	if rec.SupersededBy != "" {
		fmt.Fprintf(&b, "- Superseded by: %s\n", rec.SupersededBy)
	}
	b.WriteString("\n## Statement\n\n" + rec.Statement + "\n")
	if rec.Rationale != "" {
		b.WriteString("\n## Rationale\n\n" + rec.Rationale + "\n")
	}
	if len(rec.Evidence) > 0 {
		b.WriteString("\n## Evidence\n\n")
		for _, ev := range rec.Evidence {
			fmt.Fprintf(&b, "- %s", ev.Kind)
			if ev.Locator != "" {
				fmt.Fprintf(&b, ": %s", ev.Locator)
			}
			if ev.Hash != "" {
				fmt.Fprintf(&b, " (hash %s)", ev.Hash)
			}
			b.WriteString("\n")
		}
	}
	if len(rec.Links) > 0 {
		b.WriteString("\n## Links\n\n")
		for _, l := range rec.Links {
			fmt.Fprintf(&b, "- %s %s", l.Rel, l.To)
			if l.Note != "" {
				fmt.Fprintf(&b, " — %s", l.Note)
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("\n---\nExported from the legacy A4C SQLite knowledge store. Original database remains the source of record until deleted by the user.\n")
	return b.String()
}
