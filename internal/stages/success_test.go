package stages

import (
	"strings"
	"testing"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
)

func newSuccessSummaryStageManager() *StageManager {
	return &StageManager{
		DeployConfig: &deploy.Config{
			Name: "web", Context: "nas", Reference: "main",
		},
		Repository: &RepositoryData{
			ResolvedReference: "refs/heads/main",
			Revision:          "0123456789abcdef",
		},
		Docker:     &Docker{},
		JobTrigger: JobTriggerWebhook,
		DeployState: &DeploymentState{
			changedServices:      []docker.Change{{Type: "file", Services: []string{"web", "api", "web"}}},
			imageChangedServices: []string{"worker", "api"},
		},
	}
}

func TestSuccessfulCommitStatusSummary(t *testing.T) {
	t.Parallel()

	sm := newSuccessSummaryStageManager()
	sm.DeployConfig.Internal.ConfigTarget = "homelab"

	want := "Successfully deployed stack `web`.\n\n" +
		"| Detail | Value |\n| --- | --- |\n" +
		"| Stack | `web` |\n" +
		"| Target | `homelab` |\n" +
		"| Docker context | `nas` |\n" +
		"| Reference | `refs/heads/main` |\n" +
		"| Commit | `0123456` |\n" +
		"| Deployment mode | Compose |\n" +
		"| Trigger | Webhook |\n" +
		"| Detected changed services | `api`, `web`, `worker` |\n"

	if got := sm.successfulCommitStatusSummary(sm.Repository.Revision); got != want {
		t.Fatalf("summary =\n%s\nwant\n%s", got, want)
	}
}

func TestSuccessfulCommitStatusSummaryMetadata(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name    string
		setup   func(*StageManager)
		want    string
		missing string
	}{
		{name: "swarm", setup: func(s *StageManager) { s.Docker.SwarmMode = true }, want: "| Deployment mode | Swarm |"},
		{name: "poll", setup: func(s *StageManager) { s.JobTrigger = JobTriggerPoll }, want: "| Trigger | Poll |"},
		{name: "default context", setup: func(s *StageManager) { s.DeployConfig.Context = "" }, want: "| Docker context | `default` |"},
		{name: "normalized default", setup: func(s *StageManager) { s.DeployConfig.Context = " DEFAULT " }, want: "| Docker context | `default` |"},
		{name: "trimmed target", setup: func(s *StageManager) { s.DeployConfig.Internal.ConfigTarget = " prod " }, want: "| Target | `prod` |"},
		{name: "reference fallback", setup: func(s *StageManager) { s.Repository.ResolvedReference = "" }, want: "| Reference | `main` |"},
		{name: "no reference", setup: func(s *StageManager) { s.Repository.ResolvedReference, s.DeployConfig.Reference = "", "" }, missing: "| Reference |"},
		{name: "no target", setup: func(*StageManager) {}, missing: "| Target |"},
		{name: "unknown mode", setup: func(s *StageManager) { s.Docker = nil }, missing: "| Deployment mode |"},
		{name: "unknown trigger", setup: func(s *StageManager) { s.JobTrigger = "" }, missing: "| Trigger |"},
		{name: "first deployment", setup: func(s *StageManager) { s.DeployState = nil }, want: "| Detected changed services | Not individually tracked |"},
		{name: "whole stack retry", setup: func(s *StageManager) {
			s.DeployState = &DeploymentState{changedServices: []docker.Change{{Type: docker.ChangeTypeFailedDeployRetry}}}
		}, want: "| Detected changed services | Not individually tracked |"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sm := newSuccessSummaryStageManager()
			tt.setup(sm)
			got := sm.successfulCommitStatusSummary("0123456789abcdef")

			if tt.want != "" && !strings.Contains(got, tt.want) {
				t.Fatalf("summary = %q, want %q", got, tt.want)
			}

			if tt.missing != "" && strings.Contains(got, tt.missing) {
				t.Fatalf("summary = %q, should omit %q", got, tt.missing)
			}
		})
	}
}

func TestSuccessfulCommitStatusSummaryEscapesMarkdown(t *testing.T) {
	t.Parallel()

	sm := newSuccessSummaryStageManager()
	sm.DeployConfig.Name = "web`|app\nname"
	sm.DeployConfig.Context = "nas|remote\nhost"
	sm.DeployState = &DeploymentState{imageChangedServices: []string{"api`|worker\nname"}}

	got := sm.successfulCommitStatusSummary("abc")
	for _, want := range []string{
		"Successfully deployed stack ``web`\\|app name``.",
		"| Stack | ``web`\\|app name`` |",
		"| Docker context | `nas\\|remote host` |",
		"| Commit | `abc` |",
		"| Detected changed services | ``api`\\|worker name`` |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary = %q, want %q", got, want)
		}
	}
}
