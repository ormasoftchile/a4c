package a4c

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

type WorkspaceSnapshotter struct {
	ignoredDirs map[string]bool
}

func NewWorkspaceSnapshotter() *WorkspaceSnapshotter {
	return &WorkspaceSnapshotter{
		ignoredDirs: map[string]bool{
			".git":         true,
			"node_modules": true,
			".squad":       true,
		},
	}
}

// ComputeSnapshot recursively hashes the files in dir, producing a deterministic WorkspaceSnapshotID.
func (s *WorkspaceSnapshotter) ComputeSnapshot(root string) (string, error) {
	hasher := sha256.New()
	var paths []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if s.ignoredDirs[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("snapshot: walk error: %w", err)
	}

	sort.Strings(paths)

	for _, rel := range paths {
		fullPath := filepath.Join(root, rel)
		data, err := os.ReadFile(fullPath)
		if err != nil {
			return "", fmt.Errorf("snapshot: read file %s: %w", rel, err)
		}
		// Hash rel path + content
		hasher.Write([]byte(rel))
		hasher.Write(data)
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// ValidateIntegrity checks that content before and after testing remained invariant.
func (s *WorkspaceSnapshotter) ValidateIntegrity(before, after string) error {
	if before != after {
		return fmt.Errorf("%w: snapshot before (%s) != snapshot after (%s)", ErrWorkspaceConcurrentEdit, before, after)
	}
	return nil
}

// ValidateAgainstEvidence checks that the current workspace matches the snapshot on the evidence record.
func (s *WorkspaceSnapshotter) ValidateAgainstEvidence(currentRoot string, evidence ValidationEvidence) error {
	current, err := s.ComputeSnapshot(currentRoot)
	if err != nil {
		return err
	}
	if current != evidence.WorkspaceSnapshotID {
		return fmt.Errorf("%w: current workspace (%s) diverges from validated evidence (%s)",
			ErrWorkspaceConcurrentEdit, current, evidence.WorkspaceSnapshotID)
	}
	if evidence.ExitCode != 0 {
		return fmt.Errorf("evidence: validation failed with exit code %d", evidence.ExitCode)
	}
	return nil
}
