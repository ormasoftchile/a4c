package a4c

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type BudgetTxType string

const (
	TxReserve BudgetTxType = "reserve"
	TxSettle  BudgetTxType = "settle"
	TxRelease BudgetTxType = "release"
)

// BudgetTx models a durable transaction journal entry for hierarchical multi-record budget operations.
type BudgetTx struct {
	TxID      string       `json:"tx_id"`
	Type      BudgetTxType `json:"type"`
	ParentID  string       `json:"parent_id"`
	ChildID   string       `json:"child_id"`
	Allocated float64      `json:"allocated,omitempty"`
	Consumed  float64      `json:"consumed,omitempty"`
	Status    string       `json:"status"` // "prepared", "committed"
	Timestamp time.Time    `json:"timestamp"`
}

type PolicyGovernor struct {
	mu                  sync.Mutex
	storageDir          string
	allowedTools        map[string]bool
	agentTools          map[string]map[string]bool
	activeBudgets       map[string]*Budget
	budgetRecords       map[string]*BudgetRecord
	authorizedProposals map[string]*ActivityRequest
}

func NewPolicyGovernor(allowedTools []string) *PolicyGovernor {
	tools := make(map[string]bool)
	for _, t := range allowedTools {
		tools[t] = true
	}
	return &PolicyGovernor{
		allowedTools:        tools,
		agentTools:          make(map[string]map[string]bool),
		activeBudgets:       make(map[string]*Budget),
		budgetRecords:       make(map[string]*BudgetRecord),
		authorizedProposals: make(map[string]*ActivityRequest),
	}
}

// SetStorageDir configures durable persistence directory for budget records and loads existing records.
func (g *PolicyGovernor) SetStorageDir(dir string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if dir == "" {
		g.storageDir = ""
		return nil
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("governor: failed to create budget storage dir %s: %w", dir, err)
	}
	g.storageDir = dir

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("governor: failed to read budget storage dir: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || strings.HasSuffix(entry.Name(), ".tmp") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var rec BudgetRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			continue
		}
		g.budgetRecords[rec.ID] = &rec
		g.activeBudgets[rec.ID] = &Budget{
			Allocated: rec.Allocated,
			Reserved:  rec.Reserved,
			Consumed:  rec.Consumed,
		}
	}

	// Persist any existing in-memory active budgets that were not on disk
	for id, b := range g.activeBudgets {
		if _, exists := g.budgetRecords[id]; !exists {
			rec := &BudgetRecord{
				ID:        id,
				Allocated: b.Allocated,
				Reserved:  b.Reserved,
				Consumed:  b.Consumed,
				Status:    "active",
				UpdatedAt: time.Now(),
			}
			g.budgetRecords[id] = rec
			_ = g.persistBudgetLocked(id)
		}
	}

	return nil
}

func (g *PolicyGovernor) persistBudgetLocked(id string) error {
	if g.storageDir == "" {
		return nil
	}
	rec, ok := g.budgetRecords[id]
	if !ok {
		b, exists := g.activeBudgets[id]
		if !exists {
			return nil
		}
		rec = &BudgetRecord{
			ID:        id,
			Allocated: b.Allocated,
			Reserved:  b.Reserved,
			Consumed:  b.Consumed,
			Status:    "active",
			UpdatedAt: time.Now(),
		}
		g.budgetRecords[id] = rec
	}

	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}

	tmpFile := filepath.Join(g.storageDir, fmt.Sprintf("%s.tmp", id))
	targetFile := filepath.Join(g.storageDir, fmt.Sprintf("%s.json", id))

	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmpFile, targetFile)
}

func (g *PolicyGovernor) logTxLocked(tx BudgetTx) {
	if g.storageDir == "" {
		return
	}
	path := filepath.Join(g.storageDir, "transactions.jsonl")
	data, err := json.Marshal(tx)
	if err != nil {
		return
	}
	data = append(data, '\n')
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err == nil {
		_, _ = f.Write(data)
		_ = f.Close()
	}
}

// SetAgentTools configures role-based tool capabilities for a specific agent identity.
func (g *PolicyGovernor) SetAgentTools(agentID string, tools []string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	tm := make(map[string]bool)
	for _, t := range tools {
		tm[t] = true
	}
	g.agentTools[agentID] = tm
}

