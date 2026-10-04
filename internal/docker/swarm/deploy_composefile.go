package swarm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/cli/cli/command"

	"github.com/kimdre/doco-cd/internal/common/types/set"

	"github.com/kimdre/doco-cd/internal/docker/options"
	"github.com/kimdre/doco-cd/internal/docker/registryauth"

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

		pruneServices(ctx, dockerCli, namespace, services, opts.Logger)
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

	deployedServices, err := deployServices(ctx, dockerCli, services, namespace, opts)
	if err != nil {
		return err
	}

	if opts.Detach {
		return nil
	}

	// Exclude job-mode and scheduler-managed services from the wait.
	// Job-mode services converge only after completions, and scheduler-managed
	// services are intentionally allowed to be non-running at deploy time.
	waitServices := make([]deployedService, 0, len(deployedServices))

	for _, entry := range deployedServices {
		if shouldWaitForService(entry) {
			waitServices = append(waitServices, entry)
		}
	}

	return waitOnServicesWith(ctx, waitServices, opts.Timeout, func(ctx context.Context, service deployedService) error {
		logService(opts.Logger, "waiting for service to converge", service)
		_, _ = fmt.Fprintf(dockerCli.Out(), "Waiting for service %s to converge\n", ServiceIdentity(service.name, service.id))

		err := waitOnService(ctx, dockerCli, service.id)
		if err != nil {
			logServiceError(opts.Logger, "service convergence failed", service, err)
		}

		return err
	})
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
	name        string
	isJobMode   bool
	isScheduled bool
}

// UnavailableIdentity marks a service name or ID that could not be determined.
const UnavailableIdentity = "unavailable"

// OrUnavailable returns value, or UnavailableIdentity when value is blank.
func OrUnavailable(value string) string {
	if strings.TrimSpace(value) == "" {
		return UnavailableIdentity
	}

	return value
}

// ServiceIdentity preserves the full Docker identifier alongside its name.
func ServiceIdentity(name, id string) string {
	return fmt.Sprintf("%s (id=%s)", OrUnavailable(name), OrUnavailable(id))
}

// ServiceLogAttrs returns the structured service name and ID fields.
func ServiceLogAttrs(name, id string) []any {
	return []any{slog.String("service", OrUnavailable(name)), slog.String("service_id", OrUnavailable(id))}
}

func logService(log *slog.Logger, message string, service deployedService) {
	if log != nil {
		log.Debug(message, ServiceLogAttrs(service.name, service.id)...)
	}
}

func logServiceError(log *slog.Logger, message string, service deployedService, err error) {
	if log != nil {
		log.Error(message, append(ServiceLogAttrs(service.name, service.id), slog.Any("error", err))...)
	}
}

const scheduledJobEnabledLabel = "cd.doco.job.enabled"

// scheduledJobRestartReplicasLabel stores the intended replica count for a
// restart-mode scheduled job. The service is deployed at 0 replicas so it does
// not run on deployment; the scheduler scales it up to this count when the job
// runs on its schedule.
//
// This must stay in sync with docker.DocoCDJobLabels.JobRestartReplicas. It is
// duplicated here because the docker package imports this package, so the label
// definitions cannot be imported from there without creating an import cycle.
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

