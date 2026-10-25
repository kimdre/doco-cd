package docker

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"github.com/moby/moby/api/types/swarm"

	"github.com/kimdre/doco-cd/internal/config/app"
	swarmInternal "github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/prometheus"
)

// NewServiceStateSource returns a source for the service state metrics.
// It queries every Docker context in Swarm mode and reports the services
// that doco-cd deployed. A context that fails to answer reports no services,
// so its metrics never show old values.
//
// The contexts are queried at the same time. A slow or unreachable context
// then does not delay the result of the other contexts.
func NewServiceStateSource(contexts *ContextRegistry, log *slog.Logger) prometheus.ServiceStateSource {
	return func(ctx context.Context) []prometheus.ServiceState {
		names, err := contexts.Names()
		if err != nil {
			log.Debug("failed to list docker contexts for service state metrics", slog.Any("error", err))

			return nil
		}

		// Names are sorted. Each goroutine writes only to its own slot,
		// so the result order stays stable.
		perContext := make([][]prometheus.ServiceState, len(names))

		var wg sync.WaitGroup

		for i, name := range names {
			wg.Go(func() {
				perContext[i] = contextServiceStates(ctx, contexts, name, log)
			})
		}

		wg.Wait()

		var states []prometheus.ServiceState
		for _, s := range perContext {
			states = append(states, s...)
		}

		return states
	}
}

// contextServiceStates returns the service states of one Docker context.
// It returns nil if the context is not in Swarm mode or does not answer.
func contextServiceStates(ctx context.Context, contexts *ContextRegistry, name string, log *slog.Logger) []prometheus.ServiceState {
	contextName := DisplayContextName(name)

	contextClient, err := contexts.getKnown(ctx, name)
	if err != nil {
		log.Debug("failed to get docker context for service state metrics",
			slog.String("context", contextName), slog.Any("error", err))

		return nil
	}

	if !contextClient.SwarmMode {
		return nil
	}

	services, err := swarmInternal.GetServicesWithStatusByLabel(ctx, contextClient.Cli.Client(), DocoCDLabels.Metadata.Manager, app.Name)
	if err != nil {
		log.Debug("failed to list services for service state metrics",
			slog.String("context", contextName), slog.Any("error", err))

		return nil
	}

	return serviceStatesFromSwarm(contextName, services)
}

// serviceStatesFromSwarm converts Swarm services to service state metric samples.
// Jobs are left out because they have no steady task count to compare against.
func serviceStatesFromSwarm(contextName string, services []swarm.Service) []prometheus.ServiceState {
	states := make([]prometheus.ServiceState, 0, len(services))

	for _, service := range services {
		stack := service.Spec.Labels[swarmInternal.StackNamespaceLabel]
		if stack == "" || isSwarmJobService(service) {
			continue
		}

		state := prometheus.ServiceState{
			Context: contextName,
			Stack:   stack,
			Service: strings.TrimPrefix(service.Spec.Name, stack+"_"),
		}

		if service.ServiceStatus != nil {
			state.DesiredTasks = service.ServiceStatus.DesiredTasks
			state.RunningTasks = service.ServiceStatus.RunningTasks
		}

		if service.UpdateStatus != nil {
			state.UpdateState = string(service.UpdateStatus.State)
		}

		states = append(states, state)
	}

	return states
}

// isSwarmJobService reports whether a service runs jobs instead of long-running tasks.
// Scheduled services are jobs too. Their tasks stop after each run, so their
// running task count is often lower than the desired task count.
func isSwarmJobService(service swarm.Service) bool {
	if service.Spec.Mode.ReplicatedJob != nil || service.Spec.Mode.GlobalJob != nil {
		return true
	}

	// doco-cd writes the ephemeral label to the service spec of one-off runs.
	// The user writes the job config labels, which live in the task template.
	return isTrueLabel(service.Spec.Labels[DocoCDJobLabels.JobEphemeral]) ||
		isTrueLabel(SwarmJobLabels(service)[DocoCDJobLabels.JobEnabled])
}

func isTrueLabel(value string) bool {
	enabled, err := strconv.ParseBool(strings.TrimSpace(value))

	return err == nil && enabled
}
