package a4c

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// InitiativeService coordinates the end-to-end multi-stage project lifecycle.
type InitiativeService struct {
	workspaceDir string
	baseDir      string
	teamService  TeamService
	runner       *CopilotProcessRunner
}

// NewInitiativeService creates a new InitiativeService for the given workspace.
func NewInitiativeService(workspaceDir string, teamService TeamService, runner *CopilotProcessRunner) *InitiativeService {
	absWorkDir, err := filepath.Abs(workspaceDir)
	if err != nil {
		absWorkDir = workspaceDir
	}
	baseDir := filepath.Join(absWorkDir, ".a4c", "initiatives")
	return &InitiativeService{
		workspaceDir: absWorkDir,
		baseDir:      baseDir,
		teamService:  teamService,
		runner:       runner,
	}
}

// CreateInitiative initializes a new initiative record on disk.
func (s *InitiativeService) CreateInitiative(title, objective string) (*Initiative, error) {
	if strings.TrimSpace(title) == "" {
		return nil, errors.New("initiative title cannot be empty")
	}
	if strings.TrimSpace(objective) == "" {
		return nil, errors.New("initiative objective cannot be empty")
	}

	id := fmt.Sprintf("init-%d", time.Now().UnixNano())
	initDir := filepath.Join(s.baseDir, id)
	if err := os.MkdirAll(initDir, 0755); err != nil {
		return nil, fmt.Errorf("creating initiative directory: %w", err)
	}

	now := time.Now().UTC()
	init := &Initiative{
		ID:        id,
		Title:     title,
		Objective: objective,
		Status:    InitiativeStatusPlanning,
		CreatedAt: now,
		UpdatedAt: now,
		Stages:    []Stage{},
	}

	if err := s.SaveInitiative(init); err != nil {
		return nil, err
	}
	return init, nil
}

// GetInitiative reads and returns an initiative by ID.
func (s *InitiativeService) GetInitiative(id string) (*Initiative, error) {
	filePath := filepath.Join(s.baseDir, id, "initiative.json")
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("reading initiative %q: %w", id, err)
	}

	var init Initiative
	if err := json.Unmarshal(data, &init); err != nil {
		return nil, fmt.Errorf("parsing initiative %q: %w", id, err)
	}
	return &init, nil
}

// SaveInitiative persists an initiative to disk.
func (s *InitiativeService) SaveInitiative(init *Initiative) error {
	init.UpdatedAt = time.Now().UTC()
	initDir := filepath.Join(s.baseDir, init.ID)
	if err := os.MkdirAll(initDir, 0755); err != nil {
		return fmt.Errorf("ensuring initiative directory: %w", err)
	}

	data, err := json.MarshalIndent(init, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling initiative: %w", err)
	}

	filePath := filepath.Join(initDir, "initiative.json")
	if err := os.WriteFile(filePath, data, 0644); err != nil {
		return fmt.Errorf("writing initiative file: %w", err)
	}
	return nil
}

// ExecuteDiscovery prompts Squad to explore and challenge the objective, producing scope.md.
func (s *InitiativeService) ExecuteDiscovery(ctx context.Context, initID string) (*ProcessRunResult, error) {
	init, err := s.GetInitiative(initID)
	if err != nil {
		return nil, err
	}

	outputFile := filepath.Join(s.baseDir, initID, "scope.md")
	prompt := fmt.Sprintf(`Objective: %s
Task: Challenge this ask. Search the codebase. Define What, Why, and What-NOT.
List 3-5 critical clarifying questions for the human.
Write your analysis directly to: %s`, init.Objective, outputFile)

	req := ProcessRunRequest{
		Dir:           s.workspaceDir,
		Agent:         "squad",
		Prompt:        prompt,
		AllowAllTools: true,
		Prefix:        "[squad:discovery] ",
		Timeout:       5 * time.Minute,
		Stdout:        os.Stdout,
		Stderr:        os.Stderr,
	}

	res, err := s.runner.Run(ctx, req)
	if err != nil {
		return res, err
	}

	if err := s.reconcileWorktreeArtifact(initID, "scope.md"); err != nil {
		return res, err
	}
	return res, nil
}

