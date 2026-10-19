package a4c

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type LockMetadata struct {
	PID           int       `json:"pid"`
	CoordinatorID string    `json:"coordinator_id"`
	Hostname      string    `json:"hostname"`
	StartedAt     time.Time `json:"started_at"`
	TaskID        string    `json:"task_id,omitempty"`
}

type WorkspaceLock struct {
	mu       sync.Mutex
	path     string
	file     *os.File
	metadata LockMetadata
	locked   bool
}

// AcquireWorkspaceLock acquires an exclusive kernel-level lock on .a4c/workspace.lock.
// Returns ErrWorkspaceLocked if already held by another process.
func AcquireWorkspaceLock(workspaceDir string, coordID string, taskID string) (*WorkspaceLock, error) {
	if workspaceDir == "" {
		workspaceDir = "."
	}
	dotDir := filepath.Join(workspaceDir, ".a4c")
	if err := os.MkdirAll(dotDir, 0755); err != nil {
		return nil, fmt.Errorf("lock: failed to create directory %s: %w", dotDir, err)
	}
	lockPath := filepath.Join(dotDir, "workspace.lock")

	f, err := openLockFile(lockPath)
	if err != nil {
		return nil, fmt.Errorf("lock: failed to open %s: %w", lockPath, err)
	}

	locked, flockErr := osFlock(f, false)
	if !locked || flockErr != nil {
		// Read existing lock file content to provide actionable error diagnostic
		var meta LockMetadata
		data, readErr := os.ReadFile(lockPath)
		_ = f.Close()
		if readErr == nil && len(data) > 0 {
			_ = json.Unmarshal(data, &meta)
		}
		if meta.PID > 0 {
			return nil, fmt.Errorf("%w: held by PID %d (coordinator %s on host %s since %s)",
				ErrWorkspaceLocked, meta.PID, meta.CoordinatorID, meta.Hostname, meta.StartedAt.Format(time.RFC3339))
		}
		return nil, fmt.Errorf("%w: lock is currently held by another active process (%v)", ErrWorkspaceLocked, flockErr)
	}

	hostname, _ := os.Hostname()
	meta := LockMetadata{
		PID:           os.Getpid(),
		CoordinatorID: coordID,
		Hostname:      hostname,
		StartedAt:     time.Now().UTC(),
		TaskID:        taskID,
	}

	data, err := json.MarshalIndent(meta, "", "  ")
	if err == nil {
		_ = f.Truncate(0)
		_, _ = f.Seek(0, 0)
		_, _ = f.Write(data)
		_ = f.Sync()
	}

	return &WorkspaceLock{
		path:     lockPath,
		file:     f,
		metadata: meta,
		locked:   true,
	}, nil
}

func (l *WorkspaceLock) Release() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.locked || l.file == nil {
		return nil
	}

	// Update metadata to show released before releasing lock
	l.metadata.CoordinatorID = "released"
	data, _ := json.MarshalIndent(l.metadata, "", "  ")
	_ = l.file.Truncate(0)
	_, _ = l.file.Seek(0, 0)
	_, _ = l.file.Write(data)
	_ = l.file.Sync()

	err := osFlockUnlock(l.file)
	closeErr := l.file.Close()
	l.file = nil
	l.locked = false

	if err != nil {
		return err
	}
	return closeErr
}

func (l *WorkspaceLock) Metadata() LockMetadata {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.metadata
}

func (l *WorkspaceLock) IsLocked() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.locked
}
