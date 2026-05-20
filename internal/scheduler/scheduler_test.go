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

func TestShouldTriggerRunOnDeploy(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, time.May, 12, 10, 0, 0, 0, time.UTC)
	startedAtSubSecond := startedAt.Add(500 * time.Millisecond)
	deployedAt := time.Date(2026, time.May, 12, 10, 5, 0, 0, time.UTC)

	tests := []struct {
		name                 string
		cfg                  docker.JobScheduleConfig
		deploymentID         string
		deploymentAt         time.Time
		schedulerStartedAt   time.Time
		stateExists          bool
		previousDeploymentID string
		want                 bool
	}{
		{
			name:         "disabled label",
			cfg:          docker.JobScheduleConfig{RunOnDeploy: false},
			deploymentID: "dep-1",
			deploymentAt: deployedAt,
			schedulerStartedAt: startedAt,
			want:         false,
		},
		{
			name:         "deployment before scheduler start",
			cfg:          docker.JobScheduleConfig{RunOnDeploy: true},
			deploymentID: "dep-1",
			deploymentAt: startedAt,
			schedulerStartedAt: startedAt,
			want:         false,
		},
		{
			name:         "deployment timestamp in same second as sub-second scheduler start",
			cfg:          docker.JobScheduleConfig{RunOnDeploy: true},
			deploymentID: "dep-1",
			deploymentAt: startedAt,
			schedulerStartedAt: startedAtSubSecond,
			want:         true,
		},
		{
			name:         "deployment older than scheduler start second",
			cfg:          docker.JobScheduleConfig{RunOnDeploy: true},
			deploymentID: "dep-1",
			deploymentAt: startedAt.Add(-time.Second),
			schedulerStartedAt: startedAtSubSecond,
			want:         false,
		},
		{
			name:         "initial discovery after deploy",
			cfg:          docker.JobScheduleConfig{RunOnDeploy: true},
			deploymentID: "dep-2",
			deploymentAt: deployedAt,
			schedulerStartedAt: startedAt,
			want:         true,
		},
		{
			name:                 "already processed deployment",
			cfg:                  docker.JobScheduleConfig{RunOnDeploy: true},
			deploymentID:         "dep-2",
			deploymentAt:         deployedAt,
			schedulerStartedAt:   startedAt,
			stateExists:          true,
			previousDeploymentID: "dep-2",
			want:                 false,
		},
		{
			name:                 "new deployment after previous",
			cfg:                  docker.JobScheduleConfig{RunOnDeploy: true},
			deploymentID:         "dep-3",
			deploymentAt:         deployedAt.Add(time.Minute),
			schedulerStartedAt:   startedAt,
			stateExists:          true,
			previousDeploymentID: "dep-2",
			want:                 true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := shouldTriggerRunOnDeploy(
				tt.cfg,
				tt.deploymentID,
				tt.deploymentAt,
				tt.schedulerStartedAt,
				tt.stateExists,
				tt.previousDeploymentID,
			)

			if got != tt.want {
				t.Fatalf("shouldTriggerRunOnDeploy()=%v want=%v", got, tt.want)
			}
		})
	}
}
