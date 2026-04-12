package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"time"

	containerTypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

const (
	defaultWaitTimeout = 5 * time.Minute
	waitInterval       = 1 * time.Second
)

var (
	ErrEmptyContainerID        = errors.New("container id is empty")
	ErrUpdateInProgress        = errors.New("self-update already in progress")
	ErrContainerNameNotFound   = errors.New("container name not found")
	ErrImageReferenceNotFound  = errors.New("image reference not found")
	ErrNoUpdateAvailable       = errors.New("no update available")
	ErrContainerStartFailed    = errors.New("replacement container failed to start")
	ErrContainerHealthFailed   = errors.New("replacement container failed health checks")
	ErrUnsupportedSwarmService = errors.New("self-update is not supported for swarm-managed containers")
)

type Result struct {
	Updated          bool   `json:"updated"`
	ContainerID      string `json:"container_id"`
	ContainerName    string `json:"container_name"`
	ImageRef         string `json:"image_ref"`
	OldImageID       string `json:"old_image_id,omitempty"`
	NewImageID       string `json:"new_image_id,omitempty"`
	OldContainerID   string `json:"old_container_id,omitempty"`
	OldContainerName string `json:"old_container_name,omitempty"`
}

type Updater struct {
	dockerClient *client.Client
	containerID  string
	log          *slog.Logger
	waitHealthy  bool
	mu           sync.Mutex
}

func New(dockerClient *client.Client, containerID string, log *slog.Logger, waitHealthy bool) (*Updater, error) {
	if dockerClient == nil {
		return nil, errors.New("docker client is required")
	}

	if strings.TrimSpace(containerID) == "" {
		return nil, ErrEmptyContainerID
	}

	if log == nil {
		log = slog.Default()
	}

	return &Updater{
		dockerClient: dockerClient,
		containerID:  containerID,
		log:          log.With(slog.String("component", "self_updater")),
		waitHealthy:  waitHealthy,
	}, nil
}

func (u *Updater) StartAsync(ctx context.Context) error {
	if !u.mu.TryLock() {
		return ErrUpdateInProgress
	}

	go func() {
		defer u.mu.Unlock()

		result, err := u.runIfNeeded(ctx)
		if err != nil {
			if errors.Is(err, ErrNoUpdateAvailable) {
				u.log.Debug("self-update check completed without changes",
					slog.String("container_name", result.ContainerName),
					slog.String("image_ref", result.ImageRef),
				)

				return
			}

			u.log.Error("self-update failed",
				slog.Any("error", err),
				slog.String("container_name", result.ContainerName),
				slog.String("image_ref", result.ImageRef),
			)

			return
		}

		u.log.Info("self-update completed",
			slog.String("container_name", result.ContainerName),
			slog.String("image_ref", result.ImageRef),
			slog.String("new_image_id", result.NewImageID),
		)
	}()

	return nil
}

func (u *Updater) RunScheduled(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return nil
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := u.StartAsync(context.WithoutCancel(ctx)); err != nil {
				if errors.Is(err, ErrUpdateInProgress) {
					u.log.Debug("scheduled self-update check skipped because an update is already in progress")
					continue
				}

				u.log.Error("scheduled self-update check failed to start", slog.Any("error", err))
			}
		}
	}
}

func (u *Updater) RunIfNeeded(ctx context.Context) (Result, error) {
	if !u.mu.TryLock() {
		return Result{}, ErrUpdateInProgress
	}
	defer u.mu.Unlock()

	return u.runIfNeeded(ctx)
}

