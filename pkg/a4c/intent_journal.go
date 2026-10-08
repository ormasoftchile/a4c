package a4c

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type EffectReconciler interface {
	// Reconcile queries the external system to check if the effect already occurred.
	// Returns (alreadyOccurred bool, externalID string, err error)
	Reconcile(ctx context.Context, intent IntentRecord) (alreadyOccurred bool, externalID string, err error)
}

type DurableIntentJournal struct {
	mu      sync.Mutex
	dir     string
	intents map[string]*IntentRecord
}

func NewDurableIntentJournal(dir string) (*DurableIntentJournal, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("journal: failed to create dir: %w", err)
	}

	j := &DurableIntentJournal{
		dir:     dir,
		intents: make(map[string]*IntentRecord),
	}

	if err := j.load(); err != nil {
		return nil, err
	}
	return j, nil
}

func (j *DurableIntentJournal) load() error {
	entries, err := os.ReadDir(j.dir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(j.dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var record IntentRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return err
		}
		j.intents[record.IntentID] = &record
	}
	return nil
}

func (j *DurableIntentJournal) persist(record *IntentRecord) error {
	path := filepath.Join(j.dir, fmt.Sprintf("%s.json", record.IntentID))
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (j *DurableIntentJournal) RecordIntent(intent IntentRecord) error {
	j.mu.Lock()
	defer j.mu.Unlock()

	intent.Status = IntentPersisted
	intent.CreatedAt = time.Now()
	intent.UpdatedAt = intent.CreatedAt

	if err := j.persist(&intent); err != nil {
		return err
	}
	j.intents[intent.IntentID] = &intent
	return nil
}

func (j *DurableIntentJournal) RecordOutcome(intentID string, externalID string) error {
	j.mu.Lock()
	defer j.mu.Unlock()

	intent, exists := j.intents[intentID]
	if !exists {
		return fmt.Errorf("journal: intent %q not found", intentID)
	}

	intent.Status = IntentCommitted
	intent.ExternalID = externalID
	intent.UpdatedAt = time.Now()

	return j.persist(intent)
}

// FindUnfinishedIntents returns intents that were persisted before a crash but never marked committed.
func (j *DurableIntentJournal) FindUnfinishedIntents() []IntentRecord {
	j.mu.Lock()
	defer j.mu.Unlock()

	var unfinished []IntentRecord
	for _, intent := range j.intents {
		if intent.Status == IntentPersisted {
			unfinished = append(unfinished, *intent)
		}
	}
	return unfinished
}

// ReconcileUnfinished invokes the domain reconciler for any unfinished intents on restart.
func (j *DurableIntentJournal) ReconcileUnfinished(ctx context.Context, reconciler EffectReconciler) ([]IntentRecord, error) {
	unfinished := j.FindUnfinishedIntents()
	var resolved []IntentRecord

	for _, intent := range unfinished {
		occurred, externalID, err := reconciler.Reconcile(ctx, intent)
		if err != nil {
			return nil, fmt.Errorf("reconciliation error for intent %s: %w", intent.IntentID, err)
		}

		if occurred {
			// External effect did occur! Adopt the external ID and commit outcome without re-executing
			if err := j.RecordOutcome(intent.IntentID, externalID); err != nil {
				return nil, err
			}
			intent.Status = IntentCommitted
			intent.ExternalID = externalID
			resolved = append(resolved, intent)
		}
	}

	return resolved, nil
}

// FindIntentsForActivity returns all intent records associated with an activity ID.
func (j *DurableIntentJournal) FindIntentsForActivity(activityID string) []IntentRecord {
	j.mu.Lock()
	defer j.mu.Unlock()

	var list []IntentRecord
	for _, in := range j.intents {
		if in.ActivityID == activityID {
			list = append(list, *in)
		}
	}
	return list
}
