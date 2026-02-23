package main

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/kimdre/doco-cd/internal/prometheus"
)

type waiter struct {
	ref string
	ch  chan struct{}
}

type repoEntry struct {
	mu        sync.Mutex
	activeRef string
	refCounts map[string]int
	waiters   []waiter
	lastUsed  time.Time
}

// DeployerLimiter controls concurrent deployments and groups by repository+reference with the rule:
// - Multiple deployments with the same repo+ref may run concurrently (subject to global semaphore)
// - Deployments for different refs in the same repo are serialized (they wait for the current ref group to finish)
// - Different repositories do not block each other (subject to global semaphore)
// It also exposes prometheus metrics for queued/active deployments per repo.
type DeployerLimiter struct {
	sem             chan struct{}
	mu              sync.Mutex
	locks           map[string]*repoEntry
	cleanupInterval time.Duration
	lockTTL         time.Duration
}

// NewDeployerLimiter creates a limiter allowing maxConcurrent deployments.
// It also starts a background cleanup goroutine to remove unused repo lock entries.
func NewDeployerLimiter(maxConcurrent uint) *DeployerLimiter {
	if maxConcurrent == 0 {
		maxConcurrent = 1
	}

	l := &DeployerLimiter{
		sem:             make(chan struct{}, maxConcurrent),
		locks:           make(map[string]*repoEntry),
		cleanupInterval: 1 * time.Minute,
		lockTTL:         5 * time.Minute,
	}
	go l.cleanupLoop()

	return l
}

func (d *DeployerLimiter) getOrCreateRepoEntry(repo string) *repoEntry {
	d.mu.Lock()
	defer d.mu.Unlock()

	ent, ok := d.locks[repo]
	if !ok {
		ent = &repoEntry{
			refCounts: make(map[string]int),
			waiters:   make([]waiter, 0),
			lastUsed:  time.Now(),
		}
		d.locks[repo] = ent
	}

	ent.lastUsed = time.Now()

	return ent
}

// acquire obtains permission to run for given repo+ref. It blocks until the repo allows this ref group to run, then reserves a global slot.
// It returns an unlock function which must be called to release the slot and potentially wake queued waiters.
func (d *DeployerLimiter) acquire(ctx context.Context, repo, ref string) (func(), error) {
	ent := d.getOrCreateRepoEntry(repo)

	// Fast path: try to join existing active ref or claim if none.
	ent.mu.Lock()
	switch ent.activeRef {
	case "":
		// no active group -> claim it
		ent.activeRef = ref
		ent.refCounts[ref] = 1
		ent.mu.Unlock()
		// Now acquire global semaphore, with rollback on context cancel
		select {
		case d.sem <- struct{}{}:
			// acquired global slot
			prometheus.DeploymentsActive.WithLabelValues(repo).Inc()
			return d.makeUnlock(repo, ref), nil
		case <-ctx.Done():
			// rollback: decrement refCounts and clear activeRef
			ent.mu.Lock()

			ent.refCounts[ref]--
			if ent.refCounts[ref] == 0 {
				delete(ent.refCounts, ref)
				ent.activeRef = ""
			}
			ent.mu.Unlock()

			return nil, ctx.Err()
		}
	case ref:
		// same ref active group -> join
		ent.refCounts[ref]++
		ent.mu.Unlock()

		select {
		case d.sem <- struct{}{}:
			prometheus.DeploymentsActive.WithLabelValues(repo).Inc()
			return d.makeUnlock(repo, ref), nil
		case <-ctx.Done():
			// rollback
			ent.mu.Lock()
			ent.refCounts[ref]--
			ent.mu.Unlock()

			return nil, ctx.Err()
		}
	default:
		// Different ref active -> need to enqueue and wait
		w := waiter{ref: ref, ch: make(chan struct{})}
		ent.waiters = append(ent.waiters, w)

		prometheus.DeploymentsQueued.WithLabelValues(repo).Inc()
		ent.mu.Unlock()

		select {
		case <-w.ch:
			// we've been awakened and should now join the active group
			ent.mu.Lock()
			ent.refCounts[ref]++
			ent.mu.Unlock()
			prometheus.DeploymentsQueued.WithLabelValues(repo).Dec()

			select {
			case d.sem <- struct{}{}:
				prometheus.DeploymentsActive.WithLabelValues(repo).Inc()
				return d.makeUnlock(repo, ref), nil
			case <-ctx.Done():
				// rollback: decrease refCounts and maybe trigger next
				ent.mu.Lock()

				ent.refCounts[ref]--
				if ent.refCounts[ref] == 0 {
					delete(ent.refCounts, ref)

					if len(ent.refCounts) == 0 {
						ent.activeRef = ""
						// wake next waiter if any
						d.wakeNext(ent)
					}
				}
				ent.mu.Unlock()

				return nil, ctx.Err()
			}
		case <-ctx.Done():
			// remove from waiters list and decrement queued metric
			ent.mu.Lock()
			for i := range ent.waiters {
				if ent.waiters[i].ch == w.ch {
					// remove
					ent.waiters = append(ent.waiters[:i], ent.waiters[i+1:]...)
					break
				}
			}
			ent.mu.Unlock()
			prometheus.DeploymentsQueued.WithLabelValues(repo).Dec()

			return nil, ctx.Err()
		}
	}
}

