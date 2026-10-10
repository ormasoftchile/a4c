package governed

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ormasoftchile/a4c/pkg/a4c"
	"github.com/ormasoftchile/yawr/runtime/pkg/run"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

var (
	ErrFilesystemEscape     = errors.New("governance: filesystem escape attempt detected")
	ErrStaleSnapshot        = errors.New("governance: stale snapshot precondition mismatch")
	ErrUnauthorizedMutation = errors.New("governance: mutating action not permitted by caller authority")
)

func stringPtr(s string) *string {
	return &s
}

// Manager orchestrates governed tool execution through an embedded YAWR SDK.
type Manager struct {
	service  a4c.TeamService
	invoker  tool.ToolInvoker
	registry tool.ToolRegistry
	runtime  *governedRuntime
	stateDir string
	role     string
}

// NewManager initializes the YAWR registry, governed runtime, and ToolInvoker.
func NewManager(service a4c.TeamService, stateDir string) (*Manager, error) {
	return NewManagerWithRole(service, stateDir, "")
}

// NewManagerWithRole initializes the YAWR registry with an explicit agent role boundary.
func NewManagerWithRole(service a4c.TeamService, stateDir, role string) (*Manager, error) {
	if stateDir == "" {
		stateDir = filepath.Join(service.WorkspaceDir(), ".a4c", "yawr-dispatch")
	}
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return nil, fmt.Errorf("governed manager: create state dir: %w", err)
	}

	reg := buildRegistry()
	rt := &governedRuntime{
		service: service,
	}

	invoker, err := run.NewToolInvoker(tool.InvokerOptions{
		Registry: reg,
		Runtime:  rt,
		StateDir: stateDir,
	})
	if err != nil {
		return nil, fmt.Errorf("governed manager: create invoker: %w", err)
	}

	return &Manager{
		service:  service,
		invoker:  invoker,
		registry: reg,
		runtime:  rt,
		stateDir: stateDir,
		role:     role,
	}, nil
}

// Invoker returns the underlying YAWR ToolInvoker.
func (m *Manager) Invoker() tool.ToolInvoker {
	return m.invoker
}

// Registry returns the registered YAWR tool definitions.
func (m *Manager) Registry() tool.ToolRegistry {
	return m.registry
}

// StateDir returns the durable dispatch directory.
func (m *Manager) StateDir() string {
	return m.stateDir
}

func buildRegistry() tool.ToolRegistry {
	readOnly := "read-only"
	mutating := "mutating"

	workspaceTool := tool.ToolDef{
		Name:      "workspace",
		Transport: tool.TransportNative,
		Actions: map[string]*tool.ToolAction{
			"read_file": {
				Classification: &readOnly,
				Description:    "Read an authorized file inside the task worktree",
				Args: map[string]*tool.ArgDef{
					"task_id": {Type: "string", Required: true, Description: "Task identifier"},
					"path":    {Type: "string", Required: true, Description: "Relative file path within worktree"},
				},
			},
			"apply_patch": {
				Classification: &mutating,
				Description:    "Apply a snapshot-bound patch strictly inside the task worktree",
				Args: map[string]*tool.ArgDef{
					"task_id":           {Type: "string", Required: true, Description: "Task identifier"},
					"patch":             {Type: "string", Required: true, Description: "Unified diff or patch text to apply"},
					"expected_snapshot": {Type: "string", Required: false, Description: "Expected git commit SHA prior to mutation"},
				},
			},
		},
	}

	gitTool := tool.ToolDef{
		Name:      "git",
		Transport: tool.TransportNative,
		Actions: map[string]*tool.ToolAction{
			"inspect": {
				Classification: &readOnly,
				Description:    "Inspect git diff, status, log, or commit SHA in the task worktree",
				Args: map[string]*tool.ArgDef{
					"task_id":    {Type: "string", Required: true, Description: "Task identifier"},
					"subcommand": {Type: "string", Required: true, Description: "Subcommand: diff, status, log, rev-parse"},
					"target":     {Type: "string", Required: false, Description: "Target commit, branch, or file path"},
				},
			},
		},
	}

	testTool := tool.ToolDef{
		Name:      "test",
		Transport: tool.TransportNative,
		Actions: map[string]*tool.ToolAction{
			"run": {
				Classification: &mutating,
				Description:    "Run bounded go test in an isolated disposable sandbox and collect structured validation evidence",
				Args: map[string]*tool.ArgDef{
					"task_id": {Type: "string", Required: true, Description: "Task identifier"},
					"package": {Type: "string", Required: false, Description: "Go package to test (default: ./...)"},
					"race":    {Type: "boolean", Required: false, Description: "Enable Go data race detector (default: true)"},
				},
			},
		},
	}

	return tool.NewRegistry(workspaceTool, gitTool, testTool)
}

