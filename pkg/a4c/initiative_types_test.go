package a4c

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDependencyGraph_ValidationAndSort(t *testing.T) {
	tests := []struct {
		name        string
		dag         DependencyGraph
		wantErr     bool
		errContains string
		wantOrder   []string
	}{
		{
			name: "valid linear pipeline",
			dag: DependencyGraph{
				Initiative: "Auth Initiative",
				Stages: []Stage{
					{ID: "stage-01", Title: "Token Bucket", Dependencies: []string{}},
					{ID: "stage-02", Title: "HTTP Middleware", Dependencies: []string{"stage-01"}},
					{ID: "stage-03", Title: "Redis Store", Dependencies: []string{"stage-02"}},
				},
			},
			wantErr:   false,
			wantOrder: []string{"stage-01", "stage-02", "stage-03"},
		},
		{
			name: "valid diamond graph",
			dag: DependencyGraph{
				Initiative: "Diamond Pipeline",
				Stages: []Stage{
					{ID: "stage-01", Title: "Core", Dependencies: []string{}},
					{ID: "stage-02a", Title: "Branch A", Dependencies: []string{"stage-01"}},
					{ID: "stage-02b", Title: "Branch B", Dependencies: []string{"stage-01"}},
					{ID: "stage-03", Title: "Merge", Dependencies: []string{"stage-02a", "stage-02b"}},
				},
			},
			wantErr: false,
		},
		{
			name: "cycle detected",
			dag: DependencyGraph{
				Initiative: "Cyclic Pipeline",
				Stages: []Stage{
					{ID: "stage-01", Title: "First", Dependencies: []string{"stage-02"}},
					{ID: "stage-02", Title: "Second", Dependencies: []string{"stage-01"}},
				},
			},
			wantErr:     true,
			errContains: "cycle",
		},
		{
			name: "self dependency",
			dag: DependencyGraph{
				Initiative: "Self Loop",
				Stages: []Stage{
					{ID: "stage-01", Title: "Loop", Dependencies: []string{"stage-01"}},
				},
			},
			wantErr:     true,
			errContains: "cannot depend on itself",
		},
		{
			name: "missing dependency",
			dag: DependencyGraph{
				Initiative: "Missing Dep",
				Stages: []Stage{
					{ID: "stage-01", Title: "Stage", Dependencies: []string{"non-existent"}},
				},
			},
			wantErr:     true,
			errContains: "non-existent",
		},
		{
			name: "duplicate ID",
			dag: DependencyGraph{
				Initiative: "Duplicate ID",
				Stages: []Stage{
					{ID: "stage-01", Title: "One", Dependencies: []string{}},
					{ID: "stage-01", Title: "Two", Dependencies: []string{}},
				},
			},
			wantErr:     true,
			errContains: "duplicate",
		},
		{
			name: "empty stages",
			dag: DependencyGraph{
				Initiative: "Empty",
				Stages:     []Stage{},
			},
			wantErr:     true,
			errContains: "at least one",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.dag.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() error = %v, wantErr = %v", err, tc.wantErr)
			}
			if tc.wantErr && tc.errContains != "" {
				if err == nil || !contains(err.Error(), tc.errContains) {
					t.Errorf("error %q does not contain %q", err, tc.errContains)
				}
				return
			}
			if !tc.wantErr && len(tc.wantOrder) > 0 {
				ordered, err := tc.dag.TopologicalSort()
				if err != nil {
					t.Fatalf("TopologicalSort() failed: %v", err)
				}
				for i, wantID := range tc.wantOrder {
					if ordered[i].ID != wantID {
						t.Errorf("ordered[%d] = %q, want %q", i, ordered[i].ID, wantID)
					}
				}
			}
		})
	}
}

func TestLoadDependencyGraph_DiskFile(t *testing.T) {
	tempDir := t.TempDir()
	dagPath := filepath.Join(tempDir, "dag.json")
	content := `{
		"initiative": "Disk Test",
		"stages": [
			{
				"id": "stage-01",
				"title": "First Step",
				"dependencies": [],
				"spec": {"summary": "S1", "what": ["w1"], "what_not": ["wn1"]},
				"acceptance_criteria": ["ac1"]
			}
		]
	}`
	if err := os.WriteFile(dagPath, []byte(content), 0644); err != nil {
		t.Fatalf("writing test dag.json: %v", err)
	}

	dag, err := LoadDependencyGraph(dagPath, "Disk Test")
	if err != nil {
		t.Fatalf("LoadDependencyGraph() failed: %v", err)
	}
	if dag.Initiative != "Disk Test" {
		t.Errorf("dag.Initiative = %q, want 'Disk Test'", dag.Initiative)
	}
	if len(dag.Stages) != 1 || dag.Stages[0].ID != "stage-01" {
		t.Errorf("unexpected stages: %+v", dag.Stages)
	}
}

func TestStageReviewUnmarshal(t *testing.T) {
	// Format 1: String slice
	raw1 := `{"stage_id": "test", "verdict": "FAIL", "violations": ["violation 1", "violation 2"], "summary": "failed"}`
	var rev1 StageReview
	if err := json.Unmarshal([]byte(raw1), &rev1); err != nil {
		t.Fatalf("unmarshaling string slice: %v", err)
	}
	if len(rev1.Violations) != 2 || rev1.Violations[0] != "violation 1" {
		t.Errorf("unexpected violations: %+v", rev1.Violations)
	}

	// Format 2: Object slice
	raw2 := `{"stage_id": "test", "verdict": "FAIL", "violations": [{"description": "obj violation 1"}, {"description": "obj violation 2"}], "summary": "failed"}`
	var rev2 StageReview
	if err := json.Unmarshal([]byte(raw2), &rev2); err != nil {
		t.Fatalf("unmarshaling object slice: %v", err)
	}
	if len(rev2.Violations) != 2 || rev2.Violations[0] != "obj violation 1" {
		t.Errorf("unexpected violations: %+v", rev2.Violations)
	}
}

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}
