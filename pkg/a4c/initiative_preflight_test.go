package a4c_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ormasoftchile/a4c/pkg/a4c"
)

func TestCheckInitiativePrerequisites_EmptyDir(t *testing.T) {
	tempDir := t.TempDir()

	report := a4c.CheckInitiativePrerequisites(tempDir)
	if report.AllSatisfied {
		t.Fatal("expected AllSatisfied to be false in fresh empty temp dir")
	}

	foundGit := false
	foundRepo := false
	for _, check := range report.Checks {
		if check.Name == "git" {
			foundGit = true
			if !check.Satisfied {
				t.Logf("git binary not found: %s", check.Detail)
			}
		}
		if check.Name == "Git Repository" {
			foundRepo = true
			if check.Satisfied {
				t.Error("Git Repository check should have failed in empty directory")
			}
		}
	}

	if !foundGit || !foundRepo {
		t.Errorf("expected git and Git Repository checks; foundGit=%v, foundRepo=%v", foundGit, foundRepo)
	}
}

func TestCheckInitiativePrerequisites_ValidGitRepo(t *testing.T) {
	tempDir := t.TempDir()
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = tempDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("git init failed: %v", err)
	}

	report := a4c.CheckInitiativePrerequisites(tempDir)
	for _, check := range report.Checks {
		if check.Name == "Git Repository" && !check.Satisfied {
			t.Errorf("Git Repository should be satisfied after git init: %s", check.Detail)
		}
	}
}

func TestCheckInitiativePrerequisites_WorkspaceAssetsDetection(t *testing.T) {
	tempDir := t.TempDir()
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = tempDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("git init failed: %v", err)
	}

	// Create dummy squad and specify dirs
	if err := os.MkdirAll(filepath.Join(tempDir, ".squad"), 0755); err != nil {
		t.Fatalf("mkdir .squad: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(tempDir, ".github", "agents"), 0755); err != nil {
		t.Fatalf("mkdir .github/agents: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tempDir, ".github", "agents", "squad.agent.md"), []byte("# Squad Agent"), 0644); err != nil {
		t.Fatalf("write squad.agent.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tempDir, ".github", "agents", "a4c.agent.md"), []byte("# A4C Agent"), 0644); err != nil {
		t.Fatalf("write a4c.agent.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tempDir, ".mcp.json"), []byte("{}"), 0644); err != nil {
		t.Fatalf("write .mcp.json: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(tempDir, ".specify"), 0755); err != nil {
		t.Fatalf("mkdir .specify: %v", err)
	}

	report := a4c.CheckInitiativePrerequisites(tempDir)
	for _, check := range report.Checks {
		if check.Category == "Workspace Config" && !check.Satisfied {
			t.Errorf("expected workspace check %s to be satisfied, got: %s", check.Name, check.Detail)
		}
	}
}
