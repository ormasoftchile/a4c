package a4c

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	ErrWorktreeConflict = errors.New("worktree: active lease held by another task")
	ErrWorktreeNotFound = errors.New("worktree: record not found")
	ErrWorktreeDirty    = errors.New("worktree: uncommitted changes exist; refusal to destroy work")
)

type WorktreeStatus string

const (
	WorktreeStatusActive   WorktreeStatus = "active"
	WorktreeStatusReleased WorktreeStatus = "released"
	WorktreeStatusFailed   WorktreeStatus = "failed"
)

// WorktreeRecord tracks an isolated git worktree for a task.
type WorktreeRecord struct {
	TaskID       string         `json:"task_id"`
	Branch       string         `json:"branch"`
	WorktreePath string         `json:"worktree_path"`
	OwnerAgent   string         `json:"owner_agent"`
	Status       WorktreeStatus `json:"status"`
	LeaseHeld    bool           `json:"lease_held"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
}

// WorktreeManager manages dedicated Git worktrees for coding assignments.
type WorktreeManager struct {
	mu       sync.Mutex
	rootDir  string // base repo directory (user checkout)
	stateDir string // .a4c/worktrees directory
	records  map[string]*WorktreeRecord
}

func NewWorktreeManager(rootDir, stateDir string) (*WorktreeManager, error) {
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return nil, fmt.Errorf("worktree: failed to create state dir: %w", err)
	}

	mgr := &WorktreeManager{
		rootDir:  rootDir,
		stateDir: stateDir,
		records:  make(map[string]*WorktreeRecord),
	}

	if err := mgr.load(); err != nil {
		return nil, fmt.Errorf("worktree: failed to load records: %w", err)
	}

	return mgr, nil
}

func (m *WorktreeManager) load() error {
	entries, err := os.ReadDir(m.stateDir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(m.stateDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var rec WorktreeRecord
		if err := json.Unmarshal(data, &rec); err == nil {
			m.records[rec.TaskID] = &rec
		}
	}
	return nil
}

func (m *WorktreeManager) persist(rec *WorktreeRecord) error {
	path := filepath.Join(m.stateDir, fmt.Sprintf("%s.json", rec.TaskID))
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

// EnsureGitRepo ensures the root directory has a valid git repository with at least one commit.
func (m *WorktreeManager) EnsureGitRepo(ctx context.Context) error {
	gitDir := filepath.Join(m.rootDir, ".git")
	if _, err := os.Stat(gitDir); os.IsNotExist(err) {
		cmd := exec.CommandContext(ctx, "git", "init")
		cmd.Dir = m.rootDir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git init failed: %s (%w)", string(out), err)
		}
	}

	// Check if HEAD exists
	cmdRev := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	cmdRev.Dir = m.rootDir
	if err := cmdRev.Run(); err != nil {
		// Create initial empty commit
		cmdCommit := exec.CommandContext(ctx, "git", "commit", "--allow-empty", "-m", "chore: initial commit")
		cmdCommit.Dir = m.rootDir
		_ = cmdCommit.Run()
	}
	return nil
}

// AcquireWorktree creates or resumes an isolated Git worktree for the given task.
func (m *WorktreeManager) AcquireWorktree(ctx context.Context, taskID, ownerAgent string) (*WorktreeRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.EnsureGitRepo(ctx); err != nil {
		return nil, err
	}

	// Check if already exists (restart recovery)
	if existing, found := m.records[taskID]; found {
		if existing.LeaseHeld && existing.OwnerAgent != ownerAgent {
			return nil, fmt.Errorf("%w: task %s owned by %s", ErrWorktreeConflict, taskID, existing.OwnerAgent)
		}
		// Confirm directory exists on disk
		if _, err := os.Stat(existing.WorktreePath); err == nil {
			existing.LeaseHeld = true
			existing.Status = WorktreeStatusActive
			existing.UpdatedAt = time.Now()
			_ = m.persist(existing)
			return existing, nil
		}
	}

	// Generate safe branch name
	safeID := strings.ReplaceAll(taskID, " ", "-")
	safeID = strings.ReplaceAll(safeID, "/", "-")
	branchName := fmt.Sprintf("a4c/%s", safeID)

	worktreeDir := filepath.Join(m.stateDir, safeID, "tree")
	if err := os.MkdirAll(filepath.Dir(worktreeDir), 0755); err != nil {
		return nil, err
	}

	// Add worktree using git
	cmdAdd := exec.CommandContext(ctx, "git", "worktree", "add", "-b", branchName, worktreeDir, "HEAD")
	cmdAdd.Dir = m.rootDir
	if out, err := cmdAdd.CombinedOutput(); err != nil {
		// If branch already exists, attach without -b
		if strings.Contains(string(out), "already exists") {
			cmdAttach := exec.CommandContext(ctx, "git", "worktree", "add", worktreeDir, branchName)
			cmdAttach.Dir = m.rootDir
			if outAttach, errAttach := cmdAttach.CombinedOutput(); errAttach != nil {
				return nil, fmt.Errorf("failed to attach existing worktree branch: %s (%w)", string(outAttach), errAttach)
			}
		} else {
			return nil, fmt.Errorf("git worktree add failed: %s (%w)", string(out), err)
		}
	}

	// Bootstrap CodeTopo index for worktree if codetopo is available and primary repo is indexed
	primaryDB := filepath.Join(m.rootDir, ".codetopo", "index.sqlite")
	if _, statErr := os.Stat(primaryDB); statErr == nil {
		if ctPath := FindCodeTopoCLI(); ctPath != "" {
			cmdInit := exec.CommandContext(ctx, ctPath, "worktree", "init", "--root", worktreeDir)
			_ = cmdInit.Run()
		}
	}

	record := &WorktreeRecord{
		TaskID:       taskID,
		Branch:       branchName,
		WorktreePath: worktreeDir,
		OwnerAgent:   ownerAgent,
		Status:       WorktreeStatusActive,
		LeaseHeld:    true,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}

	if err := m.persist(record); err != nil {
		return nil, err
	}

	m.records[taskID] = record
	return record, nil
}

// ReleaseWorktree releases the active lease on a worktree without deleting uncommitted changes.
func (m *WorktreeManager) ReleaseWorktree(taskID string, success bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	rec, found := m.records[taskID]
	if !found {
		return ErrWorktreeNotFound
	}

	rec.LeaseHeld = false
	if success {
		rec.Status = WorktreeStatusReleased
	} else {
		rec.Status = WorktreeStatusFailed
	}
	rec.UpdatedAt = time.Now()

	return m.persist(rec)
}

// GetWorktree returns the record for taskID.
func (m *WorktreeManager) GetWorktree(taskID string) (*WorktreeRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	rec, found := m.records[taskID]
	if !found {
		return nil, ErrWorktreeNotFound
	}
	cp := *rec
	return &cp, nil
}

// ListWorktrees returns all known worktrees.
func (m *WorktreeManager) ListWorktrees() []*WorktreeRecord {
	m.mu.Lock()
	defer m.mu.Unlock()

	var list []*WorktreeRecord
	for _, rec := range m.records {
		cp := *rec
		list = append(list, &cp)
	}
	return list
}

// RemoveWorktree safely removes a worktree if it has no uncommitted changes.
// If uncommitted changes exist and force is false, it returns ErrWorktreeDirty to preserve work.
func (m *WorktreeManager) RemoveWorktree(ctx context.Context, taskID string, force bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	rec, found := m.records[taskID]
	if !found {
		return ErrWorktreeNotFound
	}

	// Check if dirty
	cmdStat := exec.CommandContext(ctx, "git", "status", "--porcelain")
	cmdStat.Dir = rec.WorktreePath
	out, err := cmdStat.CombinedOutput()
	if err == nil && len(strings.TrimSpace(string(out))) > 0 && !force {
		return fmt.Errorf("%w: path %s has uncommitted changes", ErrWorktreeDirty, rec.WorktreePath)
	}

	// Remove worktree
	cmdRm := exec.CommandContext(ctx, "git", "worktree", "remove", rec.WorktreePath)
	if force {
		cmdRm = exec.CommandContext(ctx, "git", "worktree", "remove", "--force", rec.WorktreePath)
	}
	cmdRm.Dir = m.rootDir
	_ = cmdRm.Run()

	_ = os.RemoveAll(filepath.Dir(rec.WorktreePath))
	rec.LeaseHeld = false
	rec.Status = WorktreeStatusReleased
	_ = m.persist(rec)
	delete(m.records, taskID)
	return nil
}

