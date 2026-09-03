package stages

import (
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/commitstatus"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func TestSuccessfulCommitStatusDescription(t *testing.T) {
	start := time.Date(2026, time.July, 18, 9, 0, 0, 0, time.UTC)

	tests := []struct {
		name       string
		startedAt  time.Time
		finishedAt time.Time
		want       string
	}{
		{
			name:       "missing timestamps",
			startedAt:  time.Time{},
			finishedAt: time.Time{},
			want:       "Successful",
		},
		{
			name:       "sub second duration",
			startedAt:  start,
			finishedAt: start.Add(500 * time.Millisecond),
			want:       "Successful in <1s",
		},
		{
			name:       "whole seconds",
			startedAt:  start,
			finishedAt: start.Add(47 * time.Second),
			want:       "Successful in 47s",
		},
		{
			name:       "multi unit duration",
			startedAt:  start,
			finishedAt: start.Add(time.Minute + 2*time.Second),
			want:       "Successful in 1m2s",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := successfulCommitStatusDescription(tt.startedAt, tt.finishedAt)
			if got != tt.want {
				t.Fatalf("successfulCommitStatusDescription() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDeploymentMetricLabels(t *testing.T) {
	t.Parallel()

	if got := deploymentMetricsRepository(nil); got != "unknown" {
		t.Fatalf("deploymentMetricsRepository(nil) = %q, want unknown", got)
	}

	if got := deploymentMetricsRepository(&RepositoryData{Name: " owner/repo "}); got != "owner/repo" {
		t.Fatalf("deploymentMetricsRepository() = %q, want owner/repo", got)
	}

	if got := deploymentMetricsName(nil); got != "unknown" {
		t.Fatalf("deploymentMetricsName(nil) = %q, want unknown", got)
	}

	if got := deploymentMetricsName(&deploy.Config{Name: " app "}); got != "app" {
		t.Fatalf("deploymentMetricsName() = %q, want app", got)
	}

	if got := deploymentMetricsContext(nil); got != "default" {
		t.Fatalf("deploymentMetricsContext(nil) = %q, want default", got)
	}

	if got := deploymentMetricsContext(&deploy.Config{Context: " remote "}); got != "remote" {
		t.Fatalf("deploymentMetricsContext() = %q, want remote", got)
	}
}

func TestShouldPostPendingCommitStatus(t *testing.T) {
	tests := []struct {
		name           string
		stageName      StageName
		destroyEnabled bool
		pendingPosted  bool
		want           bool
	}{
		{
			name:      "posts after pre deploy",
			stageName: StagePreDeploy,
			want:      true,
		},
		{
			name:      "does not post after init",
			stageName: StageInit,
			want:      false,
		},
		{
			name:           "does not post for destroy",
			stageName:      StagePreDeploy,
			destroyEnabled: true,
			want:           false,
		},
		{
			name:          "does not repost pending",
			stageName:     StagePreDeploy,
			pendingPosted: true,
			want:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldPostPendingCommitStatus(tt.stageName, tt.destroyEnabled, tt.pendingPosted)
			if got != tt.want {
				t.Fatalf("shouldPostPendingCommitStatus() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestShouldSendDeploymentStartedNotification(t *testing.T) {
	tests := []struct {
		name            string
		stageName       StageName
		destroyEnabled  bool
		startedNotified bool
		want            bool
	}{
		{
			name:      "sends after pre deploy",
			stageName: StagePreDeploy,
			want:      true,
		},
		{
			name:      "does not send after init",
			stageName: StageInit,
			want:      false,
		},
		{
			name:           "does not send for destroy",
			stageName:      StagePreDeploy,
			destroyEnabled: true,
			want:           false,
		},
		{
			name:            "does not resend after sent",
			stageName:       StagePreDeploy,
			startedNotified: true,
			want:            false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldSendDeploymentStartedNotification(tt.stageName, tt.destroyEnabled, tt.startedNotified)
			if got != tt.want {
				t.Fatalf("shouldSendDeploymentStartedNotification() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestShouldPostFailureCommitStatus(t *testing.T) {
	tests := []struct {
		name           string
		destroyEnabled bool
		want           bool
	}{
		{
			name: "posts for init failures",
			want: true,
		},
		{
			name: "posts for pre deploy failures",
			want: true,
		},
		{
			name: "posts for deploy failures",
			want: true,
		},
		{
			name:           "does not post for destroy",
			destroyEnabled: true,
			want:           false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldPostFailureCommitStatus(tt.destroyEnabled)
			if got != tt.want {
				t.Fatalf("shouldPostFailureCommitStatus() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestShouldPostWebhookCommitStatus(t *testing.T) {
	tests := []struct {
		name           string
		jobTrigger     JobTrigger
		destroyEnabled bool
		want           bool
	}{
		{name: "webhook deployment", jobTrigger: JobTriggerWebhook, want: true},
		{name: "poll deployment", jobTrigger: JobTriggerPoll, want: false},
		{name: "webhook destroy", jobTrigger: JobTriggerWebhook, destroyEnabled: true, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldPostWebhookCommitStatus(tt.jobTrigger, tt.destroyEnabled)
			if got != tt.want {
				t.Fatalf("shouldPostWebhookCommitStatus() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestMatchesWebhookEventFilter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		trigger JobTrigger
		filter  string
		ref     string
		want    bool
	}{
		{name: "matching webhook", trigger: JobTriggerWebhook, filter: `^refs/heads/main$`, ref: "refs/heads/main", want: true},
		{name: "non-matching webhook", trigger: JobTriggerWebhook, filter: `^refs/heads/main$`, ref: "refs/heads/feature", want: false},
		{name: "webhook without filter", trigger: JobTriggerWebhook, ref: "refs/heads/feature", want: true},
		{name: "poll ignores filter", trigger: JobTriggerPoll, filter: `^refs/heads/main$`, ref: "refs/heads/feature", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sm := &StageManager{
				JobTrigger:   tt.trigger,
				DeployConfig: &deploy.Config{WebhookEventFilter: tt.filter},
				Payload:      &webhook.ParsedPayload{Ref: tt.ref},
			}

			if got := sm.MatchesWebhookEventFilter(); got != tt.want {
				t.Fatalf("MatchesWebhookEventFilter() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestFailureCommitStatusState(t *testing.T) {
	tests := []struct {
		name      string
		stageName StageName
		want      commitstatus.State
	}{
		{
			name:      "init uses error",
			stageName: StageInit,
			want:      commitstatus.StateError,
		},
		{
			name:      "pre deploy uses error",
			stageName: StagePreDeploy,
			want:      commitstatus.StateError,
		},
		{
			name:      "deploy uses failure",
			stageName: StageDeploy,
			want:      commitstatus.StateFailure,
		},
		{
			name:      "cleanup uses failure",
			stageName: StageCleanup,
			want:      commitstatus.StateFailure,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := failureCommitStatusState(tt.stageName)
			if got != tt.want {
				t.Fatalf("failureCommitStatusState() = %q, want %q", got, tt.want)
			}
		})
	}
}
