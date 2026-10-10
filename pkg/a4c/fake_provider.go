package a4c

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrProviderFailure = errors.New("provider: remote service unavailable")
	ErrMalformedOutput = errors.New("provider: malformed or empty model response")
	ErrSessionCanceled = errors.New("provider: session canceled")
)

type FakeResponseFunc func(ctx context.Context, prompt string) (*AgentMessage, error)
type FakeSessionResponseFunc func(ctx context.Context, session *FakeSession, prompt string) (*AgentMessage, error)

type FakeSession struct {
	mu                  sync.Mutex
	sessionID           string
	config              AgentConfig
	usage               UsageInfo
	canceled            bool
	responseFunc        FakeResponseFunc
	sessionResponseFunc FakeSessionResponseFunc
	modelDuration       time.Duration
	toolDuration        time.Duration
	turnCount           int
	closed              bool
}

func (s *FakeSession) SessionID() string {
	return s.sessionID
}

func (s *FakeSession) Config() AgentConfig {
	return s.config
}

func (s *FakeSession) Send(ctx context.Context, prompt string) (*AgentMessage, error) {
	s.mu.Lock()
	s.turnCount++
	if s.config.Limits != nil && s.config.Limits.MaxModelTurns > 0 && s.turnCount > s.config.Limits.MaxModelTurns {
		turns := s.turnCount
		maxTurns := s.config.Limits.MaxModelTurns
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: turn %d exceeds max model turns %d", ErrTurnLimitExceeded, turns, maxTurns)
	}
	if s.canceled {
		s.mu.Unlock()
		return nil, ErrSessionCanceled
	}
	if s.config.Budget.Allocated > 0 && float64(s.usage.TotalTokens) >= s.config.Budget.Allocated {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: fake session budget exhausted before send", ErrBudgetExceeded)
	}
	s.mu.Unlock()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	var msg *AgentMessage
	var err error
	if s.sessionResponseFunc != nil {
		msg, err = s.sessionResponseFunc(ctx, s, prompt)
	} else if s.responseFunc != nil {
		msg, err = s.responseFunc(ctx, prompt)
	} else {
		s.mu.Lock()
		s.usage.PromptTokens += 50
		s.usage.CompletionTokens += 100
		s.usage.TotalTokens += 150
		s.mu.Unlock()
		msg = &AgentMessage{
			Role:      "assistant",
			Content:   "Default simulated response",
			Timestamp: time.Now(),
		}
	}

	if err != nil {
		return msg, err
	}

	s.mu.Lock()
	total := s.usage.TotalTokens
	alloc := s.config.Budget.Allocated
	s.mu.Unlock()

	if alloc > 0 && float64(total) > alloc {
		return msg, fmt.Errorf("%w: fake session consumed %d tokens exceeds allocated %.0f", ErrBudgetExceeded, total, alloc)
	}

	return msg, nil
}

func (s *FakeSession) Cancel() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.canceled = true
	return nil
}

func (s *FakeSession) Usage() UsageInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage
}

func (s *FakeSession) SetUsage(usage UsageInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage = usage
}

func (s *FakeSession) Durations() (time.Duration, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.modelDuration, s.toolDuration
}

func (s *FakeSession) SetDurations(model, tool time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.modelDuration = model
	s.toolDuration = tool
}

func (s *FakeSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

type FakeProvider struct {
	mu                  sync.Mutex
	sessions            map[string]*FakeSession
	nextID              int
	ResponseFunc        FakeResponseFunc
	SessionResponseFunc FakeSessionResponseFunc
	FailNext            bool
}

func NewFakeProvider() *FakeProvider {
	return &FakeProvider{
		sessions: make(map[string]*FakeSession),
	}
}

func (p *FakeProvider) Capabilities() RuntimeCapabilities {
	return RuntimeCapabilities{
		GovernedCustomTools:    true,
		GovernedProposalMode:   true,
		NativeToolRestrictions: true,
		SessionResumption:      true,
		SessionCancellation:    true,
		UsageReporting:         true,
		ProviderTokenLimits:    true,
	}
}

func (p *FakeProvider) SetSessionResponseFunc(fn FakeSessionResponseFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.SessionResponseFunc = fn
}

func (p *FakeProvider) SetResponseFunc(fn FakeResponseFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ResponseFunc = fn
}

func (p *FakeProvider) CreateSession(ctx context.Context, config AgentConfig) (AgentSession, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.FailNext {
		p.FailNext = false
		return nil, ErrProviderFailure
	}

	p.nextID++
	id := fmt.Sprintf("fake-session-%d", p.nextID)
	session := &FakeSession{
		sessionID:           id,
		config:              config,
		responseFunc:        p.ResponseFunc,
		sessionResponseFunc: p.SessionResponseFunc,
	}
	p.sessions[id] = session
	return session, nil
}

func (p *FakeProvider) ResumeSession(ctx context.Context, sessionID string, config AgentConfig) (AgentSession, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	session, exists := p.sessions[sessionID]
	if !exists {
		// In a restart, re-create session state with the existing ID
		session = &FakeSession{
			sessionID:           sessionID,
			config:              config,
			responseFunc:        p.ResponseFunc,
			sessionResponseFunc: p.SessionResponseFunc,
		}
		p.sessions[sessionID] = session
	}
	return session, nil
}

func (p *FakeProvider) HostCapabilities() HostCapabilities {
	return HostCapabilities{
		HostID:               "fake",
		DisplayName:          "In-Memory Test Simulator",
		Version:              "test",
		Available:            true,
		SessionInvocation:    true,
		WorktreeBinding:      true,
		ContextInjection:     true,
		CompletionCollection: true,
		SessionCancellation:  true,
		ModelSelection:       true,
		NativeSubagents:      false,
		SessionResumption:    true,
		StructuredTelemetry:  true,
		ToolRestrictions:     true,
		DefaultModel:         "simulated",
		Limitations: []string{
			"Simulated in-memory provider for unit tests and local benchmarks",
		},
	}
}
