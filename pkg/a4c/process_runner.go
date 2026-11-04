package a4c

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// ProcessRunRequest specifies execution parameters for a Copilot subprocess.
type ProcessRunRequest struct {
	Dir                string
	Agent              string
	Prompt             string
	AllowAllTools      bool
	Prefix             string
	Timeout            time.Duration
	ExpectedOutputFile string
	Stdout             io.Writer
	Stderr             io.Writer
}

// ProcessRunResult captures the execution output and status.
type ProcessRunResult struct {
	ExitCode int
	Duration time.Duration
	Stdout   string
	Stderr   string
}

// CopilotProcessRunner manages the lifecycle of non-interactive Copilot sessions.
type CopilotProcessRunner struct {
	copilotBin string
}

// NewCopilotProcessRunner creates a new CopilotProcessRunner, locating the copilot binary.
func NewCopilotProcessRunner() (*CopilotProcessRunner, error) {
	bin, err := exec.LookPath("copilot")
	if err != nil {
		return nil, errors.New("copilot binary not found in PATH")
	}
	return &CopilotProcessRunner{copilotBin: bin}, nil
}

// Run executes a non-interactive Copilot prompt in its own process group.
func (r *CopilotProcessRunner) Run(ctx context.Context, req ProcessRunRequest) (*ProcessRunResult, error) {
	if req.Prompt == "" {
		return nil, errors.New("prompt cannot be empty")
	}

	var absDir string
	if req.Dir != "" {
		if p, err := filepath.Abs(req.Dir); err == nil {
			absDir = p
		} else {
			absDir = req.Dir
		}
	}

	args := []string{"-p", req.Prompt}
	if absDir != "" {
		args = append([]string{"-C", absDir}, args...)
	}
	if req.Agent != "" {
		args = append(args, "--agent", req.Agent)
	}
	if req.AllowAllTools {
		args = append(args, "--allow-all-tools")
	}

	cmdCtx := ctx
	var cancel context.CancelFunc
	if req.Timeout > 0 {
		cmdCtx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(cmdCtx, r.copilotBin, args...)
	if absDir != "" {
		cmd.Dir = absDir
	}

	// Stdin from /dev/null ensures non-interactive failure instead of hanging on missing TTY
	cmd.Stdin = nil

	// Configure OS Process Group for clean teardown of child process trees
	setProcessGroup(cmd)

	var stdoutBuf, stderrBuf bytes.Buffer

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("creating stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("creating stderr pipe: %w", err)
	}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting copilot process: %w", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		streamOutput(stdoutPipe, &stdoutBuf, req.Stdout, req.Prefix)
	}()

	go func() {
		defer wg.Done()
		streamOutput(stderrPipe, &stderrBuf, req.Stderr, req.Prefix+"[err] ")
	}()

	waitErr := cmd.Wait()
	wg.Wait()
	duration := time.Since(start)

	exitCode := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	res := &ProcessRunResult{
		ExitCode: exitCode,
		Duration: duration,
		Stdout:   stdoutBuf.String(),
		Stderr:   stderrBuf.String(),
	}

	if waitErr != nil {
		// Clean up process group if context timed out or failed
		killProcessGroup(cmd)
		return res, fmt.Errorf("copilot process exited with code %d: %w", exitCode, waitErr)
	}

	// Physical file validation (No-Stdout-Scraping rule)
	if req.ExpectedOutputFile != "" {
		info, err := os.Stat(req.ExpectedOutputFile)
		if err != nil {
			if os.IsNotExist(err) {
				return res, fmt.Errorf("expected output file %q was not created by copilot", req.ExpectedOutputFile)
			}
			return res, fmt.Errorf("validating output file %q: %w", req.ExpectedOutputFile, err)
		}
		if info.Size() == 0 {
			return res, fmt.Errorf("expected output file %q was created but is empty", req.ExpectedOutputFile)
		}
	}

	return res, nil
}

func streamOutput(r io.Reader, capture *bytes.Buffer, live io.Writer, prefix string) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		capture.WriteString(line + "\n")
		if live != nil {
			if prefix != "" {
				fmt.Fprintf(live, "%s%s\n", prefix, line)
			} else {
				fmt.Fprintln(live, line)
			}
		}
	}
}


