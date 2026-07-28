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

type deploymentRunTracker struct {
	mu         sync.RWMutex
	runs       map[string]deploymentRun
	order      []string
	maxEntries int
}

func newDeploymentRunTracker(maxEntries int) *deploymentRunTracker {
	if maxEntries < 1 {
		maxEntries = 500
	}

	return &deploymentRunTracker{
		runs:       make(map[string]deploymentRun),
		order:      make([]string, 0, maxEntries),
		maxEntries: maxEntries,
	}
}

func (t *deploymentRunTracker) TrackAccepted(jobID string, trigger deploymentRunTrigger) {
	now := time.Now().UTC()

	t.upsert(deploymentRun{
		JobID:     jobID,
		Trigger:   trigger,
		Status:    deploymentRunStatusAccepted,
		CreatedAt: now,
		UpdatedAt: now,
	})
}

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

func (t *deploymentRunTracker) Get(jobID string) (deploymentRun, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	r, ok := t.runs[jobID]

	return r, ok
}

func (t *deploymentRunTracker) List(limit int, trigger string, status string) []deploymentRun {
	if limit < 1 {
		limit = 50
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	runs := make([]deploymentRun, 0, min(limit, len(t.order)))
	for i := len(t.order) - 1; i >= 0; i-- {
		if len(runs) >= limit {
			break
		}

		r := t.runs[t.order[i]]

		if trigger != "" && string(r.Trigger) != trigger {
			continue
		}

		if status != "" && string(r.Status) != status {
			continue
		}

		runs = append(runs, r)
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

func (t *deploymentRunTracker) upsert(run deploymentRun) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if existing, ok := t.runs[run.JobID]; ok {
		if run.CreatedAt.IsZero() {
			run.CreatedAt = existing.CreatedAt
		}

		t.runs[run.JobID] = run

		return
	}

	t.runs[run.JobID] = run
	t.order = append(t.order, run.JobID)

	if len(t.order) <= t.maxEntries {
		return
	}

	oldestJobID := t.order[0]
	t.order = t.order[1:]
	delete(t.runs, oldestJobID)
}

func (t *deploymentRunTracker) update(jobID string, fn func(*deploymentRun)) {
	t.mu.Lock()
	defer t.mu.Unlock()

	run, ok := t.runs[jobID]
	if !ok {
		return
	}

	fn(&run)
	t.runs[jobID] = run
}
