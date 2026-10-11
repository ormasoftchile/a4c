package a4c_test

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ormasoftchile/a4c/pkg/a4c"
)

func TestDoctorRunChecksWorkspaceAndHostCapabilities(t *testing.T) {
	root := t.TempDir()
	cmd := exec.Command("git", "-C", root, "init", "-q")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("initialize git repository: %v\n%s", err, output)
	}
	for _, dir := range []string{"activities", "roster"} {
		if err := os.MkdirAll(filepath.Join(root, ".a4c", dir), 0755); err != nil {
			t.Fatalf("create .a4c/%s: %v", dir, err)
		}
	}

	report := a4c.RunDoctor(root)
	if report.RootDir != root {
		t.Errorf("RootDir = %q, want %q", report.RootDir, root)
	}
	if report.Timestamp.IsZero() {
		t.Error("Timestamp should be set")
	}
	assertDoctorCheck(t, report, "Git", "Repository & HEAD", true)
	assertDoctorCheck(t, report, "A4C State", "Durable Storage (.a4c)", true)

	if len(report.Hosts) == 0 {
		t.Fatal("RunDoctor returned no universal host capabilities")
	}
	foundOpenCode := false
	for _, host := range report.Hosts {
		if host.HostID == "opencode" {
			foundOpenCode = true
		}
		if host.HostID == "" || host.DisplayName == "" {
			t.Errorf("host capabilities missing identity: %+v", host)
		}
	}
	if !foundOpenCode {
		t.Errorf("host capabilities do not include OpenCode: %+v", report.Hosts)
	}
}

func TestDoctorRunReportsMissingWorkspaceState(t *testing.T) {
	report := a4c.RunDoctor(t.TempDir())
	assertDoctorCheck(t, report, "Git", "Repository", false)
	assertDoctorCheck(t, report, "A4C State", "Durable Storage (.a4c)", false)
}

func TestDoctorReportPrint(t *testing.T) {
	report := a4c.DoctorReport{
		Timestamp: time.Date(2026, time.October, 10, 12, 30, 0, 0, time.UTC),
		RootDir:   "/workspace/example",
		Checks: []a4c.DoctorCheck{
			{Category: "Git", Name: "Repository", Passed: true, Detail: "Repository initialized"},
			{Category: "A4C State", Name: "Durable Storage (.a4c)", Passed: false, Detail: "Storage missing"},
		},
		Hosts: []a4c.HostCapabilities{{
			HostID:            "opencode",
			DisplayName:       "OpenCode Headless Server",
			Available:         false,
			DefaultModel:      "opencode-go/mimo-v2.5",
			SessionInvocation: false,
			Limitations:       []string{"Requires running headless server"},
		}},
	}

	oldStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdout pipe: %v", err)
	}
	os.Stdout = writer
	defer func() { os.Stdout = oldStdout }()

	report.Print()
	if err := writer.Close(); err != nil {
		t.Fatalf("close stdout writer: %v", err)
	}
	os.Stdout = oldStdout
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read diagnostic output: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close stdout reader: %v", err)
	}

	for _, want := range []string{
		"A4C Environment Doctor Diagnosis (2026-10-10T12:30:00Z)",
		"Workspace Root: /workspace/example",
		"[Git]",
		"Repository initialized",
		"[A4C State]",
		"Storage missing",
		"Universal Host Capabilities & Limitations",
		"OpenCode Headless Server (opencode)",
		"Requires running headless server",
	} {
		if !strings.Contains(string(output), want) {
			t.Errorf("diagnostic output missing %q:\n%s", want, output)
		}
	}
}

func assertDoctorCheck(t *testing.T, report a4c.DoctorReport, category, name string, passed bool) {
	t.Helper()
	for _, check := range report.Checks {
		if check.Category == category && check.Name == name {
			if check.Passed != passed {
				t.Errorf("check %q/%q Passed = %t, want %t (detail: %s)", category, name, check.Passed, passed, check.Detail)
			}
			return
		}
	}
	t.Errorf("missing doctor check %q/%q", category, name)
}
