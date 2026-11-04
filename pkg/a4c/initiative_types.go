package a4c

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// InitiativeStatus represents the overall lifecycle of an initiative.
type InitiativeStatus string

const (
	InitiativeStatusPlanning   InitiativeStatus = "planning"
	InitiativeStatusReady      InitiativeStatus = "ready"
	InitiativeStatusInProgress InitiativeStatus = "in_progress"
	InitiativeStatusCompleted  InitiativeStatus = "completed"
	InitiativeStatusFailed     InitiativeStatus = "failed"
)

// StageStatus represents the execution state of an individual stage in the DAG.
type StageStatus string

const (
	StageStatusPending    StageStatus = "pending"
	StageStatusRunning    StageStatus = "running"
	StageStatusVerified   StageStatus = "verified"
	StageStatusAccepted   StageStatus = "accepted"
	StageStatusIntegrated StageStatus = "integrated"
	StageStatusFailed     StageStatus = "failed"
)

// StageSpec defines the functional boundary and non-goals of a stage.
type StageSpec struct {
	Summary string   `json:"summary"`
	What    []string `json:"what"`
	WhatNot []string `json:"what_not"`
}

// Stage represents a single bounded unit of work in the initiative DAG.
type Stage struct {
	ID                 string      `json:"id"`
	Title              string      `json:"title"`
	Dependencies       []string    `json:"dependencies"`
	DependsOn          []string    `json:"depends_on,omitempty"`
	Objective          string      `json:"objective,omitempty"`
	Status             StageStatus `json:"status,omitempty"`
	Spec               StageSpec   `json:"spec,omitempty"`
	AcceptanceCriteria []string    `json:"acceptance_criteria"`
	TaskID             string      `json:"task_id,omitempty"`
	WorktreePath       string      `json:"worktree_path,omitempty"`
}

// GetDependencies returns the list of stage IDs this stage depends on.
func (s *Stage) GetDependencies() []string {
	if len(s.Dependencies) > 0 {
		return s.Dependencies
	}
	return s.DependsOn
}

// DependencyGraph represents the machine-readable DAG interchange artifact (dag.json).
type DependencyGraph struct {
	Initiative string   `json:"initiative"`
	Stages     []Stage  `json:"stages"`
}

// StageReview represents the structured adversarial review verdict artifact (review.json).
type StageReview struct {
	StageID    string    `json:"stage_id"`
	Verdict    string    `json:"verdict"` // "PASS" or "FAIL"
	Violations []string  `json:"violations,omitempty"`
	Summary    string    `json:"summary"`
	ReviewedAt time.Time `json:"reviewed_at,omitempty"`
}

// UnmarshalJSON implements custom JSON unmarshaling to handle string slices, object slices, or mixed formats for Violations.
func (r *StageReview) UnmarshalJSON(data []byte) error {
	type rawReview struct {
		StageID    string          `json:"stage_id"`
		Verdict    string          `json:"verdict"`
		Violations json.RawMessage `json:"violations,omitempty"`
		Summary    string          `json:"summary"`
		ReviewedAt time.Time       `json:"reviewed_at,omitempty"`
	}
	var raw rawReview
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	r.StageID = raw.StageID
	r.Verdict = raw.Verdict
	r.Summary = raw.Summary
	r.ReviewedAt = raw.ReviewedAt

	if len(raw.Violations) > 0 && string(raw.Violations) != "null" {
		// First try slice of strings
		var strList []string
		if err := json.Unmarshal(raw.Violations, &strList); err == nil {
			r.Violations = strList
			return nil
		}
		// Try slice of objects or mixed
		var objList []json.RawMessage
		if err := json.Unmarshal(raw.Violations, &objList); err == nil {
			for _, item := range objList {
				var s string
				if err := json.Unmarshal(item, &s); err == nil {
					r.Violations = append(r.Violations, s)
					continue
				}
				var itemObj map[string]any
				if err := json.Unmarshal(item, &itemObj); err == nil {
					if desc, ok := itemObj["description"].(string); ok && desc != "" {
						r.Violations = append(r.Violations, desc)
						continue
					}
				}
				r.Violations = append(r.Violations, string(item))
			}
			return nil
		}
		// If single string
		var singleStr string
		if err := json.Unmarshal(raw.Violations, &singleStr); err == nil {
			r.Violations = []string{singleStr}
			return nil
		}
	}
	return nil
}