// ExecutePlanning prompts Squad to generate architecture.md and dag.json from scope.md.
func (s *InitiativeService) ExecutePlanning(ctx context.Context, initID string) (*DependencyGraph, error) {
	init, err := s.GetInitiative(initID)
	if err != nil {
		return nil, err
	}

	dagFile := filepath.Join(s.baseDir, initID, "dag.json")
	_ = s.reconcileWorktreeArtifact(initID, "dag.json")
	if _, err := os.Stat(dagFile); err == nil {
		if dag, err := LoadDependencyGraph(dagFile, init.Title); err == nil {
			init.Stages = dag.Stages
			for i := range init.Stages {
				if init.Stages[i].Status == "" {
					init.Stages[i].Status = StageStatusPending
				}
			}
			init.Status = InitiativeStatusReady
			_ = s.SaveInitiative(init)
			return dag, nil
		}
	}

	if err := s.reconcileWorktreeArtifact(initID, "scope.md"); err != nil {
		return nil, fmt.Errorf("scope.md not found for initiative %s; run discovery first: %w", initID, err)
	}
	scopeFile := filepath.Join(s.baseDir, initID, "scope.md")

	prompt := fmt.Sprintf(`Read scope from: %s
Task: Design the technical architecture, testable acceptance criteria, and decompose into a stage DAG.
Write the stage DAG strictly matching this JSON schema directly to: %s
Schema:
{
  "initiative": "%s",
  "stages": [
    {
      "id": "stage-01",
      "title": "Stage Title",
      "dependencies": [],
      "spec": {
        "summary": "...",
        "what": ["..."],
        "what_not": ["..."]
      },
      "acceptance_criteria": ["..."]
    }
  ]
}`, scopeFile, dagFile, init.Title)

	req := ProcessRunRequest{
		Dir:           s.workspaceDir,
		Agent:         "squad",
		Prompt:        prompt,
		AllowAllTools: true,
		Prefix:        "[squad:planning] ",
		Timeout:       10 * time.Minute,
		Stdout:        os.Stdout,
		Stderr:        os.Stderr,
	}

	if _, err := s.runner.Run(ctx, req); err != nil {
		return nil, fmt.Errorf("planning run failed: %w", err)
	}

	if err := s.reconcileWorktreeArtifact(initID, "dag.json"); err != nil {
		return nil, err
	}

	dag, err := LoadDependencyGraph(dagFile, init.Title)
	if err != nil {
		return nil, fmt.Errorf("loading generated dag.json: %w", err)
	}

	init.Stages = dag.Stages
	for i := range init.Stages {
		init.Stages[i].Status = StageStatusPending
	}
	init.Status = InitiativeStatusReady

	if err := s.SaveInitiative(init); err != nil {
		return nil, err
	}

	return dag, nil
}

func (s *InitiativeService) absPath(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(s.workspaceDir, p)
}

