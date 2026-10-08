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
	if s.canceled {
		s.mu.Unlock()
		return nil, ErrSessionCanceled
	}
	s.mu.Unlock()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	if s.sessionResponseFunc != nil {
		return s.sessionResponseFunc(ctx, s, prompt)
	}

	if s.responseFunc != nil {
		return s.responseFunc(ctx, prompt)
	}

	// Default response
	s.mu.Lock()
	s.usage.PromptTokens += 50
	s.usage.CompletionTokens += 100
	s.usage.TotalTokens += 150
	s.mu.Unlock()

	return &AgentMessage{
		Role:      "assistant",
		Content:   "Default simulated response",
		Timestamp: time.Now(),
	}, nil
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
