package a4c

import ()

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
