package swarm

import (
	"context"
	"errors"
	"fmt"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/compose/convert"
	composetypes "github.com/docker/cli/cli/compose/types"
	swarmTypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
	"github.com/moby/moby/client/pkg/versions"

	"github.com/kimdre/doco-cd/internal/utils/set"

	"github.com/kimdre/doco-cd/internal/docker/options"
)

const defaultNetworkDriver = "overlay"

// ResolveImage constants for controlling image resolution during deployment.
const (
	ResolveImageAlways  = "always"
	ResolveImageChanged = "changed"
	ResolveImageNever   = "never"
)

var ErrNotReplicatedService = errors.New("service is not in replicated or replicated-job mode")

// RunDeploy is the swarm implementation of docker stack deploy.
func RunDeploy(ctx context.Context, dockerCLI command.Cli, opts *options.Deploy, cfg *composetypes.Config) error {
	if err := validateResolveImageFlag(opts); err != nil {
		return err
	}
	// client side image resolution should not be done when the supported
	// server version is older than 1.30
	if versions.LessThan(dockerCLI.Client().ClientVersion(), "1.30") {
		opts.ResolveImage = ResolveImageNever
	}

	return deployCompose(ctx, dockerCLI, opts, cfg)
}

// validateResolveImageFlag validates the opts.resolveImage command line option.
func validateResolveImageFlag(opts *options.Deploy) error {
	switch opts.ResolveImage {
	case ResolveImageAlways, ResolveImageChanged, ResolveImageNever:
		return nil
	default:
		return fmt.Errorf("invalid option %s for resolve-image", opts.ResolveImage)
	}
}

// CheckDaemonIsSwarmManager does an Info API call to verify that the daemon is
// a swarm manager. This is necessary because we must create networks before we
// create services, but the API call for creating a network does not return a
// proper status code when it can't create a network in the "global" scope.
func CheckDaemonIsSwarmManager(ctx context.Context, dockerCli command.Cli) (bool, error) {
	result, err := dockerCli.Client().Info(ctx, client.InfoOptions{})
	if err != nil {
		return false, err
	}

	if !result.Info.Swarm.ControlAvailable {
		return false, nil
	}

	return true, nil
}

// pruneServices removes services that are no longer referenced in the source.
func pruneServices(ctx context.Context, dockerCCLI command.Cli, namespace convert.Namespace, services set.Set[string]) {
	apiClient := dockerCCLI.Client()

	oldServices, err := GetStackServices(ctx, apiClient, namespace.Name())
	if err != nil {
		_, _ = fmt.Fprintln(dockerCCLI.Err(), "Failed to list services:", err)
	}

	var pruneSvcSlice []swarmTypes.Service

	for _, service := range oldServices {
		if _, exists := services[namespace.Descope(service.Spec.Name)]; !exists {
			pruneSvcSlice = append(pruneSvcSlice, service)
		}
	}

	removeServices(ctx, dockerCCLI, pruneSvcSlice)
}

func ScaleService(ctx context.Context, dockerCLI command.Cli, serviceName string, replicas uint64, wait, force bool) error {
	apiClient := dockerCLI.Client()

	result, err := apiClient.ServiceInspect(ctx, serviceName, client.ServiceInspectOptions{})
	if err != nil {
		return err
	}

	service := result.Service

	if force {
		service.Spec.TaskTemplate.ForceUpdate++
	}

	// Handle replicated-job services
	if service.Spec.Mode.ReplicatedJob != nil {
		// Jobs may not have an update config (daemon rejects ServiceUpdate otherwise).
		service.Spec.UpdateConfig = nil
		service.Spec.RollbackConfig = nil

		// Treat `replicas` as "how many completions to run" and allow that many concurrently.
		service.Spec.Mode.ReplicatedJob.TotalCompletions = &replicas
		service.Spec.Mode.ReplicatedJob.MaxConcurrent = &replicas

		_, err = apiClient.ServiceUpdate(ctx, service.ID, client.ServiceUpdateOptions{
			Version: service.Version,
			Spec:    service.Spec,
		})

		return err
	}

	// Handle classic replicated services
	if service.Spec.Mode.Replicated == nil {
		return fmt.Errorf("%w: %s", ErrNotReplicatedService, serviceName)
	}

	service.Spec.Mode.Replicated.Replicas = &replicas

	_, err = apiClient.ServiceUpdate(ctx, service.ID, client.ServiceUpdateOptions{
		Version: service.Version,
		Spec:    service.Spec,
	})
	if err != nil {
		return err
	}

	if wait {
		if err := waitOnService(ctx, dockerCLI, serviceName); err != nil {
			return err
		}
	}

	return nil
}
