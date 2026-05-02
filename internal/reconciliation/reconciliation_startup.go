package reconciliation

import (
	"context"
	"log/slog"
	"strings"

	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	gitInternal "github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/utils/id"
	"github.com/kimdre/doco-cd/internal/utils/set"
)

func (j *job) restartUnhealthyContainersOnStartup(ctx context.Context, jobLog *slog.Logger) {
	unhealthyDCs := j.deployConfigGroupByEvent["unhealthy"]
	if len(unhealthyDCs) == 0 || swarm.GetModeEnabled() {
		return
	}

	repositoryLabelValue := gitInternal.GetFullName(string(j.info.repoData.CloneURL))
	if j.info.payload != nil && strings.TrimSpace(j.info.payload.FullName) != "" {
		repositoryLabelValue = j.info.payload.FullName
	}

	filterArgs := make(client.Filters)
	filterArgs.Add("label", docker.DocoCDLabels.Metadata.Manager+"="+config.AppName)
	filterArgs.Add("label", docker.DocoCDLabels.Repository.Name+"="+repositoryLabelValue)

	containerResult, err := j.info.dockerCli.Client().ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filterArgs})
	if err != nil {
		jobLog.Error("failed to list containers for startup unhealthy scan", logger.ErrAttr(err))
		return
	}

	for _, c := range containerResult.Items {
		stackName := strings.TrimSpace(c.Labels[docker.DocoCDLabels.Deployment.Name])
		if stackName == "" {
			continue
		}

		stackDCs := deployConfigsByName(unhealthyDCs, stackName)

		restartDC := selectRestartDeployConfig(stackDCs, c.Labels)
		if restartDC == nil {
			continue
		}

		inspectResult, err := j.info.dockerCli.Client().ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
		if err != nil {
			jobLog.Debug("failed to inspect container during startup unhealthy scan",
				slog.String("container_id", shortID(c.ID)),
				logger.ErrAttr(err),
			)

			continue
		}

		inspect := inspectResult.Container

		if inspect.State == nil || inspect.State.Health == nil || strings.ToLower(strings.TrimSpace(string(inspect.State.Health.Status))) != "unhealthy" {
			continue
		}

		containerName := ""
		if len(c.Names) > 0 {
			containerName = strings.TrimPrefix(c.Names[0], "/")
		}

		eventLog := logger.
			WithoutAttr(jobLog, "job_id").
			With(
				slog.Group("reconciliation",
					slog.String("event", "startup_unhealthy"),
					slog.Group("container",
						slog.String("id", shortID(c.ID)),
						slog.String("name", containerName),
					),
					slog.String("trace_id", id.GenID()),
				),
				slog.String("stack", stackName),
			)

		j.restartContainer(ctx, eventLog, events.Message{
			Action: events.Action("unhealthy"),
			Actor: events.Actor{
				ID: c.ID,
				Attributes: map[string]string{
					"name": containerName,
				},
			},
		}, restartDC)
	}
}

// uniqueRedeployDCsFromGroupByEvent returns a deduplicated slice (by stack name) of deploy configs
// that have at least one non-restart reconciliation event configured (e.g. "die", "destroy", "update").
// These are the stacks that should be redeployed when their containers/services go missing.
func uniqueRedeployDCsFromGroupByEvent(grouped map[string][]*config.DeployConfig) []*config.DeployConfig {
	seen := set.New[string]()

	var result []*config.DeployConfig

	for action, dcs := range grouped {
		if isRestartReconciliationAction(action) {
			continue
		}

		for _, dc := range dcs {
			if dc == nil {
				continue
			}

			if !seen.Contains(dc.Name) {
				seen.Add(dc.Name)
				result = append(result, dc)
			}
		}
	}

	return result
}

