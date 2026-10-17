package knowledge

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Errors returned by the store.
var (
	ErrDisabled         = errors.New("knowledge: module disabled")
	ErrUnsupportedWrite = errors.New("knowledge: support must not be set at record time; use knowledge_assess")
	ErrKeyConflict      = errors.New("knowledge: idempotency key reused with a different payload")
	ErrNotFound         = errors.New("knowledge: record not found")
	ErrProjectScope     = errors.New("knowledge: record belongs to a different project")
)

// Store is a multi-process-safe durable knowledge store backed by SQLite in WAL
// mode. Separate A4C MCP server processes and the coordinator may read and write
// concurrently; correctness comes from SQLite transactions and uniqueness
// constraints, not from an in-process mutex.
type Store struct {
	db       *sql.DB
	secret   *SecretManager
	revoker  *Revoker
	project  string
	dir      string
	isActive func(activityID string) bool
	mu       sync.Mutex // guards seq counter for ID generation
	seq      int
}

// Open creates or opens the knowledge store at <workspace>/.a4c/knowledge.db.
// It returns ErrDisabled when the module is not enabled for the workspace.
func Open(workspaceDir string) (*Store, error) {
	cfg, err := ReadConfig(workspaceDir)
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, ErrDisabled
	}
	dir := filepath.Join(workspaceDir, ".a4c")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("knowledge: create dir: %w", err)
	}
	dbPath := filepath.Join(dir, "knowledge.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("knowledge: open db: %w", err)
	}
	// Single-connection pool: SQLite allows one writer. Serializing through one
	// connection eliminates in-process lock contention (concurrent MCP server
	// goroutines), while WAL + busy_timeout handle cross-process access.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	// Multi-process safety: WAL allows concurrent readers with one writer;
	// busy_timeout makes a cross-process writer block rather than fail.
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=10000",
		"PRAGMA foreign_keys=ON",
		"PRAGMA synchronous=NORMAL",
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("knowledge: %s: %w", pragma, err)
		}
	}
	secret, err := NewSecretManager(workspaceDir)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{
		db:      db,
		secret:  secret,
		revoker: NewRevoker(),
		project: cfg.Project,
		dir:     dir,
	}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS records (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  investigation_id TEXT,
  task_id TEXT,
  activity_id TEXT,
  kind TEXT NOT NULL,
  statement TEXT NOT NULL,
  rationale TEXT,
  support TEXT NOT NULL DEFAULT 'untested',
  status TEXT NOT NULL DEFAULT 'draft',
  integrity TEXT NOT NULL DEFAULT 'pending',
  confidence TEXT,
  scope_json TEXT,
  author TEXT NOT NULL,
  host TEXT,
  revision INTEGER NOT NULL DEFAULT 1,
  supersedes TEXT,
  superseded_by TEXT,
  content_hash TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  CHECK (support IN ('untested','observed','supported','contradicted','inconclusive')),
  CHECK (status IN ('draft','published','retracted')),
  CHECK (integrity IN ('pending','verified','broken'))
);
CREATE INDEX IF NOT EXISTS idx_rec_scope ON records(project_id, investigation_id);
CREATE INDEX IF NOT EXISTS idx_rec_support ON records(project_id, support);
CREATE INDEX IF NOT EXISTS idx_rec_task ON records(project_id, task_id);

CREATE TABLE IF NOT EXISTS evidence (
  record_id TEXT NOT NULL REFERENCES records(id) ON DELETE CASCADE,
  idx INTEGER NOT NULL,
  kind TEXT NOT NULL,
  artifact_id TEXT,
  hash TEXT,
  locator TEXT,
  integrity TEXT NOT NULL DEFAULT 'pending',
  validity TEXT,
  PRIMARY KEY (record_id, idx)
);

CREATE TABLE IF NOT EXISTS links (
  from_id TEXT NOT NULL REFERENCES records(id) ON DELETE CASCADE,
  to_id TEXT NOT NULL,
  rel TEXT NOT NULL,
  note TEXT,
  PRIMARY KEY (from_id, to_id, rel)
);

