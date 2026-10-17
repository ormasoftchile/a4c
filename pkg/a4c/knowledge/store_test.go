package knowledge

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// openEnabled opens a store in a fresh temp workspace with the module enabled.
func openEnabled(t *testing.T, project string) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	if err := WriteConfig(dir, Config{Enabled: true, Project: project}); err != nil {
		t.Fatal(err)
	}
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, dir
}

func ctxFor(activity string) *ExecContext {
	return &ExecContext{ProjectID: "skiff", TaskID: "T-1", ActivityID: activity, Role: "coder", Host: "opencode"}
}

func TestStore_DisabledDoesNotCreateDB(t *testing.T) {
	dir := t.TempDir() // no config -> disabled
	if _, err := Open(dir); !errors.Is(err, ErrDisabled) {
		t.Fatalf("expected ErrDisabled, got %v", err)
	}
	if _, e := NewService(dir, ActivityIsActive); !errors.Is(e, ErrDisabled) {
		t.Fatalf("NewService on disabled workspace must return ErrDisabled, got %v", e)
	}
	if _, err := os.Stat(filepath.Join(dir, ".a4c", "knowledge.db")); !os.IsNotExist(err) {
		t.Fatalf("disabled module must not create a knowledge.db, stat err=%v", err)
	}
}

