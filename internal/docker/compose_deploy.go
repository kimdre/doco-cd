package docker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"

	"github.com/kimdre/doco-cd/internal/common/types/set"
	"github.com/kimdre/doco-cd/internal/common/types/slice"
	"github.com/kimdre/doco-cd/internal/common/validation"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker/registryauth"
	gitInternal "github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/lock"
	"github.com/kimdre/doco-cd/internal/prometheus"
	"github.com/kimdre/doco-cd/internal/webhook"
)

// deployCompose deploys a project as specified by the Docker Compose specification (LoadCompose).
func deployCompose(ctx context.Context, dockerCli command.Cli, project *types.Project,
	deployConfig *deploy.Config, recreateMode string, services []string,
	needSignal []SignalService, setPhase func(string),
) error {
	var (
		err          error
		beforeImages map[string]api.ImageSummary // Images used by stack before deployment
		afterImages  map[string]api.ImageSummary // Images used by stack after deployment
	)

	service, err := compose.NewComposeService(dockerCli)
	if err != nil {
		return err
	}

	if len(needSignal) > 0 {
		setDeploymentPhase(setPhase, "signaling services")

		if err := ComposeSignal(ctx, dockerCli, project, needSignal); err != nil {
			return err
		}
	}

	if deployConfig.PruneImages {
		beforeImages, err = service.Images(ctx, project.Name, api.ImagesOptions{})
		if err != nil {
			// No such image error is okay since we wanted to remove the image anyway
			if !strings.Contains(strings.ToLower(err.Error()), ErrNoSuchImage.Error()) {
				return fmt.Errorf("failed to get existing images: %w", err)
			}
		}
	}

	if deployConfig.ForceImagePull {
		for i, s := range project.Services {
			s.PullPolicy = types.PullPolicyAlways
			project.Services[i] = s
		}
	}

	setDeploymentPhase(setPhase, "pulling images")

	err = service.Pull(ctx, project, api.PullOptions{
		Quiet:           true,
		IgnoreBuildable: true,
	})
	if err != nil {
		imageRefs := make([]string, 0, len(project.Services))
		for _, svc := range project.Services {
			if svc.Image == "" {
				continue
			}

			imageRefs = append(imageRefs, svc.Image)
		}

		hint := registryauth.BuildFailureHint(dockerCli.ConfigFile(), imageRefs, err)
		if hint != "" {
			return fmt.Errorf("failed to pull images: %w; %s", err, hint)
		}

		return fmt.Errorf("failed to pull images: %w", err)
	}

	if recreateMode == "" {
		recreateMode = api.RecreateDiverged
	}

	// Convert deployConfig.BuildOpts.Args to types.MappingWithEquals
	buildArgs := make(types.MappingWithEquals)
	for k, v := range deployConfig.BuildOpts.Args {
		buildArgs[k] = &v
	}

	buildOpts := api.BuildOptions{
		Pull:     deployConfig.BuildOpts.ForceImagePull,
		Quiet:    deployConfig.BuildOpts.Quiet,
		Progress: "auto",
		Args:     buildArgs,
		NoCache:  deployConfig.BuildOpts.NoCache,
	}

	setDeploymentPhase(setPhase, "building images")

	err = service.Build(ctx, project, buildOpts)
	if err != nil {
		return err
	}

	createOpts := api.CreateOptions{
		Services:             services,
		RemoveOrphans:        deployConfig.RemoveOrphans,
		Recreate:             recreateMode,
		RecreateDependencies: api.RecreateDiverged,
		QuietPull:            true,
	}

	autostartDisabledServices, err := getAutostartDisabledServices(project)
	if err != nil {
		return err
	}

	runningServices := set.New[string]()

	if autostartDisabledServices.Len() > 0 {
		containers, err := GetProjectContainers(ctx, dockerCli, project.Name)
		if err != nil {
			return fmt.Errorf("failed to inspect existing services before deployment: %w", err)
		}

		runningServices = getRunningServices(containers)
	}

	startServices, err := getStartServicesForDeploy(project, autostartDisabledServices, runningServices)
	if err != nil {
		return err
	}

	jobServices, err := getJobServices(project)
	if err != nil {
		return err
	}

	stoppedAutostartServices := autostartDisabledServices.Difference(runningServices)

	// Remove mismatched recreatable volumes (tmpfs, NFS, CIFS mounts) before create.
	// Docker Compose then recreates them with the desired configuration during service.Create.
	setDeploymentPhase(setPhase, "preparing deployment resources")

	if err = removeMismatchedRecreatableVolumes(ctx, dockerCli.Client(), deployConfig.Name, project); err != nil {
		return fmt.Errorf("failed to remove mismatched recreatable volumes: %w", err)
	}

	setDeploymentPhase(setPhase, "creating services")

	err = service.Create(ctx, project, createOpts)
	if err != nil {
		return err
	}

	if len(startServices) > 0 {
		setDeploymentPhase(setPhase, "starting services")

		// Docker Compose's Start ignores StartOptions.Services and starts every
		// service in the passed project (including containers in the "created" or
		// "exited" state), so narrow the project to services allowed to start.
		startProject, err := projectForStart(project, jobServices, stoppedAutostartServices)
		if err != nil {
			return err
		}

		startOpts := api.StartOptions{
			Project:  startProject,
			Wait:     false,
			Services: startServices,
		}

		err = service.Start(ctx, startProject.Name, startOpts)
		if err != nil {
			if !errors.Is(err, ErrNoContainerToStart) {
				return err
			}
		}

		setDeploymentPhase(setPhase, "waiting for services to start")

		err = waitForStartedServices(ctx, dockerCli, project.Name, startServices, jobServices,
			time.Duration(deployConfig.Timeout)*time.Second)
		if err != nil {
			return err
		}
	}

	if deployConfig.PruneImages {
		setDeploymentPhase(setPhase, "pruning unused images")

		afterImages, err = service.Images(ctx, project.Name, api.ImagesOptions{})
		if err != nil {
			// No such image error is okay since we wanted to remove the image anyway
			if !strings.Contains(strings.ToLower(err.Error()), ErrNoSuchImage.Error()) {
				return fmt.Errorf("failed to get images after deployment: %w", err)
			}
		}

		// Determine unused images by comparing image SHAs used by services before and after the deployment

		var ids []string

		for svc, beforeImg := range beforeImages {
			afterImg, exists := afterImages[svc]
			if !exists || beforeImg.ID != afterImg.ID {
				ids = append(ids, beforeImg.ID)
			}
		}

		_, err = pruneImages(ctx, dockerCli, slice.Unique(ids))
		if err != nil {
			return fmt.Errorf("failed to prune images: %w", err)
		}
	}

	return nil
}

