package a4c

import (
	"errors"
	"fmt"
	"sync"
)

// HostCapabilities describes the technical and governance guarantees provided by a coding host.
// It explicitly separates mandatory capabilities from optional capabilities and records
// provider-specific limitations transparently.
type HostCapabilities struct {
	// Identity & Discovery
	HostID      string   `json:"host_id"`      // "copilot", "opencode", "antigravity", "fake"
	DisplayName string   `json:"display_name"` // e.g. "GitHub Copilot CLI", "OpenCode", "Antigravity"
	Version     string   `json:"version"`      // Host version string (e.g. "1.0.95", "1.18.35")
	Available   bool     `json:"available"`    // Whether host binary/server is installed and accessible

	// Mandatory Execution Capabilities (Required for A4C Host-Native execution)
	SessionInvocation    bool `json:"session_invocation"`    // Can launch native coding turns
	WorktreeBinding      bool `json:"worktree_binding"`      // Can bind execution to isolated worktree
	ContextInjection     bool `json:"context_injection"`     // Can inject A4C charter, memory, handoffs
	CompletionCollection bool `json:"completion_collection"` // Can collect responses, diffs, and evidence
	SessionCancellation  bool `json:"session_cancellation"`  // Can abort in-flight execution cleanly

	// Optional Capabilities (Discovered and adapted per host)
	ModelSelection        bool `json:"model_selection"`        // Supports specifying custom model per activity
	NativeSubagents       bool `json:"native_subagents"`       // Supports spawning native subagents
	SessionResumption     bool `json:"session_resumption"`     // Supports native session ID resumption
	StructuredTelemetry   bool `json:"structured_telemetry"`   // Exposes prompt, completion, cache, reasoning tokens
	ToolRestrictions      bool `json:"tool_restrictions"`      // Enforces read-only permissions for reviewers
	InteractiveAttachment bool `json:"interactive_attachment"` // Supports interactive human attachment

	// Models & Defaults
	DefaultModel    string   `json:"default_model"`
	SupportedModels []string `json:"supported_models,omitempty"`

	// Transparent Limitations
	Limitations []string `json:"limitations,omitempty"`
}

// HostProvider extends AgentProvider with discovery and capability reporting.
type HostProvider interface {
	AgentProvider
	HostCapabilities() HostCapabilities
}

// UniversalHostRegistry manages runtime provider registration and capability reporting.
type UniversalHostRegistry struct {
	mu        sync.RWMutex
	providers map[string]AgentProvider
}

// NewUniversalHostRegistry creates an empty host registry.
func NewUniversalHostRegistry() *UniversalHostRegistry {
	return &UniversalHostRegistry{
		providers: make(map[string]AgentProvider),
	}
}

// Register registers a provider by host identifier (e.g. "copilot", "opencode", "antigravity").
func (r *UniversalHostRegistry) Register(hostID string, p AgentProvider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[hostID] = p
}

// Get retrieves a provider by host ID.
func (r *UniversalHostRegistry) Get(hostID string) (AgentProvider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[hostID]
	return p, ok
}

// Sentinel errors for host capability validation
var (
	ErrUnsupportedCapability = errors.New("host does not support required capability")
	ErrHostUnavailable       = errors.New("host provider is unavailable")
)

// ListCapabilities reports discoverable capabilities across all registered hosts.
func (r *UniversalHostRegistry) ListCapabilities() []HostCapabilities {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []HostCapabilities
	for hostID, p := range r.providers {
		result = append(result, GetHostCapability(p, hostID))
	}
	return result
}

