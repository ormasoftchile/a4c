package a4c

import (
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

// ListCapabilities reports discoverable capabilities across all registered hosts.
func (r *UniversalHostRegistry) ListCapabilities() []HostCapabilities {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []HostCapabilities
	for hostID, p := range r.providers {
		if hp, ok := p.(HostProvider); ok {
			result = append(result, hp.HostCapabilities())
		} else {
			rc := p.Capabilities()
			result = append(result, HostCapabilities{
				HostID:               hostID,
				DisplayName:          hostID,
				Available:            true,
				SessionInvocation:    true,
				WorktreeBinding:      true,
				ContextInjection:     true,
				CompletionCollection: true,
				SessionCancellation:  rc.SessionCancellation,
				SessionResumption:    rc.SessionResumption,
				StructuredTelemetry:  rc.UsageReporting,
				ToolRestrictions:     rc.NativeToolRestrictions,
			})
		}
	}
	return result
}

// GetHostCapability returns capabilities for a specific provider.
func GetHostCapability(p AgentProvider, fallbackID string) HostCapabilities {
	if hp, ok := p.(HostProvider); ok {
		return hp.HostCapabilities()
	}
	rc := p.Capabilities()
	return HostCapabilities{
		HostID:               fallbackID,
		DisplayName:          fallbackID,
		Available:            true,
		SessionInvocation:    true,
		WorktreeBinding:      true,
		ContextInjection:     true,
		CompletionCollection: true,
		SessionCancellation:  rc.SessionCancellation,
		SessionResumption:    rc.SessionResumption,
		StructuredTelemetry:  rc.UsageReporting,
		ToolRestrictions:     rc.NativeToolRestrictions,
		Limitations: []string{
			fmt.Sprintf("Provider %s does not implement detailed HostProvider interface", fallbackID),
		},
	}
}
