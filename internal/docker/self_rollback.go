package docker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/selfupdate"
)

// RestoreCreateFromSnapshot derives create options that bring back the exact
// container a self-update replaced. The image is pinned to the snapshot's image
// ID, not its tag, so a moved tag cannot change what comes back.
func RestoreCreateFromSnapshot(snap container.InspectResponse) client.ContainerCreateOptions {
	config := *snap.Config
	config.Image = snap.Image

	hostConfig := *snap.HostConfig

	opts := client.ContainerCreateOptions{
		Config:     &config,
		HostConfig: &hostConfig,
		Name:       strings.TrimPrefix(snap.Name, "/"),
	}

	if snap.NetworkSettings != nil && len(snap.NetworkSettings.Networks) > 0 &&
		!hostConfig.NetworkMode.IsHost() && !hostConfig.NetworkMode.IsContainer() {
		endpoints := make(map[string]*network.EndpointSettings, len(snap.NetworkSettings.Networks))

		for name, settings := range snap.NetworkSettings.Networks {
			if settings == nil {
				endpoints[name] = &network.EndpointSettings{}
				continue
			}

			endpoints[name] = &network.EndpointSettings{
				Aliases:    settings.Aliases,
				IPAMConfig: settings.IPAMConfig,
				Links:      settings.Links,
				DriverOpts: settings.DriverOpts,
			}
		}

		opts.NetworkingConfig = &network.NetworkingConfig{EndpointsConfig: endpoints}
	}

	return opts
}

// restoreSelfPredecessor brings the previous doco-cd container back after a
// failed apply, then removes every other container of the self service so the
// stack is left with exactly one.
func restoreSelfPredecessor(
	ctx context.Context,
	apiClient client.APIClient,
	store *selfupdate.Store,
	record selfupdate.Record,
	log *slog.Logger,
) (selfupdate.ContainerRef, error) {
	restoreCtx := context.WithoutCancel(ctx)

	if _, err := apiClient.ContainerInspect(restoreCtx, record.Predecessor.ID, client.ContainerInspectOptions{}); err == nil {
		if _, err = apiClient.ContainerStart(restoreCtx, record.Predecessor.ID, client.ContainerStartOptions{}); err != nil {
			return selfupdate.ContainerRef{}, fmt.Errorf("restart the previous container: %w", err)
		}

		log.Info("self-update: previous container restarted", slog.String("container_id", record.Predecessor.ID))

		if err = removeOtherSelfContainers(restoreCtx, apiClient, record, record.Predecessor.ID, log); err != nil {
			return selfupdate.ContainerRef{}, err
		}

		return record.Predecessor, nil
	}

	snap, ok, err := store.ReadSnapshot(record.ID)
	if err != nil {
		return selfupdate.ContainerRef{}, err
	}

	if !ok {
		return selfupdate.ContainerRef{}, fmt.Errorf("no snapshot to restore the previous container from")
	}

	// The old container is gone, so free its name before recreating it.
	if err = removeOtherSelfContainers(restoreCtx, apiClient, record, "", log); err != nil {
		return selfupdate.ContainerRef{}, err
	}

	createOpts := RestoreCreateFromSnapshot(snap)

	created, err := apiClient.ContainerCreate(restoreCtx, createOpts)
	if err != nil {
		return selfupdate.ContainerRef{}, fmt.Errorf("recreate the previous container: %w", err)
	}

	if _, err = apiClient.ContainerStart(restoreCtx, created.ID, client.ContainerStartOptions{}); err != nil {
		return selfupdate.ContainerRef{}, fmt.Errorf("start the recreated previous container: %w", err)
	}

	log.Info("self-update: previous container recreated from the snapshot", slog.String("container_id", created.ID))

	return selfupdate.ContainerRef{ID: created.ID, Name: createOpts.Name, Number: record.Predecessor.Number}, nil
}

// removeOtherSelfContainers drops every container of the self service except
// keepID, so the next deploy sees exactly one.
func removeOtherSelfContainers(
	ctx context.Context,
	apiClient client.APIClient,
	record selfupdate.Record,
	keepID string,
	log *slog.Logger,
) error {
	list, err := apiClient.ContainerList(ctx, client.ContainerListOptions{
		All: true,
		Filters: make(client.Filters).
			Add("label", api.ProjectLabel+"="+record.Stack).
			Add("label", api.ServiceLabel+"="+record.Service),
	})
	if err != nil {
		return fmt.Errorf("list self service containers: %w", err)
	}

	for _, c := range list.Items {
		if c.ID == keepID {
			continue
		}

		if _, err = apiClient.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true}); err != nil {
			log.Warn("self-update: failed to remove a leftover container",
				slog.String("container_id", c.ID), slog.Any("error", err))
		}
	}

	return nil
}