// GetHostCapability returns capabilities for a specific provider.
// If the provider is nil, reports Available: false.
// If the provider does not implement HostProvider, capabilities are marked unsupported
// rather than assuming unverified capabilities.
func GetHostCapability(p AgentProvider, fallbackID string) HostCapabilities {
	if p == nil {
		return HostCapabilities{
			HostID:      fallbackID,
			DisplayName: fallbackID,
			Available:   false,
			Limitations: []string{
				fmt.Sprintf("Provider %s is nil or uninitialized", fallbackID),
			},
		}
	}

	if hp, ok := p.(HostProvider); ok {
		caps := hp.HostCapabilities()
		if (caps.HostID == "" || caps.HostID == "fake") && fallbackID != "" {
			caps.HostID = fallbackID
		}
		return caps
	}

	rc := p.Capabilities()
	return HostCapabilities{
		HostID:               fallbackID,
		DisplayName:          fallbackID,
		Available:            false, // Cannot verify availability without HostProvider implementation
		SessionInvocation:    false,
		WorktreeBinding:      false,
		ContextInjection:     false,
		CompletionCollection: false,
		SessionCancellation:  rc.SessionCancellation,
		SessionResumption:    rc.SessionResumption,
		StructuredTelemetry:  rc.UsageReporting,
		ToolRestrictions:     rc.NativeToolRestrictions,
		Limitations: []string{
			fmt.Sprintf("Provider %s does not implement HostProvider interface; capabilities unverified", fallbackID),
		},
	}
}

// ValidateRequiredCapability verifies that a host is available and supports a specific capability.
func ValidateRequiredCapability(caps HostCapabilities, required string) error {
	if !caps.Available {
		return fmt.Errorf("%w: host %s is not available", ErrHostUnavailable, caps.HostID)
	}

	switch required {
	case "SessionInvocation":
		if !caps.SessionInvocation {
			return fmt.Errorf("%w: host %s does not support %s", ErrUnsupportedCapability, caps.HostID, required)
		}
	case "WorktreeBinding":
		if !caps.WorktreeBinding {
			return fmt.Errorf("%w: host %s does not support %s", ErrUnsupportedCapability, caps.HostID, required)
		}
	case "ContextInjection":
		if !caps.ContextInjection {
			return fmt.Errorf("%w: host %s does not support %s", ErrUnsupportedCapability, caps.HostID, required)
		}
	case "CompletionCollection":
		if !caps.CompletionCollection {
			return fmt.Errorf("%w: host %s does not support %s", ErrUnsupportedCapability, caps.HostID, required)
		}
	case "SessionCancellation":
		if !caps.SessionCancellation {
			return fmt.Errorf("%w: host %s does not support %s", ErrUnsupportedCapability, caps.HostID, required)
		}
	case "ModelSelection":
		if !caps.ModelSelection {
			return fmt.Errorf("%w: host %s does not support %s", ErrUnsupportedCapability, caps.HostID, required)
		}
	case "NativeSubagents":
		if !caps.NativeSubagents {
			return fmt.Errorf("%w: host %s does not support %s", ErrUnsupportedCapability, caps.HostID, required)
		}
	case "SessionResumption":
		if !caps.SessionResumption {
			return fmt.Errorf("%w: host %s does not support %s", ErrUnsupportedCapability, caps.HostID, required)
		}
	case "StructuredTelemetry":
		if !caps.StructuredTelemetry {
			return fmt.Errorf("%w: host %s does not support %s", ErrUnsupportedCapability, caps.HostID, required)
		}
	case "ToolRestrictions":
		if !caps.ToolRestrictions {
			return fmt.Errorf("%w: host %s does not support %s", ErrUnsupportedCapability, caps.HostID, required)
		}
	case "InteractiveAttachment":
		if !caps.InteractiveAttachment {
			return fmt.Errorf("%w: host %s does not support %s", ErrUnsupportedCapability, caps.HostID, required)
		}
	default:
		return fmt.Errorf("%w: unrecognized capability %q", ErrUnsupportedCapability, required)
	}
	return nil
}

// RequireExecutionSupport verifies all mandatory capabilities required for an A4C execution turn.
func RequireExecutionSupport(caps HostCapabilities) error {
	mandatory := []string{
		"SessionInvocation",
		"WorktreeBinding",
		"ContextInjection",
		"CompletionCollection",
		"SessionCancellation",
	}
	for _, req := range mandatory {
		if err := ValidateRequiredCapability(caps, req); err != nil {
			return err
		}
	}
	return nil
}

// RequireModelSelection verifies that a host supports custom model selection.
func RequireModelSelection(caps HostCapabilities) error {
	return ValidateRequiredCapability(caps, "ModelSelection")
}

// RequireToolRestrictions verifies that a host supports strict tool restriction enforcement.
func RequireToolRestrictions(caps HostCapabilities) error {
	return ValidateRequiredCapability(caps, "ToolRestrictions")
}

