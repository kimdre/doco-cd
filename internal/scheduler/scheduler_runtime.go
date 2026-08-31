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

// runtimeStore keeps the status exposed by scheduler APIs separate from the
// worker-local scheduling state. Each Manager owns one store shared by its
// background workers and on-demand operations.
type runtimeStore struct {
	mu            sync.RWMutex
	states        map[string]scheduledJobState
	runStatuses   map[string]string
	runningStates map[string]int
	clearing      map[string]bool
	cond          *sync.Cond
}

func newRuntimeStore() *runtimeStore {
	store := &runtimeStore{
		states:        map[string]scheduledJobState{},
		runStatuses:   map[string]string{},
		runningStates: map[string]int{},
		clearing:      map[string]bool{},
	}
	store.cond = sync.NewCond(&store.mu)

	return store
}

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

// setStatesSnapshot replaces one context's states while preserving all
// other context partitions and newer manual-run timestamps.
func (s *runtimeStore) setStatesSnapshot(contextName string, mode scheduledJobMode, states map[string]scheduledJobState) {
	s.mu.Lock()
	defer s.mu.Unlock()

	merged := make(map[string]scheduledJobState, len(s.states)+len(states))
	for key, state := range s.states {
		if !runtimeKeyInContextMode(contextName, mode, key) {
			merged[key] = state
		}
	}

	for key, state := range states {
		if existing, ok := s.states[key]; ok && existing.lastRun.After(state.lastRun) {
			state.lastRun = existing.lastRun
		}

		merged[key] = state
	}

	s.states = merged
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

func (s *runtimeStore) clearContextMode(contextName string, mode scheduledJobMode) {
	s.mu.Lock()
	defer s.mu.Unlock()

	partition := runtimePartitionKey(contextName, mode)

	s.clearing[partition] = true
	for s.hasRunningStateLocked(contextName, mode) {
		s.cond.Wait()
	}

	for key := range s.states {
		if runtimeKeyInContextMode(contextName, mode, key) {
			delete(s.states, key)
		}
	}

	for key := range s.runStatuses {
		if runtimeKeyInContextMode(contextName, mode, key) {
			delete(s.runStatuses, key)
		}
	}

	for key := range s.runningStates {
		if runtimeKeyInContextMode(contextName, mode, key) {
			delete(s.runningStates, key)
		}
	}

	delete(s.clearing, partition)
	s.cond.Broadcast()
}

func (s *runtimeStore) statesSnapshot() map[string]scheduledJobState {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return copyMap(s.states)
}

func (s *runtimeStore) setLastRun(key string, lastRun time.Time) {
	if key == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	state := s.states[key]
	state.lastRun = lastRun
	s.states[key] = state
}

func (s *runtimeStore) runStatusesSnapshot() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return copyMap(s.runStatuses)
}

func (s *runtimeStore) setRunStatus(key, status string) {
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.runStatuses[key] = strings.TrimSpace(status)
}

func (s *runtimeStore) runningStatesSnapshot() map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	states := make(map[string]bool, len(s.runningStates))
	for key, count := range s.runningStates {
		states[key] = count > 0
	}

	return states
}

func (s *runtimeStore) isRunInProgress(key string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.runningStates[key] > 0
}

func (s *runtimeStore) beginRun(contextName string, mode scheduledJobMode, key string) {
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	partition := runtimePartitionKey(contextName, mode)

	contextPartition := runtimePartitionKey(contextName, "")
	for s.clearing[partition] || s.clearing[contextPartition] {
		s.cond.Wait()
	}

	s.runningStates[key]++
}

func (s *runtimeStore) endRun(key string) {
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.runningStates[key] <= 1 {
		delete(s.runningStates, key)
	} else {
		s.runningStates[key]--
	}

	s.cond.Broadcast()
}

func (s *runtimeStore) updateRunStatus(job scheduledJob, cfg docker.JobScheduleConfig, runErr error) {
	if job.mode != scheduledJobModeContainer || cfg.ExecutionMode != docker.JobExecutionModeOneOff {
		return
	}

	if runErr == nil {
		s.setRunStatus(job.key, formatExitStatus(0))
		return
	}

	if exitErr, ok := errors.AsType[*docker.ContainerExitError](runErr); ok {
		s.setRunStatus(job.key, formatExitStatus(exitErr.ExitCode))
	}
}

func copyMap[K comparable, V any](m map[K]V) map[K]V {
	ret := make(map[K]V, len(m))
	maps.Copy(ret, m)

	return ret
}

func runtimePartitionKey(contextName string, mode scheduledJobMode) string {
	return docker.NormalizeContextName(contextName) + "\x00" + string(mode)
}

func (s *runtimeStore) hasRunningStateLocked(contextName string, mode scheduledJobMode) bool {
	for key, count := range s.runningStates {
		if count > 0 && runtimeKeyInContextMode(contextName, mode, key) {
			return true
		}
	}

	return false
}
