package reconciliation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/kimdre/doco-cd/internal/config"
)

const (
	cleanupRetryInitialDelay = 30 * time.Second
	cleanupRetryMaxDelay     = 5 * time.Minute
	cleanupRetryTimeout      = 2 * time.Minute
)

type cleanupRetry struct {
	request DeployRequest
	next    time.Time
	delay   time.Duration
	cancel  context.CancelFunc
}

type cleanupRetryState struct {
	mu      sync.Mutex
	pending map[string]*cleanupRetry
	// Keep only generation watermarks after completion, so a late initial
	// attempt cannot recreate work already superseded by a newer request.
	generations map[string]uint64
	wake        chan struct{}
	done        chan struct{}
	cancel      context.CancelFunc
	stopped     bool
}

func cleanupRetryKey(req DeployRequest) string {
	reference := strings.TrimPrefix(strings.TrimSpace(req.Repository.ResolvedReference), "refs/heads/")
	if req.Repository.Source == config.SourceTypeOCI {
		sourceURL := strings.TrimSpace(req.Repository.ConfigSourceUrl)
		if sourceURL == "" {
			sourceURL = strings.TrimSpace(req.Repository.SourceUrl)
		}

		reference = "oci:" + strings.TrimPrefix(sourceURL, "oci://")
	}

	return req.key() + "\x00" + reference
}

func (m *Manager) cancelCleanupRetry(req DeployRequest) {
	if req.pauseGeneration == 0 {
		return
	}

	r := &m.cleanupRetries
	r.mu.Lock()
	defer r.mu.Unlock()

	key := cleanupRetryKey(req)
	if r.stopped || r.generations[key] > req.pauseGeneration {
		return
	}

	if r.generations == nil {
		r.generations = make(map[string]uint64)
	}

	r.generations[key] = req.pauseGeneration
	if entry := r.pending[key]; entry != nil {
		if entry.cancel != nil {
			entry.cancel()
		}

		delete(r.pending, key)
		r.notifyLocked()
	}
}

func (m *Manager) scheduleCleanupRetry(ctx context.Context, req DeployRequest, pending bool) {
	if req.pauseGeneration == 0 {
		return
	}

	pending = pending && req.RefreshCleanup != nil && req.TestName == ""

	r := &m.cleanupRetries
	r.mu.Lock()
	defer r.mu.Unlock()

	key := cleanupRetryKey(req)
	if r.stopped || r.generations[key] > req.pauseGeneration {
		return
	}

	if r.generations == nil {
		r.generations = make(map[string]uint64)
	}

	r.generations[key] = req.pauseGeneration
	if old := r.pending[key]; old != nil {
		if pending && old.request.pauseGeneration == req.pauseGeneration {
			return
		}

		if old.cancel != nil {
			old.cancel()
		}
	}

	delete(r.pending, key)

	if !pending {
		r.notifyLocked()

		return
	}

	if r.pending == nil {
		r.pending = make(map[string]*cleanupRetry)
	}

	r.pending[key] = &cleanupRetry{
		request: req,
		next:    time.Now().Add(cleanupRetryInitialDelay),
		delay:   cleanupRetryInitialDelay,
	}

	if r.cancel == nil {
		ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		r.cancel = cancel
		r.wake = make(chan struct{}, 1)
		r.done = make(chan struct{})

		go m.runCleanupRetries(ctx, m.cleanupObsolete)
	}

	r.notifyLocked()
}

func (r *cleanupRetryState) notifyLocked() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) stopCleanupRetries() {
	r := &m.cleanupRetries
	r.mu.Lock()

	r.stopped = true
	if r.cancel != nil {
		r.cancel()
	}

	for _, entry := range r.pending {
		if entry.cancel != nil {
			entry.cancel()
		}
	}

	r.pending = nil
	r.generations = nil
	done := r.done
	r.mu.Unlock()

	if done != nil {
		<-done
	}
}

func (m *Manager) runCleanupRetries(ctx context.Context,
	cleanup func(context.Context, DeployRequest, *syncWindowGate) (bool, error),
) {
	defer close(m.cleanupRetries.done)

	timer := time.NewTimer(0)
	defer timer.Stop()

	for ctx.Err() == nil {
		if m.runDueCleanupRetry(ctx, time.Now(), cleanup) {
			continue
		}

		var next time.Time

		r := &m.cleanupRetries
		r.mu.Lock()
		for _, entry := range r.pending {
			if entry.cancel == nil && (next.IsZero() || entry.next.Before(next)) {
				next = entry.next
			}
		}
		r.mu.Unlock()

		var tick <-chan time.Time

		if !next.IsZero() {
			timer.Reset(max(time.Until(next), 0))
			tick = timer.C
		}

		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		case <-tick:
		}
	}
}

