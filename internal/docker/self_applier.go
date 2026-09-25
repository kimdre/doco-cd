package docker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/selfupdate"
)

// BuildSelfApplierCreate derives the create options for a throwaway clone of
// the running doco-cd container that finishes the self-update from outside.
func BuildSelfApplierCreate(inspect container.InspectResponse, journalID, stack string) client.ContainerCreateOptions {
	config := *inspect.Config
	config.Cmd = []string{"apply-self", journalID}
	config.Healthcheck = &container.HealthConfig{Test: []string{"NONE"}}
	config.ExposedPorts = nil
	// A clone must not answer DNS for the service or inherit the compose
	// identity, so it cannot be mistaken for a replica or reaped as an orphan.
	config.Hostname = ""
	config.Labels = applierLabels(inspect.Config.Labels, journalID, stack)

	hostConfig := *inspect.HostConfig
	hostConfig.PortBindings = nil
	hostConfig.AutoRemove = false
	hostConfig.Links = nil
	// Bridge survives project-network recreation. The clone also joins the
	// predecessor's networks for secret-provider DNS during preflight, then
	// leaves project networks before Compose may recreate them.
	// Host, container and none modes have no project networks to drift, so they
	// keep the predecessor's mode (e.g. for a secret provider on localhost).
	if mode := hostConfig.NetworkMode; !mode.IsHost() && !mode.IsContainer() && !mode.IsNone() {
		hostConfig.NetworkMode = "bridge"
	}

	hostConfig.RestartPolicy = container.RestartPolicy{
		Name:              container.RestartPolicyOnFailure,
		MaximumRetryCount: selfupdate.ApplierMaxRestarts,
	}

	opts := client.ContainerCreateOptions{
		Config:     &config,
		HostConfig: &hostConfig,
		Name:       applierName(inspect.Name),
	}

	return opts
}

// applierLabels strips every compose and doco-cd deployment label so the clone
// is invisible to reconciliation, and tags it for cleanup.
func applierLabels(source map[string]string, journalID, stack string) map[string]string {
	labels := make(map[string]string, len(source)+2)

	for k, v := range source {
		if strings.HasPrefix(k, "com.docker.compose.") || strings.HasPrefix(k, "cd.doco.") {
			continue
		}

		labels[k] = v
	}

	labels[SelfApplierLabel] = journalID
	labels[SelfStackLabel] = stack

	return labels
}

func applierName(containerName string) string {
	base := strings.TrimPrefix(containerName, "/")
	if base == "" {
		base = "doco-cd"
	}

	return base + "-self-applier-" + strconv.FormatInt(time.Now().UTC().UnixNano(), 10)
}

// selfUpdateStageApplier starts the clone that replaces this container. The
// predecessor keeps serving until the applier stops it.
func selfUpdateStageApplier(ctx context.Context, apiClient client.APIClient, record *selfupdate.Record, log *slog.Logger) error {
	opts := SelfUpdateConfig()
	return stageSelfApplier(ctx, apiClient, opts.Store, opts.Identity.ContainerID, opts.DataMountPath, record, log)
}

type selfApplierJournal interface {
	Save(selfupdate.Record) error
	Update(selfupdate.Record, selfupdate.State, selfupdate.Actor) (selfupdate.Record, error)
}

// stageSelfApplier creates and starts a recoverable clone, cleaning it up or
// retaining its journal entry if any staging step fails.
func stageSelfApplier(
	ctx context.Context,
	apiClient client.APIClient,
	store selfApplierJournal,
	ownID, dataMountPath string,
	record *selfupdate.Record,
	log *slog.Logger,
) error {
	inspect, err := apiClient.ContainerInspect(ctx, ownID, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect own container for applier clone: %w", err)
	}

	createOpts := BuildSelfApplierCreate(inspect.Container, record.ID, record.Stack)

	created, err := apiClient.ContainerCreate(ctx, createOpts)
	if err != nil {
		return fmt.Errorf("create applier container: %w", err)
	}

	record.Applier = selfupdate.ContainerRef{ID: created.ID, Name: createOpts.Name}

	if err = connectSelfApplierNetworks(ctx, apiClient, created.ID, inspect.Container, nil); err != nil {
		return fmt.Errorf("connect applier for preflight: %w", err)
	}

	if err = store.Save(*record); err != nil {
		return fmt.Errorf("record applier: %w", err)
	}

	updated, err := store.Update(*record, selfupdate.StateApplying, selfupdate.ActorPredecessor)
	if err != nil {
		return fmt.Errorf("mark applier applying: %w", err)
	}

	*record = updated

	if _, err = apiClient.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start applier container: %w", err)
	}

	log.Info("self-update: applier started",
		slog.String("applier_id", created.ID),
		slog.String("applier_name", createOpts.Name),
	)

	selfupdate.MaybeCrash(dataMountPath, string(selfupdate.StateApplying), log)

	return nil
}

