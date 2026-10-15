package a4c

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type ActivityRecord struct {
	Request ActivityRequest `json:"request"`
	Status  ActivityStatus  `json:"status"`
	Result  *ActivityResult `json:"result,omitempty"`
	Error   string          `json:"error,omitempty"`
}

type DurableActivityStore struct {
	mu           sync.Mutex
	dir          string
	decisionsDir string
	records      map[string]*ActivityRecord
	decisions    map[string]*HumanDecisionRequest
	order        []string
}

func NewDurableActivityStore(dir string) (*DurableActivityStore, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("store: failed to create store dir: %w", err)
	}

	decDir := filepath.Join(dir, "decisions")
	if err := os.MkdirAll(decDir, 0755); err != nil {
		return nil, fmt.Errorf("store: failed to create decisions dir: %w", err)
	}

	store := &DurableActivityStore{
		dir:          dir,
		decisionsDir: decDir,
		records:      make(map[string]*ActivityRecord),
		decisions:    make(map[string]*HumanDecisionRequest),
		order:        make([]string, 0),
	}

	if err := store.load(); err != nil {
		return nil, fmt.Errorf("store: failed to load existing records: %w", err)
	}

	return store, nil
}

func (s *DurableActivityStore) load() error {
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
			return err
		}
		var rec ActivityRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return err
		}
		s.records[rec.Request.ID] = &rec
		s.order = append(s.order, rec.Request.ID)
	}

	// Load decisions
	decEntries, err := os.ReadDir(s.decisionsDir)
	if err == nil {
		for _, entry := range decEntries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
				continue
			}
			path := filepath.Join(s.decisionsDir, entry.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			var dec HumanDecisionRequest
			if err := json.Unmarshal(data, &dec); err == nil {
				s.decisions[dec.DecisionID] = &dec
			}
		}
	}

	return nil
}