func deployServices(ctx context.Context, dockerCLI command.Cli, services map[string]swarmTypes.ServiceSpec, namespace convert.Namespace, opts *options.Deploy) ([]deployedService, error) {
	apiClient := dockerCLI.Client()
	out := dockerCLI.Out()
	log := opts.Logger

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

		if opts.SendRegistryAuth {
			// Retrieve encoded auth token from the image reference
			encodedAuth, err = command.RetrieveAuthTokenFromImage(dockerCLI.ConfigFile(), image)
			if err != nil {
				return nil, registryauth.WrapLookupError(dockerCLI.ConfigFile(), image, err)
			}
		}

		if service, exists := existingServiceMap[name]; exists {
			identity := deployedService{id: service.ID, name: name, isJobMode: isJob, isScheduled: isScheduled}
			_, _ = fmt.Fprintf(out, "Updating service %s\n", ServiceIdentity(name, service.ID))

			logService(log, "updating service", identity)

			updateOpts := client.ServiceUpdateOptions{
				Version:             service.Version,
				Spec:                serviceSpec,
				EncodedRegistryAuth: encodedAuth,
			}

			switch opts.ResolveImage {
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

			serviceSpec.TaskTemplate.ForceUpdate = service.Spec.TaskTemplate.ForceUpdate
			// Recover stale mounts in the same update as deployment metadata, so a failed
			// restart cannot erase the timestamp-based recovery signal.
			if !isJob && slices.Contains(opts.ForceUpdateServices, internalName) {
				serviceSpec.TaskTemplate.ForceUpdate++
			}

			updateOpts.Spec = serviceSpec

			response, err := apiClient.ServiceUpdate(ctx, service.ID, updateOpts)
			if err != nil {
				logServiceError(log, "failed to update service", identity, err)
				return nil, fmt.Errorf("failed to update service %s: %w", ServiceIdentity(name, service.ID), err)
			}

			for _, warning := range response.Warnings {
				_, _ = fmt.Fprintln(dockerCLI.Err(), warning)
			}

			deployed = append(deployed, identity)
		} else {
			_, _ = fmt.Fprintln(out, "Creating service", ServiceIdentity(name, ""))
			createOpts := client.ServiceCreateOptions{
				Spec:                serviceSpec,
				EncodedRegistryAuth: encodedAuth,
			}

			// query registry if flag disabling it was not set
			if opts.ResolveImage == ResolveImageAlways || opts.ResolveImage == ResolveImageChanged {
				createOpts.QueryRegistry = true
			}

			response, err := apiClient.ServiceCreate(ctx, createOpts)
			if err != nil {
				logServiceError(log, "failed to create service", deployedService{name: name}, err)
				return nil, fmt.Errorf("failed to create service %s: %w", ServiceIdentity(name, ""), err)
			}

			identity := deployedService{id: response.ID, name: name, isJobMode: isJob, isScheduled: isScheduled}
			_, _ = fmt.Fprintln(out, "Created service", ServiceIdentity(name, response.ID))

			logService(log, "created service", identity)
			deployed = append(deployed, identity)
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

// WaitOnServices waits for the specified Swarm services to converge.
func WaitOnServices(ctx context.Context, dockerCli command.Cli, serviceIDs []string) error {
	return waitOnServices(ctx, servicesFromIDs(serviceIDs), func(ctx context.Context, service deployedService) error {
		return waitOnService(ctx, dockerCli, service.id)
	})
}

// WaitOnServicesWithTimeout waits for Swarm services to converge within timeout.
func WaitOnServicesWithTimeout(
	ctx context.Context,
	dockerCli command.Cli,
	serviceIDs []string,
	timeout time.Duration,
) error {
	return waitOnServicesWith(ctx, servicesFromIDs(serviceIDs), timeout, func(ctx context.Context, service deployedService) error {
		return waitOnService(ctx, dockerCli, service.id)
	})
}

func servicesFromIDs(ids []string) []deployedService {
	services := make([]deployedService, 0, len(ids))
	for _, id := range ids {
		services = append(services, deployedService{id: id})
	}

	return services
}

// waitOnServices waits for every service and joins their errors.
func waitOnServices(
	ctx context.Context,
	services []deployedService,
	wait func(context.Context, deployedService) error,
) error {
	var errs []error

	for _, service := range services {
		if err := wait(ctx, service); err != nil {
			errs = append(errs, fmt.Errorf("service %s: %w", ServiceIdentity(service.name, service.id), err))
		}
	}

	return errors.Join(errs...)
}

// waitOnServicesWith applies one timeout across all service waits.
func waitOnServicesWith(
	ctx context.Context,
	services []deployedService,
	timeout time.Duration,
	wait func(context.Context, deployedService) error,
) error {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	err := waitOnServices(waitCtx, services, wait)
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return fmt.Errorf("timed out after %s waiting for swarm services to converge: %w", timeout, err)
	}

	return err
}
