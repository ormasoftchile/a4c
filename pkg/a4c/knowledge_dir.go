package a4c

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrKnowledgeExists is returned when an A4C-managed write would replace an
// existing knowledge document. Published documents are never overwritten;
// corrections create new documents that reference the earlier ones.
var ErrKnowledgeExists = errors.New("knowledge: document already exists; create a new document that references it instead of overwriting")

// ErrKnowledgeOutsideDir is returned when a claimed knowledge path resolves
// outside the resolved knowledge directory (traversal or absolute escape).
var ErrKnowledgeOutsideDir = errors.New("knowledge: path escapes the knowledge directory")

// KnowledgePathStatus is the verification outcome for a single claimed path.
type KnowledgePathStatus struct {
	Path   string `json:"path"`
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

// knowledgeRootOverride is an internal test hook so tests never touch the
// developer's real home directory. Production always uses ~/.a4c.
var knowledgeRootOverride string

// DefaultKnowledgeRoot returns the root under which per-repository knowledge
// directories live (default: ~/.a4c). It is a separate function so a future
// location override (e.g. an environment variable) changes one place only.
func DefaultKnowledgeRoot() (string, error) {
	if knowledgeRootOverride != "" {
		return knowledgeRootOverride, nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("knowledge: cannot resolve home directory: %w", err)
	}
	return filepath.Join(home, ".a4c"), nil
}

// ResolveKnowledgeDir derives the per-repository knowledge directory
// ~/.a4c/<repo-name>/knowledge from the repository identity of workDir.
//
// The repository name comes from the Git common directory, which resolves to
// the MAIN checkout's .git even when workDir is a linked A4C worktree (whose
// directory is named "tree"). No configuration is required. On any resolution
// failure the caller should degrade to a pending obligation, never fail the
// engineering task.
func ResolveKnowledgeDir(workDir string) (string, error) {
	if strings.TrimSpace(workDir) == "" {
		workDir = "."
	}
	mainRoot, err := mainRepoRoot(workDir)
	if err != nil {
		return "", err
	}
	name := filepath.Base(mainRoot)
	if err := validateRepoName(name); err != nil {
		return "", err
	}
	root, err := DefaultKnowledgeRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, name, "knowledge"), nil
}

// mainRepoRoot returns the MAIN repository root for any directory inside the
// repository (checkout or linked worktree), via git's common-directory pointer.
func mainRepoRoot(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		// Older git without --path-format: resolve a relative result against dir.
		out, err = exec.Command("git", "-C", dir, "rev-parse", "--git-common-dir").Output()
		if err != nil {
			return "", fmt.Errorf("knowledge: cannot resolve repository identity from %s: %w", dir, err)
		}
		gcd := strings.TrimSpace(string(out))
		if !filepath.IsAbs(gcd) {
			gcd = filepath.Join(dir, gcd)
		}
		gcd, err = filepath.Abs(gcd)
		if err != nil {
			return "", fmt.Errorf("knowledge: cannot resolve git common dir: %w", err)
		}
		return filepath.Dir(gcd), nil
	}
	gcd := strings.TrimSpace(string(out))
	if !filepath.IsAbs(gcd) {
		gcd = filepath.Join(dir, gcd)
	}
	return filepath.Dir(gcd), nil
}

func validateRepoName(name string) error {
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("knowledge: invalid repository name %q", name)
	}
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("knowledge: repository name %q is not usable as a knowledge directory", name)
	}
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("knowledge: invalid repository name %q", name)
	}
	return nil
}

