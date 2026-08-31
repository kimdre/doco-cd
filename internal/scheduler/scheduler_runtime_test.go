package scheduler

import (
	"fmt"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/docker"
)

func TestStatusForScheduledJob(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		job           scheduledJob
		cfg           docker.JobScheduleConfig
		runtimeStatus string
		running       bool
		want          string
	}{
		{
			name: "container one_off created without runtime status stays created",
			job: scheduledJob{
				mode:           scheduledJobModeContainer,
				containerState: "created",
			},
			cfg:  docker.JobScheduleConfig{ExecutionMode: docker.JobExecutionModeOneOff},
			want: "created",
		},
		{
			name: "container one_off created with runtime status uses exit code",
			job: scheduledJob{
				mode:           scheduledJobModeContainer,
				containerState: "created",
			},
			cfg:           docker.JobScheduleConfig{ExecutionMode: docker.JobExecutionModeOneOff},
			runtimeStatus: "exited (143)",
			want:          "exited (143)",
		},
		{
			name: "container restart keeps docker state",
			job: scheduledJob{
				mode:            scheduledJobModeContainer,
				containerState:  "exited",
				containerStatus: "Exited (0) 2 seconds ago",
			},
			cfg:  docker.JobScheduleConfig{ExecutionMode: docker.JobExecutionModeRestart},
			want: "exited (0)",
		},
		{
			name: "swarm one_off not rewritten",
			job: scheduledJob{
				mode: scheduledJobModeSwarm,
			},
			cfg:           docker.JobScheduleConfig{ExecutionMode: docker.JobExecutionModeOneOff},
			runtimeStatus: "exited (0)",
			want:          "",
		},
		{
			name: "running state has priority",
			job: scheduledJob{
				mode:           scheduledJobModeContainer,
				containerState: "created",
			},
			cfg:           docker.JobScheduleConfig{ExecutionMode: docker.JobExecutionModeOneOff},
			runtimeStatus: "exited (0)",
			running:       true,
			want:          "running",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := statusForScheduledJob(tt.job, tt.cfg, tt.runtimeStatus, tt.running)
			if got != tt.want {
				t.Fatalf("statusForScheduledJob()=%q want=%q", got, tt.want)
			}
		})
	}
}

// TestSetRuntimeStatesSnapshot_PreservesNewerManualLastRun guards against a
// manual TriggerNow's last_run_at being wiped by the scheduler's next tick.
func TestSetRuntimeStatesSnapshot_PreservesNewerManualLastRun(t *testing.T) {
	key := "container:project/service"
	runtime := newRuntimeStore()

	manualRun := time.Now()
	runtime.setLastRun(key, manualRun)

	// Simulate the loop's next refresh, unaware of the manual run.
	runtime.setStatesSnapshot("", "", map[string]scheduledJobState{
		key: {nextRun: manualRun.Add(time.Hour)},
	})

	got := runtime.statesSnapshot()[key]
	if !got.lastRun.Equal(manualRun) {
		t.Fatalf("expected manually triggered last run %v to survive scheduler refresh, got %v", manualRun, got.lastRun)
	}

	// A genuinely newer lastRun must still win.
	newerRun := manualRun.Add(2 * time.Hour)
	runtime.setStatesSnapshot("", "", map[string]scheduledJobState{
		key: {lastRun: newerRun},
	})

	got = runtime.statesSnapshot()[key]
	if !got.lastRun.Equal(newerRun) {
		t.Fatalf("expected newer scheduler-tracked last run %v to win, got %v", newerRun, got.lastRun)
	}
}

func TestSetRuntimeStatesSnapshotPreservesOtherContexts(t *testing.T) {
	runtime := newRuntimeStore()
	runtime.states = map[string]scheduledJobState{
		"remote::container:shared/job": {deployment: "remote"},
	}

	runtime.setStatesSnapshot("", "", map[string]scheduledJobState{
		"container:shared/job": {deployment: "default"},
	})

	states := runtime.statesSnapshot()
	if len(states) != 2 {
		t.Fatalf("expected both context partitions, got %#v", states)
	}

	runtime.setStatesSnapshot("remote", "", map[string]scheduledJobState{})

	states = runtime.statesSnapshot()
	if _, ok := states["remote::container:shared/job"]; ok {
		t.Fatal("expected stale remote state to be removed")
	}

	if _, ok := states["container:shared/job"]; !ok {
		t.Fatal("expected default state to be preserved")
	}
}

func TestClearRuntimeContext(t *testing.T) {
	runtime := newRuntimeStore()
	runtime.states = map[string]scheduledJobState{
		"container:shared/job":         {deployment: "default"},
		"remote::container:shared/job": {deployment: "remote"},
	}
	runtime.runStatuses = map[string]string{
		"container:shared/job":         "running",
		"remote::container:shared/job": "exited (0)",
	}
	runtime.runningStates = map[string]int{
		"container:shared/job":         1,
		"remote::container:shared/job": 1,
	}

	started := make(chan struct{})
	cleared := make(chan struct{})

	go func() {
		close(started)
		runtime.clearContextMode("remote", "")
		close(cleared)
	}()

	<-started

	select {
	case <-cleared:
		t.Fatal("expected cleanup to wait for the active remote run")
	case <-time.After(10 * time.Millisecond):
	}

	runtime.endRun("remote::container:shared/job")
	<-cleared

	if _, ok := runtime.statesSnapshot()["remote::container:shared/job"]; ok {
		t.Fatal("expected remote runtime state to be removed")
	}

	if _, ok := runtime.runStatusesSnapshot()["remote::container:shared/job"]; ok {
		t.Fatal("expected remote run status to be removed")
	}

	if _, ok := runtime.runningStatesSnapshot()["remote::container:shared/job"]; ok {
		t.Fatal("expected remote running state to be removed")
	}

	if _, ok := runtime.statesSnapshot()["container:shared/job"]; !ok {
		t.Fatal("expected default runtime state to be preserved")
	}
}

