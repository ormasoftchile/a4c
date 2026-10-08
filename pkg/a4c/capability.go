package a4c

import (
	"fmt"
	"strings"
	"sync"
)

// Standard Capability Profile Identifiers
const (
	ProfileArchitectureInvestigation = "profile-architecture-investigation"
	ProfileCodeImplementation        = "profile-code-implementation"
	ProfileIndependentReview         = "profile-independent-review"
	ProfileHumanEscalation           = "profile-human-escalation"
)

// DefaultCapabilityRegistry provides standard and extensible capability profiles.
type CapabilityRegistry struct {
	mu       sync.RWMutex
	profiles map[string]CapabilityProfile
}

// NewCapabilityRegistry creates a registry pre-loaded with standard engineering capability profiles.
func NewCapabilityRegistry() *CapabilityRegistry {
	reg := &CapabilityRegistry{
		profiles: make(map[string]CapabilityProfile),
	}

	// 1. Architecture Investigation (Lead / Explorer)
	reg.RegisterProfile(CapabilityProfile{
		ID:          ProfileArchitectureInvestigation,
		Name:        "Architecture Investigation",
		Description: "Read-only structural symbol search, caller analysis, file inspection, and activity delegation.",
		AllowedTools: []string{
			"governed_read_file",
			"governed_propose_activity",
			"governed_request_human_decision",
			"governed_propose_memory",
			"governed_structural_search_symbols",
			"governed_structural_context_for",
			"governed_structural_callers",
		},
	})

	// 2. Code Implementation (Programmer / Coder)
	reg.RegisterProfile(CapabilityProfile{
		ID:          ProfileCodeImplementation,
		Name:        "Code Implementation",
		Description: "Targeted file inspection, governed file patching, and memory proposals. No structural search or unmediated git.",
		AllowedTools: []string{
			"governed_read_file",
			"governed_apply_patch",
			"governed_request_human_decision",
			"governed_propose_memory",
		},
	})

	// 3. Independent Review (Reviewer / QA)
	reg.RegisterProfile(CapabilityProfile{
		ID:          ProfileIndependentReview,
		Name:        "Independent Review",
		Description: "Read-only inspection of diffs and evidence, and review submission. Mutation strictly prohibited.",
		AllowedTools: []string{
			"governed_read_file",
			"governed_submit_review",
			"governed_request_human_decision",
		},
	})

	// 4. Human Escalation
	reg.RegisterProfile(CapabilityProfile{
		ID:          ProfileHumanEscalation,
		Name:        "Human Escalation",
		Description: "Decision submission and guidance requests to the human software engineer.",
		AllowedTools: []string{
			"governed_request_human_decision",
		},
	})

	return reg
}

// RegisterProfile stores a capability profile in the registry.
func (r *CapabilityRegistry) RegisterProfile(p CapabilityProfile) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.profiles[p.ID] = p.Clone()
}

// GetProfile retrieves a profile by ID.
func (r *CapabilityRegistry) GetProfile(id string) (CapabilityProfile, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.profiles[id]
	if !ok {
		return CapabilityProfile{}, false
	}
	return p.Clone(), true
}

// ListProfiles returns all registered profiles.
func (r *CapabilityRegistry) ListProfiles() []CapabilityProfile {
	r.mu.RLock()
	defer r.mu.RUnlock()
	list := make([]CapabilityProfile, 0, len(r.profiles))
	for _, p := range r.profiles {
		list = append(list, p.Clone())
	}
	return list
}

// ResolveProfile dynamically derives the appropriate capability profile for an activity,
// enforcing that YAWR and PolicyGovernor remain the ultimate authority.
func (r *CapabilityRegistry) ResolveProfile(
	agentID string,
	role string,
	objective string,
	requested *CapabilityProfile,
	gov *PolicyGovernor,
) CapabilityProfile {
	var base CapabilityProfile

	if requested != nil && requested.ID != "" {
		if registered, found := r.GetProfile(requested.ID); found {
			base = registered
		} else {
			base = requested.Clone()
		}
	} else {
		// Derive from agentID, role, and objective
		lowerRole := strings.ToLower(role)
		lowerAgent := strings.ToLower(agentID)
		lowerObj := strings.ToLower(objective)

		switch {
		case lowerAgent == "lead" || strings.Contains(lowerRole, "architect") || strings.Contains(lowerRole, "lead"):
			base, _ = r.GetProfile(ProfileArchitectureInvestigation)

		case lowerAgent == "reviewer" || strings.Contains(lowerRole, "reviewer") || strings.Contains(lowerRole, "qa"):
			base, _ = r.GetProfile(ProfileIndependentReview)

		case lowerAgent == "coder" || strings.Contains(lowerRole, "programmer") || strings.Contains(lowerRole, "engineer"):
			base, _ = r.GetProfile(ProfileCodeImplementation)

		case strings.Contains(lowerObj, "review") || strings.Contains(lowerObj, "verify"):
			base, _ = r.GetProfile(ProfileIndependentReview)

		case strings.Contains(lowerObj, "escalate") || strings.Contains(lowerObj, "decision"):
			base, _ = r.GetProfile(ProfileHumanEscalation)

		default:
			base, _ = r.GetProfile(ProfileCodeImplementation)
		}
	}

	// Filter allowed tools strictly through YAWR / Governor authority
	if gov != nil {
		var authorizedTools []string
		for _, tool := range base.AllowedTools {
			if err := gov.AuthorizeToolInvocation(agentID, tool); err == nil {
				authorizedTools = append(authorizedTools, tool)
			}
		}
		base.AllowedTools = authorizedTools
	}

	return base
}

// FormatProfile returns a concise string representation of a profile for diagnostics.
func (p CapabilityProfile) Format() string {
	return fmt.Sprintf("[%s] %s (Tools: %s)", p.ID, p.Name, strings.Join(p.AllowedTools, ", "))
}