type deploymentPhaseState struct {
	mu    sync.RWMutex
	phase string
}

func newDeploymentPhaseState(initialPhase string) *deploymentPhaseState {
	return &deploymentPhaseState{
		phase: normalizeDeploymentPhase(initialPhase),
	}
}

func (s *deploymentPhaseState) Set(phase string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.phase = normalizeDeploymentPhase(phase)
}

func (s *deploymentPhaseState) Get() string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.phase
}

func normalizeDeploymentPhase(phase string) string {
	phase = strings.TrimSpace(phase)
	if phase == "" {
		return "unknown"
	}

	return phase
}

func setDeploymentPhase(setPhase func(string), phase string) {
	if setPhase == nil {
		return
	}

	setPhase(phase)
}

func logDeploymentHeartbeat(log *slog.Logger, phase string) {
	log.Info("deployment in progress", slog.String("phase", normalizeDeploymentPhase(phase)))
}

func deploymentRepositoryKey(payload *webhook.ParsedPayload) string {
	if payload == nil {
		return ""
	}

	for _, candidate := range []string{payload.CloneURL, payload.FullName, payload.Artifact} {
		if value := strings.TrimSpace(candidate); value != "" {
			return value
		}
	}

	return ""
}

func resolveDeploymentMetricsRepositoryLabel(payload *webhook.ParsedPayload) string {
	repository := normalizeRepositoryForLabelMatch(deploymentRepositoryKey(payload))
	if repository == "" {
		return "unknown"
	}

	return repository
}

func resolveDeploymentMetricsDeploymentLabel(deployName string) string {
	deployment := strings.TrimSpace(deployName)
	if deployment == "" {
		return "unknown"
	}

	return deployment
}

// DeployStack deploys the stack using the provided deployment configuration.
// DeployRequest bundles DeployStack's per-deployment input: the job logger, the external
// repository path used to resolve compose file locations, the Docker CLI, the parsed webhook
// payload (may be nil for non-webhook triggers), the resolved deploy config, detected service
// changes and signal targets from change detection, the latest commit, the running app version,
// the ComposeLoadOptions used to reload the compose project, Swarm config/secret retention counts,
// whether Swarm mode is active, and the PKI role normalization map.
type DeployRequest struct {
	JobLog                     *slog.Logger `validate:"required,nostructlevel"`
	ExternalRepoPath           string       `validate:"required"`
	DockerCLI                  command.Cli  `validate:"required,nostructlevel"`
	Payload                    *webhook.ParsedPayload
	DeployConfig               *deploy.Config `validate:"required,nostructlevel"`
	DetectedChanges            []Change
	NeedSignal                 []SignalService
	LatestCommit               string
	AppVersion                 string `validate:"required"`
	ComposeLoad                ComposeLoadOptions
	GlobalSwarmConfigRetention int
	GlobalSwarmSecretRetention int
	SwarmMode                  bool
	HashNormMap                map[string]string
}

