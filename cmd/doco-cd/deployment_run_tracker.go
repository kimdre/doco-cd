package main

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"time"
)

type (
	deploymentRunStatus  string
	deploymentRunTrigger string
)

const (
	deploymentRunStatusAccepted  deploymentRunStatus = "accepted"
	deploymentRunStatusRunning   deploymentRunStatus = "running"
	deploymentRunStatusSucceeded deploymentRunStatus = "succeeded"
	deploymentRunStatusFailed    deploymentRunStatus = "failed"
	deploymentRunStatusSkipped   deploymentRunStatus = "skipped"
)

const (
	deploymentRunTriggerWebhook      deploymentRunTrigger = "webhook"
	deploymentRunTriggerPoll         deploymentRunTrigger = "poll"
	deploymentRunTriggerScheduledJob deploymentRunTrigger = "scheduled_job"
)

const deploymentRunTTL = 7 * 24 * time.Hour

var (
	errInvalidDeploymentRunStatus  = errors.New("invalid deployment run status")
	errInvalidDeploymentRunTrigger = errors.New("invalid deployment run trigger")
)

type deploymentRun struct {
	JobID      string               `json:"job_id"`
	Trigger    deploymentRunTrigger `json:"trigger"`
	Status     deploymentRunStatus  `json:"status"`
	Repository string               `json:"repository,omitempty"`
	Target     string               `json:"target,omitempty"`
	Revision   string               `json:"revision,omitempty"`
	Message    string               `json:"message,omitempty"`
	CreatedAt  time.Time            `json:"created_at"`
	StartedAt  *time.Time           `json:"started_at,omitempty"`
	FinishedAt *time.Time           `json:"finished_at,omitempty"`
	UpdatedAt  time.Time            `json:"updated_at"`
}

// deploymentRunTracker is a thread-safe, in-memory registry for tracking deployment runs.
// Runs are stored by jobID and organized by trigger type (webhook, poll, scheduled_job).
// Memory is bounded by: max entries per type + 7-day TTL expiration.
type deploymentRunTracker struct {
	mu                sync.RWMutex
	runs              map[string]deploymentRun
	orderByTrigger    map[deploymentRunTrigger][]string
	maxEntriesPerType map[deploymentRunTrigger]int
	ttl               time.Duration
}

// newDeploymentRunTracker creates a new deployment run tracker with per-type limits.
// Defaults to 50 entries per trigger type (webhook, poll, scheduled_job) if not specified.
// Runs older than 7 days are automatically evicted.
func newDeploymentRunTracker(maxPerType map[deploymentRunTrigger]int) *deploymentRunTracker {
	if maxPerType == nil {
		maxPerType = make(map[deploymentRunTrigger]int)
	}

	// Set defaults for missing types
	if maxPerType[deploymentRunTriggerWebhook] < 1 {
		maxPerType[deploymentRunTriggerWebhook] = 50
	}

	if maxPerType[deploymentRunTriggerPoll] < 1 {
		maxPerType[deploymentRunTriggerPoll] = 50
	}

	if maxPerType[deploymentRunTriggerScheduledJob] < 1 {
		maxPerType[deploymentRunTriggerScheduledJob] = 50
	}

	return &deploymentRunTracker{
		runs:              make(map[string]deploymentRun),
		orderByTrigger:    make(map[deploymentRunTrigger][]string),
		maxEntriesPerType: maxPerType,
		ttl:               deploymentRunTTL,
	}
}

// TrackAccepted records a new deployment run in accepted state.
// This is called when a deployment is initiated (webhook, API trigger, or scheduled).
// Triggers cleanup of expired runs before insertion.
func (t *deploymentRunTracker) TrackAccepted(jobID string, trigger deploymentRunTrigger) {
	now := time.Now().UTC()

	t.cleanup(now)

	t.upsert(deploymentRun{
		JobID:     jobID,
		Trigger:   trigger,
		Status:    deploymentRunStatusAccepted,
		CreatedAt: now,
		UpdatedAt: now,
	})
}

