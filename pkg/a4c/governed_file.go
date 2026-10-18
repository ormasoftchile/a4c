package a4c

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ApplyWorkspaceFileRequest is a governed, journaled file write against a
// task's isolated worktree. It is the interactive counterpart of the headless
// governed_apply_patch tool: the same path containment, mutating-role
// boundary, policy authorization, and intent-journal guarantees, exposed to
// host-native agents over MCP so a strict-governance profile can route every
// mutation through one audited path.
type ApplyWorkspaceFileRequest struct {
	TaskID  string
	AgentID string
	// Path is relative to the task worktree. Absolute paths and traversal are
	// rejected; the write never escapes the worktree.
	Path string
	// Content is the full file content. Writes are whole-file and atomic
	// (temp file + rename), matching the governed-write semantics used by the
	// headless adapters.
	Content string
}

// ApplyFileResult reports a completed governed write.
type ApplyFileResult struct {
	TaskID       string `json:"task_id"`
	Path         string `json:"path"`
	AbsolutePath string `json:"absolute_path"`
	Bytes        int    `json:"bytes"`
	Sha256       string `json:"sha256"`
	IntentID     string `json:"intent_id"`
	Journaled    bool   `json:"journaled"`
}

// ApplyWorkspaceFile writes req.Content to req.Path inside the task worktree,
// enforcing in order: parameter validity, mutating-role boundary, policy
// authorization, and path containment. The intent is journaled before the
// write and its outcome recorded after, so a crash between the two is
// detectable and reconcilable.
func (s *TeamServiceImpl) ApplyWorkspaceFile(ctx context.Context, req ApplyWorkspaceFileRequest) (*ApplyFileResult, error) {
	taskID := strings.TrimSpace(req.TaskID)
	if taskID == "" {
		return nil, fmt.Errorf("service: task_id is required")
	}
	rel := strings.TrimSpace(req.Path)
	if rel == "" {
		return nil, fmt.Errorf("service: path is required")
	}
	if strings.TrimSpace(req.Content) == "" {
		return nil, fmt.Errorf("service: refusing to write empty content")
	}

	// Non-mutating roles (lead, reviewer) may never write through this path.
	// This mirrors the reviewer boundary the headless adapters enforce, and is
	// belt-and-braces with the role-filtered MCP registration.
	if !isMutatingSpecialist(req.AgentID) {
		return nil, fmt.Errorf("service: agent %q is non-mutating and may not apply workspace files", req.AgentID)
	}

	// Resolve the isolated worktree. GetWorktreePath takes the service lock
	// internally, so it must be called before any lock is held here.
	worktree, err := s.GetWorktreePath(taskID)
	if err != nil {
		return nil, fmt.Errorf("service: resolve worktree: %w", err)
	}
	if strings.TrimSpace(worktree) == "" {
		return nil, fmt.Errorf("service: task %s has no worktree to write into", taskID)
	}

	// Containment: the resolved target must stay inside the worktree. Both
	// sides are canonicalized so a symlinked ancestor cannot smuggle a write
	// outside, matching the knowledge-base containment guarantee.
	abs, err := workspacePath(worktree, rel)
	if err != nil {
		return nil, err
	}

	// Policy authorization, same gate the headless governed tool uses.
	if s.coordinator != nil && s.coordinator.Governor != nil {
		if err := s.coordinator.Governor.AuthorizeToolInvocation(req.AgentID, "governed_apply_patch"); err != nil {
			return nil, fmt.Errorf("service: policy denied governed write for %s: %w", req.AgentID, err)
		}
	}

	sum := sha256.Sum256([]byte(req.Content))
	intentID := fmt.Sprintf("intent-%s-apply-%d", taskID, time.Now().UnixNano())
	journaled := false
	if s.journal != nil {
		if err := s.journal.RecordIntent(IntentRecord{
			IntentID:    intentID,
			ActivityID:  taskID,
			Action:      "apply_patch",
			Target:      abs,
			PayloadHash: hex.EncodeToString(sum[:]),
			Status:      IntentPersisted,
			CreatedAt:   time.Now(),
		}); err == nil {
			journaled = true
		}
	}

	if err := atomicWriteFile(abs, []byte(req.Content)); err != nil {
		if s.journal != nil && journaled {
			_ = s.journal.RecordOutcome(intentID, "failed-"+filepath.Base(abs))
		}
		return nil, fmt.Errorf("service: write %s: %w", rel, err)
	}

	if s.journal != nil && journaled {
		_ = s.journal.RecordOutcome(intentID, "committed-"+filepath.Base(abs))
	}

	return &ApplyFileResult{
		TaskID:       taskID,
		Path:         rel,
		AbsolutePath: abs,
		Bytes:        len(req.Content),
		Sha256:       hex.EncodeToString(sum[:]),
		IntentID:     intentID,
		Journaled:    journaled,
	}, nil
}

// workspacePath resolves a worktree-relative path, rejecting absolute paths
// and traversal, and enforces containment within the worktree.
func workspacePath(worktree, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("service: path must be worktree-relative, got absolute %q", rel)
	}
	cleaned := filepath.Clean(rel)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("service: path escapes the worktree: %q", rel)
	}
	root, err := filepath.Abs(worktree)
	if err != nil {
		return "", fmt.Errorf("service: resolve worktree: %w", err)
	}
	root = canonicalizePath(root)
	target := canonicalizePath(filepath.Join(root, cleaned))
	if err := requireContained(root, target); err != nil {
		return "", fmt.Errorf("service: path escapes the worktree: %q", rel)
	}
	return target, nil
}

// atomicWriteFile writes to a temporary file in the target directory and
// renames it into place, so a crash mid-write cannot leave a truncated file.
func atomicWriteFile(target string, content []byte) error {
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".a4c-write-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, target); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	// Never report success without confirming the file landed.
	fi, err := os.Lstat(target)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != int64(len(content)) {
		return fmt.Errorf("file not present after write: %s", target)
	}
	return nil
}
