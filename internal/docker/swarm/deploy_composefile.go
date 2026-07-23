package swarm

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/containerd/errdefs"
	"github.com/docker/cli/cli/command"

	"github.com/kimdre/doco-cd/internal/utils/set"

	"github.com/kimdre/doco-cd/internal/docker/options"

	"github.com/docker/cli/cli/compose/convert"
	composetypes "github.com/docker/cli/cli/compose/types"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	swarmTypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

func deployCompose(ctx context.Context, dockerCli command.Cli, opts *options.Deploy, config *composetypes.Config) error {
	isSwarmManager, err := checkDaemonIsSwarmManager(ctx, dockerCli.Client())
	if err != nil {
		return err
	}

	if !isSwarmManager {
		return errors.New("this node is not a swarm manager")
	}

	namespace := convert.NewNamespace(opts.Namespace)

	if opts.Prune {
		services := set.New[string]()
		for _, service := range config.Services {
			services.Add(service.Name)
		}

		pruneServices(ctx, dockerCli, namespace, services)
	}

	serviceNetworks := getServicesDeclaredNetworks(config.Services)

	networks, externalNetworks := convert.Networks(namespace, config.Networks, serviceNetworks)
	if err = validateExternalNetworks(ctx, dockerCli.Client(), externalNetworks); err != nil {
		return err
	}

	if err = createNetworks(ctx, dockerCli, namespace, networks); err != nil {
		return err
	}

	secrets, err := convert.Secrets(namespace, config.Secrets)
	if err != nil {
		return err
	}

	if err = createSecrets(ctx, dockerCli, secrets); err != nil {
		return err
	}

	configs, err := convert.Configs(namespace, config.Configs)
	if err != nil {
		return err
	}

	if err = createConfigs(ctx, dockerCli, configs); err != nil {
		return err
	}

	// Wait for all resources to be ready before deploying services
	if err = waitForResources(ctx, dockerCli.Client(), networks, secrets, configs); err != nil {
		return err
	}

	services, err := convert.Services(ctx, namespace, config, dockerCli.Client())
	if err != nil {
		return err
	}

	// Scheduled job services must not run as a side effect of the deployment.
	// Pin replicated scheduled services to 0 replicas (recording the intended
	// count) so only the scheduler runs them on their schedule.
	applyScheduledJobDeployReplicas(services)

	serviceIDs, err := deployServices(ctx, dockerCli, services, namespace, opts.SendRegistryAuth, opts.ResolveImage)
	if err != nil {
		return err
	}

	if opts.Detach {
		return nil
	}

	// Exclude job-mode and scheduler-managed services from the wait.
	// Job-mode services converge only after completions, and scheduler-managed
	// services are intentionally allowed to be non-running at deploy time.
	waitIDs := make([]string, 0, len(serviceIDs))

	for _, entry := range serviceIDs {
		if shouldWaitForService(entry) {
			waitIDs = append(waitIDs, entry.id)
		}
	}

	return WaitOnServices(ctx, dockerCli, waitIDs)
}

func getServicesDeclaredNetworks(serviceConfigs []composetypes.ServiceConfig) set.Set[string] {
	serviceNetworks := set.New[string]()

	for _, serviceConfig := range serviceConfigs {
		if len(serviceConfig.Networks) == 0 {
			serviceNetworks.Add("default")
			continue
		}

		for nw := range serviceConfig.Networks {
			serviceNetworks.Add(nw)
		}
	}

	return serviceNetworks
}

func validateExternalNetworks(ctx context.Context, apiClient client.NetworkAPIClient, externalNetworks []string) error {
	for _, networkName := range externalNetworks {
		if !container.NetworkMode(networkName).IsUserDefined() {
			// Networks that are not user defined always exist on all nodes as
			// local-scoped networks, so there's no need to inspect them.
			continue
		}

		result, err := apiClient.NetworkInspect(ctx, networkName, client.NetworkInspectOptions{})
		switch {
		case errdefs.IsNotFound(err):
			return fmt.Errorf("network %q is declared as external, but could not be found. You need to create a swarm-scoped network before the stack is deployed", networkName)
		case err != nil:
			return err
		case result.Network.Scope != "swarm":
			return fmt.Errorf("network %q is declared as external, but it is not in the right scope: %q instead of \"swarm\"", networkName, result.Network.Scope)
		}
	}

	return nil
}

