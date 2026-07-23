package scheduler

import (
	"errors"
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

	updateRuntimeRunStatus(job, cfg, errors.New("one-off container abc exited with status 143"))

	if got := getRuntimeRunStatusesSnapshot()[job.key]; got != "exited (143)" {
		t.Fatalf("updateRuntimeRunStatus() error status=%q want=%q", got, "exited (143)")
	}
}
