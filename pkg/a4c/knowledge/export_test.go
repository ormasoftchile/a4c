package knowledge

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// seedLegacyDB creates a legacy-schema knowledge database with two records.
func seedLegacyDB(t *testing.T, workspaceDir string) {
	t.Helper()
	dir := filepath.Join(workspaceDir, ".a4c")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "knowledge.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
 ddl := `
CREATE TABLE records (
  id TEXT PRIMARY KEY, project_id TEXT NOT NULL, investigation_id TEXT, task_id TEXT,
  activity_id TEXT, kind TEXT NOT NULL, statement TEXT NOT NULL, rationale TEXT,
  support TEXT NOT NULL DEFAULT 'untested', status TEXT NOT NULL DEFAULT 'draft',
  integrity TEXT NOT NULL DEFAULT 'pending', confidence TEXT, scope_json TEXT,
  author TEXT NOT NULL, host TEXT, revision INTEGER NOT NULL DEFAULT 1,
  supersedes TEXT, superseded_by TEXT, content_hash TEXT NOT NULL,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE TABLE evidence (
  record_id TEXT NOT NULL, idx INTEGER NOT NULL, kind TEXT NOT NULL,
  artifact_id TEXT, hash TEXT, locator TEXT,
  integrity TEXT NOT NULL DEFAULT 'pending', validity TEXT,
  PRIMARY KEY (record_id, idx)
);
CREATE TABLE links (from_id TEXT NOT NULL, to_id TEXT NOT NULL, rel TEXT NOT NULL, note TEXT, PRIMARY KEY (from_id, to_id, rel));
`
	if _, err := db.Exec(ddl); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO records (id,project_id,task_id,kind,statement,rationale,support,status,integrity,author,host,revision,content_hash,created_at,updated_at)
	  VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
	if _, err := db.Exec(insert,
		"k-test-1", "proj", "task-1", "finding",
		"Pool exhausts under burst load.",
		"Observed in benchmark run 7.",
		"observed", "published", "verified",
		"coder", "copilot", 1, "hash1",
		"2026-10-17T10:00:00Z", "2026-10-17T10:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(insert,
		"k-test-2", "proj", "task-1", "hypothesis",
		"Retry storms amplify the exhaustion.",
		"",
		"untested", "draft", "pending",
		"alice", "opencode", 1, "hash2",
		"2026-10-17T11:00:00Z", "2026-10-17T11:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO evidence (record_id,idx,kind,artifact_id,hash,locator,integrity,validity)
	  VALUES (?,?,?,?,?,?,?,?)`,
		"k-test-1", 0, "log", "ev-1", "abc123", ".a4c/evidence/ev-1.log", "verified", "reviewer_confirmed"); err != nil {
		t.Fatal(err)
	}
}

func TestOpenExport_MissingDB(t *testing.T) {
	if _, err := OpenExport(t.TempDir()); !errors.Is(err, ErrNoDatabase) {
		t.Fatalf("err = %v, want ErrNoDatabase", err)
	}
}

func TestOpenExport_ReadsAllRecords(t *testing.T) {
	ws := t.TempDir()
	seedLegacyDB(t, ws)

	s, err := OpenExport(ws)
	if err != nil {
		t.Fatalf("OpenExport: %v", err)
	}
	defer s.Close()

	recs, err := s.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("expected 2 records, got %d", len(recs))
	}
	if recs[0].ID != "k-test-1" || recs[0].Kind != Kind("finding") {
		t.Fatalf("first record = %+v", recs[0])
	}
	if recs[0].Statement != "Pool exhausts under burst load." {
		t.Fatalf("statement lost: %q", recs[0].Statement)
	}
	if len(recs[0].Evidence) != 1 || recs[0].Evidence[0].Locator != ".a4c/evidence/ev-1.log" {
		t.Fatalf("evidence lost: %+v", recs[0].Evidence)
	}
	if recs[0].Support != Support("observed") {
		t.Fatalf("support lost: %q", recs[0].Support)
	}

	// The legacy database must not be mutated: reopen and confirm both records.
	s.Close()
	s2, err := OpenExport(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	all, err := s2.All()
	if err != nil || len(all) != 2 {
		t.Fatalf("re-read after export open: %d (%v)", len(all), err)
	}
	// No new files besides the db itself (no -wal mutation side effects that
	// remove data; WAL sidecar files may exist but the records must survive).
	if _, err := os.Stat(filepath.Join(ws, ".a4c", "knowledge.db")); err != nil {
		t.Fatalf("legacy db disappeared: %v", err)
	}
}

func TestRenderLegacyRecordMarkdown(t *testing.T) {
	rec := KnowledgeRecord{
		ID:        "k-x",
		Kind:      Kind("decision"),
		Statement: "Chose approach B.",
		Rationale: "A failed under load.",
		Support:   Support("supported"),
		Status:    Status("published"),
		Author:    "alice",
		TaskID:    "task-9",
		Evidence: []EvidenceRef{
			{Kind: "log", Locator: ".a4c/evidence/ev-9.log", Hash: "deadbeef"},
		},
	}
	md := RenderMarkdown(rec)
	for _, want := range []string{
		"# Legacy record: k-x",
		"Kind: decision",
		"Chose approach B.",
		"A failed under load.",
		"Author: alice",
		"Task: task-9",
		".a4c/evidence/ev-9.log",
		"deadbeef",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("rendered markdown missing %q:\n%s", want, md)
		}
	}
}