// AuthorizeProposal evaluates a child proposal against parent's budget and idempotency rules.
func (g *PolicyGovernor) AuthorizeProposal(parent ActivityRequest, proposal ActivityProposal, newID string) (*ActivityRequest, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	// 1. Idempotency check: prevent duplicate delegation after restart/interruption
	key := proposal.ProposalID
	if key == "" {
		h := sha256.New()
		h.Write([]byte(parent.ID))
		h.Write([]byte(proposal.ProposedAgentID))
		h.Write([]byte(proposal.Objective))
		key = hex.EncodeToString(h.Sum(nil))
	}
	dedupKey := fmt.Sprintf("%s:%s", parent.ID, key)
	if existing, found := g.authorizedProposals[dedupKey]; found {
		return existing, nil
	}

	// 2. Budget verification
	budget, ok := g.activeBudgets[parent.ID]
	if !ok {
		budgetCopy := parent.Budget
		budget = &budgetCopy
		g.activeBudgets[parent.ID] = budget
	}

	requested := proposal.RequestedBudget.Allocated
	if budget.Allocated > 0 && requested > budget.Available() {
		return nil, fmt.Errorf("%w: requested %.2f exceeds parent available %.2f",
			ErrBudgetExceeded, requested, budget.Available())
	}

	// Deduct requested from parent reservation
	txID := fmt.Sprintf("tx-res-%d-%s", time.Now().UnixNano(), newID)
	g.logTxLocked(BudgetTx{
		TxID:      txID,
		Type:      TxReserve,
		ParentID:  parent.ID,
		ChildID:   newID,
		Allocated: requested,
		Status:    "prepared",
		Timestamp: time.Now().UTC(),
	})

	budget.Reserved += requested
	if parentRec, exists := g.budgetRecords[parent.ID]; exists {
		parentRec.Reserved = budget.Reserved
		parentRec.UpdatedAt = time.Now()
		_ = g.persistBudgetLocked(parent.ID)
	} else {
		g.budgetRecords[parent.ID] = &BudgetRecord{
			ID:        parent.ID,
			Allocated: budget.Allocated,
			Reserved:  budget.Reserved,
			Consumed:  budget.Consumed,
			Status:    "active",
			UpdatedAt: time.Now(),
		}
		_ = g.persistBudgetLocked(parent.ID)
	}

	childBudget := Budget{
		Allocated: requested,
		Reserved:  0,
		Consumed:  0,
	}

	childReq := &ActivityRequest{
		ID:        newID,
		ParentID:  parent.ID,
		AgentID:   proposal.ProposedAgentID,
		Objective: proposal.Objective,
		Inputs:    proposal.Inputs,
		DependsOn: proposal.DependsOn,
		Budget:    childBudget,
		CreatedAt: time.Now(),
	}

	childBudgetCopy := childBudget
	g.activeBudgets[newID] = &childBudgetCopy
	g.budgetRecords[newID] = &BudgetRecord{
		ID:        newID,
		ParentID:  parent.ID,
		Allocated: requested,
		Reserved:  0,
		Consumed:  0,
		Status:    "active",
		UpdatedAt: time.Now(),
	}
	_ = g.persistBudgetLocked(newID)

	g.logTxLocked(BudgetTx{
		TxID:      txID,
		Type:      TxReserve,
		ParentID:  parent.ID,
		ChildID:   newID,
		Allocated: requested,
		Status:    "committed",
		Timestamp: time.Now().UTC(),
	})

	g.authorizedProposals[dedupKey] = childReq
	return childReq, nil
}

// SetBudget sets or updates the budget record for an activity or root task.
func (g *PolicyGovernor) SetBudget(activityID string, b Budget) {
	g.mu.Lock()
	defer g.mu.Unlock()

	bCopy := b
	g.activeBudgets[activityID] = &bCopy
	if rec, ok := g.budgetRecords[activityID]; ok {
		rec.Allocated = b.Allocated
		rec.Reserved = b.Reserved
		rec.Consumed = b.Consumed
		rec.UpdatedAt = time.Now()
		_ = g.persistBudgetLocked(activityID)
	} else {
		g.budgetRecords[activityID] = &BudgetRecord{
			ID:        activityID,
			Allocated: b.Allocated,
			Reserved:  b.Reserved,
			Consumed:  b.Consumed,
			Status:    "active",
			UpdatedAt: time.Now(),
		}
		_ = g.persistBudgetLocked(activityID)
	}
}

