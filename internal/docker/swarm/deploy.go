package swarm

import (
	"context"
	"fmt"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/command/stack/swarm"
	"github.com/docker/cli/cli/compose/convert"
	composetypes "github.com/docker/cli/cli/compose/types"
	swarmTypes "github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/versions"

	"github.com/kimdre/doco-cd/internal/docker/options"
)

const defaultNetworkDriver = "overlay"

// RunDeploy is the swarm implementation of docker stack deploy.
func RunDeploy(ctx context.Context, dockerCLI command.Cli, opts *options.Deploy, cfg *composetypes.Config) error {
	if err := validateResolveImageFlag(opts); err != nil {
		return err
	}
	// client side image resolution should not be done when the supported
	// server version is older than 1.30
	if versions.LessThan(dockerCLI.Client().ClientVersion(), "1.30") {
		opts.ResolveImage = swarm.ResolveImageNever
	}

	return deployCompose(ctx, dockerCLI, opts, cfg)
}

// validateResolveImageFlag validates the opts.resolveImage command line option.
func validateResolveImageFlag(opts *options.Deploy) error {
	switch opts.ResolveImage {
	case swarm.ResolveImageAlways, swarm.ResolveImageChanged, swarm.ResolveImageNever:
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
	info, err := dockerCli.Client().Info(ctx)
	if err != nil {
		return false, err
	}

	if !info.Swarm.ControlAvailable {
		return false, nil
	}

	return true, nil
}

// pruneServices removes services that are no longer referenced in the source.
func pruneServices(ctx context.Context, dockerCCLI command.Cli, namespace convert.Namespace, services map[string]struct{}) {
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

	service, _, err := apiClient.ServiceInspectWithRaw(ctx, serviceName, swarmTypes.ServiceInspectOptions{})
	if err != nil {
		return err
	}

	if force {
		service.Spec.TaskTemplate.ForceUpdate++
	}

	if service.Spec.Mode.Replicated == nil {
		return fmt.Errorf("service %s is not in replicated mode", serviceName)
	}

	service.Spec.Mode.Replicated.Replicas = &replicas

	updateOpts := swarmTypes.ServiceUpdateOptions{}

	_, err = apiClient.ServiceUpdate(ctx, service.ID, service.Version, service.Spec, updateOpts)
	if err != nil {
		return err
	}

	if wait {
		// Wait for the service to scale
		err = waitOnService(ctx, dockerCLI, serviceName)
		if err != nil {
			return err
		}
	}

	return nil
}
