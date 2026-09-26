package scheduler

import (
	"errors"
	"slices"
	"testing"

	"github.com/docker/compose/v5/pkg/api"

	"github.com/kimdre/doco-cd/internal/docker"
)

func TestSchedulerModes(t *testing.T) {
	t.Parallel()

	if got := schedulerModes(false); !slices.Equal(got, []scheduledJobMode{scheduledJobModeContainer}) {
		t.Fatalf("schedulerModes(false) = %v, want compose only", got)
	}

	if got := schedulerModes(true); !slices.Equal(got, []scheduledJobMode{scheduledJobModeContainer, scheduledJobModeSwarm}) {
		t.Fatalf("schedulerModes(true) = %v, want compose and swarm", got)
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

func TestFindRunnableJob_Ownership(t *testing.T) {
	t.Parallel()

	ownerLabels := map[string]string{
		docker.DocoCDJobLabels.JobEnabled:  "true",
		docker.DocoCDJobLabels.JobSchedule: "@every 1m",
		docker.DocoCDJobLabels.JobOwner:    "host-a",
	}
	unownedLabels := map[string]string{
		docker.DocoCDJobLabels.JobEnabled:  "true",
		docker.DocoCDJobLabels.JobSchedule: "@hourly",
	}

	tests := []struct {
		name     string
		job      scheduledJob
		policy   OwnershipOptions
		rejected bool
	}{
		{name: "owner on default context", job: scheduledJob{name: "backup", labels: ownerLabels}, policy: OwnershipOptions{InstanceID: "host-a", RequireOwnerContexts: []string{"default"}}},
		{name: "different context alias and owner", job: scheduledJob{name: "backup", context: "docker-host", labels: ownerLabels}, policy: OwnershipOptions{InstanceID: "host-b", RequireOwnerContexts: []string{"docker-host"}}, rejected: true},
		{name: "unowned strict", job: scheduledJob{name: "backup", labels: unownedLabels}, policy: OwnershipOptions{InstanceID: "host-a", RequireOwnerContexts: []string{"default"}}, rejected: true},
		{name: "unowned local", job: scheduledJob{name: "backup", labels: unownedLabels}, policy: OwnershipOptions{InstanceID: "host-b", RequireOwnerContexts: []string{"docker-host"}}},
		{name: "unowned legacy", job: scheduledJob{name: "backup", labels: unownedLabels}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, _, err := findRunnableJob([]scheduledJob{tc.job}, "backup", "", tc.policy)
			if tc.rejected && !errors.Is(err, ErrScheduledJobNotOwned) {
				t.Fatalf("error = %v, want ErrScheduledJobNotOwned", err)
			}

			if !tc.rejected && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
