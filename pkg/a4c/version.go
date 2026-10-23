package a4c

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// versionFile holds the a4c release version, stamped into the binary at build
// time so `a4c --version` never depends on a file being present at runtime.
//
//go:embed VERSION
var versionFile string

// Version is the a4c release version (the trimmed contents of the VERSION
// file).
var Version = strings.TrimSpace(versionFile)

// VersionFilePath is the workspace-relative location of the release version.
// It sits next to the package it stamps so the embedded value and the file a
// release bump rewrites are the same source of truth.
const VersionFilePath = "pkg/a4c/VERSION"

// WorkspaceVersionFile returns the absolute path of the release version file
// inside the given workspace checkout.
func WorkspaceVersionFile(workspaceDir string) string {
	return filepath.Join(workspaceDir, filepath.FromSlash(VersionFilePath))
}

// BumpPatch returns v with its patch component incremented: "0.1.0" becomes
// "0.1.1" and "0.1.9" becomes "0.1.10". Anything that is not a plain
// three-component version is an error rather than a silently passed-through
// value, because a release rule that skips silently is a release rule nobody
// can trust.
func BumpPatch(v string) (string, error) {
	parts := strings.Split(strings.TrimSpace(v), ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("version: %q is not a major.minor.patch version", v)
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return "", fmt.Errorf("version: %q is not a major.minor.patch version", v)
		}
		nums[i] = n
	}
	nums[2]++
	return fmt.Sprintf("%d.%d.%d", nums[0], nums[1], nums[2]), nil
}

// PendingVersionBump computes the patch-level successor of the version file in
// workspaceDir. ok is false when the workspace carries no version file (an
// ordinary project a4c manages) or its contents are not a plain
// major.minor.patch version; in both cases there is nothing to bump.
func PendingVersionBump(workspaceDir string) (from, to string, ok bool) {
	data, err := os.ReadFile(WorkspaceVersionFile(workspaceDir))
	if err != nil {
		return "", "", false
	}
	from = strings.TrimSpace(string(data))
	to, err = BumpPatch(from)
	if err != nil {
		return "", "", false
	}
	return from, to, true
}

// NextRelease decides the version an integration must write given the version
// currently in the tree and the bump recorded at acceptance. It returns false
// when no write is needed:
//
//   - the bump recorded at acceptance is already present (a retried
//     integration must not bump twice), or
//   - there is no version file to advance.
//
// A recorded bump whose recorded "from" no longer matches the tree (the change
// itself moved the version) is recomputed from the tree, so the release rule
// always advances the version that is actually being shipped.
func NextRelease(current, recordedFrom, recordedTo string) (string, bool) {
	if recordedTo != "" {
		if current == recordedTo {
			return recordedTo, false
		}
		if recordedFrom == "" || current == recordedFrom {
			return recordedTo, true
		}
	}
	next, err := BumpPatch(current)
	if err != nil {
		return "", false
	}
	return next, true
}
