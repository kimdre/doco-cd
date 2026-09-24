package docker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/containerd/errdefs"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/common/types/set"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/selfupdate"
)

// captureSelfDriftSnapshot is persisted before staging the applier. A restart
// of the applier can therefore restore the *previous* project even when Compose
// has already removed a network and some of its attached containers.
func captureSelfDriftSnapshot(ctx context.Context, apiClient client.APIClient, project *types.Project) (*selfupdate.DriftSnapshot, error) {
	snapshot := &selfupdate.DriftSnapshot{
		Networks:   make(map[string]network.Inspect),
		Containers: make(map[string]container.InspectResponse),
	}
	filter := make(client.Filters).Add("label", api.ProjectLabel+"="+project.Name)

	containers, err := apiClient.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filter})
	if err != nil {
		return nil, fmt.Errorf("list project containers: %w", err)
	}

	for _, c := range containers.Items {
		inspected, inspectErr := apiClient.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
		if inspectErr != nil {
			return nil, fmt.Errorf("inspect project container %s: %w", c.ID, inspectErr)
		}

		snapshot.Containers[c.ID] = inspected.Container
	}

	nets, err := apiClient.NetworkList(ctx, client.NetworkListOptions{Filters: filter})
	if err != nil {
		return nil, fmt.Errorf("list project networks: %w", err)
	}

	for _, net := range nets.Items {
		inspected, inspectErr := apiClient.NetworkInspect(ctx, net.ID, client.NetworkInspectOptions{})
		if inspectErr != nil {
			return nil, fmt.Errorf("inspect project network %s: %w", net.Name, inspectErr)
		}

		key := inspected.Network.Labels[api.NetworkLabel]
		if desired, ok := project.Networks[key]; ok && !bool(desired.External) {
			hash, hashErr := compose.NetworkHash(&desired)
			if hashErr != nil {
				return nil, fmt.Errorf("hash network %s for snapshot: %w", net.Name, hashErr)
			}

			name := desired.Name
			if name == "" {
				name = project.Name + "_" + key
			}

			if net.Name != name || (inspected.Network.Labels[api.ConfigHashLabel] != "" &&
				inspected.Network.Labels[api.ConfigHashLabel] != hash) {
				for id := range inspected.Network.Containers {
					if _, ok := snapshot.Containers[id]; !ok {
						return nil, fmt.Errorf("network %s has a non-project container %s attached; cannot safely recreate it", net.Name, id)
					}
				}
			}
		}

		snapshot.Networks[net.Name] = inspected.Network
	}

	return snapshot, nil
}

// selfDriftStartServices selects services to restart based on which were
// running before the project networks changed.
func selfDriftStartServices(project *types.Project, snapshot *selfupdate.DriftSnapshot) (*types.Project, []string, set.Set[string], set.Set[string], error) {
	disabled, err := getAutostartDisabledServices(project)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	running := set.New[string]()

	for _, c := range snapshot.Containers {
		if c.State != nil && c.State.Running && c.Config != nil {
			running.Add(c.Config.Labels[api.ServiceLabel])
		}
	}

	services, err := getStartServicesForDeploy(project, disabled, running)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	jobs, err := getJobServices(project)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	oneShot, err := getOneShotServices(project)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	addOneShotServiceLabels(project, oneShot)
	startProject, err := projectForStart(project, jobs, disabled.Difference(running))

	return startProject, services, jobs, oneShot, err
}

