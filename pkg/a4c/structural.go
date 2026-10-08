package a4c

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// IndexFreshness captures whether the structural index accurately represents current workspace content.
type IndexFreshness string

const (
	FreshnessCurrent IndexFreshness = "CURRENT" // Index verified against current workspace content
	FreshnessStale   IndexFreshness = "STALE"   // Workspace content has diverged from index
	FreshnessUnknown IndexFreshness = "UNKNOWN" // Freshness cannot be verified
)

// StructuralSymbol represents an indexed code symbol.
type StructuralSymbol struct {
	NodeID    int      `json:"node_id"`
	Kind      string   `json:"kind"`
	Name      string   `json:"name"`
	QualName  string   `json:"qualname,omitempty"`
	FilePath  string   `json:"file_path"`
	StartLine int      `json:"start_line"`
	EndLine   int      `json:"end_line"`
	Signature string   `json:"signature,omitempty"`
	Source    string   `json:"source,omitempty"`
}

// StructuralCaller represents a verified or approximated caller relationship.
type StructuralCaller struct {
	CallerName string  `json:"caller"`
	FilePath   string  `json:"file"`
	Line       int     `json:"line"`
	Confidence float64 `json:"confidence"`
}

// StructuralContextPacket bundles queried structural facts with complete provenance and freshness.
type StructuralContextPacket struct {
	ToolName               string             `json:"tool_name"`
	QueryParams            map[string]any     `json:"query_params"`
	WorkspaceDir           string             `json:"workspace_dir"`
	IndexDBPath            string             `json:"index_db_path"`
	IndexTimestamp         time.Time          `json:"index_timestamp"`
	IndexedSnapshotSHA     string             `json:"indexed_snapshot_sha,omitempty"`
	CurrentSnapshotSHA     string             `json:"current_snapshot_sha,omitempty"`
	Freshness              IndexFreshness     `json:"freshness"`
	Symbols                []StructuralSymbol `json:"symbols,omitempty"`
	Callers                []StructuralCaller `json:"callers,omitempty"`
	Siblings               []StructuralSymbol `json:"siblings,omitempty"`
	Truncated              bool               `json:"truncated"`
	QueryDurationMs        int64              `json:"query_duration_ms"`
	RawBytesReturned       int                `json:"raw_bytes_returned"`
}

// IndexMaintenanceMetrics records the operational overhead of maintaining the code graph index.
type IndexMaintenanceMetrics struct {
	InitialIndexDuration       time.Duration `json:"initial_index_duration"`
	IncrementalRefreshDuration time.Duration `json:"incremental_refresh_duration"`
	IndexDatabaseSizeBytes     int64         `json:"index_database_size_bytes"`
	TotalQueriesCount          int           `json:"total_queries_count"`
	TotalQueryDuration         time.Duration `json:"total_query_duration"`
	AverageQueryLatencyMs      float64       `json:"average_query_latency_ms"`
}

// StructuralProvider defines the contract for querying code topology facts.
type StructuralProvider interface {
	Available() bool
	CheckFreshness(ctx context.Context, workspaceDir string) (IndexFreshness, error)
	SearchSymbols(ctx context.Context, query string) (*StructuralContextPacket, error)
	GetContextFor(ctx context.Context, symbolName string) (*StructuralContextPacket, error)
	GetCallers(ctx context.Context, nodeID int) (*StructuralContextPacket, error)
	GetMaintenanceMetrics() IndexMaintenanceMetrics
}

// CodeTopoProvider implements StructuralProvider via the official local codetopo CLI.
type CodeTopoProvider struct {
	mu                  sync.Mutex
	cliPath             string
	repoRoot            string
	dbPath              string
	snapshotter         *WorkspaceSnapshotter
	indexedSnapshot     string
	indexTimestamp      time.Time
	initialDuration     time.Duration
	refreshDuration     time.Duration
	queryCount          int
	totalQueryDuration  time.Duration
}