func (u *Updater) runIfNeeded(ctx context.Context) (Result, error) {
	inspectResult, err := u.dockerClient.ContainerInspect(ctx, u.containerID, client.ContainerInspectOptions{})
	if err != nil {
		return Result{}, fmt.Errorf("failed to inspect current container: %w", err)
	}

	containerInspect := inspectResult.Container

	containerName := normalizeContainerName(containerInspect.Name)
	if containerName == "" {
		return Result{}, ErrContainerNameNotFound
	}

	imageRef := ""
	if containerInspect.Config != nil {
		imageRef = strings.TrimSpace(containerInspect.Config.Image)
	}

	if imageRef == "" {
		return Result{}, ErrImageReferenceNotFound
	}

	result := Result{
		ContainerID:   containerInspect.ID,
		ContainerName: containerName,
		ImageRef:      imageRef,
		OldImageID:    containerInspect.Image,
	}

	if isSwarmManaged(containerInspect.Config) {
		return result, ErrUnsupportedSwarmService
	}

	imageInspect, err := u.pullAndInspectImage(ctx, imageRef)
	if err != nil {
		return result, err
	}

	result.NewImageID = imageInspect.ID

	if imageInspect.ID == containerInspect.Image {
		return result, ErrNoUpdateAvailable
	}

	rollbackContainerName := buildRollbackContainerName(containerName, time.Now().UTC())
	result.OldContainerID = containerInspect.ID
	result.OldContainerName = rollbackContainerName

	if _, err := u.dockerClient.ContainerRename(ctx, containerInspect.ID, client.ContainerRenameOptions{NewName: rollbackContainerName}); err != nil {
		return result, fmt.Errorf("failed to rename current container: %w", err)
	}

	createOptions, err := buildCreateOptions(containerInspect, imageRef, containerName)
	if err != nil {
		return result, err
	}

	createResult, err := u.dockerClient.ContainerCreate(ctx, createOptions)
	if err != nil {
		if _, renameErr := u.dockerClient.ContainerRename(ctx, containerInspect.ID, client.ContainerRenameOptions{NewName: containerName}); renameErr != nil {
			u.log.Error("failed to restore original container name after create failure",
				slog.Any("error", renameErr),
				slog.String("rollback_container_name", rollbackContainerName),
			)
		}

		return result, fmt.Errorf("failed to create replacement container: %w", err)
	}

	stopTimeout := getStopTimeout(containerInspect.Config)
	if _, err := u.dockerClient.ContainerStop(ctx, containerInspect.ID, client.ContainerStopOptions{Timeout: stopTimeout}); err != nil {
		return result, fmt.Errorf("failed to stop current container: %w", err)
	}

	if _, err := u.dockerClient.ContainerStart(ctx, createResult.ID, client.ContainerStartOptions{}); err != nil {
		return result, fmt.Errorf("failed to start replacement container: %w", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, defaultWaitTimeout)
	defer cancel()

	if err := u.waitForContainer(waitCtx, createResult.ID); err != nil {
		return result, err
	}

	if _, err := u.dockerClient.ContainerRemove(ctx, containerInspect.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: false}); err != nil {
		return result, fmt.Errorf("failed to remove previous container: %w", err)
	}

	if result.OldImageID != "" && result.OldImageID != result.NewImageID {
		if _, err := u.dockerClient.ImageRemove(ctx, result.OldImageID, client.ImageRemoveOptions{Force: false, PruneChildren: false}); err != nil {
			u.log.Warn("failed to remove previous image after successful self-update",
				slog.Any("error", err),
				slog.String("image_id", result.OldImageID),
			)
		}
	}

	result.Updated = true
	result.ContainerID = createResult.ID

	return result, nil
}

func (u *Updater) pullAndInspectImage(ctx context.Context, imageRef string) (client.ImageInspectResult, error) {
	response, err := u.dockerClient.ImagePull(ctx, imageRef, client.ImagePullOptions{})
	if err != nil {
		return client.ImageInspectResult{}, fmt.Errorf("failed to pull image %s: %w", imageRef, err)
	}

	defer func() {
		_ = response.Close()
	}()

	if err := response.Wait(ctx); err != nil {
		return client.ImageInspectResult{}, fmt.Errorf("failed to wait for pulled image %s: %w", imageRef, err)
	}

	imageInspect, err := u.dockerClient.ImageInspect(ctx, imageRef)
	if err != nil {
		return client.ImageInspectResult{}, fmt.Errorf("failed to inspect pulled image %s: %w", imageRef, err)
	}

	return imageInspect, nil
}