func (s *DurableActivityStore) persist(rec *ActivityRecord) error {
	path := filepath.Join(s.dir, fmt.Sprintf("%s.json", rec.Request.ID))
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

func (s *DurableActivityStore) persistDecision(dec *HumanDecisionRequest) error {
	path := filepath.Join(s.decisionsDir, fmt.Sprintf("%s.json", dec.DecisionID))
	data, err := json.MarshalIndent(dec, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *DurableActivityStore) Enqueue(req ActivityRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.records[req.ID]; exists {
		return fmt.Errorf("store: activity %q already exists", req.ID)
	}

	rec := &ActivityRecord{
		Request: req,
		Status:  StatusPending,
	}

	if err := s.persist(rec); err != nil {
		return err
	}

	s.records[req.ID] = rec
	s.order = append(s.order, req.ID)
	return nil
}

func (s *DurableActivityStore) dependenciesSatisfied(req ActivityRequest) bool {
	for _, depID := range req.DependsOn {
		depRec, ok := s.records[depID]
		if !ok || depRec.Status != StatusCompleted {
			return false
		}
	}
	return true
}

func (s *DurableActivityStore) dependenciesBlocked(req ActivityRequest) (bool, string) {
	for _, depID := range req.DependsOn {
		depRec, ok := s.records[depID]
		if ok && depRec.Status == StatusFailed {
			return true, fmt.Sprintf("dependency %q failed: %s", depID, depRec.Error)
		}
	}
	return false, ""
}

// ClaimNext claims the next available activity whose dependencies have completed.
func (s *DurableActivityStore) ClaimNext() (*ActivityRequest, error) {
	return s.ClaimNextForAgent("")
}

// ClaimNextForAgent claims the next pending activity for the specified agent (or any agent if agentID is empty).
func (s *DurableActivityStore) ClaimNextForAgent(agentID string) (*ActivityRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, id := range s.order {
		rec := s.records[id]
		if rec.Status != StatusPending {
			continue
		}

		// Cascade failure if any dependency failed
		if blocked, reason := s.dependenciesBlocked(rec.Request); blocked {
			rec.Status = StatusFailed
			rec.Error = reason
			_ = s.persist(rec)
			continue
		}

		// Check if dependencies are satisfied
		if !s.dependenciesSatisfied(rec.Request) {
			continue
		}

		// Check agent assignment filter if specified
		if agentID != "" && rec.Request.AgentID != "" && rec.Request.AgentID != "team" && rec.Request.AgentID != agentID {
			continue
		}

		rec.Status = StatusClaimed
		if err := s.persist(rec); err != nil {
			return nil, err
		}
		req := rec.Request
		return &req, nil
	}
	return nil, nil // None pending and ready
}

func (s *DurableActivityStore) Complete(id string, result ActivityResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, exists := s.records[id]
	if !exists {
		return ErrActivityNotFound
	}

	rec.Status = StatusCompleted
	rec.Result = &result
	return s.persist(rec)
}

// Requeue returns an activity to the pending pool with an updated request.
// It is used for explicit, bounded budget-exhaustion recovery: the activity's
// terminal error and result are cleared so it can be claimed again under a
// reallocated budget, preserving any workspace progress already made.
func (s *DurableActivityStore) Requeue(req ActivityRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, exists := s.records[req.ID]
	if !exists {
		return ErrActivityNotFound
	}

	rec.Request = req
	rec.Status = StatusPending
	rec.Error = ""
	rec.Result = nil
	return s.persist(rec)
}

func (s *DurableActivityStore) Fail(id string, failureErr error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, exists := s.records[id]
	if !exists {
		return ErrActivityNotFound
	}

	rec.Status = StatusFailed
	if failureErr != nil {
		rec.Error = failureErr.Error()
	}
	return s.persist(rec)
}

func (s *DurableActivityStore) FailWithResult(id string, result ActivityResult, failureErr error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, exists := s.records[id]
	if !exists {
		return ErrActivityNotFound
	}

	rec.Status = StatusFailed
	rec.Result = &result
	if failureErr != nil {
		rec.Error = failureErr.Error()
	}
	return s.persist(rec)
}

// InterruptWithResult pauses an activity due to an infrastructure or provider outage.
func (s *DurableActivityStore) InterruptWithResult(id string, result ActivityResult, interruptionErr error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, exists := s.records[id]
	if !exists {
		return ErrActivityNotFound
	}

	rec.Status = StatusInterrupted
	rec.Result = &result
	if interruptionErr != nil {
		rec.Error = interruptionErr.Error()
	}
	return s.persist(rec)
}

// MarkAwaitingHuman pauses an activity pending a decision from the human engineer.
func (s *DurableActivityStore) MarkAwaitingHuman(activityID string, dec HumanDecisionRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, exists := s.records[activityID]
	if !exists {
		return ErrActivityNotFound
	}

	rec.Status = StatusAwaitingHuman
	if err := s.persist(rec); err != nil {
		return err
	}

	dec.ActivityID = activityID
	dec.Status = HumanDecisionPending
	if dec.CreatedAt.IsZero() {
		dec.CreatedAt = time.Now()
	}

	if err := s.persistDecision(&dec); err != nil {
		return err
	}
	s.decisions[dec.DecisionID] = &dec
	return nil
}

// ResolveHumanDecision records the engineer's response and unblocks or fails the activity.
func (s *DurableActivityStore) ResolveHumanDecision(decisionID string, resolution string, approve bool, reassignTo string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	dec, exists := s.decisions[decisionID]
	if !exists {
		return errors.New("store: decision not found")
	}

	now := time.Now()
	dec.Resolution = resolution
	dec.ResolvedAt = &now

	rec, recExists := s.records[dec.ActivityID]
	if !recExists {
		return ErrActivityNotFound
	}

	if !approve {
		dec.Status = HumanDecisionRejected
		rec.Status = StatusFailed
		rec.Error = fmt.Sprintf("rejected by human engineer: %s", resolution)
	} else if reassignTo != "" {
		dec.Status = HumanDecisionReassigned
		dec.ReassignedTo = reassignTo
		rec.Request.AgentID = reassignTo
		rec.Status = StatusPending
	} else {
		dec.Status = HumanDecisionApproved
		rec.Status = StatusPending
	}

	if err := s.persistDecision(dec); err != nil {
		return err
	}
	return s.persist(rec)
}

func (s *DurableActivityStore) ListPendingDecisions() []*HumanDecisionRequest {
	s.mu.Lock()
	defer s.mu.Unlock()

	var list []*HumanDecisionRequest
	for _, dec := range s.decisions {
		if dec.Status == HumanDecisionPending {
			cp := *dec
			list = append(list, &cp)
		}
	}
	return list
}

func (s *DurableActivityStore) GetDecision(decisionID string) (*HumanDecisionRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	dec, exists := s.decisions[decisionID]
	if !exists {
		return nil, errors.New("store: decision not found")
	}
	cp := *dec
	return &cp, nil
}

// FindActivityByProposalID returns an existing activity that matches parentID and objective (idempotency).
func (s *DurableActivityStore) FindActivityByProposalID(parentID, objective string) *ActivityRecord {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, rec := range s.records {
		if rec.Request.ParentID == parentID && rec.Request.Objective == objective {
			return rec
		}
	}
	return nil
}

func (s *DurableActivityStore) Get(id string) (*ActivityRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := filepath.Join(s.dir, id+".json")
	if data, err := os.ReadFile(path); err == nil {
		var r ActivityRecord
		if err := json.Unmarshal(data, &r); err == nil && r.Request.ID == id {
			s.records[id] = &r
		}
	}

	rec, exists := s.records[id]
	if !exists {
		return nil, ErrActivityNotFound
	}
	return rec, nil
}

// Update updates an existing activity record and persists it to disk.
func (s *DurableActivityStore) Update(rec ActivityRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.records[rec.Request.ID]
	if !ok {
		return ErrActivityNotFound
	}
	*existing = rec
	return s.persist(existing)
}

func (s *DurableActivityStore) HasPending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, id := range s.order {
		if s.records[id].Status == StatusPending {
			return true
		}
	}
	return false
}

// HasClaimable returns true if there is at least one pending activity whose dependencies are met.
func (s *DurableActivityStore) HasClaimable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, id := range s.order {
		rec := s.records[id]
		if rec.Status == StatusPending {
			if blocked, reason := s.dependenciesBlocked(rec.Request); blocked {
				rec.Status = StatusFailed
				rec.Error = reason
				_ = s.persist(rec)
				continue
			}
			if s.dependenciesSatisfied(rec.Request) {
				return true
			}
		}
	}
	return false
}