// NewCodeTopoProvider initializes a provider with discovery of codetopo binary and database.
func NewCodeTopoProvider(repoRoot string, dbPath string) (*CodeTopoProvider, error) {
	cliPath := os.Getenv("CODETOPO_CLI_PATH")
	if cliPath == "" {
		if path, err := exec.LookPath("codetopo"); err == nil {
			cliPath = path
		} else {
			// Check standard install locations
			home, _ := os.UserHomeDir()
			candidate := filepath.Join(home, ".local", "bin", "codetopo")
			if _, statErr := os.Stat(candidate); statErr == nil {
				cliPath = candidate
			}
		}
	}

	if cliPath == "" {
		return nil, fmt.Errorf("codetopo binary not found in PATH or CODETOPO_CLI_PATH")
	}

	if dbPath == "" {
		dbPath = filepath.Join(repoRoot, ".codetopo", "index.sqlite")
	}

	p := &CodeTopoProvider{
		cliPath:     cliPath,
		repoRoot:    repoRoot,
		dbPath:      dbPath,
		snapshotter: NewWorkspaceSnapshotter(),
	}

	return p, nil
}

func (p *CodeTopoProvider) Available() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cliPath == "" {
		return false
	}
	_, err := os.Stat(p.dbPath)
	return err == nil
}

// IndexRepository runs codetopo index and captures maintenance metrics and snapshot identity.
func (p *CodeTopoProvider) IndexRepository(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	start := time.Now()
	cmd := exec.CommandContext(ctx, p.cliPath, "index", "--root", p.repoRoot, "--db", p.dbPath, "--turbo")
	out, err := cmd.CombinedOutput()
	dur := time.Since(start)

	if err != nil {
		return fmt.Errorf("codetopo index failed (%v): %s", err, string(out))
	}

	p.initialDuration = dur
	p.indexTimestamp = time.Now()

	// Capture workspace snapshot at time of indexing
	if snap, sErr := p.snapshotter.ComputeSnapshot(p.repoRoot); sErr == nil {
		p.indexedSnapshot = snap
	}

	return nil
}

// RefreshIndex runs incremental reindex and measures refresh duration.
func (p *CodeTopoProvider) RefreshIndex(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	start := time.Now()
	cmd := exec.CommandContext(ctx, p.cliPath, "index", "--root", p.repoRoot, "--db", p.dbPath, "--turbo")
	out, err := cmd.CombinedOutput()
	dur := time.Since(start)

	if err != nil {
		return fmt.Errorf("codetopo incremental refresh failed (%v): %s", err, string(out))
	}

	p.refreshDuration = dur
	p.indexTimestamp = time.Now()

	if snap, sErr := p.snapshotter.ComputeSnapshot(p.repoRoot); sErr == nil {
		p.indexedSnapshot = snap
	}

	return nil
}

// CheckFreshness verifies whether the index matches current workspace content.
func (p *CodeTopoProvider) CheckFreshness(ctx context.Context, workspaceDir string) (IndexFreshness, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.indexedSnapshot == "" {
		return FreshnessUnknown, nil
	}

	currentSnap, err := p.snapshotter.ComputeSnapshot(workspaceDir)
	if err != nil {
		return FreshnessUnknown, err
	}

	if currentSnap == p.indexedSnapshot {
		return FreshnessCurrent, nil
	}

	return FreshnessStale, nil
}

// SearchSymbols invokes codetopo query symbol_search with structured parameters.
func (p *CodeTopoProvider) SearchSymbols(ctx context.Context, query string) (*StructuralContextPacket, error) {
	params := map[string]any{
		"query": query,
		"limit": 20,
	}
	return p.runQuery(ctx, "symbol_search", params)
}

// GetContextFor invokes codetopo query context_by_name to retrieve symbol definition and context.
func (p *CodeTopoProvider) GetContextFor(ctx context.Context, symbolName string) (*StructuralContextPacket, error) {
	params := map[string]any{
		"name": symbolName,
	}
	return p.runQuery(ctx, "context_by_name", params)
}

// GetCallers invokes codetopo query callers_approx to retrieve call sites.
func (p *CodeTopoProvider) GetCallers(ctx context.Context, nodeID int) (*StructuralContextPacket, error) {
	params := map[string]any{
		"node_id": nodeID,
	}
	return p.runQuery(ctx, "callers_approx", params)
}

