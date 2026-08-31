package scheduler

import (
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/docker"
)

var (
	runtimeStatesMu      sync.RWMutex
	runtimeStates        = map[string]scheduledJobState{}
	runtimeRunStatuses   = map[string]string{}
	runtimeRunningStates = map[string]bool{}
)

func formatRunStatus(state, status string) string {
	state = strings.TrimSpace(state)
	if state != string(container.StateExited) {
		return state
	}

	status = strings.TrimSpace(status)

	start := strings.Index(status, "(")
	if start < 0 {
		return state
	}

	end := strings.Index(status[start:], ")")
	if end <= 0 {
		return state
	}

	code := strings.TrimSpace(status[start+1 : start+end])
	if code == "" {
		return state
	}

	return state + " (" + code + ")"
}

// formatExitStatus renders an exited container status with its exit code,
// matching the format produced by formatRunStatus (e.g. "exited (0)").
func formatExitStatus(code int) string {
	return fmt.Sprintf("%s (%d)", container.StateExited, code)
}

func statusForScheduledJob(job scheduledJob, cfg docker.JobScheduleConfig, runtimeStatus string, running bool) string {
	if running {
		return string(container.StateRunning)
	}

	status := formatRunStatus(job.containerState, job.containerStatus)

	if job.mode != scheduledJobModeContainer || cfg.ExecutionMode != docker.JobExecutionModeOneOff {
		return status
	}

	if strings.TrimSpace(job.containerState) != string(container.StateCreated) {
		return status
	}

	runtimeStatus = strings.TrimSpace(runtimeStatus)
	if runtimeStatus == "" {
		return status
	}

	return runtimeStatus
}

func parseRFC3339Time(raw string) *time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil
	}

	return new(t.UTC())
}

// schedulerNow returns the current time in local timezone for consistent scheduling behavior regardless of host timezone settings.
func schedulerNow() time.Time {
	return time.Now().In(time.Local)
}

// setRuntimeStatesSnapshot replaces one context's states while preserving all
// other context partitions and newer manual-run timestamps.
func setRuntimeStatesSnapshot(contextName string, states map[string]scheduledJobState) {
	setRuntimeStatesSnapshotForMode(contextName, "", states)
}

func setRuntimeStatesSnapshotForMode(contextName string, mode scheduledJobMode, states map[string]scheduledJobState) {
	runtimeStatesMu.Lock()
	defer runtimeStatesMu.Unlock()

	merged := make(map[string]scheduledJobState, len(runtimeStates)+len(states))
	for key, state := range runtimeStates {
		if !runtimeKeyInContextMode(contextName, mode, key) {
			merged[key] = state
		}
	}

	for key, state := range states {
		if existing, ok := runtimeStates[key]; ok && existing.lastRun.After(state.lastRun) {
			state.lastRun = existing.lastRun
		}

		merged[key] = state
	}

	runtimeStates = merged
}

func runtimeKeyInContext(contextName, key string) bool {
	if contextName == "" {
		return !strings.Contains(key, "::")
	}

	return strings.HasPrefix(key, jobKeyPrefix(contextName))
}

func runtimeKeyInContextMode(contextName string, mode scheduledJobMode, key string) bool {
	if !runtimeKeyInContext(contextName, key) {
		return false
	}

	if mode == "" {
		return true
	}

	key = strings.TrimPrefix(key, jobKeyPrefix(contextName))

	return strings.HasPrefix(key, string(mode)+":")
}

func clearRuntimeContext(contextName string) {
	clearRuntimeContextMode(contextName, "")
}

func clearRuntimeContextMode(contextName string, mode scheduledJobMode) {
	runtimeStatesMu.Lock()
	defer runtimeStatesMu.Unlock()

	for key := range runtimeStates {
		if runtimeKeyInContextMode(contextName, mode, key) {
			delete(runtimeStates, key)
		}
	}

	for key := range runtimeRunStatuses {
		if runtimeKeyInContextMode(contextName, mode, key) {
			delete(runtimeRunStatuses, key)
		}
	}

	for key := range runtimeRunningStates {
		if runtimeKeyInContextMode(contextName, mode, key) {
			delete(runtimeRunningStates, key)
		}
	}
}

func getRuntimeStatesSnapshot() map[string]scheduledJobState {
	runtimeStatesMu.RLock()
	defer runtimeStatesMu.RUnlock()

	return copyMapLocked(runtimeStates)
}

func setRuntimeLastRun(key string, lastRun time.Time) {
	if key == "" {
		return
	}

	runtimeStatesMu.Lock()
	defer runtimeStatesMu.Unlock()

	state := runtimeStates[key]
	state.lastRun = lastRun
	runtimeStates[key] = state
}

func getRuntimeRunStatusesSnapshot() map[string]string {
	runtimeStatesMu.RLock()
	defer runtimeStatesMu.RUnlock()

	return copyMapLocked(runtimeRunStatuses)
}

func setRuntimeRunStatus(key, status string) {
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}

	runtimeStatesMu.Lock()
	defer runtimeStatesMu.Unlock()

	runtimeRunStatuses[key] = strings.TrimSpace(status)
}

func getRuntimeRunningStatesSnapshot() map[string]bool {
	runtimeStatesMu.RLock()
	defer runtimeStatesMu.RUnlock()

	return copyMapLocked(runtimeRunningStates)
}

func setRuntimeRunInProgress(key string, inProgress bool) {
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}

	runtimeStatesMu.Lock()
	defer runtimeStatesMu.Unlock()

	if inProgress {
		runtimeRunningStates[key] = true
		return
	}

	delete(runtimeRunningStates, key)
}

func updateRuntimeRunStatus(job scheduledJob, cfg docker.JobScheduleConfig, runErr error) {
	if job.mode != scheduledJobModeContainer || cfg.ExecutionMode != docker.JobExecutionModeOneOff {
		return
	}

	if runErr == nil {
		setRuntimeRunStatus(job.key, formatExitStatus(0))
		return
	}

	if exitErr, ok := errors.AsType[*docker.ContainerExitError](runErr); ok {
		setRuntimeRunStatus(job.key, formatExitStatus(exitErr.ExitCode))
	}
}

// copyMapLocked returns a shallow copy of m. Callers must hold runtimeStatesMu.
func copyMapLocked[K comparable, V any](m map[K]V) map[K]V {
	ret := make(map[K]V, len(m))
	maps.Copy(ret, m)

	return ret
}