// wakeNext makes the next waiter group active and notifies all waiters with that ref.
func (d *DeployerLimiter) wakeNext(ent *repoEntry) {
	// assumes ent.mu locked by caller
	if len(ent.waiters) == 0 {
		return
	}

	nextRef := ent.waiters[0].ref
	ent.activeRef = nextRef
	// collect indices to wake
	toWake := make([]int, 0)

	for i, w := range ent.waiters {
		if w.ref == nextRef {
			toWake = append(toWake, i)
		}
	}
	// wake in reverse order to remove without reindex issues
	for i := len(toWake) - 1; i >= 0; i-- {
		iidx := toWake[i]
		w := ent.waiters[iidx]
		// remove
		ent.waiters = append(ent.waiters[:iidx], ent.waiters[iidx+1:]...)

		close(w.ch)
	}
}

func (d *DeployerLimiter) makeUnlock(repo, ref string) func() {
	return func() {
		// release global semaphore
		<-d.sem
		prometheus.DeploymentsActive.WithLabelValues(repo).Dec()

		ent := d.getOrCreateRepoEntry(repo)
		ent.mu.Lock()
		if ent.refCounts[ref] > 0 {
			ent.refCounts[ref]--
		}

		if ent.refCounts[ref] == 0 {
			delete(ent.refCounts, ref)
			// if no more active refs, clear activeRef and wake next waiter group
			if len(ent.refCounts) == 0 {
				ent.activeRef = ""
				// wake next group if present
				d.wakeNext(ent)
			}
		}

		ent.lastUsed = time.Now()
		ent.mu.Unlock()
	}
}

// TryAcquire attempts to acquire without blocking; returns nil unlock if successful.
func (d *DeployerLimiter) TryAcquire(repo, ref string) (func(), bool) {
	ent := d.getOrCreateRepoEntry(repo)
	ent.mu.Lock()
	defer ent.mu.Unlock()
	// If activeRef empty or same as ref, try to get sem non-blocking
	if ent.activeRef == "" || ent.activeRef == ref {
		select {
		case d.sem <- struct{}{}:
			// succeeded
			ent.activeRef = ref
			ent.refCounts[ref]++

			prometheus.DeploymentsActive.WithLabelValues(repo).Inc()

			return d.makeUnlock(repo, ref), true
		default:
			return nil, false
		}
	}
	// different ref active -> cannot acquire
	return nil, false
}

// cleanupLoop periodically removes unused lock entries to avoid unbounded growth.
func (d *DeployerLimiter) cleanupLoop() {
	t := time.NewTicker(d.cleanupInterval)
	defer t.Stop()

	for range t.C {
		now := time.Now()

		d.mu.Lock()
		for repo, ent := range d.locks {
			ent.mu.Lock()
			if len(ent.waiters) == 0 && len(ent.refCounts) == 0 && now.Sub(ent.lastUsed) > d.lockTTL {
				delete(d.locks, repo)
				prometheus.DeploymentsActive.WithLabelValues(repo).Set(0)
				prometheus.DeploymentsQueued.WithLabelValues(repo).Set(0)
			}
			ent.mu.Unlock()
		}
		d.mu.Unlock()
	}
}

// NormalizeReference canonicalizes git refs using go-git.
func NormalizeReference(ref string) string {
	if ref == "" {
		return ""
	}

	// If it looks like a full ref (contains a slash), use go-git to get the short name.
	if strings.Contains(ref, "/") {
		return plumbing.ReferenceName(ref).Short()
	}

	// Otherwise keep as-is (commit SHA or already short name).
	return ref
}
