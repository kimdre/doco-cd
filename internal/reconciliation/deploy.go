package reconciliation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	dockerSwarm "github.com/kimdre/doco-cd/internal/docker/swarm"

	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/stages"
	"github.com/kimdre/doco-cd/internal/test"
	"github.com/kimdre/doco-cd/internal/webhook"
)

var ErrOCIArtifactNotVerified = errors.New("OCI artifact is not verified")

func (m *Manager) Deploy(ctx context.Context,
	jobLog *slog.Logger,
	appConfig *app.Config,
	dataMountPoint container.MountPoint,
	dockerCli command.Cli,
	contexts *docker.ContextRegistry,
	secretProvider secretprovider.SecretProvider,
	metadata notification.Metadata,
	jobTrigger stages.JobTrigger,
	repoData stages.RepositoryData,
	deployConfigs []*deployConfig.Config,
	payload *webhook.ParsedPayload,
	testName string,
) error {
	if m == nil {
		return errors.New("reconciliation manager is required")
	}

	if err := m.beginDeploy(); err != nil {
		return err
	}
	defer m.deployWG.Done()

	err := m.deploy(ctx, jobLog, appConfig,
		dataMountPoint, dockerCli, contexts, secretProvider, metadata,
		jobTrigger, repoData, deployConfigs, payload, testName)

	// Skip long-lived reconciliation listeners for test-triggered deployments.
	// Test runs use testName only to make stacks unique and do not need background
	// Docker event watchers that can outlive the test and race with TempDir cleanup.
	if testName == "" {
		m.addJob(ctx, jobInfo{
			appConfig:      appConfig,
			dataMountPoint: dataMountPoint,
			dockerCli:      dockerCli,
			contexts:       contexts,
			secretProvider: secretProvider,
			log:            jobLog,
			metadata:       metadata,
			jobTrigger:     jobTrigger,
			repoData:       repoData,
			deployConfigs:  deployConfigs,
			payload:        payload,
			testName:       testName,
		})
	}

	return err
}

func (m *Manager) deploy(ctx context.Context,
	jobLog *slog.Logger,
	appConfig *app.Config,
	dataMountPoint container.MountPoint,
	dockerCli command.Cli,
	contexts *docker.ContextRegistry,
	secretProvider secretprovider.SecretProvider,
	metadata notification.Metadata,
	jobTrigger stages.JobTrigger,
	repoData stages.RepositoryData,
	deployConfigs []*deployConfig.Config,
	payload *webhook.ParsedPayload,
	testName string,
) error {
	if repoData.Source == config.SourceTypeOCI && !repoData.OCITrusted {
		return fmt.Errorf("%w: refusing to run reconciliation cleanup before trust-policy verification", ErrOCIArtifactNotVerified)
	}

	configsByContext := map[string][]*deployConfig.Config{}

	for _, dc := range deployConfigs {
		contextName := docker.NormalizeContextName(dc.Context)
		configsByContext[contextName] = append(configsByContext[contextName], dc)
	}

	dockerQuiet := false
	if appConfig != nil {
		dockerQuiet = appConfig.DockerQuietDeploy
	}

	for contextName, groupedConfigs := range configsByContext {
		entry := resolveDeployContext(ctx, contexts, dockerCli, dockerQuiet, contextName)
		if entry.err != nil {
			// Isolate per-context failures: an unreachable context must not block
			// cleanup/deploy for other (healthy) contexts. handleDeploy below fails
			// only the affected deployments.
			jobLog.Error("failed to create docker client for context, skipping cleanup for it",
				slog.String("context", docker.DisplayContextName(contextName)), logger.ErrAttr(entry.err))

			continue
		}

		for swarmMode, modeConfigs := range groupDeployConfigsByMode(groupedConfigs, entry.swarmMode) {
			if err := cleanupObsoleteAutoDiscoveredContainers(ctx, jobLog,
				entry.cli, swarmMode, contextName, repoData.SourceUrl,
				modeConfigs,
				metadata); err != nil {
				jobLog.Error("failed to clean up obsolete auto-discovered containers for context",
					slog.String("context", docker.DisplayContextName(contextName)),
					slog.Bool("swarm_mode", swarmMode),
					logger.ErrAttr(err))
			}
		}

		if entry.closeFn != nil {
			entry.closeFn()
		}
	}

	return m.handleDeploy(ctx, jobLog, appConfig,
		dataMountPoint, dockerCli, contexts, secretProvider, metadata.JobID, jobTrigger,
		repoData, deployConfigs, payload, testName, metadata)
}