func createSecrets(ctx context.Context, dockerCLI command.Cli, secrets []swarmTypes.SecretSpec) error {
	apiClient := dockerCLI.Client()

	for _, secretSpec := range secrets {
		result, err := apiClient.SecretInspect(ctx, secretSpec.Name, client.SecretInspectOptions{})
		switch {
		case err == nil:
			// secret already exists, then we update that
			if _, err := apiClient.SecretUpdate(ctx, result.Secret.ID, client.SecretUpdateOptions{
				Version: result.Secret.Version,
				Spec:    secretSpec,
			}); err != nil {
				return fmt.Errorf("failed to update secret %s: %w", secretSpec.Name, err)
			}
		case errdefs.IsNotFound(err):
			// secret does not exist, then we create a new one.
			_, _ = fmt.Fprintln(dockerCLI.Out(), "Creating secret", secretSpec.Name)
			if _, err := apiClient.SecretCreate(ctx, client.SecretCreateOptions{Spec: secretSpec}); err != nil {
				return fmt.Errorf("failed to create secret %s: %w", secretSpec.Name, err)
			}
		default:
			return err
		}
	}

	return nil
}

func createConfigs(ctx context.Context, dockerCLI command.Cli, configs []swarmTypes.ConfigSpec) error {
	apiClient := dockerCLI.Client()

	for _, configSpec := range configs {
		result, err := apiClient.ConfigInspect(ctx, configSpec.Name, client.ConfigInspectOptions{})
		switch {
		case err == nil:
			// config already exists, then we update that
			if _, err := apiClient.ConfigUpdate(ctx, result.Config.ID, client.ConfigUpdateOptions{
				Version: result.Config.Version,
				Spec:    configSpec,
			}); err != nil {
				return fmt.Errorf("failed to update config %s: %w", configSpec.Name, err)
			}
		case errdefs.IsNotFound(err):
			// config does not exist, then we create a new one.
			_, _ = fmt.Fprintln(dockerCLI.Out(), "Creating config", configSpec.Name)
			if _, err := apiClient.ConfigCreate(ctx, client.ConfigCreateOptions{Spec: configSpec}); err != nil {
				return fmt.Errorf("failed to create config %s: %w", configSpec.Name, err)
			}
		default:
			return err
		}
	}

	return nil
}

func createNetworks(ctx context.Context, dockerCLI command.Cli, namespace convert.Namespace, networks map[string]client.NetworkCreateOptions) error {
	apiClient := dockerCLI.Client()

	existingNetworks, err := getStackNetworks(ctx, apiClient, namespace.Name())
	if err != nil {
		return err
	}

	existingNetworkMap := make(map[string]network.Summary)
	for _, nw := range existingNetworks {
		existingNetworkMap[nw.Name] = nw
	}

	for name, createOpts := range networks {
		if _, exists := existingNetworkMap[name]; exists {
			continue
		}

		if createOpts.Driver == "" {
			createOpts.Driver = defaultNetworkDriver
		}

		_, _ = fmt.Fprintln(dockerCLI.Out(), "Creating network", name)
		if _, err := apiClient.NetworkCreate(ctx, name, createOpts); err != nil {
			return fmt.Errorf("failed to create network %s: %w", name, err)
		}
	}

	return nil
}

type deployedService struct {
	id          string
	isJobMode   bool
	isScheduled bool
}

const scheduledJobEnabledLabel = "cd.doco.job.enabled"

// scheduledJobRestartReplicasLabel stores the intended replica count for a
// restart-mode scheduled job. The service is deployed at 0 replicas so it does
// not run on deployment; the scheduler scales it up to this count when the job
// runs on its schedule.
const scheduledJobRestartReplicasLabel = "cd.doco.job.swarm.restart_replicas"

// applyScheduledJobDeployReplicas ensures scheduled job services do not run as a
// side effect of a deployment. Classic replicated scheduled services are pinned
// to 0 replicas and their intended replica count is recorded in a label so the
// scheduler can scale them up when the job runs on its schedule.
//
// Global-mode scheduled services cannot be scaled to 0 (a global service always
// runs one task per node) and are left unchanged.
func applyScheduledJobDeployReplicas(services map[string]swarmTypes.ServiceSpec) {
	for name, spec := range services {
		if !isScheduledServiceSpec(spec) {
			continue
		}

		if spec.Mode.Replicated == nil || spec.TaskTemplate.ContainerSpec == nil {
			continue
		}

		replicas := uint64(1)
		if spec.Mode.Replicated.Replicas != nil {
			replicas = *spec.Mode.Replicated.Replicas
		}

		if spec.TaskTemplate.ContainerSpec.Labels == nil {
			spec.TaskTemplate.ContainerSpec.Labels = map[string]string{}
		}

		spec.TaskTemplate.ContainerSpec.Labels[scheduledJobRestartReplicasLabel] = strconv.FormatUint(replicas, 10)

		zero := uint64(0)
		spec.Mode.Replicated.Replicas = &zero

		services[name] = spec
	}
}

