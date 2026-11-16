package docker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/moby/moby/api/types/swarm"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/selfupdate"
)

// deploySwarmRuntime deploys a swarm stack using the provided request parameters.
func deploySwarmRuntime(ctx context.Context, req runtimeDeployRequest) (retErr error) {
	deployConfig := req.request.DeployConfig
	configRetention := deployConfig.ResolveSwarmConfigRetention(req.request.SwarmRetention.Config)
	secretRetention := deployConfig.ResolveSwarmSecretRetention(req.request.SwarmRetention.Secret)

	req.phase.Set("deploying swarm stack")
	req.stackLog.Info("deploying swarm stack")

	cfg, opts, err := LoadSwarmStack(req.request.DockerCLI, req.project, deployConfig, req.externalWorkingDir)
	if err != nil {
		return fmt.Errorf("failed to load swarm stack: %w", err)
	}

	opts.Logger = req.stackLog
	opts.ForceUpdateServices = staleArtifactServices(req.request.DetectedChanges)
	opts.SelfService = swarmSelfService(deployConfig)

	var handover *selfupdate.SwarmHandover

	if opts.SelfService != "" {
		opts.PrepareSelfUpdate = func(ctx context.Context, current swarm.Service, desired *swarm.ServiceSpec, waitIDs []string) error {
			var err error

			handover, err = prepareSwarmSelfUpdate(ctx, req, current, desired, waitIDs)

			return err
		}
	}

	defer func() {
		retErr = finishSwarmSelfUpdate(ctx, req.request.DockerCLI.Client(), handover, retErr)
		if errors.Is(retErr, selfupdate.ErrHandover) && ctx.Err() == nil {
			req.stackLog.Warn("self-update: Swarm result reporting continues in the background", slog.Any("error", retErr))
		}

		// A shutdown-time result is counted by the task that finishes its durable report.
		if retErr != nil && !errors.Is(retErr, selfupdate.ErrHandover) && (handover == nil || ctx.Err() == nil) {
			req.recordError()
		}
	}()

	addSwarmServiceLabels(cfg, req.project, deployConfig, req.request.Payload, req.request.SourceURL, req.externalWorkingDir,
		req.request.AppVersion, req.timestamp, req.request.LatestCommit, req.projectHash)
	addSwarmVolumeLabels(cfg, deployConfig, req.request.Payload)
	addSwarmConfigLabels(cfg, deployConfig, req.request.Payload, req.request.SourceURL, req.externalWorkingDir,
		req.request.AppVersion, req.timestamp, req.request.LatestCommit)
	addSwarmSecretLabels(cfg, deployConfig, req.request.Payload, req.request.SourceURL, req.externalWorkingDir,
		req.request.AppVersion, req.timestamp, req.request.LatestCommit)

	// Pinning is an optimization: if it fails, services are simply updated to the new artifact.
	if err = pinUnchangedSwarmServices(ctx, req.request.DockerCLI.Client(), cfg, deployConfig.Name, req.request.ExternalRepoPath, req.stackLog); err != nil {
		req.stackLog.Warn("failed to keep unchanged services on their current artifact", slog.Any("error", err))
	}

	if err = removeMismatchedRecreatableVolumes(ctx, req.request.DockerCLI.Client(), deployConfig.Name, req.project); err != nil {
		return fmt.Errorf("failed to remove mismatched recreatable volumes: %w", err)
	}

	if err = DeploySwarmStack(ctx, req.request.DockerCLI, cfg, opts); err != nil {
		if errors.Is(err, selfupdate.ErrHandover) {
			if req.phase != nil {
				req.phase.stopReporting()
			}

			req.stackLog.Info("self-update: swarm rolling update replaced this instance")

			return err
		}

		return fmt.Errorf("failed to deploy swarm stack %s: %w", deployConfig.Name, err)
	}

	if configRetention >= 0 {
		req.phase.Set("pruning stack configs")

		if err = PruneStackConfigs(ctx, req.request.DockerCLI.Client(), deployConfig.Name, configRetention); err != nil {
			return fmt.Errorf("failed to prune stack configs: %w", err)
		}
	} else {
		req.stackLog.Info("skipping swarm config prune: retention disabled", slog.Int("retention", configRetention))
	}

	if secretRetention >= 0 {
		req.phase.Set("pruning stack secrets")

		if err = PruneStackSecrets(ctx, req.request.DockerCLI.Client(), deployConfig.Name, secretRetention); err != nil {
			return fmt.Errorf("failed to prune stack secrets: %w", err)
		}
	} else {
		req.stackLog.Info("skipping swarm secret prune: retention disabled", slog.Int("retention", secretRetention))
	}

	if deployConfig.PruneImages {
		req.phase.Set("pruning images on swarm nodes")
		req.stackLog.Info("prune images on swarm nodes")

		if err = RunImagePruneJob(ctx, req.request.DockerCLI, req.stackLog); err != nil {
			return fmt.Errorf("failed to run image prune job: %w", err)
		}
	}

	return nil
}

// swarmSelfService limits handovers to this instance's service on the local Swarm.
func swarmSelfService(deployConfig *deploy.Config) string {
	identity := SelfUpdateConfig().Identity
	if !identity.Swarm || !IsSelfStack(deployConfig) {
		return ""
	}

	return identity.Project + "_" + identity.Service
}
