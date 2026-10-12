package stages

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/google/uuid"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/require"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/webhook"
)

// Unimplemented Docker methods panic through the embedded interface, so this
// empty-project deployment cannot silently start using a real Docker daemon.
type repositoryStateClient struct {
	client.APIClient
}

func (*repositoryStateClient) Ping(context.Context, client.PingOptions) (client.PingResult, error) {
	return client.PingResult{APIVersion: "1.54"}, nil
}

func (*repositoryStateClient) ClientVersion() string { return "1.54" }

func (*repositoryStateClient) ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
	return client.ContainerListResult{}, nil
}

func (*repositoryStateClient) NetworkList(context.Context, client.NetworkListOptions) (client.NetworkListResult, error) {
	return client.NetworkListResult{}, nil
}

func (*repositoryStateClient) VolumeList(context.Context, client.VolumeListOptions) (client.VolumeListResult, error) {
	return client.VolumeListResult{}, nil
}

type repositoryStateCLI struct {
	command.Cli
	apiClient client.APIClient
}

func (c repositoryStateCLI) Client() client.APIClient { return c.apiClient }

func TestRunDeployStageCachesActiveRepositoryIdentity(t *testing.T) {
	for _, repository := range []string{
		"forgejo:2222/org/repo",
		"vpn-forgejo.internal:2222/org/repo",
		"github.com/org/deployment-override",
	} {
		t.Run(repository, func(t *testing.T) {
			ctx := t.Context()
			apiClient := &repositoryStateClient{}
			stack := "repository-state-" + uuid.NewString()
			deployConfig := deploy.New(stack, "main")
			deployConfig.WaitRunningJobs = false
			deployConfig.PruneImages = false
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			manager := &StageManager{
				Stages:       &Stages{Deploy: &DeployStageData{}},
				Log:          logger,
				AppConfig:    &app.Config{},
				DeployConfig: deployConfig,
				DeployState:  &DeploymentState{latestCommit: "revision-B"},
				Repository: &RepositoryData{
					Source:       config.SourceTypeGit,
					Name:         repository,
					SourceUrl:    "ssh://git@" + repository + ".git",
					PathExternal: t.TempDir(),
				},
				Payload: &webhook.ParsedPayload{CloneURL: "https://forgejo.example.com/org/repo.git"},
				Docker: &Docker{
					Cmd:         repositoryStateCLI{apiClient: apiClient},
					Project:     &types.Project{Name: stack},
					ProjectHash: "unchanged-compose",
				},
			}

			require.NoError(t, manager.RunDeployStage(ctx, logger))

			status, err := docker.GetLatestDeployStatus(ctx, apiClient, false, repository, stack)
			require.NoError(t, err)
			require.Equal(t, "revision-B", status.GetDeploymentCommitSHA())
			require.Equal(t, "unchanged-compose", status.GetDeploymentComposeHash())

			publicStatus, err := docker.GetLatestDeployStatus(ctx, apiClient, false, manager.Payload.CloneURL, stack)
			require.NoError(t, err)
			require.Empty(t, publicStatus.GetDeploymentCommitSHA())
		})
	}
}
