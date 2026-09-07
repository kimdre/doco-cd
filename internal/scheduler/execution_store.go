package scheduler

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/filesystem"
)

const executionRecordVersion = 1

// executionRecord only persists finalization state that cannot be stored in
// Docker labels before side effects occur. Runtime identity and timestamps
// live on the temporary container or service itself.
type executionRecord struct {
	Version      int                         `json:"version"`
	RunID        string                      `json:"run_id"`
	Context      string                      `json:"context"`
	Mode         scheduledJobMode            `json:"mode"`
	Job          executionJob                `json:"job"`
	Finalization executionFinalizationConfig `json:"finalization"`
	StopPlans    []executionStopPlan         `json:"stop_plans,omitempty"`
	Restored     bool                        `json:"restored"`
	Reported     bool                        `json:"reported"`
}

type executionStopPlan struct {
	Project  string `json:"project"`
	Service  string `json:"service"`
	Replicas uint64 `json:"replicas,omitempty"`
}

type executionFinalizationConfig struct {
	NotifyOn     docker.JobNotifyOn      `json:"notify_on"`
	StopServices []docker.StopServiceRef `json:"stop_services,omitempty"`
}

func executionFinalizationConfigFromSchedule(cfg docker.JobScheduleConfig) executionFinalizationConfig {
	return executionFinalizationConfig{
		NotifyOn:     cfg.NotifyOn,
		StopServices: slices.Clone(cfg.StopServices),
	}
}

func (c executionFinalizationConfig) scheduleConfig() docker.JobScheduleConfig {
	return docker.JobScheduleConfig{
		ExecutionMode: docker.JobExecutionModeOneOff,
		NotifyOn:      c.NotifyOn,
		StopServices:  slices.Clone(c.StopServices),
	}
}

type executionJob struct {
	Key     string            `json:"key"`
	Name    string            `json:"name"`
	ID      string            `json:"id"`
	Mode    scheduledJobMode  `json:"mode"`
	Labels  map[string]string `json:"labels"`
	Context string            `json:"context"`
}

func executionJobFromScheduled(job scheduledJob) executionJob {
	return executionJob{
		Key:     job.key,
		Name:    job.name,
		ID:      job.id,
		Mode:    job.mode,
		Labels:  executionNotificationLabels(job.labels),
		Context: job.context,
	}
}

func executionNotificationLabels(labels map[string]string) map[string]string {
	keys := []string{
		docker.DocoCDLabels.Source.Name,
		docker.DocoCDLabels.Deployment.Name,
		docker.DocoCDLabels.Deployment.ConfigTarget,
		docker.DocoCDLabels.Deployment.CommitSHA,
	}

	result := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, ok := labels[key]; ok {
			result[key] = value
		}
	}

	return result
}

func (j executionJob) scheduled() scheduledJob {
	return scheduledJob{
		key:     j.Key,
		name:    j.Name,
		id:      j.ID,
		mode:    j.Mode,
		labels:  maps.Clone(j.Labels),
		context: j.Context,
	}
}

// executionStore persists finalization state that cannot be stored in Docker
// labels: stop plans, notification status, and restoration markers.
// All writes are atomic: temp file → sync → rename → directory sync.
type executionStore struct {
	mu   sync.Mutex
	root string
}

func newExecutionStore(dataMountPath string) *executionStore {
	dataMountPath = strings.TrimSpace(dataMountPath)
	if dataMountPath == "" {
		return &executionStore{}
	}

	return &executionStore{root: filepath.Join(dataMountPath, "scheduler-executions")}
}

func (s *executionStore) enabled() bool {
	return s != nil && s.root != ""
}

func (s *executionStore) create(record *executionRecord) error {
	if !s.enabled() {
		return nil
	}

	if record == nil {
		return errors.New("execution record is required")
	}

	if record.Version == 0 {
		record.Version = executionRecordVersion
	}

	if record.RunID == "" {
		return errors.New("execution record run ID is required")
	}

	return s.write(*record)
}

func (s *executionStore) update(record executionRecord) error {
	if !s.enabled() {
		return nil
	}

	if record.RunID == "" {
		return errors.New("execution record run ID is required")
	}

	return s.write(record)
}

func (s *executionStore) remove(runID string) error {
	if !s.enabled() {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	err := os.Remove(s.path(runID))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove execution record %s: %w", runID, err)
	}

	return nil
}

func (s *executionStore) list(contextName string, mode scheduledJobMode) ([]executionRecord, error) {
	if !s.enabled() {
		return nil, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read execution records: %w", err)
	}

	records := make([]executionRecord, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		path := filepath.Join(s.root, entry.Name())

		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, fmt.Errorf("read execution record %s: %w", entry.Name(), readErr)
		}

		var record executionRecord
		if unmarshalErr := json.Unmarshal(data, &record); unmarshalErr != nil {
			// Corrupt files are renamed with .corrupt suffix to prevent
			// parse errors on every startup.
			if err := s.quarantine(path); err != nil {
				return nil, fmt.Errorf("quarantine malformed execution record %s: %w", entry.Name(), err)
			}

			continue
		}

		if record.Version != executionRecordVersion {
			// Unsupported schema versions are quarantined to allow future
			// migrations without breaking startup.
			if err := s.quarantine(path); err != nil {
				return nil, fmt.Errorf("quarantine unsupported execution record %s: %w", entry.Name(), err)
			}

			continue
		}

		if record.RunID == "" || record.Context != contextName || record.Mode != mode {
			continue
		}

		records = append(records, record)
	}

	return records, nil
}

func (s *executionStore) write(record executionRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.root, filesystem.PermDir); err != nil {
		return fmt.Errorf("create execution record directory: %w", err)
	}

	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode execution record %s: %w", record.RunID, err)
	}

	// Write to temp file, sync, then rename to final name. Then sync directory.
	// Prevents corruption if process crashes during write.
	file, err := os.CreateTemp(s.root, ".execution-*.json")
	if err != nil {
		return fmt.Errorf("create temporary execution record: %w", err)
	}

	tempPath := file.Name()

	defer func() {
		_ = os.Remove(tempPath)
	}()

	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write execution record %s: %w", record.RunID, err)
	}

	if err = file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync execution record %s: %w", record.RunID, err)
	}

	if err = file.Close(); err != nil {
		return fmt.Errorf("close execution record %s: %w", record.RunID, err)
	}

	if err = os.Rename(tempPath, s.path(record.RunID)); err != nil {
		return fmt.Errorf("publish execution record %s: %w", record.RunID, err)
	}

	// Sync directory inode to durably link the new/updated record.
	dir, err := os.Open(s.root)
	if err != nil {
		return fmt.Errorf("open execution record directory: %w", err)
	}
	defer dir.Close() // nolint:errcheck

	if err = dir.Sync(); err != nil {
		return fmt.Errorf("sync execution record directory: %w", err)
	}

	return nil
}

func (s *executionStore) path(runID string) string {
	return filepath.Join(s.root, runID+".json")
}

func (s *executionStore) quarantine(path string) error {
	return os.Rename(path, path+".corrupt")
}