func deployServices(ctx context.Context, dockerCLI command.Cli, services map[string]swarmTypes.ServiceSpec, namespace convert.Namespace, sendAuth bool, resolveImage string) ([]deployedService, error) {
	apiClient := dockerCLI.Client()
	out := dockerCLI.Out()

	existingServices, err := GetStackServices(ctx, apiClient, namespace.Name())
	if err != nil {
		return nil, err
	}

	existingServiceMap := make(map[string]swarmTypes.Service)
	for _, service := range existingServices {
		existingServiceMap[service.Spec.Name] = service
	}

	var deployed []deployedService

	for internalName, serviceSpec := range services {
		var (
			name        = namespace.Scope(internalName)
			image       = serviceSpec.TaskTemplate.ContainerSpec.Image
			encodedAuth string
		)

		isJob := serviceSpec.Mode.ReplicatedJob != nil || serviceSpec.Mode.GlobalJob != nil
		isScheduled := isScheduledServiceSpec(serviceSpec)

		if sendAuth {
			// Retrieve encoded auth token from the image reference
			encodedAuth, err = command.RetrieveAuthTokenFromImage(dockerCLI.ConfigFile(), image)
			if err != nil {
				return nil, err
			}
		}

		if service, exists := existingServiceMap[name]; exists {
			_, _ = fmt.Fprintf(out, "Updating service %s (id: %s)\n", name, service.ID)

			updateOpts := client.ServiceUpdateOptions{
				Version:             service.Version,
				Spec:                serviceSpec,
				EncodedRegistryAuth: encodedAuth,
			}

			switch resolveImage {
			case ResolveImageAlways:
				// image should be updated by the server using QueryRegistry
				updateOpts.QueryRegistry = true
			case ResolveImageChanged:
				if image != service.Spec.Labels[convert.LabelImage] {
					// Query the registry to resolve digest for the updated image
					updateOpts.QueryRegistry = true
				} else {
					// image has not changed; update the serviceSpec with the
					// existing information that was set by QueryRegistry on the
					// previous deploy. Otherwise this will trigger an incorrect
					// service update.
					serviceSpec.TaskTemplate.ContainerSpec.Image = service.Spec.TaskTemplate.ContainerSpec.Image
				}
			default:
				if image == service.Spec.Labels[convert.LabelImage] {
					// image has not changed; update the serviceSpec with the
					// existing information that was set by QueryRegistry on the
					// previous deploy. Otherwise this will trigger an incorrect
					// service update.
					serviceSpec.TaskTemplate.ContainerSpec.Image = service.Spec.TaskTemplate.ContainerSpec.Image
				}
			}

			// Stack deploy does not have a `--force` option. Preserve existing
			// ForceUpdate value so that tasks are not re-deployed if not updated.
			serviceSpec.TaskTemplate.ForceUpdate = service.Spec.TaskTemplate.ForceUpdate

			updateOpts.Spec = serviceSpec

			response, err := apiClient.ServiceUpdate(ctx, service.ID, updateOpts)
			if err != nil {
				return nil, fmt.Errorf("failed to update service %s: %w", name, err)
			}

			for _, warning := range response.Warnings {
				_, _ = fmt.Fprintln(dockerCLI.Err(), warning)
			}

			deployed = append(deployed, deployedService{id: service.ID, isJobMode: isJob, isScheduled: isScheduled})
		} else {
			_, _ = fmt.Fprintln(out, "Creating service", name)

			createOpts := client.ServiceCreateOptions{
				Spec:                serviceSpec,
				EncodedRegistryAuth: encodedAuth,
			}

			// query registry if flag disabling it was not set
			if resolveImage == ResolveImageAlways || resolveImage == ResolveImageChanged {
				createOpts.QueryRegistry = true
			}

			response, err := apiClient.ServiceCreate(ctx, createOpts)
			if err != nil {
				return nil, fmt.Errorf("failed to create service %s: %w", name, err)
			}

			deployed = append(deployed, deployedService{id: response.ID, isJobMode: isJob, isScheduled: isScheduled})
		}
	}

	return deployed, nil
}

func shouldWaitForService(svc deployedService) bool {
	return !svc.isJobMode && !svc.isScheduled
}

func isScheduledServiceSpec(spec swarmTypes.ServiceSpec) bool {
	if spec.TaskTemplate.ContainerSpec == nil || spec.TaskTemplate.ContainerSpec.Labels == nil {
		return false
	}

	raw, ok := spec.TaskTemplate.ContainerSpec.Labels[scheduledJobEnabledLabel]
	if !ok {
		return false
	}

	enabled, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false
	}

	return enabled
}

// WaitOnServices waits for the specified Swarm services to complete.
func WaitOnServices(ctx context.Context, dockerCli command.Cli, serviceIDs []string) error {
	var errs []error

	for _, serviceID := range serviceIDs {
		if err := waitOnService(ctx, dockerCli, serviceID); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", serviceID, err))
		}
	}

	return errors.Join(errs...)
}