// MarkRunning updates a run to running state and records the start time.
func (t *deploymentRunTracker) MarkRunning(jobID string) {
	now := time.Now().UTC()

	t.update(jobID, func(r *deploymentRun) {
		r.Status = deploymentRunStatusRunning

		r.UpdatedAt = now
		if r.StartedAt == nil {
			r.StartedAt = &now
		}
	})
}

// MarkSucceeded marks a run as succeeded with an optional message.
func (t *deploymentRunTracker) MarkSucceeded(jobID, message string) {
	now := time.Now().UTC()

	t.update(jobID, func(r *deploymentRun) {
		r.Status = deploymentRunStatusSucceeded
		r.Message = strings.TrimSpace(message)
		r.UpdatedAt = now

		r.FinishedAt = &now
		if r.StartedAt == nil {
			r.StartedAt = &now
		}
	})
}

// MarkFailed marks a run as failed with an error message.
func (t *deploymentRunTracker) MarkFailed(jobID, message string) {
	now := time.Now().UTC()

	t.update(jobID, func(r *deploymentRun) {
		r.Status = deploymentRunStatusFailed
		r.Message = strings.TrimSpace(message)
		r.UpdatedAt = now

		r.FinishedAt = &now
		if r.StartedAt == nil {
			r.StartedAt = &now
		}
	})
}

// MarkSkipped marks a run as skipped with an optional reason message.
func (t *deploymentRunTracker) MarkSkipped(jobID, message string) {
	now := time.Now().UTC()

	t.update(jobID, func(r *deploymentRun) {
		r.Status = deploymentRunStatusSkipped
		r.Message = strings.TrimSpace(message)
		r.UpdatedAt = now

		r.FinishedAt = &now
		if r.StartedAt == nil {
			r.StartedAt = &now
		}
	})
}

// SetMetadata updates run metadata (repository, deployment target, git revision).
func (t *deploymentRunTracker) SetMetadata(jobID, repository, target, revision string) {
	repository = strings.TrimSpace(repository)
	target = strings.TrimSpace(target)
	revision = strings.TrimSpace(revision)

	t.update(jobID, func(r *deploymentRun) {
		if repository != "" {
			r.Repository = repository
		}

		if target != "" {
			r.Target = target
		}

		if revision != "" {
			r.Revision = revision
		}

		r.UpdatedAt = time.Now().UTC()
	})
}

// Get retrieves a run by its jobID. Returns the run and a boolean indicating if it was found.
func (t *deploymentRunTracker) Get(jobID string) (deploymentRun, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	r, ok := t.runs[jobID]

	return r, ok
}

// List returns recent runs, optionally filtered by trigger type and status.
// Runs are returned in reverse chronological order (newest first).
// Limit defaults to 50 if not specified. Triggers automatic cleanup of expired runs.
func (t *deploymentRunTracker) List(limit int, trigger string, status string) []deploymentRun {
	if limit < 1 {
		limit = 50
	}

	t.cleanup(time.Now().UTC())

	t.mu.RLock()
	defer t.mu.RUnlock()

	runs := make([]deploymentRun, 0, min(limit, len(t.runs)))

	// If trigger filter is specified, use that type's order; otherwise collect all in reverse order
	if trigger != "" {
		triggerType := deploymentRunTrigger(trigger)

		order := t.orderByTrigger[triggerType]
		for i := len(order) - 1; i >= 0; i-- {
			if len(runs) >= limit {
				break
			}

			r := t.runs[order[i]]

			if status != "" && string(r.Status) != status {
				continue
			}

			runs = append(runs, r)
		}
	} else {
		// Collect all runs across all trigger types, newest first
		allJobs := make([]string, 0, len(t.runs))
		for _, jobIDs := range t.orderByTrigger {
			allJobs = append(allJobs, jobIDs...)
		}

		// Sort by creation time (newest first) to get consistent ordering
		slices.SortFunc(allJobs, func(a, b string) int {
			runA, okA := t.runs[a]

			runB, okB := t.runs[b]
			if !okA || !okB {
				return 0
			}

			return runB.CreatedAt.Compare(runA.CreatedAt)
		})

		for _, jobID := range allJobs {
			if len(runs) >= limit {
				break
			}

			r := t.runs[jobID]

			if status != "" && string(r.Status) != status {
				continue
			}

			runs = append(runs, r)
		}
	}

	return runs
}