// KnowledgePath resolves a claimed knowledge-relative path to an absolute
// path, enforcing containment within kbDir. Traversal, absolute escapes, and
// symlink escapes are rejected. Both sides of the containment comparison are
// canonicalized so platform symlinked directories (e.g. macOS /var) cannot
// cause false escapes, and a symlinked ancestor pointing outside the
// knowledge dir is detected even when the leaf does not exist yet.
func KnowledgePath(kbDir, relPath string) (string, error) {
	if strings.TrimSpace(kbDir) == "" || strings.TrimSpace(relPath) == "" {
		return "", ErrKnowledgeOutsideDir
	}
	kbAbs, err := filepath.Abs(kbDir)
	if err != nil {
		return "", fmt.Errorf("knowledge: resolve knowledge dir: %w", err)
	}
	kbAbs = canonicalizePath(kbAbs)
	// Claims are relative to the knowledge dir. An absolute input is accepted
	// only when it already sits inside the knowledge dir.
	var target string
	if filepath.IsAbs(relPath) {
		target = canonicalizePath(filepath.Clean(relPath))
	} else {
		target = canonicalizePath(filepath.Clean(filepath.Join(kbAbs, relPath)))
	}
	if err := requireContained(kbAbs, target); err != nil {
		return "", err
	}
	return target, nil
}

// canonicalizePath resolves symlinks on the deepest existing ancestor of p
// and re-attaches the non-existent suffix, so containment checks compare
// canonical paths even before intermediate directories are created.
func canonicalizePath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	cur := p
	var suffix []string
	for {
		if _, err := os.Lstat(cur); err == nil {
			if resolved, rerr := filepath.EvalSymlinks(cur); rerr == nil {
				return filepath.Join(append([]string{resolved}, suffix...)...)
			}
			return p
		}
		parent := filepath.Dir(cur)
		if parent == cur || parent == string(filepath.Separator) {
			return p
		}
		suffix = append([]string{filepath.Base(cur)}, suffix...)
		cur = parent
	}
}

func requireContained(kbAbs, target string) error {
	rel, err := filepath.Rel(kbAbs, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ErrKnowledgeOutsideDir
	}
	return nil
}

// VerifyKnowledgePaths checks that each claimed path exists as a non-empty
// regular file inside kbDir. It reports per-path status; it never modifies
// anything and never treats a missing file as success.
func VerifyKnowledgePaths(kbDir string, paths []string) []KnowledgePathStatus {
	out := make([]KnowledgePathStatus, 0, len(paths))
	for _, p := range paths {
		st := KnowledgePathStatus{Path: p}
		abs, err := KnowledgePath(kbDir, p)
		if err != nil {
			st.Reason = err.Error()
			out = append(out, st)
			continue
		}
		fi, err := os.Lstat(abs)
		if err != nil {
			st.Reason = "document does not exist"
			out = append(out, st)
			continue
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			st.Reason = "path is a symlink; knowledge documents must be regular files"
			out = append(out, st)
			continue
		}
		if !fi.Mode().IsRegular() {
			st.Reason = "path is not a regular file"
			out = append(out, st)
			continue
		}
		if fi.Size() == 0 {
			st.Reason = "document is empty"
			out = append(out, st)
			continue
		}
		st.OK = true
		out = append(out, st)
	}
	return out
}

// WriteKnowledgeFile atomically creates a new knowledge document under kbDir.
// It refuses to replace an existing file (ErrKnowledgeExists), enforces path
// containment, rejects empty content, creates parent directories, and verifies
// the file exists after the rename before reporting success.
func WriteKnowledgeFile(kbDir, relPath, content string) (string, error) {
	if strings.TrimSpace(content) == "" {
		return "", fmt.Errorf("knowledge: refusing to write empty document")
	}
	abs, err := KnowledgePath(kbDir, relPath)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(abs); err == nil {
		return "", ErrKnowledgeExists
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("knowledge: stat target: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return "", fmt.Errorf("knowledge: create parent dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".a4c-knowledge-*.tmp")
	if err != nil {
		return "", fmt.Errorf("knowledge: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("knowledge: write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("knowledge: close temp file: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("knowledge: chmod temp file: %w", err)
	}
	if err := os.Rename(tmpName, abs); err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("knowledge: publish document: %w", err)
	}
	// Never claim persistence without checking the result.
	fi, err := os.Lstat(abs)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() == 0 {
		return "", fmt.Errorf("knowledge: document not present after write: %s", abs)
	}
	return abs, nil
}
