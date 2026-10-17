package a4c

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// VerificationCommand is a trusted, reproducible project verification contract.
// It is resolved ONCE from trusted configuration or the committed baseline, so
// a coder cannot change it mid-run by adding files (e.g. a go.mod to a C repo).
type VerificationCommand struct {
	// Argv is the full command, e.g. ["make","-f","Makefile.cbm","SANITIZE=","test"].
	Argv []string `json:"command"`
	// Dir is the working directory relative to the workspace root ("" = root).
	Dir string `json:"dir,omitempty"`
	// Timeout bounds execution. Zero means the runner default.
	Timeout time.Duration `json:"-"`
	// TimeoutSeconds is the JSON form of Timeout.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// Kind selects the output analyzer: "go", "make", or "generic".
	Kind string `json:"kind,omitempty"`
	// Source records where this contract came from: "config", "detected", or "unavailable".
	Source string `json:"source,omitempty"`
}

// VerificationResult is the structured outcome of running a VerificationCommand.
type VerificationResult struct {
	Command        []string      `json:"command"`
	ExitCode       int           `json:"exit_code"`
	Output         string        `json:"-"`
	OutputPath     string        `json:"output_path,omitempty"` // durable log, producer-workspace-relative
	TestsExecuted  int           `json:"tests_executed"`
	Vacuous        bool          `json:"vacuous"`
	Failed         bool          `json:"failed"`
	Unavailable    bool          `json:"unavailable"`
	Duration       time.Duration `json:"duration,omitempty"`
	SourceRevision string        `json:"source_revision,omitempty"` // git HEAD the run validated
	StartedAt      time.Time     `json:"started_at"`
	CompletedAt    time.Time     `json:"completed_at"`
}

