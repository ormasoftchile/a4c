package a4c

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
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
			".a4c":         true,
			"bin":          true,
			".codetopo":    true,
		},
	}
}

// NewStrictWorkspaceSnapshotter returns a snapshotter that only skips VCS and A4C
// bookkeeping (.git, .a4c). It is used wherever a snapshot is a security precondition
// (patch application, acceptance): ignoring bin/ or node_modules/ there would let a
// mutation change the tree without changing the snapshot identity.
func NewStrictWorkspaceSnapshotter() *WorkspaceSnapshotter {
	return &WorkspaceSnapshotter{
		ignoredDirs: map[string]bool{
			".git":      true,
			".a4c":      true,
			".codetopo": true,
		},
	}
}

func writeFrame(w io.Writer, b []byte) {
	var l [8]byte
	binary.BigEndian.PutUint64(l[:], uint64(len(b)))
	_, _ = w.Write(l[:])
	_, _ = w.Write(b)
}

// ComputeSnapshot recursively hashes the tree rooted at root, producing a deterministic
// WorkspaceSnapshotID. Every record is length-framed (path, mode, content) so distinct
// trees cannot collide by shifting bytes between a path and its content. Symlinks are
// hashed by target string and never followed.
func (s *WorkspaceSnapshotter) ComputeSnapshot(root string) (string, error) {
	hasher := sha256.New()
	var paths []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if s.ignoredDirs[info.Name()] && path != root {
			// A linked worktree's ".git" is a regular file; skip it like the directory form.
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
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
		info, err := os.Lstat(fullPath)
		if err != nil {
			return "", fmt.Errorf("snapshot: lstat %s: %w", rel, err)
		}
		writeFrame(hasher, []byte(rel))
		writeFrame(hasher, []byte(fmt.Sprintf("%o", info.Mode()&(os.ModeSymlink|os.ModePerm))))
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(fullPath)
			if err != nil {
				return "", fmt.Errorf("snapshot: readlink %s: %w", rel, err)
			}
			writeFrame(hasher, []byte(target))
			continue
		}
		data, err := os.ReadFile(fullPath)
		if err != nil {
			return "", fmt.Errorf("snapshot: read file %s: %w", rel, err)
		}
		writeFrame(hasher, data)
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// FileSHA256 returns the hex SHA-256 of a regular file, or "" if it does not exist.
func FileSHA256(path string) (string, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256([]byte("symlink:" + t))
		return hex.EncodeToString(sum[:]), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
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
