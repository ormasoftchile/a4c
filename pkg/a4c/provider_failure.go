package a4c

import (
	"errors"
	"fmt"
	"strings"
)

// ProviderFailureKind classifies host availability and infrastructure failures.
type ProviderFailureKind string

const (
	ProviderFailureQuotaExhausted     ProviderFailureKind = "quota_exhausted"
	ProviderFailureRateLimited        ProviderFailureKind = "rate_limited"
	ProviderFailureAuthInvalid        ProviderFailureKind = "auth_invalid"
	ProviderFailureTemporaryOutage    ProviderFailureKind = "temporary_outage"
	ProviderFailureProcessInterrupted ProviderFailureKind = "process_interrupted"
	ProviderFailureUserCancelled      ProviderFailureKind = "user_cancelled"
)

var (
	ErrQuotaExhausted     = errors.New("provider: usage quota exhausted")
	ErrRateLimited        = errors.New("provider: rate limit reached")
	ErrAuthInvalid        = errors.New("provider: authentication invalid or expired")
	ErrTemporaryOutage    = errors.New("provider: temporary service outage")
	ErrProcessInterrupted = errors.New("provider: process interrupted")
	ErrUserCancelled      = errors.New("provider: execution cancelled by user")
)

// ProviderError represents a typed infrastructure or availability failure from a coding host.
type ProviderError struct {
	Kind        ProviderFailureKind `json:"kind"`
	HostID      string              `json:"host_id"`
	Model       string              `json:"model,omitempty"`
	Message     string              `json:"message"`
	Retryable   bool                `json:"retryable"`
	OriginalErr error               `json:"-"`
}

func (e *ProviderError) Error() string {
	if e.OriginalErr != nil {
		return fmt.Sprintf("provider [%s] failure (%s): %s (orig: %v)", e.HostID, e.Kind, e.Message, e.OriginalErr)
	}
	return fmt.Sprintf("provider [%s] failure (%s): %s", e.HostID, e.Kind, e.Message)
}

func (e *ProviderError) Unwrap() error {
	return e.OriginalErr
}

func (e *ProviderError) Is(target error) bool {
	switch target {
	case ErrQuotaExhausted:
		return e.Kind == ProviderFailureQuotaExhausted
	case ErrRateLimited:
		return e.Kind == ProviderFailureRateLimited
	case ErrAuthInvalid:
		return e.Kind == ProviderFailureAuthInvalid
	case ErrTemporaryOutage:
		return e.Kind == ProviderFailureTemporaryOutage
	case ErrProcessInterrupted:
		return e.Kind == ProviderFailureProcessInterrupted
	case ErrUserCancelled:
		return e.Kind == ProviderFailureUserCancelled
	default:
		return false
	}
}

// AsProviderFailure unwraps and extracts a *ProviderError if present.
func AsProviderFailure(err error) (*ProviderError, bool) {
	if err == nil {
		return nil, false
	}
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe, true
	}
	return nil, false
}

// ClassifyProviderError inspects host response, error, and stderr to construct a typed ProviderError.
// It returns nil if the error is an ordinary engineering error (e.g. build failure, test failure, lint failure).
func ClassifyProviderError(hostID, model string, err error, stderr string) *ProviderError {
	if err == nil {
		return nil
	}
	if pe, ok := AsProviderFailure(err); ok {
		return pe
	}

	// Do not classify internal A4C limits or budget overruns as external provider quota
	if errors.Is(err, ErrBudgetExceeded) || errors.Is(err, ErrTurnLimitExceeded) || errors.Is(err, ErrToolCallLimitExceeded) {
		return nil
	}

	errStr := strings.ToLower(err.Error() + " " + stderr)

	// User cancellation
	if errors.Is(err, ErrSessionCanceled) || strings.Contains(errStr, "context canceled") {
		return &ProviderError{
			Kind:        ProviderFailureUserCancelled,
			HostID:      hostID,
			Model:       model,
			Message:     "session cancelled by user",
			Retryable:   false,
			OriginalErr: err,
		}
	}

	// Quota exhaustion (e.g. 5-hour window expired, monthly limit, credit balance)
	if strings.Contains(errStr, "quota") ||
		strings.Contains(errStr, "usage limit") ||
		strings.Contains(errStr, "usage window") ||
		strings.Contains(errStr, "out of quota") ||
		strings.Contains(errStr, "exhausted your") ||
		strings.Contains(errStr, "insufficient_quota") ||
		strings.Contains(errStr, "credit balance") ||
		strings.Contains(errStr, "billable limit") ||
		strings.Contains(errStr, "window expired") {
		return &ProviderError{
			Kind:        ProviderFailureQuotaExhausted,
			HostID:      hostID,
			Model:       model,
			Message:     "provider usage quota exhausted",
			Retryable:   false,
			OriginalErr: err,
		}
	}

	// Rate limiting
	if strings.Contains(errStr, "rate limit") ||
		strings.Contains(errStr, "too many requests") ||
		strings.Contains(errStr, "429") ||
		strings.Contains(errStr, "resource exhausted") {
		return &ProviderError{
			Kind:        ProviderFailureRateLimited,
			HostID:      hostID,
			Model:       model,
			Message:     "rate limit reached",
			Retryable:   true,
			OriginalErr: err,
		}
	}

	// Auth invalid / expired
	if strings.Contains(errStr, "unauthorized") ||
		strings.Contains(errStr, "unauthenticated") ||
		strings.Contains(errStr, "401") ||
		strings.Contains(errStr, "403") ||
		strings.Contains(errStr, "forbidden") ||
		strings.Contains(errStr, "token expired") ||
		strings.Contains(errStr, "auth expired") ||
		strings.Contains(errStr, "login required") ||
		strings.Contains(errStr, "invalid api key") ||
		strings.Contains(errStr, "invalid_api_key") ||
		strings.Contains(errStr, "bad credentials") {
		return &ProviderError{
			Kind:        ProviderFailureAuthInvalid,
			HostID:      hostID,
			Model:       model,
			Message:     "authentication unavailable or expired",
			Retryable:   false,
			OriginalErr: err,
		}
	}

	// Process interrupted / signal killed
	if strings.Contains(errStr, "signal: killed") ||
		strings.Contains(errStr, "signal: terminated") ||
		strings.Contains(errStr, "process killed") ||
		strings.Contains(errStr, "process terminated") ||
		strings.Contains(errStr, "broken pipe") {
		return &ProviderError{
			Kind:        ProviderFailureProcessInterrupted,
			HostID:      hostID,
			Model:       model,
			Message:     "provider process interrupted",
			Retryable:   true,
			OriginalErr: err,
		}
	}

	// Temporary outages / network unreachable
	if errors.Is(err, ErrProviderFailure) ||
		strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "service unavailable") ||
		strings.Contains(errStr, "503") ||
		strings.Contains(errStr, "502") ||
		strings.Contains(errStr, "504") ||
		strings.Contains(errStr, "bad gateway") ||
		strings.Contains(errStr, "network unreachable") ||
		strings.Contains(errStr, "remote service unavailable") {
		return &ProviderError{
			Kind:        ProviderFailureTemporaryOutage,
			HostID:      hostID,
			Model:       model,
			Message:     "temporary provider failure",
			Retryable:   true,
			OriginalErr: err,
		}
	}

	return nil
}