func (m *Manager) handleDeploy(ctx context.Context,
	jobLog *slog.Logger,
	appConfig *app.Config,
	dataMountPoint container.MountPoint,
	dockerCli command.Cli,
	contexts *docker.ContextRegistry,
	secretProvider secretprovider.SecretProvider,
	jobID string,
	jobTrigger stages.JobTrigger,
	repoData stages.RepositoryData,
	deployConfigs []*deployConfig.Config,
	payload *webhook.ParsedPayload,
	testName string,
	metadata notification.Metadata,
) error {
	dockerQuiet := false
	if appConfig != nil {
		dockerQuiet = appConfig.DockerQuietDeploy
	}

	// Build one Docker CLI per distinct context up front and share it across all
	// deployments targeting that context, instead of creating a client per deployment.
	contextCLIs := buildDeployContextCLIs(ctx, contexts, dockerCli, dockerQuiet, deployConfigs)

	defer func() {
		for contextName, entry := range contextCLIs {
			if contextName != "" && entry.closeFn != nil {
				entry.closeFn()
			}
		}
	}()

	// Deployments run concurrently, grouped by repository and reference, and
	// limited by this manager's deployment limiter.
	var wg sync.WaitGroup

	resultCh := make(chan error, len(deployConfigs))

	for _, deployCfg := range deployConfigs {
		deployLog := jobLog.
			WithGroup("deploy").
			With(slog.String("stack", deployCfg.Name))

		if repoData.Source != config.SourceTypeOCI {
			deployLog = deployLog.With(slog.String("reference", deployCfg.Reference))
		}

		if ctx := strings.TrimSpace(deployCfg.Context); ctx != "" {
			deployLog = deployLog.With(slog.String("context", ctx))
		}

		// Used to make test deployments unique and prevent conflicts between tests when running in parallel.
		// It is not used in production.
		if testName != "" {
			deployCfg.Name = test.ConvertTestName(testName)
		}

		m.deployments.start(repoData.Name, deployCfg.Context, deployCfg.Name)

		wg.Add(1)

		go func(dc *deployConfig.Config) {
			defer wg.Done()
			defer m.deployments.finish(repoData.Name, dc.Context, dc.Name)

			contextName := docker.NormalizeContextName(dc.Context)

			entry, ok := contextCLIs[contextName]
			if !ok || entry.err != nil {
				if ok && entry.err != nil {
					resultCh <- entry.err
				} else {
					resultCh <- fmt.Errorf("no docker client available for context %q", docker.DisplayContextName(contextName))
				}

				return
			}

			err := m.handleOneDeploy(ctx, deployLog,
				appConfig, dataMountPoint, entry.cli, entry.swarmMode, secretProvider,
				dc, jobID, jobTrigger, repoData, payload, metadata)

			resultCh <- err
		}(deployCfg)
	}

	// Wait for all deployments to complete
	wg.Wait()
	close(resultCh)

	var (
		errs         []error
		successCount int
	)

	for e := range resultCh {
		if e == nil {
			successCount++
			continue
		}

		if errors.Is(e, stages.ErrWebhookFilterMismatch) {
			continue // counted implicitly via successCount staying 0
		}

		errs = append(errs, e)
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	if successCount == 0 && len(deployConfigs) > 0 {
		// All deployments were skipped by the webhook filter
		return stages.ErrWebhookFilterMismatch
	}

	return nil
}

// deployContextCLI holds a resolved Docker CLI (and its metadata) for a single Docker context,
// shared across all deployments in a handleDeploy batch that target that context.
type deployContextCLI struct {
	cli       command.Cli
	closeFn   func() // nil for the default context (which reuses the base CLI)
	swarmMode bool
	err       error // set when the context CLI could not be created/probed
}

// buildDeployContextCLIs creates one Docker CLI per distinct context referenced in deployConfigs.
// The default context (empty string) reuses baseCli; custom contexts get a dedicated client whose
// closeFn must be called by the caller. Errors are captured per context so only the affected
// deployments fail rather than the whole batch.
func buildDeployContextCLIs(ctx context.Context, contexts *docker.ContextRegistry, baseCli command.Cli, quiet bool, deployConfigs []*deployConfig.Config) map[string]deployContextCLI {
	contextCLIs := make(map[string]deployContextCLI)

	for _, dc := range deployConfigs {
		contextName := docker.NormalizeContextName(dc.Context)
		if _, exists := contextCLIs[contextName]; exists {
			continue
		}

		contextCLIs[contextName] = resolveDeployContext(ctx, contexts, baseCli, quiet, contextName)
	}

	return contextCLIs
}

func resolveDeployContext(ctx context.Context, contexts *docker.ContextRegistry, baseCli command.Cli, quiet bool, contextName string) deployContextCLI {
	contextName = docker.NormalizeContextName(contextName)
	if contexts != nil {
		cc, err := contexts.Get(ctx, contextName)
		if err != nil {
			return deployContextCLI{err: err}
		}

		return deployContextCLI{cli: cc.Cli, swarmMode: cc.SwarmMode}
	}

	if contextName == "" {
		return deployContextCLI{cli: baseCli, swarmMode: dockerSwarm.GetModeEnabled()}
	}

	cli, closeFn, err := dockerCliForContext(baseCli, quiet, contextName)
	if err != nil {
		return deployContextCLI{err: err}
	}

	swarmMode, err := dockerSwarm.ResolveModeEnabled(ctx, cli.Client())
	if err != nil {
		if closeFn != nil {
			closeFn()
		}

		return deployContextCLI{err: fmt.Errorf("failed to check if docker host is running in swarm mode: %w", err)}
	}

	return deployContextCLI{cli: cli, closeFn: closeFn, swarmMode: swarmMode}
}

func (m *Manager) handleOneDeploy(ctx context.Context, deployLog *slog.Logger,
	appConfig *app.Config, dataMountPoint container.MountPoint,
	deploymentDockerCli command.Cli, swarmAvailable bool,
	secretProvider secretprovider.SecretProvider,
	dc *deployConfig.Config,
	jobID string,
	jobTrigger stages.JobTrigger,
	repoData stages.RepositoryData,
	payLad *webhook.ParsedPayload,
	metadata notification.Metadata,
) error {
	swarmMode, err := dc.ResolveSwarmMode(swarmAvailable)
	if err != nil {
		return fmt.Errorf("failed to resolve swarm mode for deployment %q on docker context %q: %w",
			dc.Name, docker.DisplayContextName(dc.Context), err)
	}

	if m.limiter != nil {
		deployLog.Debug("queuing deployment")

		unlock, lErr := m.limiter.acquire(ctx, repoData.Name, NormalizeReference(dc.Reference))
		if lErr != nil {
			return lErr
		}
		defer unlock()
	}

	stageMgr := stages.NewStageManager(
		jobID,
		jobTrigger,
		deployLog,
		failNotifyFunc,
		&repoData,
		&stages.Docker{
			Cmd:            deploymentDockerCli,
			DataMountPoint: dataMountPoint,
			SwarmMode:      swarmMode,
			SwarmAvailable: swarmAvailable,
		},
		payLad,
		appConfig,
		dc,
		secretProvider,
		metadata,
	)

	err = stageMgr.RunStages(ctx)
	if err != nil {
		return err
	}

	return nil
}

// groupDeployConfigsByMode partitions configs by their selected runtime mode.
// Invalid explicit swarm requests are excluded here; handleOneDeploy reports
// their descriptive error to the caller.
func groupDeployConfigsByMode(dcs []*deployConfig.Config, swarmAvailable bool) map[bool][]*deployConfig.Config {
	grouped := make(map[bool][]*deployConfig.Config)

	for _, dc := range dcs {
		if dc == nil {
			continue
		}

		swarmMode, err := dc.ResolveSwarmMode(swarmAvailable)
		if err != nil {
			continue
		}

		grouped[swarmMode] = append(grouped[swarmMode], dc)
	}

	return grouped
}

func dockerCliForContext(baseCli command.Cli, quiet bool, contextName string) (command.Cli, func(), error) {
	contextName = docker.NormalizeContextName(contextName)
	if contextName == "" {
		return baseCli, nil, nil
	}

	contextCli, err := docker.CreateDockerCliWithContext(quiet, contextName)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create docker client for context %q: %w", contextName, err)
	}

	closeFn := func() {
		_ = contextCli.Client().Close()
	}

	return contextCli, closeFn, nil
}

func failNotifyFunc(deployLog *slog.Logger, err error, metadata notification.Metadata) {
	// Don't write to HTTP from goroutines — just send notification and log
	go func() {
		notifyErr := notification.Send(notification.Failure, "Deployment Failed", err.Error(), metadata)
		if notifyErr != nil {
			deployLog.Error("failed to send notification", logger.ErrAttr(notifyErr))
		}
	}()

	deployLog.Error("deployment failed",
		slog.String("stack", metadata.Stack),
		logger.ErrAttr(err))
}