func DeployStack(ctx context.Context, req DeployRequest) error {
	if err := validation.Validate(req); err != nil {
		return fmt.Errorf("validate deploy stack request: %w", err)
	}

	var (
		jobLog                     = req.JobLog
		externalRepoPath           = req.ExternalRepoPath
		dockerCli                  = req.DockerCLI
		payload                    = req.Payload
		deployConfig               = req.DeployConfig
		detectedChanges            = req.DetectedChanges
		needSignal                 = req.NeedSignal
		latestCommit               = req.LatestCommit
		appVersion                 = req.AppVersion
		composeLoad                = req.ComposeLoad
		globalSwarmConfigRetention = req.GlobalSwarmConfigRetention
		globalSwarmSecretRetention = req.GlobalSwarmSecretRetention
		swarmMode                  = req.SwarmMode
		hashNormMap                = req.HashNormMap
	)

	startTime := time.Now()
	repositoryLabel := resolveDeploymentMetricsRepositoryLabel(payload)
	deploymentLabel := resolveDeploymentMetricsDeploymentLabel(deployConfig.Name)
	contextLabel := DisplayContextName(deployConfig.Context)

	stackLog := jobLog.
		With(slog.String("stack", deployConfig.Name))

	stackLog.Debug("waiting for scheduler/deploy lock")

	stackLockKey := lock.StackKey(deployConfig.Context, deployConfig.Name)
	lock.LockStack(stackLockKey)

	defer lock.UnlockStack(stackLockKey)

	stackLog.Debug("acquired scheduler/deploy lock")

	deploymentPhase := newDeploymentPhaseState("resolving working directory")

	// Path on the host
	externalWorkingDir := path.Join(externalRepoPath, deployConfig.WorkingDirectory)

	externalWorkingDir, err := filepath.Abs(externalWorkingDir)
	if err != nil || !strings.HasPrefix(externalWorkingDir, externalRepoPath) {
		errMsg := "invalid working directory: resolved path is outside the allowed base directory"
		jobLog.Error(errMsg, slog.String("resolved_path", externalWorkingDir))

		return fmt.Errorf("%s", errMsg)
	}

	deploymentPhase.Set("loading compose configuration")

	project, err := LoadCompose(ctx, dockerCli, externalRepoPath, externalWorkingDir, deployConfig.Name, deployConfig.ComposeFiles,
		deployConfig.EnvFiles, deployConfig.Profiles, deployConfig.Internal.Environment, composeLoad)
	if err != nil {
		return fmt.Errorf("failed to load compose config: %w", err)
	}

	if err = validateScheduledJobPolicies(project, swarmMode); err != nil {
		return fmt.Errorf("invalid scheduled job restart policy: %w", err)
	}

	if deployConfig.WaitRunningJobs {
		deploymentPhase.Set("waiting for running scheduled jobs")

		if err = waitForRunningJobs(ctx, dockerCli, deployConfig, project, stackLog, swarmMode); err != nil {
			return err
		}
	}

	done := make(chan struct{})
	defer close(done)

	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				logDeploymentHeartbeat(stackLog, deploymentPhase.Get())
			case <-done:
				return
			}
		}
	}()

	timestamp := time.Now().UTC().Format(time.RFC3339)

	// Generate project hash with doco-cd labels
	// We don't want to compare the hashes with these labels
	projectHash, err := ProjectHash(WithNormalizedEnvValues(project, hashNormMap))
	if err != nil {
		return fmt.Errorf("failed to generate project hash: %w", err)
	}

	// When SwarmModeEnabled is true, we deploy the stack using Docker Swarm.
	if swarmMode {
		swarmConfigRetention := deployConfig.ResolveSwarmConfigRetention(globalSwarmConfigRetention)
		swarmSecretRetention := deployConfig.ResolveSwarmSecretRetention(globalSwarmSecretRetention)

		deploymentPhase.Set("deploying swarm stack")

		stackLog.Info("deploying swarm stack")

		cfg, opts, err := LoadSwarmStack(dockerCli, project, deployConfig, externalWorkingDir)
		if err != nil {
			return fmt.Errorf("failed to load swarm stack: %w", err)
		}

		addSwarmServiceLabels(cfg, project, deployConfig, payload, externalWorkingDir, appVersion, timestamp, latestCommit, projectHash)
		addSwarmVolumeLabels(cfg, deployConfig, payload, externalWorkingDir)
		addSwarmConfigLabels(cfg, deployConfig, payload, externalWorkingDir, appVersion, timestamp, latestCommit)
		addSwarmSecretLabels(cfg, deployConfig, payload, externalWorkingDir, appVersion, timestamp, latestCommit)

		if err = removeMismatchedRecreatableVolumes(ctx, dockerCli.Client(), deployConfig.Name, project); err != nil {
			prometheus.DeploymentErrorsTotal.WithLabelValues(repositoryLabel, deploymentLabel, contextLabel).Inc()
			return fmt.Errorf("failed to remove mismatched recreatable volumes: %w", err)
		}

		err = DeploySwarmStack(ctx, dockerCli, cfg, opts)
		if err != nil {
			prometheus.DeploymentErrorsTotal.WithLabelValues(repositoryLabel, deploymentLabel, contextLabel).Inc()

			errMsg := "failed to deploy swarm stack " + deployConfig.Name

			return fmt.Errorf("%s: %w", errMsg, err)
		}

		if swarmConfigRetention >= 0 {
			deploymentPhase.Set("pruning stack configs")

			err = PruneStackConfigs(ctx, dockerCli.Client(), deployConfig.Name, swarmConfigRetention)
			if err != nil {
				prometheus.DeploymentErrorsTotal.WithLabelValues(repositoryLabel, deploymentLabel, contextLabel).Inc()

				errMsg := "failed to prune stack configs"

				return fmt.Errorf("%s: %w", errMsg, err)
			}
		} else {
			stackLog.Info("skipping swarm config prune: retention disabled", slog.Int("retention", swarmConfigRetention))
		}

		if swarmSecretRetention >= 0 {
			deploymentPhase.Set("pruning stack secrets")

			err = PruneStackSecrets(ctx, dockerCli.Client(), deployConfig.Name, swarmSecretRetention)
			if err != nil {
				prometheus.DeploymentErrorsTotal.WithLabelValues(repositoryLabel, deploymentLabel, contextLabel).Inc()

				errMsg := "failed to prune stack secrets"

				return fmt.Errorf("%s: %w", errMsg, err)
			}
		} else {
			stackLog.Info("skipping swarm secret prune: retention disabled", slog.Int("retention", swarmSecretRetention))
		}

		if deployConfig.PruneImages {
			deploymentPhase.Set("pruning images on swarm nodes")

			stackLog.Info("prune images on swarm nodes")

			err = RunImagePruneJob(ctx, dockerCli)
			if err != nil {
				prometheus.DeploymentErrorsTotal.WithLabelValues(repositoryLabel, deploymentLabel, contextLabel).Inc()

				errMsg := "failed to run image prune job"

				return fmt.Errorf("%s: %w", errMsg, err)
			}
		}
	} else {
		addComposeServiceLabels(project, deployConfig, payload, externalWorkingDir, appVersion, timestamp, ComposeVersion, latestCommit, projectHash)
		addComposeVolumeLabels(project, deployConfig, payload, appVersion, timestamp, ComposeVersion, latestCommit, projectHash)

		forcedServices := set.New[string]() // services to recreate if project files changed
		recreateMode := api.RecreateDiverged

		switch {
		case len(detectedChanges) > 0:
			recreateMode = api.RecreateForce
			forcedServices = forcedRecreateServices(detectedChanges)

			stackLog.Debug("changed project files detected, forcing recreate", slog.Any("changes", detectedChanges))
		case len(needSignal) > 0:
			stackLog.Debug("changed project files detected, sending signal to service",
				slog.Any("need_signal", needSignal))
		}

		if recreateMode == api.RecreateDiverged && hasIPv6NetworkWithoutExplicitSubnet(project) {
			recreateMode = api.RecreateForce

			stackLog.Warn("network has enable_ipv6 without explicit ipam subnet; forcing recreate to avoid diverged compare parser failure")
		}

		stackLog.Info("deploying stack",
			slog.Group("recreate",
				slog.String("mode", recreateMode),
				slog.Any("forced_services", forcedServices.ToSlice()),
			),
			slog.Any("need_signal", needSignal),
		)

		deploymentPhase.Set("deploying compose stack")

		err = deployCompose(ctx, dockerCli, project, deployConfig, recreateMode,
			forcedServices.ToSlice(), needSignal, deploymentPhase.Set)
		if err != nil {
			prometheus.DeploymentErrorsTotal.WithLabelValues(repositoryLabel, deploymentLabel, contextLabel).Inc()
			return fmt.Errorf("failed to deploy stack: %w", err)
		}
	}

	deploymentPhase.Set("finalizing deployment status")

	// cache the deployment status after successful deployment
	repositoryKey := deploymentRepositoryKey(payload)

	setDeployStatusToCache(gitInternal.GetRepoName(repositoryKey), deployConfig.Name,
		deployStatus{
			CommitSHA:   latestCommit,
			ComposeHash: projectHash,
		},
	)

	prometheus.DeploymentsTotal.WithLabelValues(repositoryLabel, deploymentLabel, contextLabel).Inc()
	prometheus.DeploymentDuration.WithLabelValues(repositoryLabel, deploymentLabel, contextLabel).Observe(time.Since(startTime).Seconds())

	return nil
}