func normalizeDeploymentRunStatus(value string) (string, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return "", nil
	}

	valid := []string{
		string(deploymentRunStatusAccepted),
		string(deploymentRunStatusRunning),
		string(deploymentRunStatusSucceeded),
		string(deploymentRunStatusFailed),
		string(deploymentRunStatusSkipped),
	}

	if !slices.Contains(valid, value) {
		return "", errInvalidDeploymentRunStatus
	}

	return value, nil
}

func normalizeDeploymentRunTrigger(value string) (string, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return "", nil
	}

	valid := []string{
		string(deploymentRunTriggerWebhook),
		string(deploymentRunTriggerPoll),
		string(deploymentRunTriggerScheduledJob),
	}

	if !slices.Contains(valid, value) {
		return "", errInvalidDeploymentRunTrigger
	}

	return value, nil
}

// cleanup removes runs that have exceeded their 7-day TTL.
// Called during TrackAccepted and List operations to enforce time-based expiration.
func (t *deploymentRunTracker) cleanup(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	cutoffTime := now.Add(-t.ttl)

	for trigger, jobIDs := range t.orderByTrigger {
		newOrder := make([]string, 0, len(jobIDs))
		for _, jobID := range jobIDs {
			run, ok := t.runs[jobID]
			if ok && (run.CreatedAt.After(cutoffTime) || !isTerminalDeploymentRunStatus(run.Status)) {
				newOrder = append(newOrder, jobID)
			} else if ok {
				delete(t.runs, jobID)
			}
		}

		t.orderByTrigger[trigger] = newOrder
	}
}

// upsert adds or updates a run and prunes terminal history above the per-type limit.
func (t *deploymentRunTracker) upsert(run deploymentRun) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if existing, ok := t.runs[run.JobID]; ok {
		if run.CreatedAt.IsZero() {
			run.CreatedAt = existing.CreatedAt
		}

		t.runs[run.JobID] = run
		t.pruneTerminalRuns(run.Trigger)

		return
	}

	t.runs[run.JobID] = run
	order := t.orderByTrigger[run.Trigger]
	t.orderByTrigger[run.Trigger] = append(order, run.JobID)
	t.pruneTerminalRuns(run.Trigger)
}

func (t *deploymentRunTracker) pruneTerminalRuns(trigger deploymentRunTrigger) {
	maxForType := t.maxEntriesPerType[trigger]
	if maxForType < 1 {
		maxForType = 50
	}

	for len(t.orderByTrigger[trigger]) > maxForType {
		terminalIndex := slices.IndexFunc(t.orderByTrigger[trigger], func(jobID string) bool {
			run, ok := t.runs[jobID]

			return ok && isTerminalDeploymentRunStatus(run.Status)
		})
		if terminalIndex < 0 {
			return
		}

		jobID := t.orderByTrigger[trigger][terminalIndex]
		t.orderByTrigger[trigger] = slices.Delete(t.orderByTrigger[trigger], terminalIndex, terminalIndex+1)
		delete(t.runs, jobID)
	}
}

// update modifies an existing run using the provided callback function.
// No-op if the run does not exist. Thread-safe with write lock.
func (t *deploymentRunTracker) update(jobID string, fn func(*deploymentRun)) {
	t.mu.Lock()
	defer t.mu.Unlock()

	run, ok := t.runs[jobID]
	if !ok {
		return
	}

	fn(&run)
	t.runs[jobID] = run
	t.pruneTerminalRuns(run.Trigger)
}

func isTerminalDeploymentRunStatus(status deploymentRunStatus) bool {
	return status == deploymentRunStatusSucceeded || status == deploymentRunStatusFailed || status == deploymentRunStatusSkipped
}