// verificationConfig is the on-disk trusted contract at <workspace>/.a4c/verify.json.
// It is authored by `a4c init` / a human / the harness BEFORE the coder runs, so
// it is trusted configuration, not a coder-generated file.
type verificationConfig struct {
	Command        []string `json:"command"`
	Dir            string   `json:"dir,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
	Kind           string   `json:"kind,omitempty"`
}

// ProjectAwareRunner is implemented by runners that can report structured,
// project-aware verification. TestRunner implementations that do not implement
// this fall back to the legacy exit-code/output contract.
type ProjectAwareRunner interface {
	TestRunner
	Verify(ctx context.Context, workspaceDir string) (VerificationResult, error)
}

// Default verification timeouts by command family.
const (
	defaultGoTimeout   = 10 * time.Minute
	defaultMakeTimeout = 90 * time.Minute
	defaultGenTimeout  = 30 * time.Minute
)

// ProjectTestRunner selects the project's verification contract from trusted
// configuration or the committed baseline, runs it, and classifies the result.
// It never trusts coder-generated files to choose the command.
type ProjectTestRunner struct {
	// LogDir overrides where durable logs are written (default <ws>/.a4c/evidence).
	LogDir string
	cache  map[string]VerificationCommand // workspaceDir -> resolved command
}

// NewProjectTestRunner creates a project-aware runner.
func NewProjectTestRunner() *ProjectTestRunner {
	return &ProjectTestRunner{cache: make(map[string]VerificationCommand)}
}

// RunTests satisfies the legacy TestRunner contract.
func (r *ProjectTestRunner) RunTests(ctx context.Context, workspaceDir string) (int, string, error) {
	res, err := r.Verify(ctx, workspaceDir)
	if err != nil {
		return -1, "", err
	}
	return res.ExitCode, res.Output, nil
}

// Verify resolves the project's verification command, runs it, and returns a
// structured result. The command is resolved from trusted config or the
// committed baseline and cached, so coder file additions cannot change it.
func (r *ProjectTestRunner) Verify(ctx context.Context, workspaceDir string) (VerificationResult, error) {
	start := time.Now()
	cmd, err := r.Resolve(ctx, workspaceDir)
	if err != nil {
		return VerificationResult{Unavailable: true, StartedAt: start, CompletedAt: time.Now()}, err
	}
	if len(cmd.Argv) == 0 {
		// No verification contract available: explicit failure, never invented evidence.
		return VerificationResult{
			Command: nil, Unavailable: true, Vacuous: true,
			StartedAt: start, CompletedAt: time.Now(),
		}, nil
	}

	timeout := cmd.Timeout
	if timeout <= 0 {
		switch cmd.Kind {
		case "go":
			timeout = defaultGoTimeout
		case "make":
			timeout = defaultMakeTimeout
		default:
			timeout = defaultGenTimeout
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	runDir := workspaceDir
	if cmd.Dir != "" && !filepath.IsAbs(cmd.Dir) {
		runDir = filepath.Join(workspaceDir, cmd.Dir)
	}
	execCmd := exec.CommandContext(runCtx, cmd.Argv[0], cmd.Argv[1:]...)
	execCmd.Dir = runDir
	outBytes, runErr := execCmd.CombinedOutput()
	output := string(outBytes)
	exitCode := 0
	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else if runCtx.Err() == context.DeadlineExceeded {
			return VerificationResult{
				Command: cmd.Argv, ExitCode: -1, Output: output, Failed: true,
				Duration: time.Since(start), StartedAt: start, CompletedAt: time.Now(),
			}, fmt.Errorf("verification timed out after %v: %s", timeout, strings.Join(cmd.Argv, " "))
		} else {
			return VerificationResult{
				Command: cmd.Argv, ExitCode: -1, Output: output, Unavailable: true,
				Duration: time.Since(start), StartedAt: start, CompletedAt: time.Now(),
			}, runErr
		}
	}

	testsExecuted, vacuous := classifyVerification(cmd.Kind, exitCode, output)
	rev := gitHeadRevision(runDir)
	res := VerificationResult{
		Command:        cmd.Argv,
		ExitCode:       exitCode,
		Output:         output,
		TestsExecuted:  testsExecuted,
		Vacuous:        vacuous,
		Failed:         exitCode != 0,
		Duration:       time.Since(start),
		SourceRevision: rev,
		StartedAt:      start,
		CompletedAt:    time.Now(),
	}
	return res, nil
}

// Resolve returns the trusted verification command for a workspace. It prefers
// authored configuration, then detects from the COMMITTED baseline (so coder
// additions cannot flip it), then falls back to Go only if go.mod is committed.
func (r *ProjectTestRunner) Resolve(ctx context.Context, workspaceDir string) (VerificationCommand, error) {
	if c, ok := r.cache[workspaceDir]; ok {
		return c, nil
	}
	cmd := resolveVerification(ctx, workspaceDir)
	r.cache[workspaceDir] = cmd
	return cmd, nil
}

// resolveVerification implements the trust-ordered resolution.
func resolveVerification(ctx context.Context, workspaceDir string) VerificationCommand {
	// 1. Trusted authored configuration.
	if cfg, ok := readVerificationConfig(workspaceDir); ok && len(cfg.Command) > 0 {
		kind := cfg.Kind
		if kind == "" {
			kind = inferKind(cfg.Command)
		}
		vc := VerificationCommand{
			Argv: cfg.Command, Dir: cfg.Dir, Kind: kind, Source: "config",
			Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second,
		}
		return vc
	}

	// 2. Detect from the COMMITTED baseline. Using the committed tree (not the
	// working tree) is the trust boundary: a coder adding go.mod/Makefile
	// mid-run cannot change which verification contract applies.
	committed := committedFiles(ctx, workspaceDir)
	if has := func(f string) bool { return committed[f] }; true {
		if has("Makefile.cbm") && committedFileContains(ctx, workspaceDir, "Makefile.cbm", "test:") {
			return VerificationCommand{
				Argv: []string{"make", "-f", "Makefile.cbm", "SANITIZE=", "test"},
				Kind: "make", Source: "detected",
			}
		}
		if has("Makefile") && committedFileContains(ctx, workspaceDir, "Makefile", "test:") {
			return VerificationCommand{
				Argv: []string{"make", "SANITIZE=", "test"},
				Kind: "make", Source: "detected",
			}
		}
		if has("go.mod") {
			return VerificationCommand{
				Argv: []string{"go", "test", "-short", "./..."},
				Kind: "go", Source: "detected",
			}
		}
	}
	// 3. No verification contract can be established.
	return VerificationCommand{Source: "unavailable"}
}

// readVerificationConfig reads <workspace>/.a4c/verify.json if present.
func readVerificationConfig(workspaceDir string) (verificationConfig, bool) {
	var cfg verificationConfig
	path := filepath.Join(workspaceDir, ".a4c", "verify.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, false
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, false
	}
	return cfg, len(cfg.Command) > 0
}

// committedFiles lists files tracked at HEAD in the workspace (the baseline).
func committedFiles(ctx context.Context, workspaceDir string) map[string]bool {
	files := make(map[string]bool)
	cmd := exec.CommandContext(ctx, "git", "ls-tree", "--name-only", "HEAD")
	cmd.Dir = workspaceDir
	out, err := cmd.Output()
	if err != nil {
		return files
	}
	for _, l := range strings.Split(string(out), "\n") {
		l = strings.TrimSpace(l)
		if l != "" {
			files[l] = true
		}
	}
	return files
}

// committedFileContains reports whether a committed file's HEAD content contains substr.
func committedFileContains(ctx context.Context, workspaceDir, file, substr string) bool {
	cmd := exec.CommandContext(ctx, "git", "show", "HEAD:"+file)
	cmd.Dir = workspaceDir
	out, err := cmd.Output()
	if err != nil {
		// Fall back to the working-tree copy if git show fails.
		data, rerr := os.ReadFile(filepath.Join(workspaceDir, file))
		return rerr == nil && strings.Contains(string(data), substr)
	}
	return strings.Contains(string(out), substr)
}

// gitHeadRevision returns the workspace's current HEAD sha, or "".
func gitHeadRevision(dir string) string {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// inferKind guesses the analyzer from the command argv.
func inferKind(argv []string) string {
	if len(argv) == 0 {
		return "generic"
	}
	switch argv[0] {
	case "go":
		return "go"
	case "make", "gmake", "cmake", "ninja":
		return "make"
	default:
		return "generic"
	}
}

// testExecutedMarkers match strong evidence that at least one test ran. It
// spans Go package/test lines AND native C test-runner output (indented
// "<name> PASS" rows, a standalone PASS, and "NNNN passed" summaries), so a
// real C suite that passed is never misread as vacuous.
var testExecutedMarkers = regexp.MustCompile(
	`(?m)^(ok(\s|$)|FAIL(\s|$)|--- (PASS|FAIL|SKIP): |\[ *(PASS|FAIL|OK|ok|passed|failed) *\])` +
		`|.*\bPASS\s*$` +
		`|\b\d+\s+(passed|failed)\b|\btests? (passed|failed|ran)\b|\bPASS:|\ball tests passed\b`)

// classifyVerification decides how many tests ran and whether the run is
// vacuous (proves nothing). Vacuous is conservatively true when the command
// exited 0 but we cannot establish that any test executed.
func classifyVerification(kind string, exitCode int, output string) (testsExecuted int, vacuous bool) {
	switch kind {
	case "go":
		return AnalyzeGoTestOutput(output)
	default:
		// make / generic.
		// Positive evidence that tests ran ALWAYS wins. A huge build log can
		// contain incidental "0 tests"-like substrings (e.g. C pipeline stats
		// "tests=0 tests_file=0") that must not override a suite that clearly
		// executed and passed.
		if n := len(testExecutedMarkers.FindAllString(output, -1)); n > 0 {
			return n, false
		}
		// No positive evidence. Exit non-zero is a genuine failure.
		if exitCode != 0 {
			return 0, false
		}
		// Exit 0 with no positive test evidence: vacuous unless we can
		// establish tests ran. Explicit no-tests markers or empty output are
		// vacuous; anything else is fail-closed vacuous.
		return 0, true
	}
}
