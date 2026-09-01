package docker

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/docker/compose/v5/pkg/api"

	"github.com/kimdre/doco-cd/internal/common/types/set"
)

// deployComposeRuntime deploys a project as specified by the Docker Compose specification (LoadCompose).
func deployComposeRuntime(ctx context.Context, req runtimeDeployRequest) error {
	deployConfig := req.request.DeployConfig

	addComposeServiceLabels(
		req.project,
		deployConfig,
		req.request.Payload,
		req.externalWorkingDir,
		req.request.AppVersion,
		req.timestamp,
		ComposeVersion,
		req.request.LatestCommit,
		req.projectHash,
	)
	addComposeVolumeLabels(
		req.project,
		deployConfig,
		req.request.Payload,
		req.request.AppVersion,
		req.timestamp,
		ComposeVersion,
		req.request.LatestCommit,
		req.projectHash,
	)

	forcedServices := set.New[string]()
	recreateMode := api.RecreateDiverged

	switch {
	case len(req.request.DetectedChanges) > 0:
		recreateMode = api.RecreateForce
		forcedServices = forcedRecreateServices(req.request.DetectedChanges)
		req.stackLog.Debug("changed project files detected, forcing recreate", slog.Any("changes", req.request.DetectedChanges))
	case len(req.request.NeedSignal) > 0:
		req.stackLog.Debug("changed project files detected, sending signal to service",
			slog.Any("need_signal", req.request.NeedSignal))
	}

	if recreateMode == api.RecreateDiverged && hasIPv6NetworkWithoutExplicitSubnet(req.project) {
		recreateMode = api.RecreateForce

		req.stackLog.Warn("network has enable_ipv6 without explicit ipam subnet; forcing recreate to avoid diverged compare parser failure")
	}

	req.stackLog.Info("deploying stack",
		slog.Group("recreate",
			slog.String("mode", recreateMode),
			slog.Any("forced_services", forcedServices.ToSlice()),
		),
		slog.Any("need_signal", req.request.NeedSignal),
	)

	req.phase.Set("deploying compose stack")

	err := deployCompose(
		ctx,
		req.request.DockerCLI,
		req.project,
		deployConfig,
		recreateMode,
		forcedServices.ToSlice(),
		req.request.NeedSignal,
		req.phase.Set,
	)
	if err != nil {
		req.recordError()

		return fmt.Errorf("failed to deploy stack: %w", err)
	}

	return nil
}