// RecordConsumption updates the consumed tokens for an activity and settles reservations on parent.
func (g *PolicyGovernor) RecordConsumption(activityID, parentID string, tokens float64) {
	g.mu.Lock()
	defer g.mu.Unlock()

	txID := fmt.Sprintf("tx-set-%d-%s", time.Now().UnixNano(), activityID)
	g.logTxLocked(BudgetTx{
		TxID:      txID,
		Type:      TxSettle,
		ParentID:  parentID,
		ChildID:   activityID,
		Consumed:  tokens,
		Status:    "prepared",
		Timestamp: time.Now().UTC(),
	})

	var allocated float64
	if b, ok := g.activeBudgets[activityID]; ok {
		b.Consumed += tokens
		allocated = b.Allocated
		b.Reserved = 0
	} else {
		g.activeBudgets[activityID] = &Budget{Consumed: tokens}
	}

	// Update child budget record
	if cRec, ok := g.budgetRecords[activityID]; ok {
		cRec.Consumed = g.activeBudgets[activityID].Consumed
		cRec.Reserved = 0
		cRec.Status = "settled"
		cRec.UpdatedAt = time.Now()
		_ = g.persistBudgetLocked(activityID)
	} else {
		g.budgetRecords[activityID] = &BudgetRecord{
			ID:        activityID,
			ParentID:  parentID,
			Allocated: allocated,
			Reserved:  0,
			Consumed:  tokens,
			Status:    "settled",
			UpdatedAt: time.Now(),
		}
		_ = g.persistBudgetLocked(activityID)
	}

	// Update parent budget and reservation
	if parentID != "" && parentID != activityID {
		if p, ok := g.activeBudgets[parentID]; ok {
			p.Consumed += tokens
			if allocated > 0 {
				if p.Reserved >= allocated {
					p.Reserved -= allocated
				} else {
					p.Reserved = 0
				}
			} else if p.Reserved >= tokens {
				p.Reserved -= tokens
			} else {
				p.Reserved = 0
			}
			if pRec, ok := g.budgetRecords[parentID]; ok {
				pRec.Consumed = p.Consumed
				pRec.Reserved = p.Reserved
				pRec.UpdatedAt = time.Now()
				_ = g.persistBudgetLocked(parentID)
			}
		}
	}

	g.logTxLocked(BudgetTx{
		TxID:      txID,
		Type:      TxSettle,
		ParentID:  parentID,
		ChildID:   activityID,
		Consumed:  tokens,
		Status:    "committed",
		Timestamp: time.Now().UTC(),
	})
}

// ExtendBudget increases the allocated tokens for an activity.
func (g *PolicyGovernor) ExtendBudget(activityID string, additionalTokens float64) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	b, ok := g.activeBudgets[activityID]
	if !ok {
		return fmt.Errorf("governor: budget for activity %q not found", activityID)
	}
	b.Allocated += additionalTokens
	if rec, ok := g.budgetRecords[activityID]; ok {
		rec.Allocated = b.Allocated
		rec.UpdatedAt = time.Now()
		_ = g.persistBudgetLocked(activityID)
	}
	return nil
}

// ReleaseReservation releases unused budget allocation from a child back to its parent reservation.
func (g *PolicyGovernor) ReleaseReservation(activityID, parentID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	b, ok := g.activeBudgets[activityID]
	if ok {
		unconsumed := b.Allocated - b.Consumed
		txID := fmt.Sprintf("tx-rel-%d-%s", time.Now().UnixNano(), activityID)
		g.logTxLocked(BudgetTx{
			TxID:      txID,
			Type:      TxRelease,
			ParentID:  parentID,
			ChildID:   activityID,
			Allocated: unconsumed,
			Status:    "prepared",
			Timestamp: time.Now().UTC(),
		})

		if unconsumed > 0 && parentID != "" && parentID != activityID {
			if p, ok := g.activeBudgets[parentID]; ok {
				if p.Reserved >= unconsumed {
					p.Reserved -= unconsumed
				} else {
					p.Reserved = 0
				}
				if pRec, ok := g.budgetRecords[parentID]; ok {
					pRec.Reserved = p.Reserved
					pRec.UpdatedAt = time.Now()
					_ = g.persistBudgetLocked(parentID)
				}
			}
		}
		b.Reserved = 0
		if rec, ok := g.budgetRecords[activityID]; ok {
			rec.Status = "released"
			rec.UpdatedAt = time.Now()
			_ = g.persistBudgetLocked(activityID)
		}

		g.logTxLocked(BudgetTx{
			TxID:      txID,
			Type:      TxRelease,
			ParentID:  parentID,
			ChildID:   activityID,
			Allocated: unconsumed,
			Status:    "committed",
			Timestamp: time.Now().UTC(),
		})
	}
	return nil
}

