// Package knowledge retains a read-only legacy reader for the retired SQLite
// knowledge store. It exists solely so `a4c knowledge export` can preserve
// user data from earlier A4C releases before this package is removed
// entirely. It is not a knowledge platform: no writes, no tokens, no MCP.
package knowledge

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Errors returned by the legacy reader.
var (
	ErrNotFound    = errors.New("knowledge: record not found")
	ErrNoDatabase  = errors.New("knowledge: no legacy knowledge database found")
	ErrProjectScope = errors.New("knowledge: record belongs to a different project")
)

// Store is a read-only view over a legacy <workspace>/.a4c/knowledge.db.
type Store struct {
	db      *sql.DB
	project string
	dir     string
}

// OpenExport opens an EXISTING legacy knowledge database read-only. It never
// creates a database, never runs migrations, and never mutates the file. A
// missing database returns ErrNoDatabase.
func OpenExport(workspaceDir string) (*Store, error) {
	dir := filepath.Join(workspaceDir, ".a4c")
	dbPath := filepath.Join(dir, "knowledge.db")
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoDatabase
		}
		return nil, fmt.Errorf("knowledge: stat legacy db: %w", err)
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("knowledge: open legacy db read-only: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, dir: dir}
	// Best-effort project identity; export works with empty project.
	_ = s.db.QueryRow(`SELECT project_id FROM records LIMIT 1`).Scan(&s.project)
	return s, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// Path returns the legacy database path.
func (s *Store) Path() string { return filepath.Join(s.dir, "knowledge.db") }

// Project returns the workspace project identifier recorded in the legacy DB.
func (s *Store) Project() string { return s.project }

// All returns every legacy record, hydrated with evidence and links, in
// insertion order. It does not filter by project so no record is lost.
func (s *Store) All() ([]KnowledgeRecord, error) {
	rows, err := s.db.Query(`SELECT id,project_id,investigation_id,task_id,activity_id,kind,
	  statement,rationale,support,status,integrity,confidence,scope_json,author,host,
	  revision,supersedes,superseded_by,content_hash,created_at,updated_at
	  FROM records ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("knowledge: query legacy records: %w", err)
	}
	var cores []*KnowledgeRecord
	for rows.Next() {
		rec, err := scanRecordCore(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		cores = append(cores, rec)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	out := make([]KnowledgeRecord, 0, len(cores))
	for _, c := range cores {
		c.Evidence = s.loadEvidence(c.ID)
		c.Links = s.loadLinks(c.ID)
		out = append(out, *c)
	}
	return out, nil
}

// scanRecordCore scans only the scalar columns. It performs NO additional pool
// queries, so it is safe to call while a rows cursor is open.
func scanRecordCore(row rowScanner) (*KnowledgeRecord, error) {
	var rec KnowledgeRecord
	var kind, support, status, integrity, createdAt, updatedAt string
	var scopeJSON, rationale sql.NullString
	var inv, task, act, conf, host, sup, supby sql.NullString
	err := row.Scan(&rec.ID, &rec.ProjectID, &inv, &task, &act, &kind, &rec.Statement,
		&rationale, &support, &status, &integrity, &conf, &scopeJSON, &rec.Author,
		&host, &rec.Revision, &sup, &supby, &rec.ContentHash, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	rec.Kind = Kind(kind)
	rec.Support = Support(support)
	rec.Status = Status(status)
	rec.Integrity = Integrity(integrity)
	rec.InvestigationID = inv.String
	rec.TaskID = task.String
	rec.ActivityID = act.String
	rec.Confidence = conf.String
	rec.Host = host.String
	rec.Supersedes = sup.String
	rec.SupersededBy = supby.String
	rec.Rationale = rationale.String
	if scopeJSON.Valid && scopeJSON.String != "" {
		_ = json.Unmarshal([]byte(scopeJSON.String), &rec.Scope)
	}
	if t, e := time.Parse(time.RFC3339Nano, createdAt); e == nil {
		rec.CreatedAt = t
	}
	if t, e := time.Parse(time.RFC3339Nano, updatedAt); e == nil {
		rec.UpdatedAt = t
	}
	return &rec, nil
}

type rowScanner interface{ Scan(dest ...any) error }

func (s *Store) loadEvidence(recordID string) []EvidenceRef {
	rows, err := s.db.Query(`SELECT kind,artifact_id,hash,locator,integrity,validity
	  FROM evidence WHERE record_id=? ORDER BY idx`, recordID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []EvidenceRef
	for rows.Next() {
		var ev EvidenceRef
		var integ string
		var validity sql.NullString
		if err := rows.Scan(&ev.Kind, &ev.ArtifactID, &ev.Hash, &ev.Locator, &integ, &validity); err != nil {
			return nil
		}
		ev.Integrity = Integrity(integ)
		ev.Validity = validity.String
		out = append(out, ev)
	}
	return out
}

func (s *Store) loadLinks(recordID string) []Link {
	rows, err := s.db.Query(`SELECT to_id,rel,note FROM links WHERE from_id=?`, recordID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Link
	for rows.Next() {
		var l Link
		var note sql.NullString
		if err := rows.Scan(&l.To, &l.Rel, &note); err != nil {
			return nil
		}
		l.Note = note.String
		out = append(out, l)
	}
	return out
}