// BuildAuthority constructs trusted InvocationAuthority based on server-side task facts.
func (m *Manager) BuildAuthority(ctx context.Context, taskID string) (tool.InvocationAuthority, error) {
	task, err := m.service.GetTask(ctx, taskID)
	if err != nil {
		return tool.InvocationAuthority{}, fmt.Errorf("governed: lookup task %s: %w", taskID, err)
	}

	wtPath, err := m.service.GetWorktreePath(taskID)
	if err != nil {
		return tool.InvocationAuthority{}, fmt.Errorf("governed: lookup worktree for %s: %w", taskID, err)
	}

	// Build map of activity statuses to evaluate dependencies
	activityStatus := make(map[string]a4c.ActivityStatus)
	for _, act := range task.Activities {
		activityStatus[act.ActivityID] = act.Status
	}

	activeAgent := "alice"
	// 1. Look for newest currently claimed activity
	for i := len(task.Activities) - 1; i >= 0; i-- {
		act := task.Activities[i]
		if act.Status == a4c.StatusClaimed {
			activeAgent = act.AgentID
			break
		}
	}

	// 2. If none claimed, look for newest pending activity whose dependencies are met
	if activeAgent == "alice" {
		for i := len(task.Activities) - 1; i >= 0; i-- {
			act := task.Activities[i]
			if act.Status == a4c.StatusPending {
				depsMet := true
				for _, depID := range act.DependsOn {
					if status, exists := activityStatus[depID]; !exists || status != a4c.StatusCompleted {
						depsMet = false
						break
					}
				}
				if depsMet {
					activeAgent = act.AgentID
					break
				}
			}
		}
	}

	if activeAgent == "alice" && len(task.Activities) > 0 {
		activeAgent = task.Activities[0].AgentID
	}

	isCoder := strings.EqualFold(activeAgent, "bob") || strings.EqualFold(activeAgent, "coder")
	isReviewer := strings.EqualFold(activeAgent, "charlie") || strings.EqualFold(activeAgent, "reviewer")

	isActiveState := task.State == a4c.TaskStateRunning ||
		task.State == a4c.TaskStateRevisionRequested ||
		task.State == a4c.TaskStateQueued

	isTerminalState := task.State == a4c.TaskStateAccepted ||
		task.State == a4c.TaskStateIntegrated ||
		task.State == a4c.TaskStateRejected

	allowRead := true
	allowMutating := false
	effectiveAgent := activeAgent

	if m.role != "" {
		if strings.EqualFold(m.role, "charlie") || strings.EqualFold(m.role, "reviewer") {
			effectiveAgent = "charlie"
			allowMutating = false // Reviewer is strictly non-mutating
		} else if strings.EqualFold(m.role, "alice") || strings.EqualFold(m.role, "lead") {
			effectiveAgent = "alice"
			allowMutating = false // Lead coordinates, does not mutate
		} else if strings.EqualFold(m.role, "bob") || strings.EqualFold(m.role, "coder") {
			effectiveAgent = "bob"
			if isCoder && isActiveState && !isTerminalState {
				allowMutating = true
			}
		}
	} else {
		if isReviewer || isTerminalState || task.State == a4c.TaskStateAwaitingHumanReview {
			effectiveAgent = "charlie"
			allowMutating = false
		} else if isCoder && isActiveState {
			effectiveAgent = "bob"
			allowMutating = true
		} else {
			effectiveAgent = activeAgent
			allowMutating = false
		}
	}

	actor := fmt.Sprintf("a4c:%s:%s", effectiveAgent, taskID)

	return tool.InvocationAuthority{
		Actor:         actor,
		AllowRead:     allowRead,
		AllowMutating: allowMutating,
		WorkspaceRoot: wtPath,
	}, nil
}

