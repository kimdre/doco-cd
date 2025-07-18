package docker

import (
	"context"
	"fmt"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/command/stack/loader"
	"github.com/docker/cli/cli/command/stack/options"
	"github.com/docker/cli/cli/command/stack/swarm"
	composetypes "github.com/docker/cli/cli/compose/types"
	"github.com/spf13/pflag"

	"github.com/kimdre/doco-cd/internal/webhook"

	"github.com/kimdre/doco-cd/internal/config"
)

const (
	StackNamespaceLabel = "com.docker.stack.namespace"
)

// deploySwarmStack deploys a Docker Swarm stack using the provided project and deploy configuration.
func deploySwarmStack(ctx context.Context, dockerCli command.Cli, project *types.Project, deployConfig *config.DeployConfig,
	payload webhook.ParsedPayload, repoDir, latestCommit, appVersion string,
) error {
	opts := options.Deploy{
		Composefiles:     project.ComposeFiles,
		Namespace:        deployConfig.Name,
		ResolveImage:     swarm.ResolveImageAlways,
		SendRegistryAuth: false,
		Prune:            deployConfig.RemoveOrphans,
		Detach:           false,
		Quiet:            true,
	}

	timestamp := time.Now().UTC().Format(time.RFC3339)

	cfg, err := loader.LoadComposefile(dockerCli, opts)
	if err != nil {
		return fmt.Errorf("failed to load compose file: %w", err)
	}

	addSwarmServiceLabels(cfg, *deployConfig, payload, repoDir, appVersion, timestamp, latestCommit)
	addSwarmVolumeLabels(cfg, *deployConfig, payload, repoDir, appVersion, timestamp, latestCommit)
	addSwarmConfigLabels(cfg, *deployConfig, payload, repoDir, appVersion, timestamp, latestCommit)
	addSwarmSecretLabels(cfg, *deployConfig, payload, repoDir, appVersion, timestamp, latestCommit)

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

// addSwarmServiceLabels adds custom labels to the services in a Docker Swarm stack.
func addSwarmServiceLabels(stack *composetypes.Config, deployConfig config.DeployConfig, payload webhook.ParsedPayload, repoDir, appVersion, timestamp, latestCommit string) {
	customLabels := map[string]string{
		DocoCDLabels.Metadata.Manager:      config.AppName,
		DocoCDLabels.Metadata.Version:      appVersion,
		DocoCDLabels.Deployment.Name:       deployConfig.Name,
		DocoCDLabels.Deployment.Timestamp:  timestamp,
		DocoCDLabels.Deployment.WorkingDir: repoDir,
		DocoCDLabels.Deployment.Trigger:    payload.CommitSHA,
		DocoCDLabels.Deployment.CommitSHA:  latestCommit,
		DocoCDLabels.Deployment.TargetRef:  deployConfig.Reference,
		DocoCDLabels.Repository.Name:       payload.FullName,
		DocoCDLabels.Repository.URL:        payload.WebURL,
	}

	for i, s := range stack.Services {
		if s.Labels == nil {
			s.Labels = make(map[string]string)
		}

		for key, val := range customLabels {
			s.Labels[key] = val
		}

		stack.Services[i] = s
	}
}

// addSwarmVolumeLabels adds custom labels to the volumes in a Docker Swarm stack.
func addSwarmVolumeLabels(stack *composetypes.Config, deployConfig config.DeployConfig, payload webhook.ParsedPayload, repoDir, appVersion, timestamp, latestCommit string) {
	customLabels := map[string]string{
		DocoCDLabels.Metadata.Manager:      config.AppName,
		DocoCDLabels.Metadata.Version:      appVersion,
		DocoCDLabels.Deployment.Name:       deployConfig.Name,
		DocoCDLabels.Deployment.Timestamp:  timestamp,
		DocoCDLabels.Deployment.WorkingDir: repoDir,
		DocoCDLabels.Deployment.Trigger:    payload.CommitSHA,
		DocoCDLabels.Deployment.CommitSHA:  latestCommit,
		DocoCDLabels.Deployment.TargetRef:  deployConfig.Reference,
		DocoCDLabels.Repository.Name:       payload.FullName,
		DocoCDLabels.Repository.URL:        payload.WebURL,
	}

	for i, v := range stack.Volumes {
		if v.Labels == nil {
			v.Labels = make(map[string]string)
		}

		for key, val := range customLabels {
			v.Labels[key] = val
		}

		stack.Volumes[i] = v
	}
}

// addSwarmConfigLabels adds custom labels to the configs in a Docker Swarm stack.
func addSwarmConfigLabels(stack *composetypes.Config, deployConfig config.DeployConfig, payload webhook.ParsedPayload, repoDir, appVersion, timestamp, latestCommit string) {
	customLabels := map[string]string{
		DocoCDLabels.Metadata.Manager:      config.AppName,
		DocoCDLabels.Metadata.Version:      appVersion,
		DocoCDLabels.Deployment.Name:       deployConfig.Name,
		DocoCDLabels.Deployment.Timestamp:  timestamp,
		DocoCDLabels.Deployment.WorkingDir: repoDir,
		DocoCDLabels.Deployment.Trigger:    payload.CommitSHA,
		DocoCDLabels.Deployment.CommitSHA:  latestCommit,
		DocoCDLabels.Deployment.TargetRef:  deployConfig.Reference,
		DocoCDLabels.Repository.Name:       payload.FullName,
		DocoCDLabels.Repository.URL:        payload.WebURL,
	}

	for i, c := range stack.Configs {
		if c.Labels == nil {
			c.Labels = make(map[string]string)
		}

		for key, val := range customLabels {
			c.Labels[key] = val
		}

		stack.Configs[i] = c
	}
}

func addSwarmSecretLabels(stack *composetypes.Config, deployConfig config.DeployConfig, payload webhook.ParsedPayload, repoDir, appVersion, timestamp, latestCommit string) {
	customLabels := map[string]string{
		DocoCDLabels.Metadata.Manager:      config.AppName,
		DocoCDLabels.Metadata.Version:      appVersion,
		DocoCDLabels.Deployment.Name:       deployConfig.Name,
		DocoCDLabels.Deployment.Timestamp:  timestamp,
		DocoCDLabels.Deployment.WorkingDir: repoDir,
		DocoCDLabels.Deployment.Trigger:    payload.CommitSHA,
		DocoCDLabels.Deployment.CommitSHA:  latestCommit,
		DocoCDLabels.Deployment.TargetRef:  deployConfig.Reference,
		DocoCDLabels.Repository.Name:       payload.FullName,
		DocoCDLabels.Repository.URL:        payload.WebURL,
	}

	for i, s := range stack.Secrets {
		if s.Labels == nil {
			s.Labels = make(map[string]string)
		}

		for key, val := range customLabels {
			s.Labels[key] = val
		}

		stack.Secrets[i] = s
	}
}