// connectSelfApplierNetworks gives the clone service DNS access without
// inheriting the predecessor's aliases or addresses.
func connectSelfApplierNetworks(
	ctx context.Context, apiClient client.APIClient, applierID string,
	predecessor container.InspectResponse, connected map[string]*network.EndpointSettings,
) error {
	if predecessor.NetworkSettings == nil {
		return nil
	}

	for _, name := range slices.Sorted(maps.Keys(predecessor.NetworkSettings.Networks)) {
		if name == "bridge" || name == "host" || name == "none" {
			continue
		}

		if _, ok := connected[name]; ok {
			continue
		}

		endpoint := predecessor.NetworkSettings.Networks[name]
		if endpoint == nil {
			return fmt.Errorf("predecessor network %s has no endpoint", name)
		}

		id := endpoint.NetworkID
		if id == "" {
			id = name
		}
		// Do not copy service aliases or addresses: the clone must be able to
		// resolve services without answering requests as the predecessor.
		if _, err := apiClient.NetworkConnect(ctx, id, client.NetworkConnectOptions{Container: applierID}); err != nil {
			return fmt.Errorf("connect applier to network %s: %w", name, err)
		}
	}

	return nil
}

// ensureSelfApplierPreflightNetworks restores project-network access after an
// applier restart so secret-provider preflight can still use service DNS.
func ensureSelfApplierPreflightNetworks(
	ctx context.Context, apiClient client.APIClient, record selfupdate.Record,
) error {
	if record.Drift == nil {
		return errors.New("network drift has no recoverable project snapshot")
	}

	predecessor, ok := record.Drift.Containers[record.Predecessor.ID]
	if !ok {
		return fmt.Errorf("predecessor %s is missing from the network snapshot", record.Predecessor.ID)
	}

	applier, err := apiClient.ContainerInspect(ctx, record.Applier.ID, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect applier preflight networks: %w", err)
	}

	var connected map[string]*network.EndpointSettings
	if applier.Container.NetworkSettings != nil {
		connected = applier.Container.NetworkSettings.Networks
	}

	return connectSelfApplierNetworks(ctx, apiClient, record.Applier.ID, predecessor, connected)
}

// detachSelfApplierProjectNetworks releases old networks before Compose
// recreates them, retaining the clone's stable bridge connection.
func detachSelfApplierProjectNetworks(
	ctx context.Context, apiClient client.APIClient, record selfupdate.Record,
) error {
	if record.Drift == nil {
		return errors.New("network drift has no recoverable project snapshot")
	}

	applier, err := apiClient.ContainerInspect(ctx, record.Applier.ID, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect applier before network recreation: %w", err)
	}

	if applier.Container.NetworkSettings == nil || applier.Container.NetworkSettings.Networks["bridge"] == nil {
		return fmt.Errorf("applier %s has no stable bridge connection for network recreation", record.Applier.ID)
	}

	for _, name := range slices.Sorted(maps.Keys(record.Drift.Networks)) {
		endpoint := applier.Container.NetworkSettings.Networks[name]
		if endpoint == nil {
			continue
		}

		old := record.Drift.Networks[name]
		if endpoint.NetworkID != "" && endpoint.NetworkID != old.ID {
			return fmt.Errorf("applier network %s changed since the snapshot; refusing recreation", name)
		}

		if _, err := apiClient.NetworkDisconnect(ctx, old.ID, client.NetworkDisconnectOptions{Container: record.Applier.ID}); err != nil {
			return fmt.Errorf("detach applier from network %s before recreation: %w", name, err)
		}
	}

	applier, err = apiClient.ContainerInspect(ctx, record.Applier.ID, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("verify applier networks before recreation: %w", err)
	}

	if applier.Container.NetworkSettings == nil || applier.Container.NetworkSettings.Networks["bridge"] == nil {
		return fmt.Errorf("applier %s lost its stable bridge connection", record.Applier.ID)
	}

	for name := range record.Drift.Networks {
		if applier.Container.NetworkSettings.Networks[name] != nil {
			return fmt.Errorf("applier is still attached to project network %s", name)
		}
	}

	return nil
}

// FindSelfAppliers lists the applier containers for a stack, including exited ones.
func FindSelfAppliers(ctx context.Context, apiClient client.APIClient, stack string) ([]container.Summary, error) {
	list, err := apiClient.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", SelfStackLabel+"="+stack),
	})
	if err != nil {
		return nil, fmt.Errorf("list self-update applier containers: %w", err)
	}

	return list.Items, nil
}

// ReleaseSelfApplierRestartLimit lets Docker restart the applier without a
// bound. The predecessor calls it right before it drains, after which only the
// applier can finish or roll back the handover.
func ReleaseSelfApplierRestartLimit(ctx context.Context, apiClient client.APIClient, applierID string) error {
	return selfupdate.Retry(ctx, func() error {
		_, err := apiClient.ContainerUpdate(ctx, applierID, client.ContainerUpdateOptions{
			RestartPolicy: &container.RestartPolicy{Name: container.RestartPolicyOnFailure},
		})
		if err != nil {
			return fmt.Errorf("update applier container %s restart policy: %w", applierID, err)
		}

		return nil
	}, nil)
}

// RemoveSelfApplier deletes a finished applier container.
func RemoveSelfApplier(ctx context.Context, apiClient client.APIClient, containerID string) error {
	if _, err := apiClient.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{Force: true}); err != nil {
		return fmt.Errorf("remove applier container %s: %w", containerID, err)
	}

	return nil
}