// ReadFile reads an authorized file within the task worktree.
func (m *Manager) ReadFile(ctx context.Context, taskID, relPath string) (*tool.InvocationResult, error) {
	auth, err := m.BuildAuthority(ctx, taskID)
	if err != nil {
		return nil, err
	}

	req := tool.InvocationRequest{
		Tool:   "workspace",
		Action: "read_file",
		Arguments: map[string]any{
			"task_id": taskID,
			"path":    relPath,
		},
		Authority: auth,
	}
	return m.invoker.Invoke(ctx, req)
}

// ApplyPatch applies a snapshot-bound patch in the task worktree.
func (m *Manager) ApplyPatch(ctx context.Context, taskID, patch, expectedSnapshot, idempotencyKey string) (*tool.InvocationResult, error) {
	auth, err := m.BuildAuthority(ctx, taskID)
	if err != nil {
		return nil, err
	}

	req := tool.InvocationRequest{
		Tool:   "workspace",
		Action: "apply_patch",
		Arguments: map[string]any{
			"task_id":           taskID,
			"patch":             patch,
			"expected_snapshot": expectedSnapshot,
		},
		Authority:      auth,
		IdempotencyKey: idempotencyKey,
	}
	return m.invoker.Invoke(ctx, req)
}

// GitInspect runs bounded git inspect queries in the task worktree.
func (m *Manager) GitInspect(ctx context.Context, taskID, subcommand, target string) (*tool.InvocationResult, error) {
	auth, err := m.BuildAuthority(ctx, taskID)
	if err != nil {
		return nil, err
	}

	req := tool.InvocationRequest{
		Tool:   "git",
		Action: "inspect",
		Arguments: map[string]any{
			"task_id":    taskID,
			"subcommand": subcommand,
			"target":     target,
		},
		Authority: auth,
	}
	return m.invoker.Invoke(ctx, req)
}

// RunTests executes bounded go test procedures in the task worktree.
func (m *Manager) RunTests(ctx context.Context, taskID, pkg string, race bool, idempotencyKey string) (*tool.InvocationResult, error) {
	auth, err := m.BuildAuthority(ctx, taskID)
	if err != nil {
		return nil, err
	}

	req := tool.InvocationRequest{
		Tool:   "test",
		Action: "run",
		Arguments: map[string]any{
			"task_id": taskID,
			"package": pkg,
			"race":    race,
		},
		Authority:      auth,
		IdempotencyKey: idempotencyKey,
	}
	return m.invoker.Invoke(ctx, req)
}

type governedRuntime struct {
	service a4c.TeamService
}

func (r *governedRuntime) Invoke(ctx context.Context, toolName, action string, args map[string]any) (*tool.ToolResult, error) {
	taskID, _ := args["task_id"].(string)
	if taskID == "" {
		return nil, errors.New("governed runtime: task_id is required")
	}

	wtPath, err := r.service.GetWorktreePath(taskID)
	if err != nil {
		return nil, fmt.Errorf("governed runtime: get worktree for task %s: %w", taskID, err)
	}
	if wtPath == "" {
		return nil, fmt.Errorf("governed runtime: no worktree allocated for task %s", taskID)
	}

	switch toolName {
	case "workspace":
		switch action {
		case "read_file":
			path, _ := args["path"].(string)
			return r.executeReadFile(wtPath, path)
		case "apply_patch":
			patch, _ := args["patch"].(string)
			expectedSnap, _ := args["expected_snapshot"].(string)
			return r.executeApplyPatch(ctx, taskID, wtPath, patch, expectedSnap)
		default:
			return nil, fmt.Errorf("unknown action %q for workspace tool", action)
		}

	case "git":
		switch action {
		case "inspect":
			subcmd, _ := args["subcommand"].(string)
			target, _ := args["target"].(string)
			return r.executeGitInspect(ctx, wtPath, subcmd, target)
		default:
			return nil, fmt.Errorf("unknown action %q for git tool", action)
		}

	case "test":
		switch action {
		case "run":
			pkg, _ := args["package"].(string)
			raceVal, _ := args["race"].(bool)
			return r.executeRunTests(ctx, wtPath, pkg, raceVal)
		default:
			return nil, fmt.Errorf("unknown action %q for test tool", action)
		}

	default:
		return nil, fmt.Errorf("unknown tool %q", toolName)
	}
}

