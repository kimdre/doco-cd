package docker

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/selfupdate"
)

// SelfUpdateOptions configures how this instance replaces its own container.
type SelfUpdateOptions struct {
	Enabled       bool
	Identity      selfupdate.Identity
	Strategy      selfupdate.Strategy
	Store         *selfupdate.Store
	DataMountPath string
	AppVersion    string
}

var (
	selfUpdateMu   sync.RWMutex
	selfUpdateOpts SelfUpdateOptions
)

// ConfigureSelfUpdate installs the process-wide self-update configuration.
func ConfigureSelfUpdate(o SelfUpdateOptions) {
	selfUpdateMu.Lock()
	defer selfUpdateMu.Unlock()

	selfUpdateOpts = o
}

// SelfUpdateConfig returns the current self-update configuration.
func SelfUpdateConfig() SelfUpdateOptions {
	selfUpdateMu.RLock()
	defer selfUpdateMu.RUnlock()

	return selfUpdateOpts
}

// SelfDeployInput carries the deploy context a successor needs to report a
// deployment it did not run itself.
type SelfDeployInput struct {
	RepoName       string
	SourceURL      string
	FullName       string
	SourceType     string
	Reference      string
	ConfigTarget   string
	CommitSHA      string
	ProjectHash    string
	JobID          string
	Trigger        string
	TimeoutSeconds int
	RecreateMode   string
	Services       []string
	RemoveOrphans  bool
	CommitStatus   *selfupdate.CommitStatusInfo
	Log            *slog.Logger
	// StopPhaseReports stops publishing deployment phases before the
	// successor takes over the commit status. Optional.
	StopPhaseReports func()
}

// stopPhaseReports stops phase reporting for the deployment, if any.
func (in *SelfDeployInput) stopPhaseReports() {
	if in != nil && in.StopPhaseReports != nil {
		in.StopPhaseReports()
	}
}

// selfTarget describes the self service inside a project being deployed.
type selfTarget struct {
	Service string
	Project string
	Context string
}

// IsSelfStack reports whether a deploy config targets the stack that contains
// this doco-cd instance.
func IsSelfStack(dc *deploy.Config) bool {
	if dc == nil {
		return false
	}

	opts := SelfUpdateConfig()
	if !opts.Identity.OK {
		return false
	}

	if NormalizeContextName(dc.Context) != "" {
		return false
	}

	return strings.EqualFold(dc.Name, opts.Identity.Project)
}

// selfUpdateFor returns the self target when project contains this instance's
// own container, or nil when the deploy does not touch us.
func selfUpdateFor(project *types.Project, contextName string) *selfTarget {
	opts := SelfUpdateConfig()

	if !opts.Identity.OK || project == nil {
		return nil
	}

	if NormalizeContextName(contextName) != "" {
		return nil
	}

	if !strings.EqualFold(project.Name, opts.Identity.Project) {
		return nil
	}

	if _, ok := project.Services[opts.Identity.Service]; !ok {
		return nil
	}

	return &selfTarget{
		Service: opts.Identity.Service,
		Project: project.Name,
		Context: NormalizeContextName(contextName),
	}
}

// networkDrift reports whether any project network the self service attaches to
// must be recreated. Compose stops every attached container to recreate a
// network, which would kill this process regardless of the strategy.
func networkDrift(ctx context.Context, apiClient client.APIClient, project *types.Project, svc types.ServiceConfig) (bool, error) {
	networks, err := selfDriftedNetworks(ctx, apiClient, project, svc)
	return len(networks) > 0, err
}

// selfDriftedNetworks finds existing project networks that the desired self
// service needs Compose to recreate.
func selfDriftedNetworks(ctx context.Context, apiClient client.APIClient, project *types.Project, svc types.ServiceConfig) ([]network.Inspect, error) {
	list, err := apiClient.NetworkList(ctx, client.NetworkListOptions{
		Filters: make(client.Filters).Add("label", api.ProjectLabel+"="+project.Name),
	})
	if err != nil {
		return nil, fmt.Errorf("list self stack networks: %w", err)
	}

	var drifted []network.Inspect

	for name := range svc.Networks {
		cfg, ok := project.Networks[name]
		if !ok || bool(cfg.External) {
			continue
		}

		want, err := composeNetworkHash(&cfg)
		if err != nil {
			return nil, fmt.Errorf("hash network %s: %w", name, err)
		}

		netName := cfg.Name
		if netName == "" {
			netName = project.Name + "_" + name
		}

		var current string
		for _, item := range list.Items {
			if item.Labels[api.NetworkLabel] == name &&
				(current == "" || item.Name == netName) {
				current = item.ID
			}
		}

		if current == "" {
			// A genuinely new network does not require stopping the predecessor.
			continue
		}

		result, err := apiClient.NetworkInspect(ctx, current, client.NetworkInspectOptions{})
		if err != nil {
			return nil, fmt.Errorf("inspect self stack network %s: %w", name, err)
		}

		if result.Network.Name != netName ||
			(result.Network.Labels[api.ConfigHashLabel] != "" && result.Network.Labels[api.ConfigHashLabel] != want) {
			drifted = append(drifted, result.Network)
		}
	}

	return drifted, nil
}