// Initiative represents the top-level persisted record tracking a multi-stage project.
type Initiative struct {
	ID        string           `json:"id"`
	Title     string           `json:"title"`
	Objective string           `json:"objective"`
	Status    InitiativeStatus `json:"status"`
	CreatedAt time.Time        `json:"created_at"`
	UpdatedAt time.Time        `json:"updated_at"`
	Stages    []Stage          `json:"stages"`
}

// LoadDependencyGraph parses and validates a dag.json file from disk.
// If defaultInitiative is provided and dag.json does not specify an initiative title, defaultInitiative is used.
func LoadDependencyGraph(path string, defaultInitiative string) (*DependencyGraph, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading dag.json: %w", err)
	}
	var dag DependencyGraph
	if err := json.Unmarshal(data, &dag); err != nil {
		return nil, fmt.Errorf("parsing dag.json: %w", err)
	}
	if dag.Initiative == "" && defaultInitiative != "" {
		dag.Initiative = defaultInitiative
	}
	if err := dag.Validate(); err != nil {
		return nil, fmt.Errorf("invalid dag.json: %w", err)
	}
	return &dag, nil
}

// Validate verifies that the dependency graph is well-formed, non-empty, has unique IDs,
// references only existing dependencies, and contains no cycles.
func (g *DependencyGraph) Validate() error {
	if g.Initiative == "" {
		return errors.New("initiative title must not be empty")
	}
	if len(g.Stages) == 0 {
		return errors.New("dag must contain at least one stage")
	}

	for i := range g.Stages {
		s := &g.Stages[i]
		if len(s.Dependencies) == 0 && len(s.DependsOn) > 0 {
			s.Dependencies = s.DependsOn
		}
		if s.Spec.Summary == "" && s.Objective != "" {
			s.Spec.Summary = s.Objective
		}
	}

	stageMap := make(map[string]bool, len(g.Stages))
	for _, s := range g.Stages {
		if s.ID == "" {
			return errors.New("stage ID must not be empty")
		}
		if s.Title == "" {
			return fmt.Errorf("stage %q: title must not be empty", s.ID)
		}
		if stageMap[s.ID] {
			return fmt.Errorf("duplicate stage ID %q", s.ID)
		}
		stageMap[s.ID] = true
	}

	for _, s := range g.Stages {
		for _, dep := range s.GetDependencies() {
			if dep == s.ID {
				return fmt.Errorf("stage %q cannot depend on itself", s.ID)
			}
			if !stageMap[dep] {
				return fmt.Errorf("stage %q depends on non-existent stage %q", s.ID, dep)
			}
		}
	}

	// Check for cycles using topological sort
	_, err := g.TopologicalSort()
	if err != nil {
		return err
	}

	return nil
}

// TopologicalSort returns stages ordered such that each stage appears after all its dependencies.
func (g *DependencyGraph) TopologicalSort() ([]Stage, error) {
	inDegree := make(map[string]int, len(g.Stages))
	stageByID := make(map[string]Stage, len(g.Stages))
	dependents := make(map[string][]string, len(g.Stages))

	for _, s := range g.Stages {
		stageByID[s.ID] = s
		deps := s.GetDependencies()
		inDegree[s.ID] = len(deps)
		for _, dep := range deps {
			dependents[dep] = append(dependents[dep], s.ID)
		}
	}

	var queue []string
	for _, s := range g.Stages {
		if inDegree[s.ID] == 0 {
			queue = append(queue, s.ID)
		}
	}

	var ordered []Stage
	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]
		ordered = append(ordered, stageByID[curr])

		for _, dep := range dependents[curr] {
			inDegree[dep]--
			if inDegree[dep] == 0 {
				queue = append(queue, dep)
			}
		}
	}

	if len(ordered) != len(g.Stages) {
		return nil, errors.New("dependency cycle detected in DAG")
	}

	return ordered, nil
}
