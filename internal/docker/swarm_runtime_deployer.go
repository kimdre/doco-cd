package docker

import (
	"context"
	"fmt"
	"log/slog"
)

// deploySwarmRuntime deploys a swarm stack using the provided request parameters.
func deploySwarmRuntime(ctx context.Context, req runtimeDeployRequest) error {
	deployConfig := req.request.DeployConfig
	configRetention := deployConfig.ResolveSwarmConfigRetention(req.request.SwarmRetention.Config)
	secretRetention := deployConfig.ResolveSwarmSecretRetention(req.request.SwarmRetention.Secret)

	req.phase.Set("deploying swarm stack")
	req.stackLog.Info("deploying swarm stack")

	cfg, opts, err := LoadSwarmStack(req.request.DockerCLI, req.project, deployConfig, req.externalWorkingDir)
	if err != nil {
		return fmt.Errorf("failed to load swarm stack: %w", err)
	}

	addSwarmServiceLabels(cfg, req.project, deployConfig, req.request.Payload, req.externalWorkingDir,
		req.request.AppVersion, req.timestamp, req.request.LatestCommit, req.projectHash)
	addSwarmVolumeLabels(cfg, deployConfig, req.request.Payload, req.externalWorkingDir)
	addSwarmConfigLabels(cfg, deployConfig, req.request.Payload, req.externalWorkingDir,
		req.request.AppVersion, req.timestamp, req.request.LatestCommit)
	addSwarmSecretLabels(cfg, deployConfig, req.request.Payload, req.externalWorkingDir,
		req.request.AppVersion, req.timestamp, req.request.LatestCommit)

	if err = removeMismatchedRecreatableVolumes(ctx, req.request.DockerCLI.Client(), deployConfig.Name, req.project); err != nil {
		req.recordError()

		return fmt.Errorf("failed to remove mismatched recreatable volumes: %w", err)
	}

	if err = DeploySwarmStack(ctx, req.request.DockerCLI, cfg, opts); err != nil {
		req.recordError()

		return fmt.Errorf("failed to deploy swarm stack %s: %w", deployConfig.Name, err)
	}

	if configRetention >= 0 {
		req.phase.Set("pruning stack configs")

		if err = PruneStackConfigs(ctx, req.request.DockerCLI.Client(), deployConfig.Name, configRetention); err != nil {
			req.recordError()

			return fmt.Errorf("failed to prune stack configs: %w", err)
		}
	} else {
		req.stackLog.Info("skipping swarm config prune: retention disabled", slog.Int("retention", configRetention))
	}

	if secretRetention >= 0 {
		req.phase.Set("pruning stack secrets")

		if err = PruneStackSecrets(ctx, req.request.DockerCLI.Client(), deployConfig.Name, secretRetention); err != nil {
			req.recordError()

			return fmt.Errorf("failed to prune stack secrets: %w", err)
		}
	} else {
		req.stackLog.Info("skipping swarm secret prune: retention disabled", slog.Int("retention", secretRetention))
	}

	if deployConfig.PruneImages {
		req.phase.Set("pruning images on swarm nodes")
		req.stackLog.Info("prune images on swarm nodes")

		if err = RunImagePruneJob(ctx, req.request.DockerCLI); err != nil {
			req.recordError()

			return fmt.Errorf("failed to run image prune job: %w", err)
		}
	}

	return nil
}
