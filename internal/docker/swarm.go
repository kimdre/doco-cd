package docker

import (
	"context"
	"fmt"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/command/stack/loader"
	"github.com/docker/cli/cli/command/stack/options"
	"github.com/docker/cli/cli/command/stack/swarm"
	"github.com/spf13/pflag"

	"github.com/kimdre/doco-cd/internal/config"
)

// deploySwarmStack deploys a Docker Swarm stack using the provided project and deploy configuration.
func deploySwarmStack(ctx context.Context, dockerCli command.Cli, project *types.Project, deployConfig *config.DeployConfig) error {
	opts := options.Deploy{
		Composefiles:     project.ComposeFiles,
		Namespace:        deployConfig.Name,
		ResolveImage:     swarm.ResolveImageAlways,
		SendRegistryAuth: false,
		Prune:            deployConfig.RemoveOrphans,
		Detach:           false,
		Quiet:            true,
	}

	cfg, err := loader.LoadComposefile(dockerCli, opts)
	if err != nil {
		return fmt.Errorf("failed to load compose file: %w", err)
	}

	return swarm.RunDeploy(ctx, dockerCli, &pflag.FlagSet{}, &opts, cfg)
}

// removeSwarmStack removes a Docker Swarm stack using the provided deploy configuration.
func removeSwarmStack(ctx context.Context, dockerCli command.Cli, deployConfig *config.DeployConfig) error {
	opts := options.Remove{
		Namespaces: []string{deployConfig.Name},
		Detach:     false,
	}

	return swarm.RunRemove(ctx, dockerCli, opts)
}
