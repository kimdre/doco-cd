package reconciliation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/docker/cli/cli/command"

	"github.com/kimdre/doco-cd/internal/common/validation"
	"github.com/kimdre/doco-cd/internal/config"
	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"

	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/stages"
	"github.com/kimdre/doco-cd/internal/test"
)

var ErrOCIArtifactNotVerified = errors.New("OCI artifact is not verified")

// Deploy validates req and runs a reconciliation deployment using the Manager's stable
// dependencies (app config, Docker CLI, context registry, secret provider) together with req's
// per-run trigger, repository, deploy configs, and notification metadata. Unless req.TestName is
// set, it also registers a long-lived reconciliation job that watches for drift after the
// initial deployment.
func (m *Manager) Deploy(ctx context.Context, req DeployRequest) error {
	if m == nil {
		return errors.New("reconciliation manager is required")
	}

	if err := m.beginDeploy(); err != nil {
		return err
	}
	defer m.deployWG.Done()

	if err := validation.Validate(req); err != nil {
		return fmt.Errorf("validate deploy request: %w", err)
	}

	err := m.deploy(ctx, req)

	// Skip long-lived reconciliation listeners for test-triggered deployments.
	// Test runs use testName only to make stacks unique and do not need background
	// Docker event watchers that can outlive the test and race with TempDir cleanup.
	if req.TestName == "" {
		m.addJob(ctx, req)
	}

	return err
}

func (m *Manager) deploy(ctx context.Context, req DeployRequest) error {
	if req.Repository.Source == config.SourceTypeOCI && !req.Repository.OCITrusted {
		return fmt.Errorf("%w: refusing to run reconciliation cleanup before trust-policy verification", ErrOCIArtifactNotVerified)
	}

	configsByContext := map[string][]*deployConfig.Config{}

	for _, dc := range req.DeployConfigs {
		contextName := docker.NormalizeContextName(dc.Context)
		configsByContext[contextName] = append(configsByContext[contextName], dc)
	}

	for contextName, groupedConfigs := range configsByContext {
		entry := resolveDeployContext(ctx, m.contexts, contextName)
		if entry.err != nil {
			// Isolate per-context failures: an unreachable context must not block
			// cleanup/deploy for other (healthy) contexts. handleDeploy below fails
			// only the affected deployments.
			req.Logger.Error("failed to create docker client for context, skipping cleanup for it",
				slog.String("context", docker.DisplayContextName(contextName)), logger.ErrAttr(entry.err))

			continue
		}

		for swarmMode, modeConfigs := range groupDeployConfigsByMode(groupedConfigs, entry.swarmMode) {
			if err := cleanupObsoleteAutoDiscoveredContainers(ctx, req.Logger,
				entry.cli, swarmMode, contextName, req.Repository.SourceUrl,
				modeConfigs,
				req.Metadata); err != nil {
				req.Logger.Error("failed to clean up obsolete auto-discovered containers for context",
					slog.String("context", docker.DisplayContextName(contextName)),
					slog.Bool("swarm_mode", swarmMode),
					logger.ErrAttr(err))
			}
		}
	}

	return m.handleDeploy(ctx, req)
}

