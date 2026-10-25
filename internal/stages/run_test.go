package stages

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/kimdre/doco-cd/internal/commitstatus"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func TestAutostartDisabledPollSkipHasNoDeploymentSideEffects(t *testing.T) {
	t.Parallel()

	var statusRequests atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		statusRequests.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	s := newTestStageManager(t)
	s.JobTrigger = JobTriggerPoll
	s.DeployConfig.Name = "autostart-poll-skip"
	s.AppConfig.GitCommitStatus = true
	s.AppConfig.GitScmProvider = "gitea"
	s.AppConfig.GitScmApiUrl = config.HttpUrl(server.URL)
	s.Repository.Source = config.SourceTypeGit
	s.Repository.SourceUrl = server.URL + "/owner/repo.git"
	s.Repository.Revision = "0123456789abcdef0123456789abcdef01234567"
	s.Docker.Project = &types.Project{Services: types.Services{"web": {
		Name: "web", Restart: "unless-stopped",
		Labels: types.Labels{docker.DocoCDLabels.Deployment.Autostart: "false"},
	}}}
	sender := &levelGatedSender{minLevel: notification.Info}
	s.Notifier = sender

	if _, ok := s.resolveCommitStatusRequest(); !ok {
		t.Fatal("commit status must be enabled to verify that skipped polls do not post it")
	}

	order := s.GetDeployStageOrder()

	var entered []StageName

	for _, stage := range order.Order {
		order.Funcs[stage] = func(context.Context, *slog.Logger) error {
			entered = append(entered, stage)
			return nil
		}
	}

	order.Funcs[StagePreDeploy] = func(context.Context, *slog.Logger) error {
		entered = append(entered, StagePreDeploy)

		mismatches, err := docker.CheckServiceMismatch(false,
			map[docker.Service]docker.ServiceStatus{"web": {Containers: 1}}, s.Docker.Project.Services)
		if err != nil {
			return err
		}

		if shouldSkipDeployment(false, false, false, nil, docker.IgnoredInfo{}, false, false, mismatches) {
			return skipDeployment(SkipReasonNoChanges, "")
		}

		return nil
	}

	for poll := range 3 {
		entered = nil
		admitted := false

		err := s.runStageOrder(t.Context(), func(context.Context) (func(), error) {
			admitted = true
			return func() {}, nil
		}, order)
		if !errors.Is(err, ErrSkipDeployment) {
			t.Fatalf("poll %d returned %v, want an unchanged skip", poll, err)
		}

		if admitted || !slices.Equal(entered, []StageName{StageInit, StagePreDeploy}) {
			t.Fatalf("poll %d entered deployment: admitted=%t, stages=%v", poll, admitted, entered)
		}
	}

	if sent := sender.sentNotifications(); len(sent) != 0 {
		t.Fatalf("skipped polls sent notifications: %+v", sent)
	}

	if got := statusRequests.Load(); got != 0 {
		t.Fatalf("skipped polls posted %d commit statuses", got)
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
