package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ormasoftchile/a4c/pkg/a4c"
)

// assertOptedOut fails unless capture is disabled for the workspace.
func assertOptedOut(t *testing.T, dir string) {
	t.Helper()
	if a4c.KnowledgeEnabled(dir) {
		t.Fatal("expected workspace to be opted out of knowledge capture")
	}
}

// settingRecorded reports whether a settings file exists and what it says. It
// exists so a rejected flag can be asserted to have written nothing at all.
func settingRecorded(t *testing.T, dir string) (exists, enabled bool) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, ".a4c", "config.json")); err != nil {
		return false, false
	}
	return true, a4c.KnowledgeEnabled(dir)
}

// `a4c init --no-knowledge` writes the durable opt-out; a plain init must not
// create the settings file at all (so it can never flip an existing opt-out).
func TestInitNoKnowledgeWritesOptOut(t *testing.T) {
	dir := t.TempDir()
	out, errOut := captureOutput(t, func() {
		runInit([]string{dir, "--no-install", "--no-knowledge"})
	})
	if !strings.Contains(out, "Knowledge capture: disabled") {
		t.Fatalf("init did not announce the opt-out: stdout=%q stderr=%q", out, errOut)
	}
	cfgPath := filepath.Join(dir, ".a4c", "config.json")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("config not written: %v", err)
	}
	if !strings.Contains(string(data), `"enabled": false`) {
		t.Fatalf("config does not record the opt-out: %s", data)
	}
	assertOptedOut(t, dir)
}

func TestInitWithoutFlagLeavesOptOutUntouched(t *testing.T) {
	dir := t.TempDir()
	// Opt out first.
	captureOutput(t, func() { runInit([]string{dir, "--no-install", "--no-knowledge"}) })
	// Re-run init with no knowledge flag (the common case: adding hosts).
	captureOutput(t, func() { runInit([]string{dir, "--no-install"}) })
	if a4c.KnowledgeEnabled(dir) {
		t.Fatal("plain init re-enabled knowledge and silently undid the opt-out")
	}

	// Fresh workspace, plain init: no settings file is created.
	fresh := t.TempDir()
	captureOutput(t, func() { runInit([]string{fresh, "--no-install"}) })
	if _, err := os.Stat(filepath.Join(fresh, ".a4c", "config.json")); !os.IsNotExist(err) {
		t.Fatalf("plain init created a config file it had no reason to write (err=%v)", err)
	}
}

func TestInitKnowledgeFlagReEnables(t *testing.T) {
	dir := t.TempDir()
	captureOutput(t, func() { runInit([]string{dir, "--no-install", "--no-knowledge"}) })
	out, _ := captureOutput(t, func() { runInit([]string{dir, "--no-install", "--knowledge"}) })
	if !strings.Contains(out, "Knowledge capture: enabled") {
		t.Fatalf("re-enable not announced: %q", out)
	}
	if !a4c.KnowledgeEnabled(dir) {
		t.Fatal("--knowledge did not re-enable capture")
	}
}

// Unknown init arguments fail loudly instead of being silently dropped —
// a typo like --no-knowledg must not look like a successful opt-out.
func TestInitRejectsUnknownArguments(t *testing.T) {
	dir := t.TempDir()
	out, errOut := captureOutput(t, func() { runInit([]string{dir, "--no-knowledg"}) })
	if !strings.Contains(errOut, "unsupported argument") && !strings.Contains(out, "unsupported argument") {
		t.Fatalf("typo'd flag was silently accepted: stdout=%q stderr=%q", out, errOut)
	}
	if exists, _ := settingRecorded(t, dir); exists {
		t.Fatal("typo'd flag must not record a setting")
	}
}

// `a4c status knowledge` is read-only reporting: state, recovery hint, path.
func TestStatusKnowledgeReportsState(t *testing.T) {
	dir := t.TempDir()
	captureOutput(t, func() { runInit([]string{dir, "--no-install"}) })

	out, _ := captureOutput(t, func() { runKnowledge([]string{"--dir", dir}) })
	if !strings.Contains(out, "Knowledge capture: enabled") {
		t.Fatalf("enabled state not reported: %q", out)
	}
	if !strings.Contains(out, "Knowledge directory: ") {
		t.Fatalf("knowledge path not reported: %q", out)
	}

	captureOutput(t, func() { runInit([]string{dir, "--no-install", "--no-knowledge"}) })
	out, _ = captureOutput(t, func() { runKnowledge([]string{"--dir", dir}) })
	if !strings.Contains(out, "Knowledge capture: disabled") {
		t.Fatalf("disabled state not reported: %q", out)
	}
	if !strings.Contains(out, "a4c init --knowledge") {
		t.Fatalf("recovery path not reported: %q", out)
	}
}

// Legacy export is a capture path: an opted-out workspace refuses it.
func TestStatusKnowledgeExportRefusedWhenOptedOut(t *testing.T) {
	dir := t.TempDir()
	captureOutput(t, func() { runInit([]string{dir, "--no-install", "--no-knowledge"}) })
	out, _ := captureOutput(t, func() { runKnowledge([]string{"export", "--dir", dir}) })
	if !strings.Contains(out, "disabled for this workspace") {
		t.Fatalf("export was not refused under opt-out: %q", out)
	}
}

// a4c init --help documents the flags (and never initializes anything).
func TestInitHelpListsKnowledgeFlags(t *testing.T) {
	dir := t.TempDir()
	out, _ := captureOutput(t, func() { runInit([]string{dir, "--help"}) })
	for _, want := range []string{"--no-install", "--no-knowledge", "--knowledge"} {
		if !strings.Contains(out, want) {
			t.Errorf("init --help does not document %s: %q", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".a4c")); !os.IsNotExist(err) {
		t.Errorf("--help initialized a workspace (err=%v)", err)
	}
}
