package a4c

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// TeamRoster manages the persistent roster of AI engineering team members.
type TeamRoster struct {
	mu      sync.Mutex
	dir     string
	members map[string]*AgentIdentity
}

func NewTeamRoster(dir string) (*TeamRoster, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("roster: failed to create dir: %w", err)
	}

	roster := &TeamRoster{
		dir:     dir,
		members: make(map[string]*AgentIdentity),
	}

	if err := roster.load(); err != nil {
		return nil, fmt.Errorf("roster: failed to load members: %w", err)
	}

	return roster, nil
}

func (r *TeamRoster) load() error {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(r.dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var ident AgentIdentity
		if err := json.Unmarshal(data, &ident); err != nil {
			return err
		}
		r.members[ident.ID] = &ident
	}
	return nil
}

func (r *TeamRoster) persist(ident *AgentIdentity) error {
	path := filepath.Join(r.dir, fmt.Sprintf("%s.json", ident.ID))
	data, err := json.MarshalIndent(ident, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (r *TeamRoster) RegisterMember(ident AgentIdentity) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.persist(&ident); err != nil {
		return err
	}
	r.members[ident.ID] = &ident
	return nil
}

func (r *TeamRoster) GetMember(id string) (*AgentIdentity, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	ident, ok := r.members[id]
	if !ok {
		return nil, false
	}
	cp := *ident
	return &cp, true
}

func (r *TeamRoster) ListMembers() []*AgentIdentity {
	r.mu.Lock()
	defer r.mu.Unlock()

	var list []*AgentIdentity
	for _, ident := range r.members {
		cp := *ident
		list = append(list, &cp)
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].ID < list[j].ID
	})
	return list
}

// DefaultTeamRoster creates or loads a team roster initialized with the standard 3 engineering personas.
func DefaultTeamRoster(dir string) (*TeamRoster, error) {
	roster, err := NewTeamRoster(dir)
	if err != nil {
		return nil, err
	}

	// If empty, initialize default team personas
	if len(roster.members) == 0 {
		defaults := []AgentIdentity{
			{
				ID:   "lead",
				Name: "Alice (Lead Architect)",
				Role: "Lead Engineer & Architect",
				Charter: `You are Alice, the Lead Software Engineer.
Your responsibilities:
1. Deconstruct human feature requests and architectural tasks.
2. Inspect repository structure and code using governed_read_file.
3. Propose subtasks and delegate implementation to 'coder'.
4. Delegate verification and review to 'reviewer'.
5. Propose architectural conventions using governed_propose_memory.
6. Escalate ambiguous requirements or high-impact trade-offs to the human engineer via governed_request_human_decision.
You NEVER directly mutate code via governed_apply_patch.`,
				Specialties: []string{"architecture", "planning", "delegation", "triage"},
				AllowedTools: []string{
					"governed_read_file",
					"governed_propose_activity",
					"governed_request_human_decision",
					"governed_propose_memory",
				},
			},
			{
				ID:   "coder",
				Name: "Bob (Senior Programmer)",
				Role: "Senior Software Engineer",
				Charter: `You are Bob, a Senior Software Engineer.
Your responsibilities:
1. Read existing code and context using governed_read_file.
2. Apply minimal, clean, robust code modifications using governed_apply_patch.
3. Fix test failures deterministically.
4. Adhere strictly to verified team architectural memory.
You do NOT manage the team or issue final review approvals.`,
				Specialties: []string{"implementation", "refactoring", "bugfix", "go"},
				AllowedTools: []string{
					"governed_read_file",
					"governed_apply_patch",
					"governed_propose_memory",
				},
			},
			{
				ID:   "reviewer",
				Name: "Charlie (Quality Reviewer)",
				Role: "Code Reviewer & Quality Engineer",
				Charter: `You are Charlie, the Quality & Verification Reviewer.
Your responsibilities:
1. Review git diffs and code modifications produced by the coder.
2. Verify that changes fulfill the objective and satisfy project architectural constraints.
3. Validate that native tests passed with valid evidence.
4. Submit your review decision using governed_submit_review (Approved=true/false with feedback).
You NEVER mutate code files directly.`,
				Specialties: []string{"code_review", "security", "correctness", "performance"},
				AllowedTools: []string{
					"governed_read_file",
					"governed_submit_review",
				},
			},
		}

		for _, member := range defaults {
			if err := roster.RegisterMember(member); err != nil {
				return nil, err
			}
		}
	}

	return roster, nil
}
