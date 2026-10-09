package a4c

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// WorkerHostConfig configures the durable worker host process.
type WorkerHostConfig struct {
	WorkspaceDir string
	PollInterval time.Duration
	MaxSteps     int
}

// WorkerHost manages durable execution and single-writer workspace ownership.
type WorkerHost struct {
	mu          sync.Mutex
	cfg         WorkerHostConfig
	coordinator *TeamCoordinator
	store       *DurableActivityStore
	journal     *DurableIntentJournal
	running     bool
	stopCh      chan struct{}
}

// NewWorkerHost constructs a WorkerHost over team coordinator and storage.
func NewWorkerHost(
	cfg WorkerHostConfig,
	coord *TeamCoordinator,
	store *DurableActivityStore,
	journal *DurableIntentJournal,
) *WorkerHost {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 250 * time.Millisecond
	}
	return &WorkerHost{
		cfg:         cfg,
		coordinator: coord,
		store:       store,
		journal:     journal,
		stopCh:      make(chan struct{}),
	}
}

// Run executes tasks until ctx is cancelled or maxSteps is reached (if MaxSteps > 0).
// Ensures single-writer lock acquisition, intent reconciliation on startup, and graceful lock release on exit.
func (h *WorkerHost) Run(ctx context.Context) error {
	// 1. Acquire exclusive workspace lock
	if err := h.coordinator.AcquireWorkspaceLock(); err != nil {
		return fmt.Errorf("worker_host: acquire workspace lock: %w", err)
	}
	defer h.coordinator.ReleaseWorkspaceLock()

	// 2. Crash recovery & intent reconciliation
	if h.journal != nil && h.coordinator.Reconciler != nil {
		_, _ = h.journal.ReconcileUnfinished(ctx, h.coordinator.Reconciler)
	}

	h.mu.Lock()
	h.running = true
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.running = false
		h.mu.Unlock()
	}()

	stepsExecuted := 0
	ticker := time.NewTicker(h.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-h.stopCh:
			return nil
		default:
		}

		if h.cfg.MaxSteps > 0 && stepsExecuted >= h.cfg.MaxSteps {
			return nil
		}

		if h.store.HasClaimable() {
			res, err := h.coordinator.Step(ctx)
			if err != nil {
				return fmt.Errorf("worker_host: execution step failed: %w", err)
			}
			if res != nil {
				stepsExecuted++
			}
		} else {
			if h.cfg.MaxSteps > 0 {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-h.stopCh:
				return nil
			case <-ticker.C:
			}
		}
	}
}

// Stop signals the host loop to exit cleanly.
func (h *WorkerHost) Stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.running {
		select {
		case <-h.stopCh:
		default:
			close(h.stopCh)
		}
	}
}

// IsRunning returns whether the host is currently executing.
func (h *WorkerHost) IsRunning() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.running
}
