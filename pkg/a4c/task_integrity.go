package a4c

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// AcquireTaskMutationLock takes a blocking, cross-process exclusive lock scoped to one
// task. It reuses the same kernel flock primitive as WorkspaceLock; the difference is
// scope (per task, not per workspace) and that waiters block instead of failing, so two
// patch operations against the same worktree serialize instead of racing.
func AcquireTaskMutationLock(workspaceDir, taskID string) (release func(), err error) {
	if strings.ContainsAny(taskID, "/\\") || taskID == "" || taskID == "." || taskID == ".." {
		return nil, fmt.Errorf("lock: invalid task id %q", taskID)
	}
	dir := filepath.Join(workspaceDir, ".a4c", "locks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, taskID+".mutation.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if ok, lerr := osFlock(f, true); !ok || lerr != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock: acquire task mutation lock: %v", lerr)
	}
	return func() {
		_ = osFlockUnlock(f)
		_ = f.Close()
	}, nil
}

// AppendAudit appends one record to .a4c/audit/<stream>.jsonl. Records form a hash chain
// (each carries the SHA-256 of the previous line), which makes truncation or editing
// detectable by VerifyAuditChain. It is tamper-EVIDENT, not tamper-proof: a process
// running as the same OS user can still rewrite the whole file.
func AppendAudit(workspaceDir, stream string, fields map[string]any) error {
	dir := filepath.Join(workspaceDir, ".a4c", "audit")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, stream+".jsonl")

	unlock, err := AcquireTaskMutationLock(workspaceDir, "audit-"+stream)
	if err != nil {
		return err
	}
	defer unlock()

	prev := ""
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
		last := ""
		for sc.Scan() {
			last = sc.Text()
		}
		_ = f.Close()
		if last != "" {
			sum := sha256.Sum256([]byte(last))
			prev = hex.EncodeToString(sum[:])
		}
	}

	rec := map[string]any{}
	for k, v := range fields {
		rec[k] = v
	}
	rec["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	rec["pid"] = os.Getpid()
	rec["prev"] = prev
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = out.Write(append(line, '\n'))
	return err
}

// VerifyAuditChain returns the number of records and an error if the hash chain is broken.
func VerifyAuditChain(workspaceDir, stream string) (int, error) {
	f, err := os.Open(filepath.Join(workspaceDir, ".a4c", "audit", stream+".jsonl"))
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	prev := ""
	n := 0
	for sc.Scan() {
		line := sc.Text()
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return n, fmt.Errorf("audit: record %d unreadable: %w", n+1, err)
		}
		if got, _ := rec["prev"].(string); got != prev {
			return n, fmt.Errorf("audit: chain broken at record %d", n+1)
		}
		sum := sha256.Sum256([]byte(line))
		prev = hex.EncodeToString(sum[:])
		n++
	}
	return n, sc.Err()
}

// ReadAudit returns all records of a stream (for tests and reports).
func ReadAudit(workspaceDir, stream string) ([]map[string]any, error) {
	f, err := os.Open(filepath.Join(workspaceDir, ".a4c", "audit", stream+".jsonl"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		var rec map[string]any
		if json.Unmarshal([]byte(sc.Text()), &rec) == nil {
			out = append(out, rec)
		}
	}
	return out, sc.Err()
}