func (m *Manager) handleDeploy(ctx context.Context, req DeployRequest) error {
	// Build one Docker CLI per distinct context up front and share it across all
	// deployments targeting that context, instead of creating a client per deployment.
	contextCLIs := buildDeployContextCLIs(ctx, m.contexts, req.DeployConfigs)

	// Deployments run concurrently, grouped by repository and reference, and
	// limited by this manager's deployment limiter.
	var wg sync.WaitGroup

	resultCh := make(chan error, len(req.DeployConfigs))

	for _, deployCfg := range req.DeployConfigs {
		deployLog := req.Logger.
			WithGroup("deploy").
			With(slog.String("stack", deployCfg.Name))

		if req.Repository.Source != config.SourceTypeOCI {
			deployLog = deployLog.With(slog.String("reference", deployCfg.Reference))
		}

		if ctxName := strings.TrimSpace(deployCfg.Context); ctxName != "" {
			deployLog = deployLog.With(slog.String("context", ctxName))
		}

		// Used to make test deployments unique and prevent conflicts between tests when running in parallel.
		// It is not used in production.
		if req.TestName != "" {
			deployCfg.Name = test.ConvertTestName(req.TestName)
		}

		m.deployments.start(req.Repository.Name, deployCfg.Context, deployCfg.Name)

		wg.Add(1)

		go func(dc *deployConfig.Config) {
			defer wg.Done()
			defer m.deployments.finish(req.Repository.Name, dc.Context, dc.Name)

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

			err := m.handleOneDeploy(ctx, req, deployLog, entry.cli, entry.swarmMode, dc)

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

	if successCount == 0 && len(req.DeployConfigs) > 0 {
		// All deployments were skipped by the webhook filter
		return stages.ErrWebhookFilterMismatch
	}

	return nil
}

// deployContextCLI holds a resolved Docker CLI (and its metadata) for a single Docker context,
// shared across all deployments in a handleDeploy batch that target that context.
type deployContextCLI struct {
	cli       command.Cli
	swarmMode bool
	err       error // set when the context CLI could not be created/probed
}

// buildDeployContextCLIs creates one Docker CLI per distinct context referenced in deployConfigs.
// Errors are captured per context so only the affected deployments fail rather
// than the whole batch.
func buildDeployContextCLIs(ctx context.Context, contexts *docker.ContextRegistry, deployConfigs []*deployConfig.Config) map[string]deployContextCLI {
	contextCLIs := make(map[string]deployContextCLI)

	for _, dc := range deployConfigs {
		contextName := docker.NormalizeContextName(dc.Context)
		if _, exists := contextCLIs[contextName]; exists {
			continue
		}

		contextCLIs[contextName] = resolveDeployContext(ctx, contexts, contextName)
	}

	return contextCLIs
}

func resolveDeployContext(ctx context.Context, contexts *docker.ContextRegistry, contextName string) deployContextCLI {
	contextName = docker.NormalizeContextName(contextName)

	cc, err := contexts.Get(ctx, contextName)
	if err != nil {
		return deployContextCLI{err: err}
	}

	return deployContextCLI{cli: cc.Cli, swarmMode: cc.SwarmMode}
}

func (m *Manager) handleOneDeploy(ctx context.Context, req DeployRequest, deployLog *slog.Logger,
	deploymentDockerCli command.Cli, swarmAvailable bool, dc *deployConfig.Config,
) error {
	swarmMode, err := dc.ResolveSwarmMode(swarmAvailable)
	if err != nil {
		return fmt.Errorf("failed to resolve swarm mode for deployment %q on docker context %q: %w",
			dc.Name, docker.DisplayContextName(dc.Context), err)
	}

	if m.limiter != nil {
		deployLog.Debug("queuing deployment")

		unlock, lErr := m.limiter.acquire(ctx, req.Repository.Name, NormalizeReference(dc.Reference))
		if lErr != nil {
			return lErr
		}
		defer unlock()
	}

	stageMgr, err := stages.NewStageManager(
		stages.Dependencies{
			AppConfig:         m.appConfig,
			SecretProvider:    m.secretProvider,
			NotifyFailureFunc: failNotifyFunc,
		},
		stages.RunInput{
			Log:        deployLog,
			JobID:      req.Metadata.JobID,
			JobTrigger: req.JobTrigger,
			Repository: &req.Repository,
			Docker: &stages.Docker{
				Cmd:            deploymentDockerCli,
				DataMountPoint: m.dataMountPoint,
				SwarmMode:      swarmMode,
				SwarmAvailable: swarmAvailable,
			},
			Payload:      req.Payload,
			DeployConfig: dc,
			Metadata:     req.Metadata,
		},
	)
	if err != nil {
		return err
	}

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

func failNotifyFunc(deployLog *slog.Logger, err error, metadata notification.Metadata) {
	// Don't write to HTTP from goroutines. Just send notification and log
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
