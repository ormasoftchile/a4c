package a4c

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// StructuralQueryRecord logs a specific query executed against the structural index.
type StructuralQueryRecord struct {
	ToolName        string    `json:"tool_name"`
	Params          string    `json:"params"`
	Timestamp       time.Time `json:"timestamp"`
	DurationMs      int64     `json:"duration_ms"`
	SymbolsReturned int       `json:"symbols_returned"`
}

// StructuralEdgeRecord captures dependency and caller relations between symbols and files.
type StructuralEdgeRecord struct {
	CallerSymbol string  `json:"caller_symbol,omitempty"`
	CallerFile   string  `json:"caller_file"`
	CallerLine   int     `json:"caller_line,omitempty"`
	CalleeSymbol string  `json:"callee_symbol"`
	CalleeFile   string  `json:"callee_file,omitempty"`
	Kind         string  `json:"kind,omitempty"` // "call", "import", "type"
	Confidence   float64 `json:"confidence,omitempty"`
}

// StructuralContextArtifact is a durable, provenance-backed record of structural code facts.
type StructuralContextArtifact struct {
	ID                string                   `json:"id"`
	RepositoryID      string                   `json:"repository_id"`
	WorkspaceSnapshot string                   `json:"workspace_snapshot"`
	IndexedSnapshot   string                   `json:"indexed_snapshot"`
	Freshness         IndexFreshness           `json:"freshness"`
	QueryRecords      []StructuralQueryRecord  `json:"query_records,omitempty"`
	RelevantSymbols   []StructuralSymbol       `json:"relevant_symbols,omitempty"`
	RelevantFiles     []string                 `json:"relevant_files,omitempty"`
	DependencyEdges   []StructuralEdgeRecord   `json:"dependency_edges,omitempty"`
	Completeness      string                   `json:"completeness"` // "complete" or "partial"
	Truncated         bool                     `json:"truncated"`
	Limitations       []string                 `json:"limitations,omitempty"`
	CreatedAt         time.Time                `json:"created_at"`
}

// ProjectForCoder formats a bounded, role-specific projection for the programmer agent.
func (a *StructuralContextArtifact) ProjectForCoder(maxLines int) string {
	if a == nil {
		return ""
	}
	if maxLines <= 0 {
		maxLines = 40
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("=== Structural Context [ID: %s | Freshness: %s] ===", a.ID, a.Freshness))
	if a.Freshness == FreshnessStale {
		lines = append(lines, "WARNING: Workspace has modified content since indexing. Structural facts are advisory.")
	}

	if len(a.RelevantFiles) > 0 {
		lines = append(lines, fmt.Sprintf("Target Files: %s", strings.Join(a.RelevantFiles, ", ")))
	}

	if len(a.RelevantSymbols) > 0 {
		lines = append(lines, "Discovered Symbols:")
		for i, sym := range a.RelevantSymbols {
			if i >= 10 {
				lines = append(lines, fmt.Sprintf("  ... and %d more symbols", len(a.RelevantSymbols)-10))
				break
			}
			sig := sym.Signature
			if sig == "" {
				sig = sym.Name
			}
			lines = append(lines, fmt.Sprintf("  - %s (%s:%d)", sig, sym.FilePath, sym.StartLine))
		}
	}

	if len(a.DependencyEdges) > 0 {
		lines = append(lines, "Affected Callers & Dependencies:")
		for i, edge := range a.DependencyEdges {
			if i >= 10 {
				lines = append(lines, fmt.Sprintf("  ... and %d more callers", len(a.DependencyEdges)-10))
				break
			}
			lines = append(lines, fmt.Sprintf("  - %s (%s:%d) -> calls %s", edge.CallerSymbol, edge.CallerFile, edge.CallerLine, edge.CalleeSymbol))
		}
	}

	if len(lines) > maxLines {
		truncated := lines[:maxLines]
		truncated = append(truncated, fmt.Sprintf("... [Structural projection truncated: %d more lines]", len(lines)-maxLines))
		return strings.Join(truncated, "\n")
	}

	return strings.Join(lines, "\n")
}