func TestRuntimeStoreTracksConcurrentRuns(t *testing.T) {
	t.Parallel()

	runtime := newRuntimeStore()
	key := "container:shared/job"
	runtime.beginRun("", scheduledJobModeContainer, key)
	runtime.beginRun("", scheduledJobModeContainer, key)

	runtime.endRun(key)

	if !runtime.isRunInProgress(key) {
		t.Fatal("expected the second concurrent run to remain active")
	}

	runtime.endRun(key)

	if runtime.isRunInProgress(key) {
		t.Fatal("expected the job to become idle after both runs ended")
	}
}

func TestUpdateRuntimeRunStatus(t *testing.T) {
	t.Parallel()

	runtime := newRuntimeStore()

	job := scheduledJob{
		key:  "container:project/service",
		mode: scheduledJobModeContainer,
	}
	cfg := docker.JobScheduleConfig{ExecutionMode: docker.JobExecutionModeOneOff}

	runtime.updateRunStatus(job, cfg, nil)

	if got := runtime.runStatusesSnapshot()[job.key]; got != "exited (0)" {
		t.Fatalf("updateRuntimeRunStatus() success status=%q want=%q", got, "exited (0)")
	}

	wrapped := fmt.Errorf("scheduled run: %w", &docker.ContainerExitError{ContainerID: "abc", ExitCode: 143})
	runtime.updateRunStatus(job, cfg, wrapped)

	if got := runtime.runStatusesSnapshot()[job.key]; got != "exited (143)" {
		t.Fatalf("updateRuntimeRunStatus() error status=%q want=%q", got, "exited (143)")
	}
}

func TestManagerRuntimeStateIsIsolated(t *testing.T) {
	t.Parallel()

	first := NewManager(nil, nil, nil, nil)
	second := NewManager(nil, nil, nil, nil)
	key := "container:project/service"

	first.runtime.setLastRun(key, time.Now())

	if _, ok := second.runtime.statesSnapshot()[key]; ok {
		t.Fatal("expected scheduler managers to own independent runtime state")
	}
}

func TestRuntimeStoreClearContextModePreservesOtherPartitions(t *testing.T) {
	t.Parallel()

	runtime := newRuntimeStore()
	runtime.states = map[string]scheduledJobState{
		"remote::container:shared/job": {deployment: "compose"},
		"remote::swarm:shared/job":     {deployment: "swarm"},
	}

	runtime.clearContextMode("remote", scheduledJobModeContainer)

	states := runtime.statesSnapshot()
	if _, ok := states["remote::container:shared/job"]; ok {
		t.Fatal("expected Compose runtime state to be removed")
	}

	if _, ok := states["remote::swarm:shared/job"]; !ok {
		t.Fatal("expected Swarm runtime state to be preserved")
	}
}

func TestManagerStopWorkersClearsRuntimeState(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil, nil)
	key := schedulerWorkerKey("remote", scheduledJobModeContainer)
	jobKey := "remote::container:shared/job"
	cancelled := false
	manager.workers[key] = managedWorker{
		cancel: func() { cancelled = true },
		mode:   scheduledJobModeContainer,
		id:     1,
	}
	manager.runtime.setLastRun(jobKey, time.Now())

	manager.stopWorkers()

	if !cancelled {
		t.Fatal("expected managed worker to be cancelled")
	}

	if worker := manager.workers[key]; !worker.stopping {
		t.Fatal("expected managed worker to be marked as stopping")
	}

	manager.workerStopped(key, 1)

	if len(manager.workers) != 0 {
		t.Fatalf("expected stopped worker to be removed, got %d", len(manager.workers))
	}

	if _, ok := manager.runtime.statesSnapshot()[jobKey]; ok {
		t.Fatal("expected worker runtime state to be cleared")
	}
}

func TestManagerOldWorkerCannotClearReplacementState(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil, nil)
	key := schedulerWorkerKey("remote", scheduledJobModeContainer)
	jobKey := "remote::container:shared/job"
	manager.workers[key] = managedWorker{
		cancel: func() {},
		mode:   scheduledJobModeContainer,
		id:     2,
	}
	manager.runtime.setLastRun(jobKey, time.Now())

	manager.workerStopped(key, 1)

	if _, ok := manager.workers[key]; !ok {
		t.Fatal("expected replacement worker to remain registered")
	}

	if _, ok := manager.runtime.statesSnapshot()[jobKey]; !ok {
		t.Fatal("expected replacement worker runtime state to be preserved")
	}
}

func TestJobInfo_ContextField(t *testing.T) {
	t.Parallel()

	// JobInfo.Context must use the external display value ("default"), not
	// the internal normalized (empty-string) representation.
	if got, want := docker.DisplayContextName(""), "default"; got != want {
		t.Fatalf("docker.DisplayContextName(\"\") = %q, want %q", got, want)
	}
}