func (s *InitiativeService) reconcileWorktreeArtifact(initID, filename string) error {
	canonicalFile := filepath.Join(s.baseDir, initID, filename)
	if info, err := os.Stat(canonicalFile); err == nil && info.Size() > 0 {
		return nil
	}

	worktreesDir := filepath.Join(s.workspaceDir, ".a4c", "worktrees")
	entries, err := os.ReadDir(worktreesDir)
	if err != nil {
		return fmt.Errorf("canonical file %q not found and could not read worktrees dir: %w", canonicalFile, err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(worktreesDir, entry.Name(), "tree", ".a4c", "initiatives", initID, filename)
		if data, err := os.ReadFile(candidate); err == nil && len(data) > 0 {
			_ = os.MkdirAll(filepath.Dir(canonicalFile), 0755)
			if err := os.WriteFile(canonicalFile, data, 0644); err == nil {
				return nil
			}
		}
	}
	return fmt.Errorf("expected artifact %q was not found in base checkout or any worktree", filename)
}

// StartStage allocates an isolated worktree for the stage after verifying that all dependencies are integrated.
func (s *InitiativeService) StartStage(ctx context.Context, initID, stageID string) (*Stage, error) {
	init, err := s.GetInitiative(initID)
	if err != nil {
		return nil, err
	}

	stageIndex := -1
	stageMap := make(map[string]Stage, len(init.Stages))
	for i, st := range init.Stages {
		stageMap[st.ID] = st
		if st.ID == stageID {
			stageIndex = i
		}
	}
	if stageIndex == -1 {
		return nil, fmt.Errorf("stage %q not found in initiative %q", stageID, initID)
	}

	currentStage := init.Stages[stageIndex]

	// Serialization and dependency validation: All dependencies must be in StageStatusIntegrated
	for _, depID := range currentStage.Dependencies {
		dep, exists := stageMap[depID]
		if !exists {
			return nil, fmt.Errorf("dependency %q does not exist", depID)
		}
		if dep.Status != StageStatusIntegrated {
			return nil, fmt.Errorf("cannot start stage %q: dependency %q is in state %q (must be integrated first)", stageID, depID, dep.Status)
		}
	}

	// Base checkout hygiene assertion: git status must be clean
	cmd := exec.CommandContext(ctx, "git", "-C", s.workspaceDir, "status", "--porcelain")
	statusOut, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("checking git status: %w", err)
	}
	if strings.TrimSpace(string(statusOut)) != "" {
		return nil, errors.New("base workspace checkout is dirty; commit or stash changes before starting a stage")
	}

	// Begin Task through A4C TeamService to allocate worktree
	taskObjective := fmt.Sprintf("[%s / %s] %s: %s", init.Title, currentStage.ID, currentStage.Title, currentStage.Spec.Summary)
	beginRes, err := s.teamService.BeginTask(ctx, BeginTaskRequest{
		Objective:    taskObjective,
		BudgetTokens: 100000.0,
	})
	if err != nil {
		return nil, fmt.Errorf("allocating worktree via teamService: %w", err)
	}

	init.Stages[stageIndex].Status = StageStatusRunning
	init.Stages[stageIndex].TaskID = beginRes.TaskID
	init.Stages[stageIndex].WorktreePath = s.absPath(beginRes.WorktreePath)
	init.Status = InitiativeStatusInProgress

	if err := s.SaveInitiative(init); err != nil {
		return nil, err
	}

	return &init.Stages[stageIndex], nil
}