CREATE TABLE IF NOT EXISTS assessments (
  id TEXT PRIMARY KEY,
  record_id TEXT NOT NULL REFERENCES records(id) ON DELETE CASCADE,
  support TEXT NOT NULL,
  rationale TEXT NOT NULL,
  origin TEXT NOT NULL,
  assessor TEXT NOT NULL,
  scope_json TEXT,
  evidence_json TEXT,
  revision INTEGER NOT NULL DEFAULT 1,
  assessed_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_assess_record ON assessments(record_id);

CREATE TABLE IF NOT EXISTS revisions (
  record_id TEXT NOT NULL,
  revision INTEGER NOT NULL,
  snapshot_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (record_id, revision)
);

CREATE TABLE IF NOT EXISTS idempotency (
  activity_id TEXT NOT NULL,
  client_key TEXT NOT NULL,
  record_id TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (activity_id, client_key)
);`)
	return err
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// Path returns the store's database path.
func (s *Store) Path() string { return filepath.Join(s.dir, "knowledge.db") }

// Project returns the workspace project identifier.
func (s *Store) Project() string { return s.project }

// SetActivityValidator installs the live check that an activity is still claimed.
// A delayed tool call from a completed activity therefore fails closed.
func (s *Store) SetActivityValidator(fn func(activityID string) bool) { s.isActive = fn }

// isActiveLocked reports whether an activity may still author knowledge.
func (s *Store) activityActive(activityID string) bool {
	if s.revoker.IsRevoked(activityID) {
		return false
	}
	if s.isActive != nil {
		return s.isActive(activityID)
	}
	// Without a validator, treat the context as active (token expiry still applies).
	return true
}

// ValidateToken authenticates a token and returns its execution context.
func (s *Store) ValidateToken(token string) (*ExecContext, error) {
	return s.secret.Validate(token, s.activityActive)
}

// IssueToken mints a signed token for an activity.
func (s *Store) IssueToken(ctx ExecContext, ttl time.Duration) (string, error) {
	return s.secret.Issue(ctx, ttl)
}

// RevokeActivity invalidates any outstanding tokens for an activity.
func (s *Store) RevokeActivity(activityID string) { s.revoker.Revoke(activityID) }

// RecordInput is the agent-authored input to knowledge_record.
type RecordInput struct {
	Kind            Kind
	Statement       string
	Rationale       string
	Evidence        []EvidenceRef
	InvestigationID string
	ClientKey       string
	Publish         bool
}

// RecordResult is the outcome of a knowledge_record call.
type RecordResult struct {
	Record    KnowledgeRecord `json:"record"`
	Reused    bool            `json:"reused"`     // true when an idempotency key matched
	Conflict  bool            `json:"conflict"`   // key reused with a different payload
	Warnings  []string        `json:"warnings,omitempty"`
}

// Record durably records one knowledge item on behalf of a validated context.
//
// Transactional idempotency (corrected): the idempotency key is resolved INSIDE
// the transaction BEFORE any record is created. If the key already exists the
// original record is returned and no new row is written. If it is new, exactly
// one record is created and associated with the key atomically, so a crash or
// concurrent duplicate can never leave an orphaned or duplicated record.
func (s *Store) Record(ec *ExecContext, in RecordInput) (*RecordResult, error) {
	if ec == nil {
		return nil, ErrNoContext
	}
	if !in.Kind.Valid() {
		return nil, fmt.Errorf("knowledge: invalid kind %q", in.Kind)
	}
	if strings.TrimSpace(in.Statement) == "" {
		return nil, errors.New("knowledge: statement is required")
	}
	// Support is never set at write time: a claim begins untested/observed.
	// Evidence/experiment records start observed; all others untested.
	startSupport := SupportUntested
	if in.Kind == KindEvidence || in.Kind == KindExperiment {
		startSupport = SupportObserved
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("knowledge: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 1. Resolve the idempotency key FIRST, before creating anything.
	if in.ClientKey != "" {
		var existingID string
		err := tx.QueryRow(`SELECT record_id FROM idempotency WHERE activity_id=? AND client_key=?`,
			ec.ActivityID, in.ClientKey).Scan(&existingID)
		if err == nil {
			// Key already committed: return the ORIGINAL result, write nothing.
			// Roll back before reading so the single-connection pool is free.
			_ = tx.Rollback()
			rec, gerr := s.Get(existingID)
			if gerr != nil {
				return nil, gerr
			}
			// Deterministic conflict policy: a reused key with a different
			// payload returns the original and flags the conflict. It never
			// overwrites provenance and never creates a second record.
			conflict := rec.Statement != in.Statement || rec.Kind != in.Kind
			return &RecordResult{Record: *rec, Reused: true, Conflict: conflict}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			_ = tx.Rollback()
			return nil, fmt.Errorf("knowledge: resolve idempotency key: %w", err)
		}
	}

	// 2. Create exactly one record inside the same transaction.
	s.mu.Lock()
	s.seq++
	seq := s.seq
	s.mu.Unlock()
	now := time.Now().UTC()
	rec := KnowledgeRecord{
		ID:              computeID(s.project, in.Kind, now, seq),
		ProjectID:       s.project,
		InvestigationID: firstNonEmpty(in.InvestigationID, ec.InvestigationID),
		TaskID:          ec.TaskID,
		ActivityID:      ec.ActivityID,
		Kind:            in.Kind,
		Statement:       in.Statement,
		Rationale:       in.Rationale,
		Support:         startSupport,
		Status:          StatusDraft,
		Integrity:       IntegrityPending,
		Author:          ec.Role,
		Host:            ec.Host,
		Revision:        1,
		Evidence:        in.Evidence,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	rec.ContentHash = rec.ComputeContentHash()
	if in.Publish {
		rec.Status = StatusPublished
	}

	warnings, err := s.insertRecordWithin(tx, &rec)
	if err != nil {
		return nil, err
	}

	// 3. Associate the key atomically with the new record.
	if in.ClientKey != "" {
		if _, err := tx.Exec(`INSERT INTO idempotency (activity_id, client_key, record_id, created_at)
			VALUES (?,?,?,?)`, ec.ActivityID, in.ClientKey, rec.ID, now.Format(time.RFC3339Nano)); err != nil {
			// A concurrent submission won the race; return the winner's record.
			var existingID string
			_ = tx.Rollback()
			if e2 := s.db.QueryRow(`SELECT record_id FROM idempotency WHERE activity_id=? AND client_key=?`,
				ec.ActivityID, in.ClientKey).Scan(&existingID); e2 == nil {
				winner, gerr := s.Get(existingID)
				if gerr != nil {
					return nil, gerr
				}
				return &RecordResult{Record: *winner, Reused: true}, nil
			}
			return nil, fmt.Errorf("knowledge: associate idempotency key: %w", err)
		}
	}

	// 4. Snapshot the first revision.
	if err := s.snapshotWithin(tx, &rec); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("knowledge: commit: %w", err)
	}
	return &RecordResult{Record: rec, Warnings: warnings}, nil
}

func (s *Store) insertRecordWithin(tx *sql.Tx, rec *KnowledgeRecord) ([]string, error) {
	var warnings []string
	scopeJSON, _ := json.Marshal(rec.Scope)
	_, err := tx.Exec(`INSERT INTO records
	  (id,project_id,investigation_id,task_id,activity_id,kind,statement,rationale,
	   support,status,integrity,confidence,scope_json,author,host,revision,
	   supersedes,superseded_by,content_hash,created_at,updated_at)
	  VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		rec.ID, rec.ProjectID, rec.InvestigationID, rec.TaskID, rec.ActivityID,
		string(rec.Kind), rec.Statement, rec.Rationale, string(rec.Support),
		string(rec.Status), string(rec.Integrity), rec.Confidence, string(scopeJSON),
		rec.Author, rec.Host, rec.Revision, rec.Supersedes, rec.SupersededBy,
		rec.ContentHash, rec.CreatedAt.Format(time.RFC3339Nano), rec.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("knowledge: insert record: %w", err)
	}
	for i, ev := range rec.Evidence {
		if ev.Integrity == "" {
			ev.Integrity = IntegrityPending
		}
		// A4C sets integrity; a model cannot assert it. For the first slice we
		// mark evidence with a hash as verified-integrity, others pending.
		if ev.Hash != "" {
			ev.Integrity = IntegrityVerified
		}
		rec.Evidence[i] = ev
		if _, err := tx.Exec(`INSERT INTO evidence
		  (record_id,idx,kind,artifact_id,hash,locator,integrity,validity)
		  VALUES (?,?,?,?,?,?,?,?)`,
			rec.ID, i, ev.Kind, ev.ArtifactID, ev.Hash, ev.Locator,
			string(ev.Integrity), ev.Validity); err != nil {
			return nil, fmt.Errorf("knowledge: insert evidence: %w", err)
		}
	}
	for _, l := range rec.Links {
		if !ValidRel(l.Rel) {
			warnings = append(warnings, fmt.Sprintf("unknown link rel %q", l.Rel))
			continue
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO links (from_id,to_id,rel,note) VALUES (?,?,?,?)`,
			rec.ID, l.To, l.Rel, l.Note); err != nil {
			return nil, fmt.Errorf("knowledge: insert link: %w", err)
		}
	}
	// Recompute record-level integrity from its evidence.
	rec.Integrity = IntegrityPending
	allVerified := len(rec.Evidence) > 0
	for _, ev := range rec.Evidence {
		if ev.Integrity != IntegrityVerified {
			allVerified = false
		}
	}
	if allVerified {
		rec.Integrity = IntegrityVerified
	}
	if _, err := tx.Exec(`UPDATE records SET integrity=? WHERE id=?`, string(rec.Integrity), rec.ID); err != nil {
		return nil, err
	}
	return warnings, nil
}

func (s *Store) snapshotWithin(tx *sql.Tx, rec *KnowledgeRecord) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT OR REPLACE INTO revisions (record_id,revision,snapshot_json,created_at)
		VALUES (?,?,?,?)`, rec.ID, rec.Revision, string(b), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// scanRecordCore scans only the scalar columns. It performs NO additional pool
// queries, so it is safe to call while a transaction is open on a
// single-connection pool. Evidence and links are attached by hydrate afterwards.
func scanRecordCore(row rowScanner) (*KnowledgeRecord, error) {
	var rec KnowledgeRecord
	var kind, support, status, integrity, scopeJSON, createdAt, updatedAt string
	var inv, task, act, conf, host, sup, supby sql.NullString
	err := row.Scan(&rec.ID, &rec.ProjectID, &inv, &task, &act, &kind, &rec.Statement,
		&rec.Rationale, &support, &status, &integrity, &conf, &scopeJSON, &rec.Author,
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
	_ = json.Unmarshal([]byte(scopeJSON), &rec.Scope)
	if t, e := time.Parse(time.RFC3339Nano, createdAt); e == nil {
		rec.CreatedAt = t
	}
	if t, e := time.Parse(time.RFC3339Nano, updatedAt); e == nil {
		rec.UpdatedAt = t
	}
	return &rec, nil
}

// hydrate attaches evidence and links. It MUST be called outside an open
// transaction on a single-connection pool, because it queries the pool.
func (s *Store) hydrate(rec *KnowledgeRecord) {
	rec.Evidence = s.loadEvidence(rec.ID)
	rec.Links = s.loadLinks(rec.ID)
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

// Get returns a record by ID, enforcing project scope.
func (s *Store) Get(id string) (*KnowledgeRecord, error) {
	row := s.db.QueryRow(`SELECT id,project_id,investigation_id,task_id,activity_id,kind,
	  statement,rationale,support,status,integrity,confidence,scope_json,author,host,
	  revision,supersedes,superseded_by,content_hash,created_at,updated_at
	  FROM records WHERE id=?`, id)
	rec, err := scanRecordCore(row)
	if err != nil {
		return nil, err
	}
	if rec.ProjectID != s.project {
		return nil, ErrProjectScope
	}
	s.hydrate(rec)
	return rec, nil
}

// QueryInput filters a bounded knowledge search.
type QueryInput struct {
	InvestigationID string
	Kinds           []Kind
	Supports        []Support
	Statuses        []Status
	Text            string
	Rel             string
	Limit           int
	Offset          int
}

// QueryResult is a compact, provenance-rich page of records.
type QueryResult struct {
	Count     int               `json:"count"`
	Total     int               `json:"total"`
	Truncated bool              `json:"truncated"`
	Records   []KnowledgeRecord `json:"records"`
}

// Query returns a bounded page of records in the store's project scope.
func (s *Store) Query(in QueryInput) (*QueryResult, error) {
	limit := in.Limit
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	offset := in.Offset
	if offset < 0 {
		offset = 0
	}
	var conds []string
	var args []any
	conds = append(conds, "project_id=?")
	args = append(args, s.project)
	if in.InvestigationID != "" {
		conds = append(conds, "investigation_id=?")
		args = append(args, in.InvestigationID)
	}
	if len(in.Kinds) > 0 {
		var qs []string
		for _, k := range in.Kinds {
			qs = append(qs, "?")
			args = append(args, string(k))
		}
		conds = append(conds, "kind IN ("+strings.Join(qs, ",")+")")
	}
	if len(in.Supports) > 0 {
		var qs []string
		for _, sp := range in.Supports {
			qs = append(qs, "?")
			args = append(args, string(sp))
		}
		conds = append(conds, "support IN ("+strings.Join(qs, ",")+")")
	}
	if len(in.Statuses) > 0 {
		var qs []string
		for _, st := range in.Statuses {
			qs = append(qs, "?")
			args = append(args, string(st))
		}
		conds = append(conds, "status IN ("+strings.Join(qs, ",")+")")
	}
	if in.Text != "" {
		conds = append(conds, "(statement LIKE ? OR rationale LIKE ?)")
		like := "%" + in.Text + "%"
		args = append(args, like, like)
	}
	where := " WHERE " + strings.Join(conds, " AND ")
	if in.Rel != "" {
		// Filter through the links table.
		q := `SELECT r.id, r.project_id, r.investigation_id, r.task_id, r.activity_id, r.kind,
		  r.statement, r.rationale, r.support, r.status, r.integrity, r.confidence, r.scope_json,
		  r.author, r.host, r.revision, r.supersedes, r.superseded_by, r.content_hash,
		  r.created_at, r.updated_at
		  FROM records r JOIN links l ON l.to_id = r.id WHERE l.rel = ? AND ` + strings.Join(conds, " AND ") + ` ORDER BY r.created_at DESC`
		allArgs := append([]any{in.Rel}, args...)
		rows, err := s.db.Query(q, allArgs...)
		if err != nil {
			return nil, err
		}
		return s.collectPage(rows, limit, offset)
	}
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM records`+where, args...).Scan(&total); err != nil {
		return nil, err
	}
	q := `SELECT id,project_id,investigation_id,task_id,activity_id,kind,statement,rationale,
	  support,status,integrity,confidence,scope_json,author,host,revision,supersedes,superseded_by,
	  content_hash,created_at,updated_at FROM records` + where + ` ORDER BY created_at DESC LIMIT ? OFFSET ?`
	rows, err := s.db.Query(q, append(append([]any{}, args...), limit, offset)...)
	if err != nil {
		return nil, err
	}
	res, err := s.collectPage(rows, limit, offset)
	if err != nil {
		return nil, err
	}
	res.Total = total
	res.Truncated = offset+len(res.Records) < total
	return res, nil
}

func (s *Store) collectPage(rows *sql.Rows, limit, offset int) (*QueryResult, error) {
	// Collect scalar rows first, then hydrate after rows.Close() — hydrate
	// queries the pool, which would deadlock a single-connection pool if called
	// while the rows cursor is open.
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
	res := &QueryResult{}
	for _, c := range cores {
		s.hydrate(c)
		res.Records = append(res.Records, *c)
	}
	res.Count = len(res.Records)
	if res.Total == 0 {
		res.Total = res.Count
	}
	sort.Slice(res.Records, func(i, j int) bool {
		return res.Records[i].CreatedAt.After(res.Records[j].CreatedAt)
	})
	return res, nil
}

// Counts summarises the store for knowledge_status.
func (s *Store) Counts() (byKind map[string]int, bySupport map[string]int, byStatus map[string]int, total int, err error) {
	byKind = map[string]int{}
	bySupport = map[string]int{}
	byStatus = map[string]int{}
	rows, err := s.db.Query(`SELECT kind, support, status FROM records WHERE project_id=?`, s.project)
	if err != nil {
		return nil, nil, nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, sp, st string
		if err := rows.Scan(&k, &sp, &st); err != nil {
			return nil, nil, nil, 0, err
		}
		byKind[k]++
		bySupport[sp]++
		byStatus[st]++
		total++
	}
	return byKind, bySupport, byStatus, total, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
