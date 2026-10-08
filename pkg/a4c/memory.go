package a4c

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrMemoryNotFound = errors.New("memory: record not found")
	ErrUnverifiedMemory = errors.New("memory: unverified statements cannot be used as authoritative conventions")
)

type MemoryStatus string

const (
	MemoryStatusProposed   MemoryStatus = "proposed"
	MemoryStatusVerified   MemoryStatus = "verified"
	MemoryStatusDeprecated MemoryStatus = "deprecated"
)

// MemoryRecord represents a single unit of organizational knowledge.
type MemoryRecord struct {
	Key              string       `json:"key"`
	Content          string       `json:"content"`
	Author           string       `json:"author"`
	SourceActivityID string       `json:"source_activity_id"`
	Revision         int          `json:"revision"`
	Status           MemoryStatus `json:"status"`
	Verifier         string       `json:"verifier,omitempty"`
	CreatedAt        time.Time    `json:"created_at"`
	UpdatedAt        time.Time    `json:"updated_at"`
}

// DurableMemoryStore manages versioned team conventions and architectural decisions on disk.
type DurableMemoryStore struct {
	mu      sync.Mutex
	dir     string
	records map[string]*MemoryRecord
}

func NewDurableMemoryStore(dir string) (*DurableMemoryStore, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("memory: failed to create memory dir: %w", err)
	}

	store := &DurableMemoryStore{
		dir:     dir,
		records: make(map[string]*MemoryRecord),
	}

	if err := store.load(); err != nil {
		return nil, fmt.Errorf("memory: failed to load records: %w", err)
	}

	return store, nil
}

func (s *DurableMemoryStore) load() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(s.dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var rec MemoryRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return err
		}
		s.records[rec.Key] = &rec
	}
	return nil
}

func (s *DurableMemoryStore) persist(rec *MemoryRecord) error {
	path := filepath.Join(s.dir, fmt.Sprintf("%s.json", rec.Key))
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Propose records a newly suggested convention or constraint in "proposed" state.
// It is NOT yet authoritative and will NOT be served as verified team context.
func (s *DurableMemoryStore) Propose(key, content, author, sourceActivityID string) (*MemoryRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	rev := 1
	if existing, ok := s.records[key]; ok {
		rev = existing.Revision + 1
	}

	rec := &MemoryRecord{
		Key:              key,
		Content:          content,
		Author:           author,
		SourceActivityID: sourceActivityID,
		Revision:         rev,
		Status:           MemoryStatusProposed,
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	if err := s.persist(rec); err != nil {
		return nil, err
	}
	s.records[key] = rec
	return rec, nil
}

// Promote transitions a proposed memory to verified status once validated
// by native test pass, reviewer approval, or human engineer sign-off.
func (s *DurableMemoryStore) Promote(key string, verifier string) (*MemoryRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.records[key]
	if !ok {
		return nil, ErrMemoryNotFound
	}

	rec.Status = MemoryStatusVerified
	rec.Verifier = verifier
	rec.UpdatedAt = time.Now()

	if err := s.persist(rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// Deprecate marks a convention as deprecated.
func (s *DurableMemoryStore) Deprecate(key string, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.records[key]
	if !ok {
		return ErrMemoryNotFound
	}

	rec.Status = MemoryStatusDeprecated
	rec.Content = fmt.Sprintf("[DEPRECATED: %s] %s", reason, rec.Content)
	rec.UpdatedAt = time.Now()

	return s.persist(rec)
}

// Get returns the record for key.
func (s *DurableMemoryStore) Get(key string) (*MemoryRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.records[key]
	if !ok {
		return nil, ErrMemoryNotFound
	}
	return rec, nil
}

// ListVerified returns all memory records that have been formally verified.
func (s *DurableMemoryStore) ListVerified() []*MemoryRecord {
	s.mu.Lock()
	defer s.mu.Unlock()

	var list []*MemoryRecord
	for _, rec := range s.records {
		if rec.Status == MemoryStatusVerified {
			list = append(list, rec)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].Key < list[j].Key
	})
	return list
}

// FormatContext returns a formatted block of verified knowledge suitable for model prompt injection.
func (s *DurableMemoryStore) FormatContext() string {
	verified := s.ListVerified()
	if len(verified) == 0 {
		return "No verified architectural memory recorded yet."
	}

	var sb strings.Builder
	sb.WriteString("Verified Team Architectural Memory:\n")
	for _, rec := range verified {
		sb.WriteString(fmt.Sprintf("- [%s (rev %d, verified by %s)]: %s\n",
			rec.Key, rec.Revision, rec.Verifier, rec.Content))
	}
	return sb.String()
}