// ExecuteStageReview runs Squad Reviewer inside the worktree against the stage's acceptance criteria.
func (s *InitiativeService) ExecuteStageReview(ctx context.Context, initID, stageID string) (*StageReview, error) {
	init, err := s.GetInitiative(initID)
	if err != nil {
		return nil, err
	}

	var stage *Stage
	var stageIndex int
	for i := range init.Stages {
		if init.Stages[i].ID == stageID {
			stage = &init.Stages[i]
			stageIndex = i
			break
		}
	}
	if stage == nil {
		return nil, fmt.Errorf("stage %q not found in initiative %q", stageID, initID)
	}

	wtPath := s.absPath(stage.WorktreePath)
	if wtPath == "" {
		return nil, fmt.Errorf("stage %q has no allocated worktree", stageID)
	}

	reviewFile := filepath.Join(wtPath, "review.json")
	criteriaJSON, err := json.Marshal(stage.AcceptanceCriteria)
	if err != nil {
		return nil, fmt.Errorf("marshaling acceptance criteria: %w", err)
	}

	prompt := fmt.Sprintf(`You are the Code Reviewer.
Inspect the git changes in this worktree strictly against these acceptance criteria:
%s

Do NOT invent new requirements or stylistic nitpicks outside the criteria.
Write review.json strictly matching: {"stage_id": "%s", "verdict": "PASS"|"FAIL", "violations": [...], "summary": "..."}`,
		string(criteriaJSON), stageID)

	req := ProcessRunRequest{
		Dir:                wtPath,
		Agent:              "squad",
		Prompt:             prompt,
		AllowAllTools:      true,
		Prefix:             fmt.Sprintf("[review:%s] ", stageID),
		ExpectedOutputFile: reviewFile,
		Timeout:            5 * time.Minute,
		Stdout:             os.Stdout,
		Stderr:             os.Stderr,
	}

	if _, err := s.runner.Run(ctx, req); err != nil {
		return nil, fmt.Errorf("reviewer run failed: %w", err)
	}

	data, err := os.ReadFile(reviewFile)
	if err != nil {
		return nil, fmt.Errorf("reading review.json: %w", err)
	}

	var rev StageReview
	if err := json.Unmarshal(data, &rev); err != nil {
		return nil, fmt.Errorf("parsing review.json: %w", err)
	}
	rev.ReviewedAt = time.Now().UTC()

	if rev.Verdict == "PASS" {
		init.Stages[stageIndex].Status = StageStatusVerified
	} else {
		init.Stages[stageIndex].Status = StageStatusFailed
	}

	if err := s.SaveInitiative(init); err != nil {
		return nil, err
	}

	return &rev, nil
}

// IntegrateStage completes the stage task and lands it on main, unblocking dependent stages.
func (s *InitiativeService) IntegrateStage(ctx context.Context, initID, stageID, acceptedBy, token string, isInteractive bool) error {
	init, err := s.GetInitiative(initID)
	if err != nil {
		return err
	}

	var stage *Stage
	var stageIndex int
	for i := range init.Stages {
		if init.Stages[i].ID == stageID {
			stage = &init.Stages[i]
			stageIndex = i
			break
		}
	}
	if stage == nil {
		return fmt.Errorf("stage %q not found in initiative %q", stageID, initID)
	}

	if stage.Status != StageStatusVerified {
		return fmt.Errorf("cannot integrate stage %q in status %q (must be %q)", stageID, stage.Status, StageStatusVerified)
	}

	// 1. Complete Activity with Review Evidence
	_, err = s.teamService.CompleteActivity(ctx, CompleteActivityRequest{
		ActivityID: stage.TaskID,
		Success:    true,
		Summary:    fmt.Sprintf("Stage %s (%s) verified by Squad review and test criteria", stage.ID, stage.Title),
	})
	if err != nil {
		return fmt.Errorf("completing activity %s: %w", stage.TaskID, err)
	}

	// 2. Human Acceptance
	_, err = s.teamService.AcceptTask(ctx, AcceptTaskRequest{
		TaskID:            stage.TaskID,
		Comments:          fmt.Sprintf("Initiative %s / Stage %s approved for integration", initID, stageID),
		AcceptedBy:        acceptedBy,
		ConfirmationToken: token,
		IsInteractive:     isInteractive,
	})
	if err != nil {
		return fmt.Errorf("accepting task %s: %w", stage.TaskID, err)
	}

	// 3. Safe Integration onto main
	_, err = s.teamService.IntegrateTask(ctx, IntegrateTaskRequest{
		TaskID: stage.TaskID,
	})
	if err != nil {
		return fmt.Errorf("integrating task %s: %w", stage.TaskID, err)
	}

	init.Stages[stageIndex].Status = StageStatusIntegrated

	// Check if all stages in the initiative are completed
	allDone := true
	for _, st := range init.Stages {
		if st.Status != StageStatusIntegrated {
			allDone = false
			break
		}
	}
	if allDone {
		init.Status = InitiativeStatusCompleted
	}

	return s.SaveInitiative(init)
}