// ProjectForReviewer formats an independent impact and caller summary for the reviewer.
func (a *StructuralContextArtifact) ProjectForReviewer(maxLines int) string {
	if a == nil {
		return ""
	}
	if maxLines <= 0 {
		maxLines = 35
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("=== Structural Impact Scope [ID: %s | Freshness: %s] ===", a.ID, a.Freshness))
	lines = append(lines, "NOTE: Structural facts indicate expected caller relationships, NOT semantic correctness. Evaluate actual diff and native test evidence.")

	if len(a.RelevantFiles) > 0 {
		lines = append(lines, fmt.Sprintf("Scoped Files: %s", strings.Join(a.RelevantFiles, ", ")))
	}

	if len(a.DependencyEdges) > 0 {
		lines = append(lines, "Expected Call Sites to Audit:")
		for i, edge := range a.DependencyEdges {
			if i >= 8 {
				lines = append(lines, fmt.Sprintf("  ... and %d more call sites", len(a.DependencyEdges)-8))
				break
			}
			lines = append(lines, fmt.Sprintf("  - %s in %s (line %d)", edge.CallerSymbol, edge.CallerFile, edge.CallerLine))
		}
	}

	if len(lines) > maxLines {
		truncated := lines[:maxLines]
		truncated = append(truncated, fmt.Sprintf("... [Reviewer projection truncated: %d more lines]", len(lines)-maxLines))
		return strings.Join(truncated, "\n")
	}

	return strings.Join(lines, "\n")
}

// DurableStructuralStore manages disk persistence of structural artifacts.
type DurableStructuralStore struct {
	mu      sync.RWMutex
	baseDir string
}

// NewDurableStructuralStore creates or loads a persistent structural artifact store.
func NewDurableStructuralStore(baseDir string) (*DurableStructuralStore, error) {
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("structural store: failed to create directory %q: %w", baseDir, err)
	}
	return &DurableStructuralStore{
		baseDir: baseDir,
	}, nil
}

// Save writes a structural artifact to disk durably.
func (s *DurableStructuralStore) Save(artifact *StructuralContextArtifact) error {
	if artifact == nil || artifact.ID == "" {
		return fmt.Errorf("structural store: invalid artifact or missing ID")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return fmt.Errorf("structural store: failed to marshal artifact: %w", err)
	}

	target := filepath.Join(s.baseDir, fmt.Sprintf("%s.json", artifact.ID))
	tmp := target + ".tmp"

	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("structural store: failed to write temp file: %w", err)
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("structural store: atomic rename failed: %w", err)
	}
	return nil
}

// Get reads a structural artifact by ID from disk.
func (s *DurableStructuralStore) Get(id string) (*StructuralContextArtifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	target := filepath.Join(s.baseDir, fmt.Sprintf("%s.json", id))
	data, err := os.ReadFile(target)
	if err != nil {
		return nil, fmt.Errorf("structural store: artifact %q not found: %w", id, err)
	}

	var artifact StructuralContextArtifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		return nil, fmt.Errorf("structural store: failed to unmarshal artifact %q: %w", id, err)
	}
	return &artifact, nil
}

// ListArtifacts returns all stored structural artifacts sorted newest first.
func (s *DurableStructuralStore) ListArtifacts() ([]*StructuralContextArtifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entries, err := os.ReadDir(s.baseDir)
	if err != nil {
		return nil, err
	}

	var artifacts []*StructuralContextArtifact
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		art, err := s.Get(id)
		if err == nil && art != nil {
			artifacts = append(artifacts, art)
		}
	}

	sort.Slice(artifacts, func(i, j int) bool {
		return artifacts[i].CreatedAt.After(artifacts[j].CreatedAt)
	})

	return artifacts, nil
}
