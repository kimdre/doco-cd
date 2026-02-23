package main

import (
	"context"
	"sync"
	"time"

	"github.com/kimdre/doco-cd/internal/prometheus"
)

// repoLockEntry holds the channel (acting as mutex), a reference count and last used time for cleanup.
type repoLockEntry struct {
	ch       chan struct{}
	refCount int
	lastUsed time.Time
}

// DeployerLimiter controls concurrent deployments and groups by repository only.
// It provides two resources:
// - a semaphore limiting total concurrent deployments
// - per-repo channels ensuring deployments for the same repo run serially (FIFO)
// It also exposes Prometheus metrics for active and queued deployments per repository.
type DeployerLimiter struct {
	sem chan struct{}
	mu  sync.Mutex
	// map repo -> lock entry
	locks map[string]*repoLockEntry
	// cleanup settings
	cleanupInterval time.Duration
	lockTTL         time.Duration
}

// NewDeployerLimiter creates a limiter allowing maxConcurrent deployments.
// It also starts a background cleanup goroutine to remove unused repo lock entries.
func NewDeployerLimiter(maxConcurrent uint) *DeployerLimiter {
	if maxConcurrent == 0 {
		maxConcurrent = 1
	}

	d := &DeployerLimiter{
		sem:             make(chan struct{}, maxConcurrent),
		locks:           make(map[string]*repoLockEntry),
		cleanupInterval: 1 * time.Minute, // cleanup every minute
		lockTTL:         5 * time.Minute, // remove lock entries unused for 5 minutes
	}

	go d.cleanupLoop()

	return d
}

// getOrCreateRepoLock returns (and creates if needed) the repo lock entry.
func (d *DeployerLimiter) getOrCreateRepoLock(repo string) *repoLockEntry {
	d.mu.Lock()
	defer d.mu.Unlock()

	entry, ok := d.locks[repo]
	if !ok {
		entry = &repoLockEntry{
			ch:       make(chan struct{}, 1),
			refCount: 0,
			lastUsed: time.Now(),
		}
		d.locks[repo] = entry
	}

	entry.refCount++
	entry.lastUsed = time.Now()

	// update queued metric: number of waiters equals approximate queued length
	prometheus.DeploymentsQueued.WithLabelValues(repo).Inc()

	return entry
}

// releaseRepoLock decrements refCount and updates lastUsed; actual removal happens in cleanupLoop.
func (d *DeployerLimiter) releaseRepoLock(repo string) {
	d.mu.Lock()

	entry, ok := d.locks[repo]
	if ok {
		if entry.refCount > 0 {
			entry.refCount--
		}

		entry.lastUsed = time.Now()
		// Decrement queued metric since one waiter has progressed to acquisition or finished
		val := prometheus.DeploymentsQueued.WithLabelValues(repo)
		// There's no direct Decrement for gauge vectors created as GaugeVec (but they expose Dec via the metric interface).
		val.Dec()
	}
	d.mu.Unlock()
}

// acquire obtains semaphore and the per-repo lock. It respects context cancellation.
// It updates Prometheus metrics for queued and active deployments.
func (d *DeployerLimiter) acquire(ctx context.Context, repo string) (unlock func(), err error) {
	// register interest (queued)
	entry := d.getOrCreateRepoLock(repo)
	// try to acquire global semaphore slot
	select {
	case d.sem <- struct{}{}:
		// acquired global slot
	case <-ctx.Done():
		// remove interest
		d.releaseRepoLock(repo)
		return nil, ctx.Err()
	}

	// Now wait for per-repo lock by sending to channel (this will block if another job holds it)
	select {
	case entry.ch <- struct{}{}:
		// acquired per-repo lock
		// update metrics: one less queued, one more active
		prometheus.DeploymentsQueued.WithLabelValues(repo).Dec()
		prometheus.DeploymentsActive.WithLabelValues(repo).Inc()

		return func() {
			// release per-repo lock
			<-entry.ch
			// decrement active
			prometheus.DeploymentsActive.WithLabelValues(repo).Dec()
			// release global semaphore with small backoff
			go func() {
				time.Sleep(10 * time.Millisecond)
				<-d.sem
			}()
		}, nil
	case <-ctx.Done():
		// failed to acquire per-repo lock, release global slot and interest
		<-d.sem
		d.releaseRepoLock(repo)

		return nil, ctx.Err()
	}
}

// TryAcquire attempts to acquire without blocking; returns nil unlock if successful.
func (d *DeployerLimiter) TryAcquire(repo string) (func(), bool) {
	// Quick attempt to get a queued slot
	entry := d.getOrCreateRepoLock(repo)
	// try to acquire semaphore non-blocking
	select {
	case d.sem <- struct{}{}:
		// acquired global slot
	default:
		d.releaseRepoLock(repo)
		return nil, false
	}

	// Try to get per-repo lock non-blocking
	select {
	case entry.ch <- struct{}{}:
		// acquired
		prometheus.DeploymentsQueued.WithLabelValues(repo).Dec()
		prometheus.DeploymentsActive.WithLabelValues(repo).Inc()

		return func() {
			<-entry.ch
			prometheus.DeploymentsActive.WithLabelValues(repo).Dec()

			go func() { time.Sleep(10 * time.Millisecond); <-d.sem }()
		}, true
	default:
		// couldn't acquire per-repo lock, release semaphore and interest
		<-d.sem
		d.releaseRepoLock(repo)

		return nil, false
	}
}

// cleanupLoop periodically removes unused lock entries to avoid unbounded growth.
func (d *DeployerLimiter) cleanupLoop() {
	ticker := time.NewTicker(d.cleanupInterval)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()

		d.mu.Lock()
		for repo, entry := range d.locks {
			// remove entries that haven't been used for lockTTL and have no active waiters
			if entry.refCount == 0 && now.Sub(entry.lastUsed) > d.lockTTL {
				delete(d.locks, repo)
				// ensure metrics are zeroed for this repo
				prometheus.DeploymentsActive.WithLabelValues(repo).Set(0)
				prometheus.DeploymentsQueued.WithLabelValues(repo).Set(0)
			}
		}
		d.mu.Unlock()
	}
}
