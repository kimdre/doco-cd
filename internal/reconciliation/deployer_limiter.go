package reconciliation

import (
	"context"
	"sync"
	"time"

	"github.com/kimdre/doco-cd/internal/prometheus"
)

// repoEntry stores per-repository metrics; stack-level exclusivity is handled by lock.LockStack.
type repoEntry struct {
	mu       sync.Mutex
	active   int
	queued   int
	lastUsed time.Time
}

// DeployerLimiter bounds concurrency with a global semaphore and reports per-repository metrics.
type DeployerLimiter struct {
	sem             chan struct{}
	mu              sync.Mutex
	entries         map[string]*repoEntry
	cleanupInterval time.Duration
	entryTTL        time.Duration
	closeOnce       sync.Once
	closeChan       chan struct{}
	doneChan        chan struct{}
}

// NewDeployerLimiter creates a limiter allowing maxConcurrent deployments.
// It also starts a background cleanup goroutine to remove unused per-repo
// metric bookkeeping entries.
func NewDeployerLimiter(maxConcurrent uint) *DeployerLimiter {
	if maxConcurrent == 0 {
		maxConcurrent = 1
	}

	l := &DeployerLimiter{
		sem:             make(chan struct{}, maxConcurrent),
		entries:         make(map[string]*repoEntry),
		cleanupInterval: 1 * time.Minute,
		entryTTL:        5 * time.Minute,
		closeChan:       make(chan struct{}),
		doneChan:        make(chan struct{}),
	}
	go l.cleanupLoop()

	return l
}

func (d *DeployerLimiter) getOrCreateEntry(repo string) *repoEntry {
	d.mu.Lock()
	defer d.mu.Unlock()

	ent, ok := d.entries[repo]
	if !ok {
		ent = &repoEntry{lastUsed: time.Now()}
		d.entries[repo] = ent
	}

	ent.mu.Lock()
	ent.lastUsed = time.Now()
	ent.mu.Unlock()

	return ent
}

// Close stops the background cleanup loop. It is safe to call more than once.
func (d *DeployerLimiter) Close() {
	if d == nil {
		return
	}

	d.closeOnce.Do(func() {
		close(d.closeChan)
		<-d.doneChan
	})
}

// acquire obtains a global concurrency slot for repo, blocking until one is
// available or ctx is done. It returns an unlock function which must be
// called to release the slot.
func (d *DeployerLimiter) acquire(ctx context.Context, repo string) (func(), error) {
	ent := d.getOrCreateEntry(repo)

	ent.mu.Lock()
	ent.queued++
	ent.mu.Unlock()
	prometheus.DeploymentsQueued.WithLabelValues(repo).Inc()

	select {
	case d.sem <- struct{}{}:
		ent.mu.Lock()
		ent.queued--
		ent.active++
		ent.lastUsed = time.Now()
		ent.mu.Unlock()

		prometheus.DeploymentsQueued.WithLabelValues(repo).Dec()
		prometheus.DeploymentsActive.WithLabelValues(repo).Inc()

		return d.makeUnlock(repo, ent), nil
	case <-ctx.Done():
		ent.mu.Lock()
		ent.queued--
		ent.mu.Unlock()
		prometheus.DeploymentsQueued.WithLabelValues(repo).Dec()

		return nil, ctx.Err()
	}
}

func (d *DeployerLimiter) makeUnlock(repo string, ent *repoEntry) func() {
	return func() {
		<-d.sem

		ent.mu.Lock()
		ent.active--
		ent.lastUsed = time.Now()
		ent.mu.Unlock()

		prometheus.DeploymentsActive.WithLabelValues(repo).Dec()
	}
}

// TryAcquire obtains a global concurrency slot without blocking; it returns false when none is available.
func (d *DeployerLimiter) TryAcquire(repo string) (func(), bool) {
	ent := d.getOrCreateEntry(repo)

	select {
	case d.sem <- struct{}{}:
		ent.mu.Lock()
		ent.active++
		ent.lastUsed = time.Now()
		ent.mu.Unlock()

		prometheus.DeploymentsActive.WithLabelValues(repo).Inc()

		return d.makeUnlock(repo, ent), true
	default:
		return nil, false
	}
}

// cleanupLoop periodically removes unused per-repo metric entries to avoid unbounded growth.
func (d *DeployerLimiter) cleanupLoop() {
	defer close(d.doneChan)

	t := time.NewTicker(d.cleanupInterval)
	defer t.Stop()

	for {
		select {
		case <-d.closeChan:
			return
		case <-t.C:
			d.cleanup(time.Now())
		}
	}
}

func (d *DeployerLimiter) cleanup(now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()

	for repo, ent := range d.entries {
		ent.mu.Lock()
		idle := ent.active == 0 && ent.queued == 0 && now.Sub(ent.lastUsed) > d.entryTTL
		ent.mu.Unlock()

		if idle {
			delete(d.entries, repo)
			prometheus.DeploymentsActive.WithLabelValues(repo).Set(0)
			prometheus.DeploymentsQueued.WithLabelValues(repo).Set(0)
		}
	}
}