// selfRecreateForced reports whether this deploy force-recreates the self service.
// An empty service list with RecreateForce applies to the whole project.
func selfRecreateForced(mode string, services []string, name string) bool {
	if mode != api.RecreateForce {
		return false
	}

	return len(services) == 0 || slices.Contains(services, name)
}

// selfServiceUnchanged reports whether this deploy can leave the running
// doco-cd container in the compose create. project must already be path-pinned.
// shortcut: ignores a disconnected project network and a missing named volume, check those before trusting a hash match.
// shortcut: image digest is read before Create pulls, recheck if ensureImagesExists can change the image.
func selfServiceUnchanged(
	ctx context.Context,
	apiClient client.APIClient,
	project *types.Project,
	target *selfTarget,
	recreateMode string,
	services []string,
) (bool, error) {
	svc := project.Services[target.Service]

	// Scale is excluded from the config hash, but a replica change still recreates.
	if svc.GetScale() != 1 || selfRecreateForced(recreateMode, services, target.Service) {
		return false, nil
	}

	for _, volume := range svc.Volumes {
		if volume.Type == types.VolumeTypeImage {
			return false, nil
		}
	}

	inspected, err := apiClient.ContainerInspect(ctx, SelfUpdateConfig().Identity.ContainerID, client.ContainerInspectOptions{})
	if err != nil {
		return false, fmt.Errorf("inspect own container: %w", err)
	}

	var labels map[string]string
	if inspected.Container.Config != nil {
		labels = inspected.Container.Config.Labels
	}

	runningHash := labels[api.ConfigHashLabel]
	if runningHash == "" || labels[api.ImageVolumeDigestLabel] != "" {
		return false, nil
	}

	drift, err := networkDrift(ctx, apiClient, project, svc)
	if err != nil {
		return false, err
	}

	if drift {
		return false, nil
	}

	want, err := composeServiceHash(svc)
	if err != nil {
		return false, err
	}

	if want != runningHash {
		return false, nil
	}

	return selfImageDigestMatches(ctx, apiClient, project.Name, svc, labels[api.ImageDigestLabel])
}

// selfImageDigestMatches reports whether Compose's image-digest label would
// also leave the container in place. That label is not part of the config hash.
func selfImageDigestMatches(
	ctx context.Context,
	apiClient client.APIClient,
	projectName string,
	svc types.ServiceConfig,
	running string,
) (bool, error) {
	if running == "" {
		return false, nil
	}

	ref := api.GetImageNameOrDefault(svc, projectName)

	inspected, err := apiClient.ImageInspect(ctx, ref, client.ImageInspectWithManifests(true))
	if err != nil {
		inspected, err = apiClient.ImageInspect(ctx, ref)
	}

	if err != nil {
		if ctx.Err() != nil {
			return false, err
		}

		return false, nil
	}

	got, err := composeImageContentDigest(inspected.InspectResponse, svc.Platform)
	if err != nil {
		if ctx.Err() != nil {
			return false, err
		}

		return false, nil
	}

	return got == running, nil
}

// selectSelfStrategy resolves the strategy for the self service, including the
// runtime constraints the selector cannot read off the compose file.
func selectSelfStrategy(
	ctx context.Context,
	dockerCli command.Cli,
	project *types.Project,
	target *selfTarget,
	sourceType string,
	log *slog.Logger,
) (selfupdate.Strategy, bool, error) {
	opts := SelfUpdateConfig()
	svc := project.Services[target.Service]

	drift, err := networkDrift(ctx, dockerCli.Client(), project, svc)
	if err != nil {
		return "", false, err
	}

	strategy, reasons, err := selfupdate.Select(svc, sourceType, target.Context, opts.Strategy, selfupdate.Constraints{NetworkDrift: drift})
	if err != nil {
		return "", false, err
	}

	if log != nil {
		log.Info("self-update: strategy selected",
			slog.String("strategy", string(strategy)),
			slog.String("stack", target.Project),
			slog.String("service", target.Service),
			slog.Any("reasons", reasons),
		)
	}

	return strategy, drift, nil
}
