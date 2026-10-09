package a4c

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DurableLifecycleStore provides persistent storage for human lifecycle decisions.
type DurableLifecycleStore struct {
	mu      sync.RWMutex
	dir     string
	records map[string]*HumanLifecycleRecord
}

func NewDurableLifecycleStore(dir string) (*DurableLifecycleStore, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("lifecycle_store: create dir: %w", err)
	}

	store := &DurableLifecycleStore{
		dir:     dir,
		records: make(map[string]*HumanLifecycleRecord),
	}

	if err := store.load(); err != nil {
		return nil, fmt.Errorf("lifecycle_store: load: %w", err)
	}

	return store, nil
}

func (s *DurableLifecycleStore) load() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(s.dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var rec HumanLifecycleRecord
		if err := json.Unmarshal(data, &rec); err == nil && rec.TaskID != "" {
			s.records[rec.TaskID] = &rec
		}
	}

	return nil
}

func (s *DurableLifecycleStore) persist(rec *HumanLifecycleRecord) error {
	path := filepath.Join(s.dir, fmt.Sprintf("%s.json", rec.TaskID))
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

// Get retrieves human lifecycle facts for a task. Returns empty record if none exists yet.
func (s *DurableLifecycleStore) Get(taskID string) (*HumanLifecycleRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := filepath.Join(s.dir, fmt.Sprintf("%s.json", taskID))
	if data, err := os.ReadFile(path); err == nil {
		var rec HumanLifecycleRecord
		if err := json.Unmarshal(data, &rec); err == nil && rec.TaskID != "" {
			s.records[taskID] = &rec
			cp := rec
			return &cp, true
		}
	}

	rec, ok := s.records[taskID]
	if !ok {
		return nil, false
	}
	cp := *rec
	return &cp, true
}

// Save records human lifecycle facts atomically.
func (s *DurableLifecycleStore) Save(rec *HumanLifecycleRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec.UpdatedAt = time.Now()
	if err := s.persist(rec); err != nil {
		return err
	}

	cp := *rec
	s.records[rec.TaskID] = &cp
	return nil
}