func (p *CodeTopoProvider) runQuery(ctx context.Context, tool string, params map[string]any) (*StructuralContextPacket, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("failed to encode query params: %w", err)
	}

	start := time.Now()
	cmd := exec.CommandContext(ctx, p.cliPath, "query", "--db", p.dbPath, tool, string(paramsJSON))
	out, err := cmd.CombinedOutput()
	duration := time.Since(start)

	p.queryCount++
	p.totalQueryDuration += duration

	packet := &StructuralContextPacket{
		ToolName:           tool,
		QueryParams:        params,
		WorkspaceDir:       p.repoRoot,
		IndexDBPath:        p.dbPath,
		IndexTimestamp:     p.indexTimestamp,
		IndexedSnapshotSHA: p.indexedSnapshot,
		QueryDurationMs:    duration.Milliseconds(),
		RawBytesReturned:   len(out),
		Freshness:          FreshnessUnknown,
	}

	// Verify current freshness
	if currentSnap, sErr := p.snapshotter.ComputeSnapshot(p.repoRoot); sErr == nil {
		packet.CurrentSnapshotSHA = currentSnap
		if p.indexedSnapshot != "" && currentSnap == p.indexedSnapshot {
			packet.Freshness = FreshnessCurrent
		} else if p.indexedSnapshot != "" && currentSnap != p.indexedSnapshot {
			packet.Freshness = FreshnessStale
		}
	}

	if err != nil {
		return packet, fmt.Errorf("codetopo query error (%v): %s", err, string(out))
	}

	p.parseToolOutput(tool, out, packet)
	return packet, nil
}

func (p *CodeTopoProvider) parseToolOutput(tool string, out []byte, packet *StructuralContextPacket) {
	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		return
	}

	if hasMore, ok := raw["has_more"].(bool); ok && hasMore {
		packet.Truncated = true
	}

	switch tool {
	case "symbol_search":
		if results, ok := raw["results"].([]any); ok {
			for _, item := range results {
				if m, ok := item.(map[string]any); ok {
					sym := StructuralSymbol{
						NodeID:   int(getFloat(m, "node_id")),
						Kind:     getString(m, "kind"),
						Name:     getString(m, "name"),
						FilePath: getString(m, "file_path"),
					}
					if span, ok := m["span"].(map[string]any); ok {
						sym.StartLine = int(getFloat(span, "start_line"))
						sym.EndLine = int(getFloat(span, "end_line"))
					}
					packet.Symbols = append(packet.Symbols, sym)
				}
			}
		}

	case "context_by_name", "context_for":
		if symMap, ok := raw["symbol"].(map[string]any); ok {
			sym := StructuralSymbol{
				NodeID:    int(getFloat(symMap, "node_id")),
				Kind:      getString(symMap, "kind"),
				Name:      getString(symMap, "name"),
				QualName:  getString(symMap, "qualname"),
				FilePath:  getString(symMap, "file_path"),
				Signature: getString(symMap, "signature"),
				Source:    getString(symMap, "source"),
			}
			packet.Symbols = append(packet.Symbols, sym)
		}
		if sibs, ok := raw["siblings"].([]any); ok {
			for _, s := range sibs {
				if sm, ok := s.(map[string]any); ok {
					packet.Siblings = append(packet.Siblings, StructuralSymbol{
						NodeID:    int(getFloat(sm, "node_id")),
						Kind:      getString(sm, "kind"),
						Name:      getString(sm, "name"),
						Signature: getString(sm, "signature"),
					})
				}
			}
		}

	case "callers_approx":
		if res, ok := raw["results"].([]any); ok {
			for _, r := range res {
				if rm, ok := r.(map[string]any); ok {
					packet.Callers = append(packet.Callers, StructuralCaller{
						CallerName: getString(rm, "caller"),
						FilePath:   getString(rm, "file"),
						Line:       int(getFloat(rm, "line")),
						Confidence: getFloat(rm, "confidence"),
					})
				}
			}
		}
	}
}

func (p *CodeTopoProvider) GetMaintenanceMetrics() IndexMaintenanceMetrics {
	p.mu.Lock()
	defer p.mu.Unlock()

	var dbSize int64
	if fi, err := os.Stat(p.dbPath); err == nil {
		dbSize = fi.Size()
	}

	avgLat := 0.0
	if p.queryCount > 0 {
		avgLat = float64(p.totalQueryDuration.Milliseconds()) / float64(p.queryCount)
	}

	return IndexMaintenanceMetrics{
		InitialIndexDuration:       p.initialDuration,
		IncrementalRefreshDuration: p.refreshDuration,
		IndexDatabaseSizeBytes:     dbSize,
		TotalQueriesCount:          p.queryCount,
		TotalQueryDuration:         p.totalQueryDuration,
		AverageQueryLatencyMs:      avgLat,
	}
}

func getString(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func getFloat(m map[string]any, k string) float64 {
	if v, ok := m[k].(float64); ok {
		return v
	}
	return 0
}
