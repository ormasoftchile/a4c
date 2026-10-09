package a4c_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ormasoftchile/a4c/pkg/a4c"
)

func TestAuditChain_IntegrityAndTamperEvidence(t *testing.T) {
	tmpDir := t.TempDir()
	stream := "approvals"

	// 1. Build an authentic 5-record audit chain
	for i := 1; i <= 5; i++ {
		err := a4c.AppendAudit(tmpDir, stream, map[string]any{
			"seq":    i,
			"action": fmt.Sprintf("action_%d", i),
			"caller": "human_cli",
		})
		if err != nil {
			t.Fatalf("AppendAudit %d failed: %v", i, err)
		}
	}

	// 2. Verify pristine chain
	n, err := a4c.VerifyAuditChain(tmpDir, stream)
	if err != nil || n != 5 {
		t.Fatalf("pristine audit chain verification failed: n=%d err=%v", n, err)
	}

	auditPath := filepath.Join(tmpDir, ".a4c", "audit", stream+".jsonl")
	rawContent, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(rawContent)), "\n")
	if len(lines) != 5 {
		t.Fatalf("expected 5 lines, got %d", len(lines))
	}

	// 3. Test tampering: modify record 3
	tamperedDir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(tamperedDir, ".a4c", "audit"), 0755)
	tamperedPath := filepath.Join(tamperedDir, ".a4c", "audit", stream+".jsonl")

	tamperedLines := make([]string, len(lines))
	copy(tamperedLines, lines)
	tamperedLines[2] = strings.Replace(tamperedLines[2], "action_3", "malicious_action", 1)
	_ = os.WriteFile(tamperedPath, []byte(strings.Join(tamperedLines, "\n")+"\n"), 0600)

	_, err = a4c.VerifyAuditChain(tamperedDir, stream)
	if err == nil {
		t.Fatalf("VIOLATION: tampered historical record was not detected by VerifyAuditChain")
	}
	t.Logf("Tamper detection verified: %v", err)

	// 4. Test truncation: remove record 1 (prefix truncation)
	prefixTruncDir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(prefixTruncDir, ".a4c", "audit"), 0755)
	_ = os.WriteFile(filepath.Join(prefixTruncDir, ".a4c", "audit", stream+".jsonl"),
		[]byte(strings.Join(lines[1:], "\n")+"\n"), 0600)

	_, err = a4c.VerifyAuditChain(prefixTruncDir, stream)
	if err == nil {
		t.Fatalf("VIOLATION: prefix truncation (missing record 1) was not detected")
	}
	t.Logf("Prefix truncation detection verified: %v", err)

	// 5. Test tail truncation: drop last 2 records
	tailTruncDir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(tailTruncDir, ".a4c", "audit"), 0755)
	_ = os.WriteFile(filepath.Join(tailTruncDir, ".a4c", "audit", stream+".jsonl"),
		[]byte(strings.Join(lines[:3], "\n")+"\n"), 0600)

	tailCount, err := a4c.VerifyAuditChain(tailTruncDir, stream)
	if err != nil {
		t.Fatalf("tail truncation verification produced error: %v", err)
	}
	if tailCount != 3 {
		t.Fatalf("expected 3 valid records, got %d", tailCount)
	}
	t.Logf("Tail truncation behavior verified: accepts valid prefix (%d records) without external head anchor", tailCount)

	// 6. Test crash consistency: corrupt partial line write
	crashDir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(crashDir, ".a4c", "audit"), 0755)
	corruptContent := string(rawContent) + `{"seq": 6, "action": "half_written`
	_ = os.WriteFile(filepath.Join(crashDir, ".a4c", "audit", stream+".jsonl"), []byte(corruptContent), 0600)

	_, err = a4c.VerifyAuditChain(crashDir, stream)
	if err == nil {
		t.Fatalf("VIOLATION: unreadable partial line after crash was not detected")
	}
	t.Logf("Crash partial line detection verified: %v", err)
}
