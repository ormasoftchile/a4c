package a4c

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestInitiativeService_CreateAndGet(t *testing.T) {
	tempDir := t.TempDir()
	svc := NewInitiativeService(tempDir, nil, nil)

	init, err := svc.CreateInitiative("Test Initiative", "Build something great")
	if err != nil {
		t.Fatalf("CreateInitiative failed: %v", err)
	}

	if init.ID == "" || init.Status != InitiativeStatusPlanning {
		t.Errorf("unexpected initiative state: %+v", init)
	}

	loaded, err := svc.GetInitiative(init.ID)
	if err != nil {
		t.Fatalf("GetInitiative failed: %v", err)
	}
	if loaded.Title != "Test Initiative" || loaded.Objective != "Build something great" {
		t.Errorf("loaded initiative mismatch: %+v", loaded)
	}
}

func TestInitiativeService_DependencyEnforcement(t *testing.T) {
	tempDir := t.TempDir()
	svc := NewInitiativeService(tempDir, nil, nil)

	init, err := svc.CreateInitiative("Dep Test", "Test deps")
	if err != nil {
		t.Fatalf("CreateInitiative failed: %v", err)
	}

	// Add 2 stages: stage-02 depends on stage-01
	init.Stages = []Stage{
		{
			ID:           "stage-01",
			Title:        "First",
			Status:       StageStatusPending, // Not integrated!
			Dependencies: []string{},
		},
		{
			ID:           "stage-02",
			Title:        "Second",
			Status:       StageStatusPending,
			Dependencies: []string{"stage-01"},
		},
	}
	if err := svc.SaveInitiative(init); err != nil {
		t.Fatalf("SaveInitiative failed: %v", err)
	}

	// Attempting to start stage-02 MUST fail because stage-01 is pending
	_, err = svc.StartStage(context.Background(), init.ID, "stage-02")
	if err == nil {
		t.Fatal("expected StartStage to fail due to unintegrated dependency, got nil")
	}
	if !contains(err.Error(), "must be integrated first") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestInitiativeService_IntegrateRequiresVerifiedStatus(t *testing.T) {
	tempDir := t.TempDir()
	svc := NewInitiativeService(tempDir, nil, nil)

	init, err := svc.CreateInitiative("Verify Test", "Test verify")
	if err != nil {
		t.Fatalf("CreateInitiative failed: %v", err)
	}

	init.Stages = []Stage{
		{
			ID:     "stage-01",
			Title:  "First",
			Status: StageStatusRunning, // Not verified!
		},
	}
	if err := svc.SaveInitiative(init); err != nil {
		t.Fatalf("SaveInitiative failed: %v", err)
	}

	err = svc.IntegrateStage(context.Background(), init.ID, "stage-01", "tester", "token", true)
	if err == nil {
		t.Fatal("expected IntegrateStage to fail for non-verified stage, got nil")
	}
	if !contains(err.Error(), "must be") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestInitiativeService_DiskFilesIsolation(t *testing.T) {
	tempDir := t.TempDir()
	svc := NewInitiativeService(tempDir, nil, nil)

	init, err := svc.CreateInitiative("Isolation", "Verify file paths")
	if err != nil {
		t.Fatalf("failed: %v", err)
	}

	initPath := filepath.Join(tempDir, ".a4c", "initiatives", init.ID, "initiative.json")
	if _, err := os.Stat(initPath); err != nil {
		t.Errorf("expected initiative.json at %s, got: %v", initPath, err)
	}
}