// runDueCleanupRetry performs at most one due attempt. Passing its time and
// cleanup operation explicitly lets tests exercise retries without sleeping.
func (m *Manager) runDueCleanupRetry(ctx context.Context, now time.Time,
	cleanup func(context.Context, DeployRequest, *syncWindowGate) (bool, error),
) bool {
	r := &m.cleanupRetries
	r.mu.Lock()
	if r.stopped || ctx.Err() != nil {
		r.mu.Unlock()

		return false
	}

	var (
		key   string
		entry *cleanupRetry
	)

	for candidateKey, candidate := range r.pending {
		if candidate.cancel == nil && !candidate.next.After(now) &&
			(entry == nil || candidate.next.Before(entry.next)) {
			key, entry = candidateKey, candidate
		}
	}

	if entry == nil {
		r.mu.Unlock()

		return false
	}

	attemptCtx, cancel := context.WithTimeout(ctx, cleanupRetryTimeout)
	entry.cancel = cancel
	req := entry.request
	r.mu.Unlock()

	pending, err := m.refreshCleanupRetry(attemptCtx, key, entry, req, cleanup)
	if err != nil && ctx.Err() == nil && !errors.Is(attemptCtx.Err(), context.Canceled) {
		req.Logger.Error("failed to retry obsolete-stack cleanup", slog.String("error", err.Error()))
	}

	cancel()
	r.mu.Lock()
	if r.pending[key] == entry {
		entry.cancel = nil

		if !pending && err == nil {
			delete(r.pending, key)
		} else {
			entry.delay = min(entry.delay*2, cleanupRetryMaxDelay)
			entry.next = time.Now().Add(entry.delay)
		}
	}
	r.mu.Unlock()

	return true
}

func (m *Manager) refreshCleanupRetry(ctx context.Context, key string, entry *cleanupRetry,
	req DeployRequest,
	cleanup func(context.Context, DeployRequest, *syncWindowGate) (bool, error),
) (bool, error) {
	if m.cleanupRetrySuperseded(req) {
		return false, nil
	}

	fresh, release, err := req.RefreshCleanup(ctx)
	if release != nil {
		defer release()
	}

	if err != nil {
		return true, fmt.Errorf("refresh obsolete-stack cleanup source: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return true, err
	}

	if fresh.Repository.Source == config.SourceTypeOCI && !fresh.Repository.OCITrusted {
		return true, ErrOCIArtifactNotVerified
	}

	if cleanupRetryKey(fresh) != key {
		return true, errors.New("refreshed obsolete-stack cleanup source changed scope")
	}

	if fresh.TestName != "" {
		return false, nil
	}

	if fresh.RefreshCleanup == nil {
		fresh.RefreshCleanup = req.RefreshCleanup
	}

	fresh, accepted := m.acceptCleanupRefresh(ctx, key, entry, fresh)
	if !accepted {
		return true, nil
	}

	if ctx.Err() != nil || m.cleanupRetrySuperseded(fresh) {
		return true, ctx.Err()
	}

	return cleanup(ctx, fresh, m.newSyncWindowGate(fresh, time.Now()))
}

func (m *Manager) cleanupRetrySuperseded(req DeployRequest) bool {
	key := cleanupRetryKey(req)
	r := &m.cleanupRetries
	r.mu.Lock()
	newer := r.generations[key] > req.pauseGeneration
	r.mu.Unlock()

	if newer {
		return true
	}

	m.pauses.mu.Lock()
	defer m.pauses.mu.Unlock()

	current := m.pauses.repos[req.key()]

	return current != nil && current.generation > req.pauseGeneration &&
		cleanupRetryKey(current.request) == key
}

// Build the normal pause snapshot privately, then publish it only if both the
// pending entry and its observed source are still current. Calling observePause
// directly after refresh could overwrite a request observed during the refresh.
func (m *Manager) acceptCleanupRefresh(ctx context.Context, key string, entry *cleanupRetry,
	fresh DeployRequest,
) (DeployRequest, bool) {
	m.pauses.mu.Lock()
	old := m.pauses.repos[fresh.key()]
	m.pauses.mu.Unlock()

	m.cleanupRetries.mu.Lock()
	current := m.cleanupRetries.pending[key] == entry && !m.cleanupRetries.stopped
	generation := entry.request.pauseGeneration
	m.cleanupRetries.mu.Unlock()

	if !current || ctx.Err() != nil ||
		old != nil && cleanupRetryKey(old.request) == key && old.generation > generation {
		return fresh, false
	}

	snapshot := &Manager{pauses: pauseRegistry{
		repos: map[string]*pauseObservation{fresh.key(): old},
	}}
	if snapshot.observePause(fresh) == 0 {
		return fresh, false
	}

	observed := snapshot.pauses.repos[fresh.key()]
	r := &m.cleanupRetries
	r.mu.Lock()
	defer r.mu.Unlock()

	m.pauses.mu.Lock()
	defer m.pauses.mu.Unlock()

	if ctx.Err() != nil || r.stopped || r.pending[key] != entry ||
		m.pauses.repos[fresh.key()] != old {
		return fresh, false
	}

	m.pauses.generation++
	observed.generation = m.pauses.generation
	observed.request.pauseGeneration = observed.generation

	if m.pauses.repos == nil {
		m.pauses.repos = make(map[string]*pauseObservation)
	}

	m.pauses.repos[fresh.key()] = observed
	entry.request = observed.request
	r.generations[key] = observed.generation

	return observed.request, true
}