// redeployMissingServicesOnStartup performs a one-time startup check for stacks whose
// reconciliation is configured for redeploy-oriented events (e.g., "die", "destroy", "update")
// and triggers a redeploy for any stacks that are completely missing their containers/services.
func (j *job) redeployMissingServicesOnStartup(ctx context.Context, jobLog *slog.Logger) {
	candidates := uniqueRedeployDCsFromGroupByEvent(j.deployConfigGroupByEvent)
	if len(candidates) == 0 {
		return
	}

	var missingDCs []*config.DeployConfig

	if swarm.GetModeEnabled() {
		missingDCs = j.findMissingSwarmServicesOnStartup(ctx, jobLog, candidates)
	} else {
		missingDCs = j.findMissingContainersOnStartup(ctx, jobLog, candidates)
	}

	if len(missingDCs) == 0 {
		return
	}

	eventLog := logger.
		WithoutAttr(jobLog, "job_id").
		With(
			slog.Group("reconciliation",
				slog.String("event", "startup_missing"),
				slog.String("trace_id", id.GenID()),
			),
		)

	j.deploy(ctx, eventLog, missingDCs, "startup_missing", events.Message{})
}

// findMissingContainersOnStartup lists all running containers for this repository and returns
// deploy configs whose stacks have no running containers at all.
func (j *job) findMissingContainersOnStartup(ctx context.Context, jobLog *slog.Logger, candidates []*config.DeployConfig) []*config.DeployConfig {
	repositoryLabelValue := gitInternal.GetFullName(string(j.info.repoData.CloneURL))
	if j.info.payload != nil && strings.TrimSpace(j.info.payload.FullName) != "" {
		repositoryLabelValue = j.info.payload.FullName
	}

	filterArgs := make(client.Filters)
	filterArgs.Add("label", docker.DocoCDLabels.Metadata.Manager+"="+config.AppName)
	filterArgs.Add("label", docker.DocoCDLabels.Repository.Name+"="+repositoryLabelValue)

	containerResult, err := j.info.dockerCli.Client().ContainerList(ctx, client.ContainerListOptions{
		All:     false, // running only
		Filters: filterArgs,
	})
	if err != nil {
		jobLog.Error("failed to list containers for startup missing scan", logger.ErrAttr(err))
		return nil
	}

	runningStacks := set.New[string]()

	for _, c := range containerResult.Items {
		if stackName := strings.TrimSpace(c.Labels[docker.DocoCDLabels.Deployment.Name]); stackName != "" {
			runningStacks.Add(stackName)
		}
	}

	var missing []*config.DeployConfig

	for _, dc := range candidates {
		if !runningStacks.Contains(dc.Name) {
			jobLog.Debug("detected missing containers on startup", slog.String("stack", dc.Name))
			missing = append(missing, dc)
		}
	}

	return missing
}

// findMissingSwarmServicesOnStartup lists all swarm services for this repository and returns
// deploy configs whose stacks have no deployed services at all.
func (j *job) findMissingSwarmServicesOnStartup(ctx context.Context, jobLog *slog.Logger, candidates []*config.DeployConfig) []*config.DeployConfig {
	repositoryLabelValue := gitInternal.GetFullName(string(j.info.repoData.CloneURL))
	if j.info.payload != nil && strings.TrimSpace(j.info.payload.FullName) != "" {
		repositoryLabelValue = j.info.payload.FullName
	}

	services, err := swarm.GetServicesByLabel(ctx, j.info.dockerCli.Client(), docker.DocoCDLabels.Metadata.Manager, config.AppName)
	if err != nil {
		jobLog.Error("failed to list swarm services for startup missing scan", logger.ErrAttr(err))
		return nil
	}

	existingStacks := set.New[string]()

	for _, svc := range services {
		// Filter by repository to avoid matching services from other repos on the same swarm.
		if strings.TrimSpace(svc.Spec.Labels[docker.DocoCDLabels.Repository.Name]) != repositoryLabelValue {
			continue
		}

		if stackName := strings.TrimSpace(svc.Spec.Labels[docker.DocoCDLabels.Deployment.Name]); stackName != "" {
			existingStacks.Add(stackName)
		}
	}

	var missing []*config.DeployConfig

	for _, dc := range candidates {
		if !existingStacks.Contains(dc.Name) {
			jobLog.Debug("detected missing swarm services on startup", slog.String("stack", dc.Name))
			missing = append(missing, dc)
		}
	}

	return missing
}
