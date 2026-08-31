package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/docker/compose/v5/pkg/api"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
)

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