// ReconcileWithStore audits budget state against durable activity store records to resolve crash inconsistencies.
func (g *PolicyGovernor) ReconcileWithStore(store *DurableActivityStore) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	for id, rec := range g.budgetRecords {
		if rec.ParentID == "" {
			continue
		}

		parentRec, parentExists := g.budgetRecords[rec.ParentID]
		pBudget, pActive := g.activeBudgets[rec.ParentID]

		actRec, err := store.Get(id)
		if err != nil || actRec == nil {
			// Scenario A: Child was reserved but activity was never created/started before crash
			if rec.Status == "active" {
				unconsumed := rec.Allocated - rec.Consumed
				if unconsumed > 0 && parentExists {
					if parentRec.Reserved >= unconsumed {
						parentRec.Reserved -= unconsumed
					} else {
						parentRec.Reserved = 0
					}
					if pActive {
						pBudget.Reserved = parentRec.Reserved
					}
					parentRec.UpdatedAt = time.Now()
					_ = g.persistBudgetLocked(rec.ParentID)
				}
				rec.Status = "released"
				rec.UpdatedAt = time.Now()
				_ = g.persistBudgetLocked(id)
			}
			continue
		}

		switch actRec.Status {
		case StatusCompleted:
			// Scenario C: Child completed in store, ensure settlement is completed on parent
			if rec.Status != "settled" {
				rec.Status = "settled"
				rec.UpdatedAt = time.Now()
				_ = g.persistBudgetLocked(id)
			}
			if parentExists {
				if parentRec.Reserved >= rec.Allocated {
					parentRec.Reserved -= rec.Allocated
				}
				if pActive {
					pBudget.Reserved = parentRec.Reserved
				}
				parentRec.UpdatedAt = time.Now()
				_ = g.persistBudgetLocked(rec.ParentID)
			}

		case StatusClaimed:
			// Scenario B: Child was running when crash terminated process
			failErr := fmt.Errorf("process terminated during execution (recovered on startup)")
			failResult := ActivityResult{
				ID:       id,
				Status:   StatusFailed,
				Consumed: Budget{Consumed: rec.Consumed},
				Outputs: map[string]any{
					"error":     failErr.Error(),
					"recovered": true,
				},
			}
			_ = store.FailWithResult(id, failResult, failErr)

			if parentExists {
				parentRec.Consumed += rec.Consumed
				if parentRec.Reserved >= rec.Allocated {
					parentRec.Reserved -= rec.Allocated
				} else {
					parentRec.Reserved = 0
				}
				if pActive {
					pBudget.Consumed = parentRec.Consumed
					pBudget.Reserved = parentRec.Reserved
				}
				parentRec.UpdatedAt = time.Now()
				_ = g.persistBudgetLocked(rec.ParentID)
			}
			rec.Status = "released"
			rec.UpdatedAt = time.Now()
			_ = g.persistBudgetLocked(id)

		case StatusFailed, StatusIndeterminate:
			// Failed child: release unconsumed reservation back to parent
			if rec.Status == "active" {
				unconsumed := rec.Allocated - rec.Consumed
				if unconsumed > 0 && parentExists {
					if parentRec.Reserved >= unconsumed {
						parentRec.Reserved -= unconsumed
					} else {
						parentRec.Reserved = 0
					}
					if pActive {
						pBudget.Reserved = parentRec.Reserved
					}
					parentRec.UpdatedAt = time.Now()
					_ = g.persistBudgetLocked(rec.ParentID)
				}
				rec.Status = "released"
				rec.UpdatedAt = time.Now()
				_ = g.persistBudgetLocked(id)
			}
		}
	}

	return nil
}

// CheckBudget verifies whether an activity has sufficient budget to proceed.
func (g *PolicyGovernor) CheckBudget(activityID string, additionalTokens float64) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	b, ok := g.activeBudgets[activityID]
	if !ok || b.Allocated <= 0 {
		return nil // Unconstrained or unrecorded
	}
	if b.Consumed+additionalTokens > b.Allocated {
		return fmt.Errorf("%w: consumption %.0f exceeds allocation %.0f for activity %s",
			ErrBudgetExceeded, b.Consumed+additionalTokens, b.Allocated, activityID)
	}
	return nil
}

// AuthorizeToolInvocation gates direct tool calls from agent sessions, rejecting ambient unmediated bypasses.
func (g *PolicyGovernor) AuthorizeToolInvocation(agentID string, toolName string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	// Check agent-specific role permissions first if defined
	if specific, ok := g.agentTools[agentID]; ok {
		if !specific[toolName] {
			return fmt.Errorf("%w: agent %q not authorized for tool %q", ErrUnauthorizedTool, agentID, toolName)
		}
		return nil
	}

	// Fallback to global tool allowlist
	if !g.allowedTools[toolName] {
		return fmt.Errorf("%w: agent %q attempted unmediated tool %q", ErrUnauthorizedTool, agentID, toolName)
	}
	return nil
}

// GetBudget retrieves active budget metrics tracked for an activity ID.
func (g *PolicyGovernor) GetBudget(activityID string) (Budget, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	b, ok := g.activeBudgets[activityID]
	if !ok {
		return Budget{}, false
	}
	return *b, true
}

// GetBudgetRecord returns the durable record for a given budget ID if tracked.
func (g *PolicyGovernor) GetBudgetRecord(activityID string) (*BudgetRecord, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	rec, ok := g.budgetRecords[activityID]
	if !ok {
		return nil, false
	}
	recCopy := *rec
	return &recCopy, true
}
