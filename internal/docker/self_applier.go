package docker

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/selfupdate"
)

// applierRestartRetries lets Docker re-run a crashed applier, which resumes
// from the journal. Docker rejects AutoRemove together with a restart policy,
// so the successor removes the applier once it has reported.
const applierRestartRetries = 2

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
	hostConfig.RestartPolicy = container.RestartPolicy{
		Name:              container.RestartPolicyOnFailure,
		MaximumRetryCount: applierRestartRetries,
	}

	opts := client.ContainerCreateOptions{
		Config:     &config,
		HostConfig: &hostConfig,
		Name:       applierName(inspect.Name),
	}

	if networking := applierNetworking(inspect, hostConfig.NetworkMode); networking != nil {
		opts.NetworkingConfig = networking
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

// applierNetworking attaches the clone to the same networks without aliases.
func applierNetworking(inspect container.InspectResponse, mode container.NetworkMode) *network.NetworkingConfig {
	if mode.IsHost() || mode.IsContainer() {
		return nil
	}

	if inspect.NetworkSettings == nil || len(inspect.NetworkSettings.Networks) == 0 {
		return nil
	}

	endpoints := make(map[string]*network.EndpointSettings, len(inspect.NetworkSettings.Networks))
	for name := range inspect.NetworkSettings.Networks {
		endpoints[name] = &network.EndpointSettings{}
	}

	return &network.NetworkingConfig{EndpointsConfig: endpoints}
}

// selfUpdateStageApplier starts the clone that replaces this container. The
// predecessor keeps serving until the applier stops it.
func selfUpdateStageApplier(ctx context.Context, apiClient client.APIClient, record *selfupdate.Record, log *slog.Logger) error {
	opts := SelfUpdateConfig()

	inspect, err := apiClient.ContainerInspect(ctx, opts.Identity.ContainerID, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect own container for applier clone: %w", err)
	}

	createOpts := BuildSelfApplierCreate(inspect.Container, record.ID, record.Stack)

	created, err := apiClient.ContainerCreate(ctx, createOpts)
	if err != nil {
		return fmt.Errorf("create applier container: %w", err)
	}

	record.Applier = selfupdate.ContainerRef{ID: created.ID, Name: createOpts.Name}

	if err = opts.Store.Save(*record); err != nil {
		return fmt.Errorf("record applier: %w", err)
	}

	updated, err := opts.Store.Update(*record, selfupdate.StateApplying, selfupdate.ActorPredecessor)
	if err != nil {
		return err
	}

	*record = updated

	if _, err = apiClient.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		removeCtx := context.WithoutCancel(ctx)
		if _, removeErr := apiClient.ContainerRemove(removeCtx, created.ID, client.ContainerRemoveOptions{Force: true}); removeErr != nil {
			log.Warn("self-update: failed to remove the applier that could not start", slog.Any("error", removeErr))
		}

		return fmt.Errorf("start applier container: %w", err)
	}

	log.Info("self-update: applier started",
		slog.String("applier_id", created.ID),
		slog.String("applier_name", createOpts.Name),
	)

	selfupdate.MaybeCrash(opts.DataMountPath, string(selfupdate.StateApplying), log)

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

// RemoveSelfApplier deletes a finished applier container.
func RemoveSelfApplier(ctx context.Context, apiClient client.APIClient, containerID string) error {
	if _, err := apiClient.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{Force: true}); err != nil {
		return fmt.Errorf("remove applier container %s: %w", containerID, err)
	}

	return nil
}
