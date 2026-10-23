package a4c

// Legacy record data. These types exist only so historical persisted records
// (Stage-era tasks with strategies, roles, and provider sessions) remain
// readable. Nothing in the simplified product creates or dispatches them.

// ExecutionStrategy is legacy record data. Direct is the only execution model;
// historical "reviewed" and "team" values describe retired records and are
// never reinterpreted as direct.
type ExecutionStrategy string

const (
	StrategyDirect   ExecutionStrategy = "direct"
	StrategyReviewed ExecutionStrategy = "reviewed" // retired: A4C-managed review
	StrategyTeam     ExecutionStrategy = "team"     // retired: A4C-managed team
)

// ProviderFailureKind is legacy record data describing why a historical
// provider-driven activity was interrupted. Provider-driven execution no
// longer exists; checkpoints still read the field.
type ProviderFailureKind string

const (
	ProviderFailureQuotaExhausted     ProviderFailureKind = "quota_exhausted"
	ProviderFailureRateLimited        ProviderFailureKind = "rate_limited"
	ProviderFailureAuthInvalid        ProviderFailureKind = "auth_invalid"
	ProviderFailureTemporaryOutage    ProviderFailureKind = "temporary_outage"
	ProviderFailureProcessInterrupted ProviderFailureKind = "process_interrupted"
	ProviderFailureUserCancelled      ProviderFailureKind = "user_cancelled"
)

// UsageInfo is legacy record data captured from historical provider sessions.
type UsageInfo struct {
	PromptTokens      int `json:"prompt_tokens"`
	CompletionTokens  int `json:"completion_tokens"`
	TotalTokens       int `json:"total_tokens"`
	PeakContextTokens int `json:"peak_context_tokens,omitempty"`
	OverrunTokens     int `json:"overrun_tokens,omitempty"`
	ReasoningTokens   int `json:"reasoning_tokens,omitempty"`
	CachedTokens      int `json:"cached_tokens,omitempty"`
	// BilledTokens counts budget-relevant consumption using the host's
	// usage semantics: uncached input + output (cache reads are re-served
	// context, not new consumption; PromptTokens keeps the total-input
	// figure for reporting). Providers that do not report cache detail
	// should leave this 0; consumers fall back to TotalTokens.
	BilledTokens     int     `json:"billed_tokens,omitempty"`
	ReportedCostUSD  float64 `json:"reported_cost_usd,omitempty"`
	EstimatedCostUSD float64 `json:"estimated_cost_usd"`
}
