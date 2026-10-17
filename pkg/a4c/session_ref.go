package a4c

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// SessionRef durably records the host session an activity was executing in, so
// an interrupted same-host task can resume that session (preserving conversational
// context) instead of starting a fresh one. A4C owns the task; the host session
// is a recoverable reference.
type SessionRef struct {
	ActivityID string    `json:"activity_id"`
	Runtime    string    `json:"runtime"`
	SessionID  string    `json:"session_id"`
	Model      string    `json:"model,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// sessionRefPath returns the durable path for an activity's session reference.
func sessionRefPath(workspaceDir, activityID string) string {
	return filepath.Join(workspaceDir, ".a4c", "sessions", activityID+".json")
}

// SaveSessionRef persists the host session a given activity is executing in.
func (c *TeamCoordinator) SaveSessionRef(activityID, runtime, sessionID, model string) {
	if activityID == "" || sessionID == "" || c.WorkspaceDir == "" {
		return
	}
	ref := SessionRef{
		ActivityID: activityID, Runtime: runtime, SessionID: sessionID,
		Model: model, UpdatedAt: time.Now(),
	}
	dir := filepath.Join(c.WorkspaceDir, ".a4c", "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	if data, err := json.Marshal(ref); err == nil {
		// Atomic write (tmp + rename) like the other durable stores.
		tmp := sessionRefPath(c.WorkspaceDir, activityID) + ".tmp"
		if err := os.WriteFile(tmp, data, 0o600); err == nil {
			_ = os.Rename(tmp, sessionRefPath(c.WorkspaceDir, activityID))
		}
	}
}

// LoadSessionRef returns the recorded session for an activity, or nil.
func (c *TeamCoordinator) LoadSessionRef(activityID string) *SessionRef {
	if c.WorkspaceDir == "" {
		return nil
	}
	data, err := os.ReadFile(sessionRefPath(c.WorkspaceDir, activityID))
	if err != nil {
		return nil
	}
	var ref SessionRef
	if err := json.Unmarshal(data, &ref); err != nil || ref.SessionID == "" {
		return nil
	}
	return &ref
}

// ClearSessionRef removes a session reference (called when an activity reaches a
// terminal state so a future re-run of the same ID starts fresh).
func (c *TeamCoordinator) ClearSessionRef(activityID string) {
	if c.WorkspaceDir == "" {
		return
	}
	_ = os.Remove(sessionRefPath(c.WorkspaceDir, activityID))
}
