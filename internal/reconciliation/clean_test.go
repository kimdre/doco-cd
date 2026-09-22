package reconciliation

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/common/types/set"
	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/notification"
)

type cleanupTestCLI struct {
	command.Cli
	apiClient client.APIClient
}

func (c cleanupTestCLI) Client() client.APIClient {
	return c.apiClient
}

type cleanupTestClient struct {
	client.APIClient
	containers []container.Summary
}

func (c *cleanupTestClient) ContainerList(_ context.Context, _ client.ContainerListOptions) (client.ContainerListResult, error) {
	return client.ContainerListResult{Items: c.containers}, nil
}

func TestCleanupObsoleteAutoDiscoveredContainers_SkipsDifferentTargetBeforeRepositoryMatch(t *testing.T) {
	t.Parallel()

	apiClient := &cleanupTestClient{
		containers: []container.Summary{
			{
				Names: []string{"/test-stack-app"},
				Labels: map[string]string{
					docker.DocoCDLabels.Deployment.Name:          "test-stack",
					docker.DocoCDLabels.Deployment.ConfigTarget:  "dev",
					docker.DocoCDLabels.Deployment.AutoDiscovery: "true",
					docker.DocoCDLabels.Source.URL:               "https://example.com/organization/repository.git",
				},
			},
		},
	}
	deployCfg := &deployConfig.Config{}
	deployCfg.Internal.ConfigTarget = "updater"

	var logs bytes.Buffer

	err := cleanupObsoleteAutoDiscoveredContainers(
		t.Context(),
		slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		cleanupTestCLI{apiClient: apiClient},
		false,
		"",
		"https://example.com/organization/repository.git",
		[]*deployConfig.Config{deployCfg},
		notification.Metadata{},
		nil,
	)
	if err != nil {
		t.Fatalf("cleanupObsoleteAutoDiscoveredContainers() error = %v", err)
	}

	if strings.Contains(logs.String(), "checking auto-discovered stack for repository match") {
		t.Fatal("foreign target stack was checked for a repository match")
	}
}

func TestIsCleanupTargetMatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		runConfigTargets set.Set[string]
		stackTarget      string
		want             bool
	}{
		{
			name:             "legacy mode when no run target is available",
			runConfigTargets: set.New[string](),
			stackTarget:      "dev",
			want:             true,
		},
		{
			name:             "custom target matches same target",
			runConfigTargets: set.New("updater"),
			stackTarget:      "updater",
			want:             true,
		},
		{
			name:             "custom target does not match different target",
			runConfigTargets: set.New("updater"),
			stackTarget:      "dev",
			want:             false,
		},
		{
			name:             "custom target does not match unlabeled stack",
			runConfigTargets: set.New("updater"),
			stackTarget:      "",
			want:             false,
		},
		{
			name:             "default target matches unlabeled stack",
			runConfigTargets: set.New(""),
			stackTarget:      "",
			want:             true,
		},
		{
			name:             "default target matches default label",
			runConfigTargets: set.New(""),
			stackTarget:      "  ",
			want:             true,
		},
		{
			name:             "default target does not match custom target stack",
			runConfigTargets: set.New(""),
			stackTarget:      "dev",
			want:             false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := isCleanupTargetMatch(tt.runConfigTargets, tt.stackTarget); got != tt.want {
				t.Fatalf("isCleanupTargetMatch() = %v, want %v", got, tt.want)
			}
		})
	}
}