func (u *Updater) waitForContainer(ctx context.Context, containerID string) error {
	ticker := time.NewTicker(waitInterval)
	defer ticker.Stop()

	for {
		inspectResult, err := u.dockerClient.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
		if err != nil {
			return fmt.Errorf("failed to inspect replacement container: %w", err)
		}

		state := inspectResult.Container.State
		if state != nil {
			if !state.Running {
				return fmt.Errorf("%w: status=%s exit_code=%d error=%s", ErrContainerStartFailed, state.Status, state.ExitCode, state.Error)
			}

			if shouldWaitForHealthy(u.waitHealthy, inspectResult.Container.Config) {
				if state.Health != nil {
					switch state.Health.Status {
					case containerTypes.Healthy:
						return nil
					case containerTypes.Unhealthy:
						return fmt.Errorf("%w: %s", ErrContainerHealthFailed, state.Health.Status)
					}
				}
			} else {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for replacement container: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func buildCreateOptions(inspect containerTypes.InspectResponse, imageRef, containerName string) (client.ContainerCreateOptions, error) {
	configCopy, err := clone(inspect.Config)
	if err != nil {
		return client.ContainerCreateOptions{}, fmt.Errorf("failed to clone container config: %w", err)
	}

	hostConfigCopy, err := clone(inspect.HostConfig)
	if err != nil {
		return client.ContainerCreateOptions{}, fmt.Errorf("failed to clone host config: %w", err)
	}

	networkingConfig, err := buildNetworkingConfig(inspect.NetworkSettings)
	if err != nil {
		return client.ContainerCreateOptions{}, fmt.Errorf("failed to clone network settings: %w", err)
	}

	if configCopy == nil {
		configCopy = &containerTypes.Config{}
	}

	configCopy.Image = imageRef

	return client.ContainerCreateOptions{
		Config:           configCopy,
		HostConfig:       hostConfigCopy,
		NetworkingConfig: networkingConfig,
		Name:             containerName,
	}, nil
}

func buildNetworkingConfig(settings *containerTypes.NetworkSettings) (*network.NetworkingConfig, error) {
	if settings == nil || len(settings.Networks) == 0 {
		return nil, nil
	}

	endpoints := make(map[string]*network.EndpointSettings, len(settings.Networks))
	for networkName, endpoint := range settings.Networks {
		endpointCopy, err := clone(endpoint)
		if err != nil {
			return nil, err
		}

		if endpointCopy == nil {
			continue
		}

		endpointCopy.NetworkID = ""
		endpointCopy.EndpointID = ""
		endpointCopy.Gateway = netip.Addr{}
		endpointCopy.IPAddress = netip.Addr{}
		endpointCopy.IPv6Gateway = netip.Addr{}
		endpointCopy.GlobalIPv6Address = netip.Addr{}
		endpointCopy.IPPrefixLen = 0
		endpointCopy.GlobalIPv6PrefixLen = 0
		endpointCopy.DNSNames = nil

		endpoints[networkName] = endpointCopy
	}

	if len(endpoints) == 0 {
		return nil, nil
	}

	return &network.NetworkingConfig{EndpointsConfig: endpoints}, nil
}

func normalizeContainerName(name string) string {
	return strings.TrimPrefix(strings.TrimSpace(name), "/")
}

func buildRollbackContainerName(containerName string, now time.Time) string {
	return fmt.Sprintf("%s-doco-cd-backup-%d", containerName, now.Unix())
}

func shouldWaitForHealthy(waitHealthy bool, config *containerTypes.Config) bool {
	return waitHealthy && config != nil && config.Healthcheck != nil
}

func getStopTimeout(config *containerTypes.Config) *int {
	if config != nil && config.StopTimeout != nil {
		return new(*config.StopTimeout)
	}

	return nil
}

func isSwarmManaged(config *containerTypes.Config) bool {
	if config == nil {
		return false
	}

	serviceName, ok := config.Labels["com.docker.swarm.service.name"]

	return ok && strings.TrimSpace(serviceName) != ""
}

func clone[T any](value *T) (*T, error) {
	if value == nil {
		return nil, nil
	}

	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}

	var result T
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}

	return &result, nil
}