func (r *governedRuntime) validatePathConfinement(worktreePath, relPath string) (string, error) {
	if strings.TrimSpace(relPath) == "" {
		return "", errors.New("path cannot be empty")
	}

	cleanRel := filepath.Clean(relPath)
	var fullPath string

	canonicalWorktree := worktreePath
	if realWt, err := filepath.EvalSymlinks(worktreePath); err == nil {
		canonicalWorktree = realWt
	}

	if filepath.IsAbs(cleanRel) {
		canonicalClean := cleanRel
		if realClean, err := filepath.EvalSymlinks(cleanRel); err == nil {
			canonicalClean = realClean
		}
		if !strings.HasPrefix(canonicalClean, canonicalWorktree) {
			return "", fmt.Errorf("%w: absolute path %q is outside worktree %q", ErrFilesystemEscape, cleanRel, worktreePath)
		}
		fullPath = cleanRel
	} else {
		if strings.HasPrefix(cleanRel, "..") || strings.Contains(cleanRel, "/../") {
			return "", fmt.Errorf("%w: directory traversal path %q is outside worktree", ErrFilesystemEscape, cleanRel)
		}
		fullPath = filepath.Join(worktreePath, cleanRel)
	}

	// Symlink escape check
	if realPath, err := filepath.EvalSymlinks(fullPath); err == nil {
		if !strings.HasPrefix(realPath, canonicalWorktree) {
			return "", fmt.Errorf("%w: symlink %q resolves outside worktree to %q", ErrFilesystemEscape, cleanRel, realPath)
		}
	}

	return fullPath, nil
}

func (r *governedRuntime) executeReadFile(worktreePath, relPath string) (*tool.ToolResult, error) {
	fullPath, err := r.validatePathConfinement(worktreePath, relPath)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(fullPath)
	if err != nil {
		return &tool.ToolResult{
			ExitCode: 1,
			Stderr:   err.Error(),
		}, fmt.Errorf("read file: %w", err)
	}

	return &tool.ToolResult{
		ExitCode: 0,
		Stdout:   string(data),
		Output: map[string]any{
			"bytes": len(data),
			"path":  relPath,
		},
	}, nil
}

func validatePatchConfinement(patch string) error {
	lines := strings.Split(patch, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				path := parts[1]
				path = strings.TrimPrefix(path, "a/")
				path = strings.TrimPrefix(path, "b/")
				if path == "/dev/null" {
					continue
				}
				if strings.Contains(path, "..") || filepath.IsAbs(path) {
					return fmt.Errorf("%w: patch targets escaping path %q", ErrFilesystemEscape, path)
				}
			}
		}
	}
	return nil
}