// applySelfDriftProject runs the deferred full project update from the applier,
// including other services attached to the changing network.
func applySelfDriftProject(
	ctx context.Context,
	dockerCli command.Cli,
	apiClient client.APIClient,
	service api.Compose,
	project *types.Project,
	record *selfupdate.Record,
	store *selfupdate.Store,
	deployConfig *deploy.Config,
	dataMountPath string,
	log *slog.Logger,
) error {
	if record.Drift == nil {
		return errors.New("network drift has no recoverable project snapshot")
	}

	if err := validateSelfUpdateVolumes(ctx, apiClient, project); err != nil {
		return err
	}

	startProject, startServices, jobs, oneShot, err := selfDriftStartServices(project, record.Drift)
	if err != nil {
		return fmt.Errorf("select services for the network update: %w", err)
	}

	// On restart, never attempt to apply a second time to partially changed
	// resources. The persistent marker makes a crash at any point in Create,
	// Start, or the health gate an unambiguous rollback on the next run.
	if !record.DriftStarted {
		record.DriftStarted = true
		if err = store.Save(*record); err != nil {
			return fmt.Errorf("record network update before changing resources: %w", err)
		}
	}

	recreate := record.Deploy.RecreateMode
	if recreate == "" {
		recreate = api.RecreateDiverged
	}

	services := slices.Clone(record.Deploy.Services)
	if len(services) > 0 && !slices.Contains(services, record.Service) {
		services = append(services, record.Service)
	}

	if err = validateSelfUpdateVolumes(ctx, apiClient, project); err != nil {
		return err
	}

	err = service.Create(ctx, project, api.CreateOptions{
		Services:             services,
		RemoveOrphans:        record.Deploy.RemoveOrphans,
		Recreate:             recreate,
		RecreateDependencies: api.RecreateDiverged,
		QuietPull:            true,
	})
	if err != nil {
		return fmt.Errorf("recreate the self stack and its networks: %w", err)
	}

	selfupdate.MaybeCrash(dataMountPath, "network_recreated", log)

	successor, err := findSelfSuccessor(ctx, apiClient, &selfTarget{
		Project: record.Stack,
		Service: record.Service,
		Context: record.Context,
	}, record.Predecessor.ID)
	if err != nil {
		return err
	}

	record.Successor = successor
	if err = store.Save(*record); err != nil {
		return fmt.Errorf("record self-update successor: %w", err)
	}

	timeout := selfHealthTimeout(*record, deployConfig)

	if len(startServices) > 0 {
		err = service.Start(ctx, project.Name, api.StartOptions{Project: startProject, Services: startServices})
		if err != nil && !errors.Is(err, ErrNoContainerToStart) {
			return fmt.Errorf("start the updated services: %w", err)
		}

		if err = waitForStartedServices(ctx, dockerCli, project.Name, startServices, jobs, oneShot, timeout); err != nil {
			return fmt.Errorf("wait for updated services: %w", err)
		}
	}

	return selfupdate.WaitHealthy(ctx, apiClient, successor.ID, timeout, log)
}

