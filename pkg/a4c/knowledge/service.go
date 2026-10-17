package knowledge

import (
	"sync"
)

// Service is the facade used by the MCP server and host adapters. It owns the
// durable store, the execution-context secret, and the live activity validator.
type Service struct {
	store *Store
	mu    sync.Mutex
}

// NewService opens the knowledge store for a workspace. It returns ErrDisabled
// when the module is not enabled, so callers can skip wiring entirely.
func NewService(workspaceDir string, activityValidator func(activityID string) bool) (*Service, error) {
	st, err := Open(workspaceDir)
	if err != nil {
		return nil, err
	}
	if activityValidator != nil {
		st.SetActivityValidator(activityValidator)
	}
	return &Service{store: st}, nil
}

// Enabled reports whether the module is enabled for the workspace.
func Enabled(workspaceDir string) bool {
	cfg, err := ReadConfig(workspaceDir)
	return err == nil && cfg.Enabled
}

// Store exposes the underlying store (for advanced/host wiring).
func (s *Service) Store() *Store { return s.store }

// Close releases the store.
func (s *Service) Close() error { return s.store.Close() }

// Project returns the workspace project identifier.
func (s *Service) Project() string { return s.store.Project() }

// Path returns the database path.
func (s *Service) Path() string { return s.store.Path() }

// IssueToken mints a signed execution-context token for an activity.
func (s *Service) IssueToken(ctx ExecContext) (string, error) {
	return s.store.IssueToken(ctx, 0)
}

// ValidateToken authenticates a token and returns its execution context.
func (s *Service) ValidateToken(token string) (*ExecContext, error) {
	return s.store.ValidateToken(token)
}

// RevokeActivity invalidates outstanding tokens for an activity.
func (s *Service) RevokeActivity(activityID string) { s.store.RevokeActivity(activityID) }

// Record durably records one knowledge item on behalf of a validated context.
func (s *Service) Record(ec *ExecContext, in RecordInput) (*RecordResult, error) {
	return s.store.Record(ec, in)
}

// Query returns a bounded page of records.
func (s *Service) Query(in QueryInput) (*QueryResult, error) {
	return s.store.Query(in)
}

// Status summarises the store.
func (s *Service) Status() (map[string]int, map[string]int, map[string]int, int, string, error) {
	byKind, bySupport, byStatus, total, err := s.store.Counts()
	if err != nil {
		return nil, nil, nil, 0, "", err
	}
	return byKind, bySupport, byStatus, total, s.store.Path(), nil
}

// ActivityIsActive is the default validator shape used by tests and the CLI when
// no A4C activity store is bound.
func ActivityIsActive(activityID string) bool { return activityID != "" }