func TestStore_RecordPreservesProvenance(t *testing.T) {
	st, _ := openEnabled(t, "skiff")
	ec := ctxFor("A-1")
	res, err := st.Record(ec, RecordInput{
		Kind: KindHypothesis, Statement: "LRU reduces p99", Rationale: "cache locality",
		InvestigationID: "I-001", Evidence: []EvidenceRef{{Kind: "log", Locator: ".a4c/evidence/x.log", Hash: "sha256:aa"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := res.Record
	if r.ProjectID != "skiff" || r.InvestigationID != "I-001" || r.TaskID != "T-1" || r.ActivityID != "A-1" {
		t.Fatalf("provenance lost: %+v", r)
	}
	if r.Author != "coder" || r.Host != "opencode" {
		t.Fatalf("author/host lost: %+v", r)
	}
	if r.Statement != "LRU reduces p99" || r.Rationale != "cache locality" {
		t.Fatalf("statement/rationale lost")
	}
	if r.Support != SupportUntested {
		t.Fatalf("hypothesis must start untested, got %q", r.Support)
	}
	if r.ContentHash == "" {
		t.Fatalf("content hash missing")
	}
	if len(r.Evidence) != 1 || r.Evidence[0].Hash != "sha256:aa" {
		t.Fatalf("evidence lost: %+v", r.Evidence)
	}
}

func TestStore_SupportNeverSetAtWriteTime(t *testing.T) {
	st, _ := openEnabled(t, "skiff")
	// A finding cannot be written as "supported"; it starts untested.
	res, err := st.Record(ctxFor("A-1"), RecordInput{Kind: KindFinding, Statement: "X is faster"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Record.Support != SupportUntested {
		t.Fatalf("finding must start untested, got %q", res.Record.Support)
	}
	// Evidence/experiment start observed.
	res2, _ := st.Record(ctxFor("A-1"), RecordInput{Kind: KindEvidence, Statement: "measured 18ms"})
	if res2.Record.Support != SupportObserved {
		t.Fatalf("evidence must start observed, got %q", res2.Record.Support)
	}
}

func TestStore_RepeatedIdenticalCallIdempotent(t *testing.T) {
	st, _ := openEnabled(t, "skiff")
	in := RecordInput{Kind: KindFinding, Statement: "same", ClientKey: "k1"}
	r1, err := st.Record(ctxFor("A-1"), in)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := st.Record(ctxFor("A-1"), in)
	if err != nil {
		t.Fatal(err)
	}
	if !r2.Reused {
		t.Fatalf("second call must be reused")
	}
	if r2.Record.ID != r1.Record.ID {
		t.Fatalf("idempotent call must return the SAME record")
	}
	if r2.Conflict {
		t.Fatalf("identical payload must not conflict")
	}
	// Only one record exists.
	res, _ := st.Query(QueryInput{})
	if res.Total != 1 {
		t.Fatalf("expected exactly 1 record, got %d", res.Total)
	}
}

func TestStore_KeyReuseWithDifferentPayload(t *testing.T) {
	st, _ := openEnabled(t, "skiff")
	r1, _ := st.Record(ctxFor("A-1"), RecordInput{Kind: KindFinding, Statement: "original", ClientKey: "k1"})
	r2, err := st.Record(ctxFor("A-1"), RecordInput{Kind: KindFinding, Statement: "DIFFERENT", ClientKey: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	// Deterministic conflict policy: return original, flag conflict, no duplicate.
	if !r2.Reused || !r2.Conflict {
		t.Fatalf("key reuse with different payload must flag conflict, got reused=%v conflict=%v", r2.Reused, r2.Conflict)
	}
	if r2.Record.ID != r1.Record.ID || r2.Record.Statement != "original" {
		t.Fatalf("conflict must return the ORIGINAL record, got %+v", r2.Record)
	}
	res, _ := st.Query(QueryInput{})
	if res.Total != 1 {
		t.Fatalf("conflict must not create a second record, got %d", res.Total)
	}
}

func TestStore_IndependentIdenticalContentNotMerged(t *testing.T) {
	st, _ := openEnabled(t, "skiff")
	// Two independent experiments, same statement, DIFFERENT keys/activities.
	r1, _ := st.Record(ctxFor("A-1"), RecordInput{Kind: KindFinding, Statement: "X wins", ClientKey: "e1"})
	r2, _ := st.Record(ctxFor("A-2"), RecordInput{Kind: KindFinding, Statement: "X wins", ClientKey: "e2"})
	if r1.Record.ID == r2.Record.ID {
		t.Fatalf("independent identical statements must remain SEPARATE records (corroboration), got same id %s", r1.Record.ID)
	}
	res, _ := st.Query(QueryInput{})
	if res.Total != 2 {
		t.Fatalf("expected 2 independent records, got %d", res.Total)
	}
}

func TestStore_ConcurrentSameKeyNoDuplicate(t *testing.T) {
	st, _ := openEnabled(t, "skiff")
	const n = 8
	ids := make(chan string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := st.Record(ctxFor("A-1"), RecordInput{Kind: KindFinding, Statement: "race", ClientKey: "shared"})
			if err == nil {
				ids <- res.Record.ID
			}
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		seen[id] = true
	}
	if len(seen) != 1 {
		t.Fatalf("concurrent same-key calls must produce exactly ONE record id, got %d distinct", len(seen))
	}
	res, _ := st.Query(QueryInput{})
	if res.Total != 1 {
		t.Fatalf("expected exactly 1 record after concurrent same-key writes, got %d", res.Total)
	}
}

func TestStore_ConcurrentDifferentKeysIndependent(t *testing.T) {
	st, _ := openEnabled(t, "skiff")
	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = st.Record(ctxFor("A-1"), RecordInput{
				Kind: KindFinding, Statement: fmt.Sprintf("finding-%d", i), ClientKey: fmt.Sprintf("k-%d", i),
			})
		}(i)
	}
	wg.Wait()
	res, _ := st.Query(QueryInput{})
	if res.Total != n {
		t.Fatalf("concurrent different-key writes must all land independently, got %d of %d", res.Total, n)
	}
}

func TestStore_CrashBeforeCommitLeavesNoOrphan(t *testing.T) {
	st, dir := openEnabled(t, "skiff")
	// Simulate a crash: open a transaction that inserts then rolls back.
	tx, err := st.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = tx.Exec(`INSERT INTO records (id,project_id,kind,statement,support,status,integrity,author,content_hash,created_at,updated_at)
		VALUES ('k-x','skiff','finding','orphan','untested','draft','pending','coder','h','2020-01-01','2020-01-01')`)
	// Roll back WITHOUT commit (crash).
	_ = tx.Rollback()
	res, _ := st.Query(QueryInput{})
	if res.Total != 0 {
		t.Fatalf("rolled-back insert must leave no record, got %d", res.Total)
	}
	// Store still usable after "crash".
	r, err := st.Record(ctxFor("A-1"), RecordInput{Kind: KindFinding, Statement: "post-crash"})
	if err != nil || r.Record.ID == "" {
		t.Fatalf("store must remain usable after a rolled-back transaction: %v", err)
	}
	_ = dir
}

func TestStore_RetryAfterRestart(t *testing.T) {
	st, dir := openEnabled(t, "skiff")
	r1, _ := st.Record(ctxFor("A-1"), RecordInput{Kind: KindFinding, Statement: "durable", ClientKey: "k1"})
	// Simulate process restart: close and reopen over the same dir.
	_ = st.Close()
	st2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	// Retry after restart with the same key -> idempotent, same record.
	r2, err := st2.Record(ctxFor("A-1"), RecordInput{Kind: KindFinding, Statement: "durable", ClientKey: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	if !r2.Reused || r2.Record.ID != r1.Record.ID {
		t.Fatalf("retry after restart must be idempotent, got %+v", r2)
	}
	// The record survives restart with provenance intact.
	got, err := st2.Get(r1.Record.ID)
	if err != nil {
		t.Fatalf("record must survive restart: %v", err)
	}
	if got.ActivityID != "A-1" || got.Statement != "durable" {
		t.Fatalf("provenance lost after restart: %+v", got)
	}
}

func TestStore_QueryFiltersAndBounds(t *testing.T) {
	st, _ := openEnabled(t, "skiff")
	_, _ = st.Record(ctxFor("A-1"), RecordInput{Kind: KindHypothesis, Statement: "h1", InvestigationID: "I-1"})
	_, _ = st.Record(ctxFor("A-2"), RecordInput{Kind: KindFinding, Statement: "f1 about LRU", InvestigationID: "I-2"})
	byKind, err := st.Query(QueryInput{Kinds: []Kind{KindFinding}})
	if err != nil || byKind.Total != 1 || byKind.Records[0].Kind != KindFinding {
		t.Fatalf("kind filter failed: %+v err=%v", byKind, err)
	}
	byInv, _ := st.Query(QueryInput{InvestigationID: "I-1"})
	if byInv.Total != 1 || byInv.Records[0].InvestigationID != "I-1" {
		t.Fatalf("investigation filter failed: %+v", byInv)
	}
	byText, _ := st.Query(QueryInput{Text: "LRU"})
	if byText.Total != 1 {
		t.Fatalf("text filter failed: %+v", byText)
	}
	// Bound: request >50 clamps to 50.
	bounded, _ := st.Query(QueryInput{Limit: 9999})
	if len(bounded.Records) > 50 {
		t.Fatalf("limit must clamp to 50, got %d", len(bounded.Records))
	}
}

func TestStore_ProjectScopeIsolation(t *testing.T) {
	dir := t.TempDir()
	_ = WriteConfig(dir, Config{Enabled: true, Project: "skiff"})
	st, _ := Open(dir)
	defer st.Close()
	r, _ := st.Record(ctxFor("A-1"), RecordInput{Kind: KindFinding, Statement: "scoped"})
	// Simulate another project's store over the same DB by flipping project.
	st2 := &Store{db: st.db, secret: st.secret, revoker: st.revoker, project: "other", dir: st.dir}
	if _, err := st2.Get(r.Record.ID); !errors.Is(err, ErrProjectScope) {
		t.Fatalf("cross-project Get must fail with ErrProjectScope, got %v", err)
	}
	// Query from another project returns nothing.
	res, _ := st2.Query(QueryInput{})
	if res.Total != 0 {
		t.Fatalf("cross-project query must return 0, got %d", res.Total)
	}
}