func (r *governedRuntime) executeApplyPatch(ctx context.Context, taskID, worktreePath, patch, expectedSnapshot string) (*tool.ToolResult, error) {
	if strings.TrimSpace(patch) == "" {
		return nil, errors.New("patch content cannot be empty")
	}

	if err := validatePatchConfinement(patch); err != nil {
		return &tool.ToolResult{
			ExitCode: 1,
			Stderr:   err.Error(),
		}, err
	}

	// Exclusive per-task lock to serialize mutations and prevent silent overwrites
	unlock, err := a4c.AcquireTaskMutationLock(r.service.WorkspaceDir(), taskID)
	if err != nil {
		return nil, fmt.Errorf("acquire task mutation lock: %w", err)
	}
	defer unlock()

	// 1. Snapshot Precondition Verification (strict content snapshot + git HEAD)
	strictSnap, snapErr := a4c.NewStrictWorkspaceSnapshotter().ComputeSnapshot(worktreePath)
	currentHead := ""
	cmdRev := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	cmdRev.Dir = worktreePath
	if out, err := cmdRev.CombinedOutput(); err == nil {
		currentHead = strings.TrimSpace(string(out))
	}

	if expectedSnapshot != "" {
		matchesStrict := (snapErr == nil && strictSnap == expectedSnapshot)
		matchesHead := (currentHead != "" && (strings.HasPrefix(currentHead, expectedSnapshot) || strings.HasPrefix(expectedSnapshot, currentHead)))
		if !matchesStrict && !matchesHead {
			return nil, fmt.Errorf("%w: expected %s, current tree is at %s (HEAD %s)", ErrStaleSnapshot, expectedSnapshot, strictSnap, currentHead)
		}
	}

	// 2. Write patch to temporary file
	tmpFile, err := os.CreateTemp("", "yawr-patch-*.diff")
	if err != nil {
		return nil, fmt.Errorf("create temp patch: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(patch); err != nil {
		_ = tmpFile.Close()
		return nil, fmt.Errorf("write temp patch: %w", err)
	}
	_ = tmpFile.Close()

	// 3. Dry-run verify patch applicability before modifying working tree
	cmdCheck := exec.CommandContext(ctx, "git", "apply", "--check", "--whitespace=nowarn", tmpFile.Name())
	cmdCheck.Dir = worktreePath
	if checkOut, checkErr := cmdCheck.CombinedOutput(); checkErr != nil {
		return &tool.ToolResult{
			ExitCode: 1,
			Stderr:   string(checkOut),
		}, fmt.Errorf("git apply --check failed: %s", string(checkOut))
	}

	// 4. Execute git apply strictly within worktree
	cmdApply := exec.CommandContext(ctx, "git", "apply", "--whitespace=nowarn", tmpFile.Name())
	cmdApply.Dir = worktreePath
	applyOut, applyErr := cmdApply.CombinedOutput()
	if applyErr != nil {
		return &tool.ToolResult{
			ExitCode: 1,
			Stderr:   string(applyOut),
		}, fmt.Errorf("git apply failed: %s", string(applyOut))
	}

	// 5. Capture git status of modified files and compute new snapshot ID
	cmdStatus := exec.CommandContext(ctx, "git", "status", "--porcelain")
	cmdStatus.Dir = worktreePath
	statusOut, _ := cmdStatus.CombinedOutput()
	newSnap, _ := a4c.NewStrictWorkspaceSnapshotter().ComputeSnapshot(worktreePath)

	// Synchronize CodeTopo index for worktree if worktree index exists
	wtDB := filepath.Join(worktreePath, ".codetopo", "index.sqlite")
	if _, statErr := os.Stat(wtDB); statErr == nil {
		if ctPath := a4c.FindCodeTopoCLI(); ctPath != "" {
			cmdSync := exec.CommandContext(ctx, ctPath, "worktree", "sync", "--root", worktreePath)
			_ = cmdSync.Run()
		}
	}

	return &tool.ToolResult{
		ExitCode: 0,
		Stdout:   fmt.Sprintf("Patch applied cleanly in worktree %s\n%s", worktreePath, string(statusOut)),
		Output: map[string]any{
			"applied":     true,
			"modified":    strings.TrimSpace(string(statusOut)),
			"snapshot_id": newSnap,
		},
	}, nil
}

func (r *governedRuntime) executeGitInspect(ctx context.Context, worktreePath, subcommand, target string) (*tool.ToolResult, error) {
	var cmdArgs []string

	switch strings.ToLower(strings.TrimSpace(subcommand)) {
	case "diff":
		cmdArgs = []string{"diff"}
		if target != "" {
			cmdArgs = append(cmdArgs, target)
		}
	case "status":
		cmdArgs = []string{"status", "--porcelain"}
	case "log":
		cmdArgs = []string{"log", "-n", "5", "--oneline"}
		if target != "" {
			cmdArgs = append(cmdArgs, target)
		}
	case "rev-parse":
		if target == "" {
			target = "HEAD"
		}
		cmdArgs = []string{"rev-parse", target}
	default:
		return nil, fmt.Errorf("git inspect: unsupported subcommand %q (supported: diff, status, log, rev-parse)", subcommand)
	}

	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	cmd.Dir = worktreePath
	out, err := cmd.CombinedOutput()
	if err != nil {
		return &tool.ToolResult{
			ExitCode: 1,
			Stderr:   string(out),
		}, fmt.Errorf("git %s failed: %w (%s)", subcommand, err, string(out))
	}

	return &tool.ToolResult{
		ExitCode: 0,
		Stdout:   string(out),
		Output: map[string]any{
			"subcommand": subcommand,
		},
	}, nil
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		name := info.Name()
		if info.IsDir() {
			if (name == ".git" || name == ".a4c") && path != src {
				return filepath.SkipDir
			}
			rel, err := filepath.Rel(src, path)
			if err != nil {
				return err
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0755)
		}
		if (name == ".git" || name == ".a4c") && path != src {
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.Mode()&os.ModeSymlink != 0 {
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(linkTarget, target)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

func (r *governedRuntime) executeRunTests(ctx context.Context, worktreePath, pkg string, race bool) (*tool.ToolResult, error) {
	if pkg == "" {
		pkg = "./..."
	}

	// 1. Snapshot worktree before test execution to verify immutability
	beforeSnap, err := a4c.NewStrictWorkspaceSnapshotter().ComputeSnapshot(worktreePath)
	if err != nil {
		beforeSnap = ""
	}

	// 2. Create isolated disposable sandbox
	sandboxDir, err := os.MkdirTemp("", "yawr-test-sandbox-*")
	if err != nil {
		return nil, fmt.Errorf("create test sandbox: %w", err)
	}
	defer os.RemoveAll(sandboxDir)

	if err := copyTree(worktreePath, sandboxDir); err != nil {
		return nil, fmt.Errorf("copy worktree to sandbox: %w", err)
	}

	// 3. Execute bounded go test in disposable sandbox
	cmdArgs := []string{"test", "-v"}
	if race {
		cmdArgs = append(cmdArgs, "-race")
	}
	cmdArgs = append(cmdArgs, pkg)

	startTime := time.Now()
	cmd := exec.CommandContext(ctx, "go", cmdArgs...)
	cmd.Dir = sandboxDir
	cmd.Env = append(os.Environ(), "GOWORK=off")

	out, err := cmd.CombinedOutput()
	durationMs := time.Since(startTime).Milliseconds()

	// 4. Verify invariant: original worktree was not modified by test execution
	if beforeSnap != "" {
		afterSnap, err := a4c.NewStrictWorkspaceSnapshotter().ComputeSnapshot(worktreePath)
		if err == nil && beforeSnap != afterSnap {
			return nil, fmt.Errorf("governance: test execution violated worktree invariant: worktree content modified (%s -> %s)", beforeSnap, afterSnap)
		}
	}

	exitCode := 0
	if err != nil {
		exitCode = 1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		return &tool.ToolResult{
			ExitCode: exitCode,
			Stdout:   string(out),
			Stderr:   string(out),
			Output: map[string]any{
				"package":     pkg,
				"race":        race,
				"passed":      false,
				"duration_ms": durationMs,
				"sandbox":     "isolated",
				"snapshot_id": beforeSnap,
			},
		}, fmt.Errorf("go test failed with exit code %d: %s", exitCode, string(out))
	}

	return &tool.ToolResult{
		ExitCode: 0,
		Stdout:   string(out),
		Output: map[string]any{
			"package":     pkg,
			"race":        race,
			"passed":      true,
			"duration_ms": durationMs,
			"sandbox":     "isolated",
			"snapshot_id": beforeSnap,
		},
	}, nil
}
