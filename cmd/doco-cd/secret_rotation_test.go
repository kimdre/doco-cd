package main

import (
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/config"
	appconfig "github.com/kimdre/doco-cd/internal/config/app"
	deployconfig "github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/docker"
)

func TestParseSecretRotationTargetFromLabels(t *testing.T) {
	t.Parallel()

	labels := map[string]string{
		docker.DocoCDLabels.Deployment.Name:                 "app",
		docker.DocoCDLabels.Deployment.DockerContext:        "production",
		docker.DocoCDLabels.Deployment.TargetRef:            "release",
		docker.DocoCDLabels.Deployment.ConfigTarget:         "prod",
		docker.DocoCDLabels.Deployment.ExternalSecretsRefs:  `{"CERT":"pki:pki:app.example.com"}`,
		docker.DocoCDLabels.Deployment.ExternalSecretsHash:  "deployed-hash",
		docker.DocoCDLabels.Deployment.SecretRotation:       "true",
		docker.DocoCDLabels.Deployment.SecretRotationPeriod: "1h",
		docker.DocoCDLabels.Deployment.SecretRotateBefore:   "168h",
		docker.DocoCDLabels.Source.Type:                     "git",
		docker.DocoCDLabels.Source.Name:                     "owner/config",
		docker.DocoCDLabels.Source.URL:                      "https://example.com/owner/config.git",
		docker.DocoCDLabels.Source.Ref:                      "main",
	}

	target, ok := parseSecretRotationTargetFromLabels(labels, "fallback")
	if !ok {
		t.Fatalf("expected labels to produce a rotation target")
	}

	if target.sourceRef != "main" || target.targetRef != "release" {
		t.Fatalf("unexpected source/target refs: %q/%q", target.sourceRef, target.targetRef)
	}

	if target.dockerContext != "production" {
		t.Fatalf("expected production context, got %q", target.dockerContext)
	}

	if target.interval != time.Hour || target.rotateBefore != 7*24*time.Hour {
		t.Fatalf("unexpected rotation durations: %s/%s", target.interval, target.rotateBefore)
	}
}

func TestParseSecretRotationTargetFromLabels_RequiresSourceRef(t *testing.T) {
	t.Parallel()

	labels := map[string]string{
		docker.DocoCDLabels.Deployment.Name:                "app",
		docker.DocoCDLabels.Deployment.TargetRef:           "release",
		docker.DocoCDLabels.Deployment.ExternalSecretsRefs: `{"TOKEN":"token-ref"}`,
		docker.DocoCDLabels.Deployment.SecretRotation:      "true",
		docker.DocoCDLabels.Source.Type:                    "git",
		docker.DocoCDLabels.Source.URL:                     "https://example.com/owner/config.git",
	}

	if _, ok := parseSecretRotationTargetFromLabels(labels, ""); ok {
		t.Fatalf("expected missing source ref to reject target")
	}
}

func TestFindSecretRotationInlineDeployments(t *testing.T) {
	t.Parallel()

	inline := []*deployconfig.Config{
		deployconfig.New("api", "main"),
		deployconfig.New("worker", "main"),
	}
	appConfig := &appconfig.Config{
		SourceURLRewrites: map[string]string{
			"https://example.com": "ssh://git@example.com",
		},
		PollConfig: []poll.Config{{
			Source:       config.SourceTypeGit,
			SourceUrl:    "https://example.com/owner/config.git",
			Reference:    "main",
			CustomTarget: "prod",
			Deployments:  inline,
		}},
	}
	target := secretRotationTarget{
		deploymentName: "worker",
		sourceType:     config.SourceTypeGit,
		sourceURL:      "ssh://git@example.com/owner/config.git",
		sourceRef:      "main",
		configTarget:   "prod",
	}

	got := findSecretRotationInlineDeployments(appConfig, target)
	if len(got) != len(inline) || got[1] != inline[1] {
		t.Fatalf("expected matching inline deployment configuration")
	}
}
