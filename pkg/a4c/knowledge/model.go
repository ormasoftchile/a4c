// Package knowledge implements A4C Knowledge: an optional, modular subsystem
// that captures, evidence-links, maintains, and exposes engineering knowledge
// accumulated during A4C activities.
//
// This package is disabled by default. When disabled, no database is created,
// no MCP tools are advertised, and normal A4C behaviour is unchanged.
package knowledge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

// Kind is the role a knowledge record plays in an investigation.
type Kind string

const (
	KindHypothesis  Kind = "hypothesis"
	KindExperiment  Kind = "experiment"
	KindEvidence    Kind = "evidence"
	KindFinding     Kind = "finding"
	KindDecision    Kind = "decision"
	KindAlternative Kind = "alternative"
	KindLesson      Kind = "lesson"
	KindQuestion    Kind = "question"
)

// Valid reports whether k is a known kind.
func (k Kind) Valid() bool {
	switch k {
	case KindHypothesis, KindExperiment, KindEvidence, KindFinding,
		KindDecision, KindAlternative, KindLesson, KindQuestion:
		return true
	}
	return false
}

// Support is the evidential state of a claim. It is deliberately orthogonal to
// Kind: a finding may be supported, contradicted, or inconclusive; a hypothesis
// starts untested.
//
// "verified" is NOT a support value. Evidence integrity, experiment validity,
// and claim support are separate properties (see Integrity and AssessOrigin).
type Support string

const (
	SupportUntested     Support = "untested"
	SupportObserved     Support = "observed"
	SupportSupported    Support = "supported"
	SupportContradicted Support = "contradicted"
	SupportInconclusive Support = "inconclusive"
)

// Valid reports whether s is a known support state.
func (s Support) Valid() bool {
	switch s {
	case SupportUntested, SupportObserved, SupportSupported, SupportContradicted, SupportInconclusive:
		return true
	}
	return false
}

// Integrity records whether referenced evidence is authentic and available.
// It is set by A4C (by recomputing artifact hashes), never by a model.
type Integrity string

const (
	IntegrityPending Integrity = "pending"
	IntegrityVerified Integrity = "verified"
	IntegrityBroken  Integrity = "broken"
)

// Status is the record lifecycle. It no longer encodes evidential state.
type Status string

const (
	StatusDraft     Status = "draft"
	StatusPublished Status = "published"
	StatusRetracted Status = "retracted"
)

// AssessOrigin identifies who produced a claim-support assessment. An agent
// assessment is an agent-authored judgement, not independently established truth.
type AssessOrigin string

const (
	AssessOriginAgent    AssessOrigin = "agent"
	AssessOriginReviewer AssessOrigin = "independent_reviewer"
	AssessOriginHuman    AssessOrigin = "human"
)

// Scope bounds the conditions under which a claim holds. A claim may only be
// promoted to SupportSupported with a non-empty scope, so that a benchmark on
// one machine is never presented as a universal result.
type Scope struct {
	Configurations []string `json:"configs,omitempty"`
	Workloads      []string `json:"workloads,omitempty"`
	Assumptions    []string `json:"assumptions,omitempty"`
	Host           string   `json:"host,omitempty"`
	Limitations    []string `json:"limitations,omitempty"`
}

// Empty reports whether the scope places no bounds on the claim.
func (s Scope) Empty() bool {
	return len(s.Configurations) == 0 && len(s.Workloads) == 0 &&
		len(s.Assumptions) == 0 && s.Host == "" && len(s.Limitations) == 0
}

// EvidenceRef points at an independently inspectable artifact. Hash is
// content-addressed integrity metadata; it is never used for semantic
// deduplication of records.
type EvidenceRef struct {
	Kind       string    `json:"kind"` // validation_evidence|log|diff|commit|url|external
	ArtifactID string    `json:"artifact_id,omitempty"`
	Hash       string    `json:"hash,omitempty"`    // sha256 of artifact bytes
	Locator    string    `json:"locator,omitempty"` // path/URL
	Integrity  Integrity `json:"integrity"`
	Validity   string    `json:"validity,omitempty"` // unknown|agent_asserted|reviewer_confirmed|human_confirmed
}

// Link is a typed relation between two knowledge records.
type Link struct {
	To   string `json:"to"`
	Rel  string `json:"rel"` // supports|contradicts|qualifies|limits|supersedes|derived_from|corroborates|answers
	Note string `json:"note,omitempty"`
}

// ValidRel reports whether rel is a known relation.
func ValidRel(rel string) bool {
	switch rel {
	case "supports", "contradicts", "qualifies", "limits", "supersedes",
		"derived_from", "corroborates", "answers":
		return true
	}
	return false
}

// Assessment is an explicit, evidence-cited judgement that a claim is supported,
// contradicted, or inconclusive. It retains assessor identity, rationale, scope,
// and origin so an agent-authored judgement is never confused with independent
// verification. Assessments are append-only; a claim may hold several, including
// contradictory ones.
type Assessment struct {
	ID         string       `json:"id"`
	RecordID   string       `json:"record_id"`
	Support    Support      `json:"support"`
	Evidence   []EvidenceRef `json:"evidence,omitempty"`
	Scope      Scope        `json:"scope,omitempty"`
	Rationale  string       `json:"rationale"`
	Origin     AssessOrigin `json:"origin"`
	Assessor   string       `json:"assessor"`
	AssessedAt time.Time    `json:"assessed_at"`
	Revision   int          `json:"revision"`
}

// KnowledgeRecord is one unit of durable engineering knowledge.
type KnowledgeRecord struct {
	ID              string        `json:"id"`
	ProjectID       string        `json:"project_id"`
	InvestigationID string        `json:"investigation_id,omitempty"`
	TaskID          string        `json:"task_id,omitempty"`
	ActivityID      string        `json:"activity_id,omitempty"`
	Kind            Kind          `json:"kind"`
	Statement       string        `json:"statement"`
	Rationale       string        `json:"rationale,omitempty"`
	Support         Support       `json:"support"`
	Status          Status        `json:"status"`
	Integrity       Integrity     `json:"integrity"`
	Confidence      string        `json:"confidence,omitempty"` // high|medium|low|unknown
	Scope           Scope         `json:"scope,omitempty"`
	Author          string        `json:"author"` // agent id | "deterministic"
	Host            string        `json:"host,omitempty"`
	Revision        int           `json:"revision"`
	Supersedes      string        `json:"supersedes,omitempty"`
	SupersededBy    string        `json:"superseded_by,omitempty"`
	Evidence        []EvidenceRef `json:"evidence,omitempty"`
	Links           []Link        `json:"links,omitempty"`
	ContentHash     string        `json:"content_hash"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
}

// ContentHash computes the integrity hash over the canonical statement and
// rationale. It detects corruption of the claim text; it is NOT used to merge
// or deduplicate semantically identical records.
func (r *KnowledgeRecord) ComputeContentHash() string {
	h := sha256.Sum256([]byte(string(r.Kind) + "\x00" + r.Statement + "\x00" + r.Rationale))
	return "sha256:" + hex.EncodeToString(h[:])
}

// computeID derives a stable, A4C-assigned record ID. It is never model-supplied.
func computeID(project string, kind Kind, ts time.Time, seq int) string {
	proj := sanitizeID(project)
	if proj == "" {
		proj = "default"
	}
	return "k-" + proj + "-" + string(kind) + "-" +
		hex.EncodeToString([]byte(ts.UTC().Format("20060102150405"))) + "-" + itoa(seq)
}

func sanitizeID(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, strings.ToLower(s))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// canonicalJSON produces a stable encoding for hashing/transport tests.
func canonicalJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
