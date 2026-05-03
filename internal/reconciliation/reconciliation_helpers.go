package reconciliation

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/utils/set"
)

func dockerEventTypeForMode(swarmMode bool) string {
	if swarmMode {
		return "service"
	}

	return "container"
}

func dockerEventFiltersForActions(actions []string, swarmMode bool) []string {
	filters := set.New[string]()

	for _, rawAction := range actions {
		action := normalizeReconciliationEventAction(rawAction)
		if action == "" {
			continue
		}

		switch action {
		case "unhealthy":
			// Only subscribe to unhealthy health transitions, not healthy ones.
			filters.Add("health_status: unhealthy")
		case "destroy":
			if swarmMode {
				filters.Add("remove")
				continue
			}

			filters.Add("destroy")
		default:
			filters.Add(action)
		}
	}

	return filters.ToSlice()
}

// containerRemovalSettleTimeout caps how long handleEvent waits for a force-removed
// container to be fully gone before kicking off a reconciliation deploy.
const containerRemovalSettleTimeout = 15 * time.Second

// waitForContainerRemovalSettled polls the given container until it is either gone
// (inspect returns not-found) or no longer reported as "removing", or until the
// timeout elapses. This prevents a race between Docker's async container teardown
// and docker compose trying to recreate the container.
func waitForContainerRemovalSettled(ctx context.Context, jobLog *slog.Logger, cli client.APIClient, containerID string, timeout time.Duration) {
	if containerID == "" || timeout <= 0 {
		return
	}

	deadline := time.Now().Add(timeout)

	for {
		inspectResult, err := cli.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
		if err != nil {
			// Treat any inspect error (most importantly "no such container") as
			// "container is gone, safe to proceed".
			if errdefs.IsNotFound(err) {
				return
			}

			jobLog.Debug("failed to inspect container while waiting for removal to settle",
				slog.String("container_id", shortID(containerID)),
				logger.ErrAttr(err),
			)

			return
		}

		state := inspectResult.Container.State
		if state == nil || !strings.EqualFold(strings.TrimSpace(string(state.Status)), "removing") {
			return
		}

		if !time.Now().Before(deadline) {
			jobLog.Debug("timed out waiting for container removal to settle",
				slog.String("container_id", shortID(containerID)),
				slog.Duration("timeout", timeout),
			)

			return
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func deployConfigsByName(dcs []*config.DeployConfig, name string) []*config.DeployConfig {
	result := make([]*config.DeployConfig, 0, len(dcs))

	for _, dc := range dcs {
		if dc.Name == name {
			result = append(result, dc)
		}
	}

	return result
}

// stackNameFromEvent attempts to determine the stack name referenced by the given Docker event
// by examining various event attributes and matching them against the candidate config.DeployConfig configs.
// Returns an empty string when no stack name could be determined or matched.
func stackNameFromEvent(event events.Message, candidates []*config.DeployConfig) string {
	attrs := event.Actor.Attributes

	for _, key := range []string{
		docker.DocoCDLabels.Deployment.Name,
		swarm.StackNamespaceLabel,
		"name",
		"service",
		"com.docker.swarm.service.name",
		"com.docker.swarm.task.name",
	} {
		identifier := strings.TrimSpace(attrs[key])
		if identifier == "" {
			continue
		}

		if matched := matchCandidateStackName(identifier, candidates); matched != "" {
			return matched
		}
	}

	return ""
}

// matchCandidateStackName checks if the given identifier matches any of the candidate DeployConfig stack names,
// either as an exact match or as a prefix followed by typical Docker naming separators.
func matchCandidateStackName(identifier string, candidates []*config.DeployConfig) string {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return ""
	}

	for _, dc := range candidates {
		if dc == nil {
			continue
		}

		name := strings.TrimSpace(dc.Name)
		if name == "" {
			continue
		}

		if identifier == name {
			return name
		}

		// Docker Swarm service names are typically formatted as <stack>_<service>.
		// Some event attributes can also contain task/container names such as
		// <stack>_<service>.<slot>.<id>, so matching by prefix keeps this resilient.
		if strings.HasPrefix(identifier, name+"_") ||
			strings.HasPrefix(identifier, name+".") ||
			strings.HasPrefix(identifier, name+"-") {
			return dc.Name
		}
	}

	return ""
}

func cloneDeployConfigsWithForcedRecreate(dcs []*config.DeployConfig) []*config.DeployConfig {
	reconcileDCs := make([]*config.DeployConfig, len(dcs))

	for i, dc := range dcs {
		dcCopy := *dc
		dcCopy.ForceRecreate = true
		reconcileDCs[i] = &dcCopy
	}

	return reconcileDCs
}

func normalizeReconciliationEventAction(action string) string {
	action = strings.ToLower(strings.TrimSpace(action))
	action = strings.Join(strings.Fields(action), " ")

	switch action {
	case "remove", "delete":
		return "destroy"
	case "health_status: unhealthy":
		return "unhealthy"
	}

	return action
}

// shortID returns the first 12 characters of a container ID, matching the Docker CLI convention.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}

	return id
}

// mapsKeys returns the keys of the given map as a slice.
func mapsKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))

	for key := range m {
		keys = append(keys, key)
	}

	return keys
}