func (s *DurableActivityStore) CompletedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	count := 0
	for _, id := range s.order {
		if s.records[id].Status == StatusCompleted {
			count++
		}
	}
	return count
}

func (s *DurableActivityStore) getDescendantIDsLocked(rootID string) map[string]bool {
	descendants := map[string]bool{rootID: true}
	for {
		added := false
		for _, rec := range s.records {
			if !descendants[rec.Request.ID] && descendants[rec.Request.ParentID] {
				descendants[rec.Request.ID] = true
				added = true
			}
		}
		if !added {
			break
		}
	}
	return descendants
}

// GetTaskTree returns the root activity and all descendant subtasks in DAG order.
func (s *DurableActivityStore) GetTaskTree(rootID string) []*ActivityRecord {
	s.mu.Lock()
	defer s.mu.Unlock()

	descendants := s.getDescendantIDsLocked(rootID)
	var list []*ActivityRecord
	for _, rec := range s.records {
		if descendants[rec.Request.ID] {
			list = append(list, rec)
		}
	}

	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Request.ID == rootID {
			return true
		}
		if list[j].Request.ID == rootID {
			return false
		}
		if list[i].Request.CreatedAt.Equal(list[j].Request.CreatedAt) {
			return list[i].Request.ID < list[j].Request.ID
		}
		return list[i].Request.CreatedAt.Before(list[j].Request.CreatedAt)
	})

	return list
}

// ListAllDecisions returns all decision records.
func (s *DurableActivityStore) ListAllDecisions() []*HumanDecisionRequest {
	s.mu.Lock()
	defer s.mu.Unlock()

	var list []*HumanDecisionRequest
	for _, dec := range s.decisions {
		cp := *dec
		list = append(list, &cp)
	}
	return list
}

// QueryActivities returns all activities matching the filter criteria.
func (s *DurableActivityStore) QueryActivities(filter ActivityFilter) []*ActivityRecord {
	s.mu.Lock()
	defer s.mu.Unlock()

	var list []*ActivityRecord
	var rootSubtasks map[string]bool
	if filter.RootID != "" {
		rootSubtasks = s.getDescendantIDsLocked(filter.RootID)
	}

	for _, id := range s.order {
		rec := s.records[id]
		if filter.Status != "" && rec.Status != filter.Status {
			continue
		}
		if filter.AgentID != "" && rec.Request.AgentID != filter.AgentID {
			continue
		}
		if filter.ParentID != "" && rec.Request.ParentID != filter.ParentID {
			continue
		}
		if filter.RootID != "" && !rootSubtasks[rec.Request.ID] {
			continue
		}
		if filter.HasErrors != nil {
			hasErr := rec.Error != ""
			if hasErr != *filter.HasErrors {
				continue
			}
		}

		cp := *rec
		list = append(list, &cp)
	}
	return list
}

// ListAllActivities returns all activity records in order.
func (s *DurableActivityStore) ListAllActivities() []*ActivityRecord {
	return s.QueryActivities(ActivityFilter{})
}
