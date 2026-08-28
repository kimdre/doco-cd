package scheduler

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/docker/compose/v5/pkg/api"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
)

func TestNextScheduledRun_PreservesScheduleAlignment(t *testing.T) {
	t.Parallel()

	schedule, err := docker.ParseJobScheduleExpression("@every 1m")
	if err != nil {
		t.Fatalf("ParseJobScheduleExpression() failed: %v", err)
	}

	scheduledAt := time.Date(2026, time.May, 9, 12, 0, 0, 0, time.UTC)
	now := scheduledAt.Add(250 * time.Millisecond)

	got := nextScheduledRun(schedule, scheduledAt, now)

	want := scheduledAt.Add(time.Minute)
	if !got.Equal(want) {
		t.Fatalf("nextScheduledRun() = %s, want %s", got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
}

func TestSchedulerModes(t *testing.T) {
	t.Parallel()

	if got := schedulerModes(false); !slices.Equal(got, []scheduledJobMode{scheduledJobModeContainer}) {
		t.Fatalf("schedulerModes(false) = %v, want compose only", got)
	}

	if got := schedulerModes(true); !slices.Equal(got, []scheduledJobMode{scheduledJobModeContainer, scheduledJobModeSwarm}) {
		t.Fatalf("schedulerModes(true) = %v, want compose and swarm", got)
	}
}

func TestNextScheduledRun_SkipsMissedRunsWithoutDrift(t *testing.T) {
	t.Parallel()

	schedule, err := docker.ParseJobScheduleExpression("@every 1m")
	if err != nil {
		t.Fatalf("ParseJobScheduleExpression() failed: %v", err)
	}

	scheduledAt := time.Date(2026, time.May, 9, 12, 0, 0, 0, time.UTC)
	now := scheduledAt.Add(3*time.Minute + 30*time.Second)

	got := nextScheduledRun(schedule, scheduledAt, now)

	want := time.Date(2026, time.May, 9, 12, 4, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("nextScheduledRun() = %s, want %s", got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
}

func TestGetNearestNextRun(t *testing.T) {
	t.Parallel()

	want := time.Date(2026, time.May, 9, 12, 1, 0, 0, time.UTC)

	got, ok := getNearestNextRun(map[string]scheduledJobState{
		"later": {
			nextRun: time.Date(2026, time.May, 9, 12, 5, 0, 0, time.UTC),
		},
		"earlier": {
			nextRun: want,
		},
		"zero": {},
	})
	if !ok {
		t.Fatalf("getNearestNextRun() reported no next run")
	}

	if !got.Equal(want) {
		t.Fatalf("getNearestNextRun() = %s, want %s", got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
}

func TestGetJobStackName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{
			name: "deployment label has priority",
			labels: map[string]string{
				docker.DocoCDLabels.Deployment.Name: "doco-stack",
				api.ProjectLabel:                    "compose-project",
				swarm.StackNamespaceLabel:           "swarm-stack",
			},
			want: "doco-stack",
		},
		{
			name: "swarm namespace fallback",
			labels: map[string]string{
				swarm.StackNamespaceLabel: "swarm-stack",
			},
			want: "swarm-stack",
		},
		{
			name: "compose project fallback",
			labels: map[string]string{
				api.ProjectLabel: "compose-project",
			},
			want: "compose-project",
		},
		{
			name:   "missing labels",
			labels: map[string]string{},
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := getJobStackName(scheduledJob{labels: tt.labels})
			if got != tt.want {
				t.Fatalf("getJobStackName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFindRunnableJob(t *testing.T) {
	t.Parallel()

	validLabels := map[string]string{
		docker.DocoCDJobLabels.JobEnabled:  "true",
		docker.DocoCDJobLabels.JobSchedule: "@every 1m",
	}

	tests := []struct {
		name      string
		jobs      []scheduledJob
		jobName   string
		stackName string
		wantErr   error
	}{
		{
			name: "single matching job",
			jobs: []scheduledJob{
				{name: "stack-backup-1", labels: validLabels},
			},
			jobName: "stack-backup-1",
		},
		{
			name: "stack filter avoids ambiguity",
			jobs: []scheduledJob{
				{name: "backup", labels: map[string]string{docker.DocoCDJobLabels.JobEnabled: "true", docker.DocoCDJobLabels.JobSchedule: "@every 1m", api.ProjectLabel: "stack-a"}},
				{name: "backup", labels: map[string]string{docker.DocoCDJobLabels.JobEnabled: "true", docker.DocoCDJobLabels.JobSchedule: "@every 1m", api.ProjectLabel: "stack-b"}},
			},
			jobName:   "backup",
			stackName: "stack-a",
		},
		{
			name: "job not found",
			jobs: []scheduledJob{
				{name: "other", labels: validLabels},
			},
			jobName: "backup",
			wantErr: ErrScheduledJobNotFound,
		},
		{
			name: "job disabled",
			jobs: []scheduledJob{
				{name: "backup", labels: map[string]string{docker.DocoCDJobLabels.JobEnabled: "false", docker.DocoCDJobLabels.JobSchedule: "@every 1m"}},
			},
			jobName: "backup",
			wantErr: ErrScheduledJobDisabled,
		},
		{
			name: "ambiguous job name",
			jobs: []scheduledJob{
				{name: "backup", labels: validLabels},
				{name: "backup", labels: validLabels},
			},
			jobName: "backup",
			wantErr: ErrScheduledJobAmbiguous,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, _, err := findRunnableJob(tt.jobs, tt.jobName, tt.stackName)
			if tt.wantErr == nil && err != nil {
				t.Fatalf("findRunnableJob() unexpected error = %v", err)
			}

			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("findRunnableJob() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestParseJobScheduleExpression_NextRunUsesLocalTimezone_Berlin(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("time.LoadLocation() failed: %v", err)
	}

	originalLocal := time.Local
	time.Local = berlin

	t.Cleanup(func() {
		time.Local = originalLocal
	})

	schedule, err := docker.ParseJobScheduleExpression("0 */6 * * *")
	if err != nil {
		t.Fatalf("ParseJobScheduleExpression() failed: %v", err)
	}

	now := time.Date(2026, time.May, 11, 0, 30, 0, 0, time.Local)
	got := schedule.Next(now)
	want := time.Date(2026, time.May, 11, 6, 0, 0, 0, time.Local)

	if !got.Equal(want) {
		t.Fatalf("schedule.Next() = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

func TestGetJobDeploymentIdentity(t *testing.T) {
	t.Parallel()

	timestamp := "2026-05-12T12:30:00Z"
	id, at := getJobDeploymentIdentity(map[string]string{
		docker.DocoCDLabels.Deployment.Timestamp:   timestamp,
		docker.DocoCDLabels.Deployment.ComposeHash: "compose-sha",
		docker.DocoCDLabels.Deployment.CommitSHA:   "commit-sha",
	})

	if id != timestamp {
		t.Fatalf("getJobDeploymentIdentity() id=%q want=%q", id, timestamp)
	}

	wantAt := time.Date(2026, time.May, 12, 12, 30, 0, 0, time.UTC)
	if !at.Equal(wantAt) {
		t.Fatalf("getJobDeploymentIdentity() at=%s want=%s", at.Format(time.RFC3339), wantAt.Format(time.RFC3339))
	}
}

func TestShouldStopContainerForOneOffDeployRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		job  scheduledJob
		cfg  docker.JobScheduleConfig
		want bool
	}{
		{
			name: "container one_off",
			job:  scheduledJob{mode: scheduledJobModeContainer},
			cfg:  docker.JobScheduleConfig{ExecutionMode: docker.JobExecutionModeOneOff},
			want: true,
		},
		{
			name: "container restart",
			job:  scheduledJob{mode: scheduledJobModeContainer},
			cfg:  docker.JobScheduleConfig{ExecutionMode: docker.JobExecutionModeRestart},
			want: false,
		},
		{
			name: "swarm one_off",
			job:  scheduledJob{mode: scheduledJobModeSwarm},
			cfg:  docker.JobScheduleConfig{ExecutionMode: docker.JobExecutionModeOneOff},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := shouldStopContainerForOneOffDeployRun(tt.job, tt.cfg)
			if got != tt.want {
				t.Fatalf("shouldStopContainerForOneOffDeployRun()=%v want=%v", got, tt.want)
			}
		})
	}
}

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

	runtimeStatesMu.Lock()
	runtimeStates = map[string]scheduledJobState{}
	runtimeStatesMu.Unlock()

	manualRun := time.Now()
	setRuntimeLastRun(key, manualRun)

	// Simulate the loop's next refresh, unaware of the manual run.
	setRuntimeStatesSnapshot("", map[string]scheduledJobState{
		key: {nextRun: manualRun.Add(time.Hour)},
	})

	got := getRuntimeStatesSnapshot()[key]
	if !got.lastRun.Equal(manualRun) {
		t.Fatalf("expected manually triggered last run %v to survive scheduler refresh, got %v", manualRun, got.lastRun)
	}

	// A genuinely newer lastRun must still win.
	newerRun := manualRun.Add(2 * time.Hour)
	setRuntimeStatesSnapshot("", map[string]scheduledJobState{
		key: {lastRun: newerRun},
	})

	got = getRuntimeStatesSnapshot()[key]
	if !got.lastRun.Equal(newerRun) {
		t.Fatalf("expected newer scheduler-tracked last run %v to win, got %v", newerRun, got.lastRun)
	}
}

func TestSetRuntimeStatesSnapshotPreservesOtherContexts(t *testing.T) {
	runtimeStatesMu.Lock()
	runtimeStates = map[string]scheduledJobState{
		"remote::container:shared/job": {deployment: "remote"},
	}
	runtimeStatesMu.Unlock()

	setRuntimeStatesSnapshot("", map[string]scheduledJobState{
		"container:shared/job": {deployment: "default"},
	})

	states := getRuntimeStatesSnapshot()
	if len(states) != 2 {
		t.Fatalf("expected both context partitions, got %#v", states)
	}

	setRuntimeStatesSnapshot("remote", map[string]scheduledJobState{})

	states = getRuntimeStatesSnapshot()
	if _, ok := states["remote::container:shared/job"]; ok {
		t.Fatal("expected stale remote state to be removed")
	}

	if _, ok := states["container:shared/job"]; !ok {
		t.Fatal("expected default state to be preserved")
	}
}

func TestClearRuntimeContext(t *testing.T) {
	t.Cleanup(func() {
		runtimeStatesMu.Lock()
		runtimeStates = map[string]scheduledJobState{}
		runtimeRunStatuses = map[string]string{}
		runtimeRunningStates = map[string]bool{}
		runtimeStatesMu.Unlock()
	})

	runtimeStatesMu.Lock()
	runtimeStates = map[string]scheduledJobState{
		"container:shared/job":         {deployment: "default"},
		"remote::container:shared/job": {deployment: "remote"},
	}
	runtimeRunStatuses = map[string]string{
		"container:shared/job":         "running",
		"remote::container:shared/job": "exited (0)",
	}
	runtimeRunningStates = map[string]bool{
		"container:shared/job":         true,
		"remote::container:shared/job": true,
	}
	runtimeStatesMu.Unlock()

	clearRuntimeContext("remote")

	if _, ok := getRuntimeStatesSnapshot()["remote::container:shared/job"]; ok {
		t.Fatal("expected remote runtime state to be removed")
	}

	if _, ok := getRuntimeRunStatusesSnapshot()["remote::container:shared/job"]; ok {
		t.Fatal("expected remote run status to be removed")
	}

	if _, ok := getRuntimeRunningStatesSnapshot()["remote::container:shared/job"]; ok {
		t.Fatal("expected remote running state to be removed")
	}

	if _, ok := getRuntimeStatesSnapshot()["container:shared/job"]; !ok {
		t.Fatal("expected default runtime state to be preserved")
	}
}

func TestUpdateRuntimeRunStatus(t *testing.T) {
	t.Parallel()

	runtimeStatesMu.Lock()
	runtimeRunStatuses = map[string]string{}
	runtimeStatesMu.Unlock()

	job := scheduledJob{
		key:  "container:project/service",
		mode: scheduledJobModeContainer,
	}
	cfg := docker.JobScheduleConfig{ExecutionMode: docker.JobExecutionModeOneOff}

	updateRuntimeRunStatus(job, cfg, nil)

	if got := getRuntimeRunStatusesSnapshot()[job.key]; got != "exited (0)" {
		t.Fatalf("updateRuntimeRunStatus() success status=%q want=%q", got, "exited (0)")
	}

	wrapped := fmt.Errorf("scheduled run: %w", &docker.ContainerExitError{ContainerID: "abc", ExitCode: 143})
	updateRuntimeRunStatus(job, cfg, wrapped)

	if got := getRuntimeRunStatusesSnapshot()[job.key]; got != "exited (143)" {
		t.Fatalf("updateRuntimeRunStatus() error status=%q want=%q", got, "exited (143)")
	}
}

func TestIsEphemeralScheduledContainer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{
			name: "missing label",
			labels: map[string]string{
				docker.DocoCDJobLabels.JobEnabled: "true",
			},
			want: false,
		},
		{
			name: "ephemeral true",
			labels: map[string]string{
				docker.DocoCDJobLabels.JobEphemeral: "true",
			},
			want: true,
		},
		{
			name: "ephemeral false",
			labels: map[string]string{
				docker.DocoCDJobLabels.JobEphemeral: "false",
			},
			want: false,
		},
		{
			name: "invalid boolean",
			labels: map[string]string{
				docker.DocoCDJobLabels.JobEphemeral: "not-bool",
			},
			want: false,
		},
		{
			name: "compose one-off label true",
			labels: map[string]string{
				api.OneoffLabel: "True",
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := isEphemeralScheduledContainer(tt.labels)
			if got != tt.want {
				t.Fatalf("isEphemeralScheduledContainer()=%v want=%v", got, tt.want)
			}
		})
	}
}

func TestContainerJobKey_EphemeralMatchesSource(t *testing.T) {
	t.Parallel()

	sourceLabels := map[string]string{
		api.ProjectLabel: "my-stack",
		api.ServiceLabel: "nas-backup",
	}

	// The ephemeral one_off clone copies the source labels and adds the ephemeral
	// marker, so it must resolve to the same key to be attributed to the source job.
	ephemeralLabels := map[string]string{
		api.ProjectLabel:                    "my-stack",
		api.ServiceLabel:                    "nas-backup",
		docker.DocoCDJobLabels.JobEphemeral: "true",
	}

	sourceKey := containerJobKey("source-id", sourceLabels)
	ephemeralKey := containerJobKey("ephemeral-id", ephemeralLabels)

	if sourceKey != ephemeralKey {
		t.Fatalf("containerJobKey() ephemeral=%q source=%q, want equal", ephemeralKey, sourceKey)
	}

	if want := "container:my-stack/nas-backup"; sourceKey != want {
		t.Fatalf("containerJobKey()=%q want=%q", sourceKey, want)
	}
}

func TestContainerJobKey_FallsBackToContainerID(t *testing.T) {
	t.Parallel()

	got := containerJobKey("abc123", map[string]string{api.ProjectLabel: "only-project"})
	if want := "container:abc123"; got != want {
		t.Fatalf("containerJobKey()=%q want=%q", got, want)
	}
}

func TestJobOwnIdentity(t *testing.T) {
	t.Parallel()

	t.Run("compose job uses compose project/service labels", func(t *testing.T) {
		t.Parallel()

		job := scheduledJob{
			mode: scheduledJobModeContainer,
			name: "irrelevant-container-name",
			labels: map[string]string{
				api.ProjectLabel: "myproject",
				api.ServiceLabel: "backup",
			},
		}

		project, service := jobOwnIdentity(job)
		if project != "myproject" || service != "backup" {
			t.Fatalf("jobOwnIdentity() = (%q, %q), want (\"myproject\", \"backup\")", project, service)
		}
	})

	t.Run("swarm job derives service from full service name", func(t *testing.T) {
		t.Parallel()

		job := scheduledJob{
			mode: scheduledJobModeSwarm,
			name: "mystack_backup",
			labels: map[string]string{
				swarm.StackNamespaceLabel: "mystack",
			},
		}

		project, service := jobOwnIdentity(job)
		if project != "mystack" || service != "backup" {
			t.Fatalf("jobOwnIdentity() = (%q, %q), want (\"mystack\", \"backup\")", project, service)
		}
	})
}

func TestValidateStopServicesSelfReference_SwarmIdentity(t *testing.T) {
	t.Parallel()

	// Regression test: swarm task labels never carry com.docker.compose.project
	// or com.docker.compose.service, so self-reference detection must use the
	// resolved stack name and the service name derived from the full swarm
	// service name (see jobOwnIdentity), not raw compose labels.
	job := scheduledJob{
		mode: scheduledJobModeSwarm,
		name: "mystack_backup",
		labels: map[string]string{
			swarm.StackNamespaceLabel:               "mystack",
			docker.DocoCDJobLabels.JobEnabled:       "true",
			docker.DocoCDJobLabels.JobSchedule:      "0 2 * * *",
			docker.DocoCDJobLabels.JobExecutionMode: "one_off",
			docker.DocoCDJobLabels.JobStopServices:  "backup",
		},
	}

	cfg, enabled, err := parseJobConfig(job)
	if err == nil {
		t.Fatalf("expected self-reference error for swarm job, got nil (enabled=%v cfg=%+v)", enabled, cfg)
	}

	if !strings.Contains(err.Error(), "cannot stop itself") {
		t.Fatalf("expected self-reference error, got: %v", err)
	}
}

func TestResolveStopServiceStacks(t *testing.T) {
	t.Parallel()

	refs := []docker.StopServiceRef{
		{Service: "db"},
		{Project: "other-project", Service: "cache"},
		{Project: "other-project", Service: "search"},
	}

	got := resolveStopServiceStacks(refs, "own-project")

	want := []string{"own-project", "other-project", "other-project"}
	if len(got) != len(want) {
		t.Fatalf("resolveStopServiceStacks() = %v, want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("resolveStopServiceStacks()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLockStacks_DeduplicatesAndLocksSortedOrder(t *testing.T) {
	t.Parallel()

	// Repeated/empty entries must not cause a self-deadlock (locking the same
	// stack twice) and must not be double-unlocked.
	unlock := lockStacks("", "zeta", "alpha", "alpha", "", "zeta")
	unlock()

	// If dedup/unlock bookkeeping were broken, acquiring the same stacks again
	// would deadlock (this call would hang forever), so a normal test timeout
	// failure would catch it.
	unlock2 := lockStacks("", "alpha", "zeta")
	unlock2()
}

func TestLockStacks_SameStackDifferentContextsDoNotBlock(t *testing.T) {
	t.Parallel()

	// Two different Docker contexts using the same stack name must not share
	// a lock (see lock.StackKey): locking "prod" on "context-a" must not block
	// locking "prod" on "context-b" concurrently.
	unlockA := lockStacks("context-a", "prod")

	done := make(chan struct{})

	go func() {
		defer close(done)

		unlockB := lockStacks("context-b", "prod")
		unlockB()
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("lockStacks() for the same stack name on a different context blocked; expected independent locks")
	}

	unlockA()
}

func TestStopHold_RefCounting(t *testing.T) {
	t.Parallel()

	s := &scheduler{stopHolds: map[stopHoldKey]*stopHoldState{}}

	// First holder must actually perform the stop.
	if isFirst := s.acquireStopHold(scheduledJobModeContainer, "proj", "db"); !isFirst {
		t.Fatal("expected first acquireStopHold() to report isFirst=true")
	}

	// A second concurrent holder of the same target must not stop it again.
	if isFirst := s.acquireStopHold(scheduledJobModeContainer, "proj", "db"); isFirst {
		t.Fatal("expected second acquireStopHold() to report isFirst=false")
	}

	// Releasing while another holder remains must not trigger a restart.
	if isLast, _ := s.releaseStopHold(scheduledJobModeContainer, "proj", "db"); isLast {
		t.Fatal("expected first releaseStopHold() to report isLast=false while a holder remains")
	}

	// The last holder releasing must trigger the actual restart.
	if isLast, _ := s.releaseStopHold(scheduledJobModeContainer, "proj", "db"); !isLast {
		t.Fatal("expected final releaseStopHold() to report isLast=true")
	}

	// The hold must be fully removed once released by the last holder.
	if _, ok := s.stopHolds[stopHoldKey{mode: scheduledJobModeContainer, project: "proj", service: "db"}]; ok {
		t.Fatal("expected stop hold to be removed after last release")
	}
}

func TestStopHold_SwarmReplicasSurviveUntilLastRelease(t *testing.T) {
	t.Parallel()

	s := &scheduler{stopHolds: map[stopHoldKey]*stopHoldState{}}

	// Two concurrent jobs both stop the same shared swarm service.
	if isFirst := s.acquireStopHold(scheduledJobModeSwarm, "stack", "shared"); !isFirst {
		t.Fatal("expected first acquireStopHold() to report isFirst=true")
	}

	// Only the first holder actually observes/records the original replica count.
	s.setStopHoldReplicas(scheduledJobModeSwarm, "stack", "shared", 3)

	if isFirst := s.acquireStopHold(scheduledJobModeSwarm, "stack", "shared"); isFirst {
		t.Fatal("expected second acquireStopHold() to report isFirst=false")
	}

	// The second job finishes first and releases its hold: since the first
	// job is still holding it, this must NOT restore the service yet.
	if isLast, _ := s.releaseStopHold(scheduledJobModeSwarm, "stack", "shared"); isLast {
		t.Fatal("expected release while a holder remains to report isLast=false")
	}

	// The first job finishes and releases its hold: now it must restore, and
	// the originally recorded replica count must still be intact.
	isLast, replicas := s.releaseStopHold(scheduledJobModeSwarm, "stack", "shared")
	if !isLast {
		t.Fatal("expected final release to report isLast=true")
	}

	if replicas != 3 {
		t.Fatalf("replicas = %d, want 3", replicas)
	}
}

func TestStopHold_IsolatedAcrossContexts(t *testing.T) {
	t.Parallel()

	// Two scheduler workers on different Docker contexts must not share a
	// stop hold for the same project/service name: a hold acquired on one
	// context must not be visible to (or block) the other context's worker.
	sDefault := &scheduler{contextName: "", stopHolds: map[stopHoldKey]*stopHoldState{}}
	sRemote := &scheduler{contextName: "remote", stopHolds: map[stopHoldKey]*stopHoldState{}}

	if isFirst := sDefault.acquireStopHold(scheduledJobModeContainer, "proj", "db"); !isFirst {
		t.Fatal("expected first acquireStopHold() on default context to report isFirst=true")
	}

	// The same project/service on a different context must be an independent
	// hold, so this must also report isFirst=true.
	if isFirst := sRemote.acquireStopHold(scheduledJobModeContainer, "proj", "db"); !isFirst {
		t.Fatal("expected first acquireStopHold() on remote context to report isFirst=true, holds leaked across contexts")
	}

	if isLast, _ := sRemote.releaseStopHold(scheduledJobModeContainer, "proj", "db"); !isLast {
		t.Fatal("expected releaseStopHold() on remote context to report isLast=true")
	}

	// Releasing the remote context's hold must not have touched the default
	// context's hold.
	if isLast, _ := sDefault.releaseStopHold(scheduledJobModeContainer, "proj", "db"); !isLast {
		t.Fatal("expected releaseStopHold() on default context to report isLast=true")
	}
}

func TestJobKeyPrefix(t *testing.T) {
	t.Parallel()

	if got := jobKeyPrefix(""); got != "" {
		t.Fatalf("jobKeyPrefix(\"\") = %q, want empty (default context keeps unprefixed keys)", got)
	}

	if got, want := jobKeyPrefix("remote"), "remote::"; got != want {
		t.Fatalf("jobKeyPrefix(%q) = %q, want %q", "remote", got, want)
	}
}

func TestGetScheduledRunMetricLabels_IncludesContext(t *testing.T) {
	t.Parallel()

	cfg := docker.JobScheduleConfig{ExecutionMode: docker.JobExecutionModeOneOff}

	defaultJob := scheduledJob{name: "backup", mode: scheduledJobModeContainer, context: ""}
	if got, want := getScheduledRunMetricLabels(defaultJob, cfg, "stack"), []string{"default", "stack", "backup", "container", "one_off"}; !slices.Equal(got, want) {
		t.Fatalf("getScheduledRunMetricLabels() = %v, want %v", got, want)
	}

	remoteJob := scheduledJob{name: "backup", mode: scheduledJobModeContainer, context: "remote"}
	if got, want := getScheduledRunMetricLabels(remoteJob, cfg, "stack"), []string{"remote", "stack", "backup", "container", "one_off"}; !slices.Equal(got, want) {
		t.Fatalf("getScheduledRunMetricLabels() = %v, want %v", got, want)
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

func TestNewSchedulerForMode_NormalizesContextAndCarriesMode(t *testing.T) {
	t.Parallel()

	s := newSchedulerForMode(docker.ContextClient{Name: "Default", SwarmMode: true}, scheduledJobModeSwarm, nil, nil, nil)
	if s.contextName != "" {
		t.Fatalf("newSchedulerForMode() contextName = %q, want empty string for the default context", s.contextName)
	}

	if s.mode != scheduledJobModeSwarm {
		t.Fatalf("newSchedulerForMode() mode = %q, want %q", s.mode, scheduledJobModeSwarm)
	}

	remote := newSchedulerForMode(docker.ContextClient{Name: "remote", SwarmMode: false}, scheduledJobModeContainer, nil, nil, nil)
	if remote.contextName != "remote" {
		t.Fatalf("newSchedulerForMode() contextName = %q, want %q", remote.contextName, "remote")
	}

	if remote.mode != scheduledJobModeContainer {
		t.Fatalf("newSchedulerForMode() mode = %q, want %q", remote.mode, scheduledJobModeContainer)
	}
}