// restoreSelfDriftProject converges a partial Create, a failed health gate, or
// an applier crash back to the exact old network and container configurations.
func restoreSelfDriftProject(
	ctx context.Context,
	apiClient client.APIClient,
	record selfupdate.Record,
	log *slog.Logger,
) (selfupdate.ContainerRef, error) {
	if record.Drift == nil {
		return selfupdate.ContainerRef{}, errors.New("network drift has no recoverable project snapshot")
	}

	ctx = context.WithoutCancel(ctx)
	snapshot := record.Drift
	filter := make(client.Filters).Add("label", api.ProjectLabel+"="+record.Stack)

	list, err := apiClient.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filter})
	if err != nil {
		return selfupdate.ContainerRef{}, fmt.Errorf("list containers for network rollback: %w", err)
	}

	// Remove replacements and old containers whose network attachment changed.
	// Leave unmodified containers (notably a still-running predecessor) alone.
	for _, c := range list.Items {
		old, existed := snapshot.Containers[c.ID]
		if existed {
			current, inspectErr := apiClient.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
			if inspectErr != nil {
				return selfupdate.ContainerRef{}, fmt.Errorf("inspect previous container %s: %w", c.ID, inspectErr)
			}

			if sameSnapshotNetworks(old, current.Container) {
				continue
			}
		}

		if _, err = apiClient.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
			return selfupdate.ContainerRef{}, fmt.Errorf("remove updated container %s: %w", c.ID, err)
		}
	}

	live, err := apiClient.NetworkList(ctx, client.NetworkListOptions{Filters: filter})
	if err != nil {
		return selfupdate.ContainerRef{}, fmt.Errorf("list networks for rollback: %w", err)
	}

	for _, net := range live.Items {
		old, existed := snapshot.Networks[net.Name]
		if existed && old.ID == net.ID {
			continue
		}

		if _, err = apiClient.NetworkRemove(ctx, net.ID, client.NetworkRemoveOptions{}); err != nil && !errdefs.IsNotFound(err) {
			return selfupdate.ContainerRef{}, fmt.Errorf("remove updated network %s: %w", net.Name, err)
		}
	}

	for _, name := range slices.Sorted(maps.Keys(snapshot.Networks)) {
		old := snapshot.Networks[name]

		found, inspectErr := apiClient.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
		if inspectErr == nil && found.Network.ID == old.ID {
			continue
		}

		if inspectErr != nil && !errdefs.IsNotFound(inspectErr) {
			return selfupdate.ContainerRef{}, fmt.Errorf("inspect previous network %s: %w", name, inspectErr)
		}

		ipv4, ipv6, ipam := old.EnableIPv4, old.EnableIPv6, old.IPAM

		_, err = apiClient.NetworkCreate(ctx, name, client.NetworkCreateOptions{
			Driver: old.Driver, Scope: old.Scope, Options: old.Options,
			Labels: old.Labels, Internal: old.Internal, Attachable: old.Attachable,
			EnableIPv4: &ipv4, EnableIPv6: &ipv6, IPAM: &ipam,
		})
		if err != nil {
			return selfupdate.ContainerRef{}, fmt.Errorf("restore previous network %s: %w", name, err)
		}
	}

	var restored selfupdate.ContainerRef

	for _, id := range slices.Sorted(maps.Keys(snapshot.Containers)) {
		old := snapshot.Containers[id]

		existing, inspectErr := apiClient.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
		if inspectErr != nil && !errdefs.IsNotFound(inspectErr) {
			return selfupdate.ContainerRef{}, fmt.Errorf("inspect previous container %s: %w", id, inspectErr)
		}

		if errdefs.IsNotFound(inspectErr) {
			opts := RestoreCreateFromSnapshot(old)

			created, createErr := apiClient.ContainerCreate(ctx, opts)
			if createErr != nil {
				return selfupdate.ContainerRef{}, fmt.Errorf("restore previous container %s: %w", opts.Name, createErr)
			}

			id = created.ID

			existing, inspectErr = apiClient.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
			if inspectErr != nil {
				return selfupdate.ContainerRef{}, fmt.Errorf("inspect restored container %s: %w", id, inspectErr)
			}
		}

		if old.State != nil && old.State.Running && (existing.Container.State == nil || !existing.Container.State.Running) {
			if _, err = apiClient.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
				return selfupdate.ContainerRef{}, fmt.Errorf("start previous container %s: %w", id, err)
			}
		} else if old.State != nil && !old.State.Running && existing.Container.State != nil && existing.Container.State.Running {
			if _, err = apiClient.ContainerStop(ctx, id, client.ContainerStopOptions{}); err != nil {
				return selfupdate.ContainerRef{}, fmt.Errorf("stop previously inactive container %s: %w", id, err)
			}
		}

		if old.ID == record.Predecessor.ID {
			restored = selfupdate.ContainerRef{ID: id, Name: strings.TrimPrefix(old.Name, "/"), Number: record.Predecessor.Number}
		}
	}

	if restored.ID == "" {
		return selfupdate.ContainerRef{}, errors.New("predecessor missing from network drift snapshot")
	}

	log.Info("self-update: previous project networks and containers restored", slog.String("predecessor_id", restored.ID))

	return restored, nil
}

// sameSnapshotNetworks checks whether a container still uses its original
// network identities, so rollback can leave unchanged containers alone.
func sameSnapshotNetworks(old, current container.InspectResponse) bool {
	if old.NetworkSettings == nil || current.NetworkSettings == nil {
		return old.NetworkSettings == nil && current.NetworkSettings == nil
	}

	if len(old.NetworkSettings.Networks) != len(current.NetworkSettings.Networks) {
		return false
	}

	for name, previous := range old.NetworkSettings.Networks {
		now, ok := current.NetworkSettings.Networks[name]
		if !ok || previous == nil || now == nil {
			if !ok || previous != now {
				return false
			}

			continue
		}

		if previous.NetworkID != now.NetworkID {
			return false
		}
	}

	return true
}
