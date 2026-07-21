package reconciliation

import (
	"context"
	"log/slog"
	"strings"

	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config/app"
	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	gitInternal "github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/utils/id"
	"github.com/kimdre/doco-cd/internal/utils/set"
)

func (j *job) restartUnhealthyContainersOnStartup(ctx context.Context, jobLog *slog.Logger, contextName string, cli command.Cli, swarmMode bool) {
	unhealthyAllDCs := j.deployConfigGroupByEvent["unhealthy"]

	unhealthyDCs := filterConfigsByContext(unhealthyAllDCs, contextName)
	if len(unhealthyDCs) == 0 || swarmMode {
		return
	}

	repositoryLabelValue := gitInternal.GetFullName(j.info.repoData.SourceUrl)
	if j.info.payload != nil && strings.TrimSpace(j.info.payload.FullName) != "" {
		repositoryLabelValue = j.info.payload.FullName
	}

	filterArgs := make(client.Filters)
	filterArgs.Add("label", docker.DocoCDLabels.Metadata.Manager+"="+app.Name)
	filterArgs.Add("label", docker.DocoCDLabels.Source.Name+"="+repositoryLabelValue)

	containerResult, err := cli.Client().ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filterArgs})
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

		inspectResult, err := cli.Client().ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
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

		traceID := id.GenID()

		eventLog := logger.
			WithoutAttr(jobLog, "job_id").
			With(
				// Keep one trace ID for both logs and notifications for this reconciliation action.
				slog.Group("reconciliation",
					slog.String("event", "startup_unhealthy"),
					slog.Group("container",
						slog.String("id", shortID(c.ID)),
						slog.String("name", containerName),
					),
					slog.String("trace_id", traceID),
				),
				slog.String("stack", stackName),
			)

		restartEvent := withReconciliationTraceID(events.Message{
			Action: events.Action("unhealthy"),
			Actor: events.Actor{
				ID: c.ID,
				Attributes: map[string]string{
					"name": containerName,
				},
			},
		}, traceID)

		j.restartContainer(ctx, eventLog, restartEvent, restartDC, cli, swarmMode)
	}
}

// uniqueRedeployDCsFromGroupByEvent returns a deduplicated slice (by stack name) of deploy configs
// that have at least one non-restart reconciliation event configured (e.g. "die", "destroy", "update").
// These are the stacks that should be redeployed when their containers/services go missing.
func uniqueRedeployDCsFromGroupByEvent(grouped map[string][]*deployConfig.Config) []*deployConfig.Config {
	seen := set.New[string]()

	var result []*deployConfig.Config

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
func (j *job) redeployMissingServicesOnStartup(ctx context.Context, jobLog *slog.Logger, contextName string, cli command.Cli, swarmMode bool) {
	allCandidates := uniqueRedeployDCsFromGroupByEvent(j.deployConfigGroupByEvent)

	candidates := filterConfigsByContext(allCandidates, contextName)
	if len(candidates) == 0 {
		return
	}

	var missingDCs []*deployConfig.Config

	if swarmMode {
		missingDCs = j.findMissingSwarmServicesOnStartup(ctx, jobLog, cli, candidates)
	} else {
		missingDCs = j.findMissingContainersOnStartup(ctx, jobLog, cli, candidates)
	}

	if len(missingDCs) == 0 {
		return
	}

	traceID := id.GenID()

	eventLog := logger.
		WithoutAttr(jobLog, "job_id").
		With(
			slog.Group("reconciliation",
				slog.String("event", "startup_missing"),
				slog.String("trace_id", traceID),
			),
		)

	j.deploy(ctx, eventLog, missingDCs, "startup_missing", events.Message{}, traceID, contextName)
}

// findMissingContainersOnStartup lists all running containers for this repository and returns
// deploy configs whose stacks have no running containers at all.
func (j *job) findMissingContainersOnStartup(ctx context.Context, jobLog *slog.Logger, cli command.Cli, candidates []*deployConfig.Config) []*deployConfig.Config {
	repositoryLabelValue := gitInternal.GetFullName(j.info.repoData.SourceUrl)
	if j.info.payload != nil && strings.TrimSpace(j.info.payload.FullName) != "" {
		repositoryLabelValue = j.info.payload.FullName
	}

	filterArgs := make(client.Filters)
	filterArgs.Add("label", docker.DocoCDLabels.Metadata.Manager+"="+app.Name)
	filterArgs.Add("label", docker.DocoCDLabels.Source.Name+"="+repositoryLabelValue)

	containerResult, err := cli.Client().ContainerList(ctx, client.ContainerListOptions{
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

	var missing []*deployConfig.Config

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
func (j *job) findMissingSwarmServicesOnStartup(ctx context.Context, jobLog *slog.Logger, cli command.Cli, candidates []*deployConfig.Config) []*deployConfig.Config {
	repositoryLabelValue := gitInternal.GetFullName(j.info.repoData.SourceUrl)
	if j.info.payload != nil && strings.TrimSpace(j.info.payload.FullName) != "" {
		repositoryLabelValue = j.info.payload.FullName
	}

	services, err := swarm.GetServicesByLabel(ctx, cli.Client(), docker.DocoCDLabels.Metadata.Manager, app.Name)
	if err != nil {
		jobLog.Error("failed to list swarm services for startup missing scan", logger.ErrAttr(err))
		return nil
	}

	existingStacks := set.New[string]()

	for _, svc := range services {
		// Filter by repository to avoid matching services from other repos on the same swarm.
		if strings.TrimSpace(svc.Spec.Labels[docker.DocoCDLabels.Source.Name]) != repositoryLabelValue {
			continue
		}

		if stackName := strings.TrimSpace(svc.Spec.Labels[docker.DocoCDLabels.Deployment.Name]); stackName != "" {
			existingStacks.Add(stackName)
		}
	}

	var missing []*deployConfig.Config

	for _, dc := range candidates {
		if !existingStacks.Contains(dc.Name) {
			jobLog.Debug("detected missing swarm services on startup", slog.String("stack", dc.Name))
			missing = append(missing, dc)
		}
	}

	return missing
}

// filterConfigsByContext returns the subset of dcs whose Context field matches contextName.
// The empty string matches configs with no explicit context (i.e. the default Docker context).
func filterConfigsByContext(dcs []*deployConfig.Config, contextName string) []*deployConfig.Config {
	var result []*deployConfig.Config

	for _, dc := range dcs {
		if dc != nil && strings.TrimSpace(dc.Context) == contextName {
			result = append(result, dc)
		}
	}

	return result
}
