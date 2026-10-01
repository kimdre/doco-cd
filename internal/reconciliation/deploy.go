package reconciliation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/docker/cli/cli/command"

	"github.com/kimdre/doco-cd/internal/commitstatus"
	"github.com/kimdre/doco-cd/internal/common/validation"
	"github.com/kimdre/doco-cd/internal/config"
	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"

	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/prometheus"
	"github.com/kimdre/doco-cd/internal/selfupdate"
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

	// Sync windows are evaluated once for the whole request, see syncWindowGate.
	gate := m.newSyncWindowGate(req, time.Now())

	err := m.deploy(ctx, req, gate)

	// Skip long-lived reconciliation listeners for test-triggered deployments.
	// Test runs use testName only to make stacks unique and do not need background
	// Docker event watchers that can outlive the test and race with TempDir cleanup.
	if req.TestName == "" {
		m.addJob(ctx, req, gate.deferred())
	}

	return err
}

func (m *Manager) deploy(ctx context.Context, req DeployRequest, gate *syncWindowGate) error {
	if req.Repository.Source == config.SourceTypeOCI && !req.Repository.OCITrusted {
		return fmt.Errorf("%w: refusing to run reconciliation cleanup before trust-policy verification", ErrOCIArtifactNotVerified)
	}

	configsByContext := map[string][]*deployConfig.Config{}
	contextCLIs := buildDeployContextCLIs(ctx, m.contexts, req.DeployConfigs)

	for _, dc := range req.DeployConfigs {
		contextName := docker.NormalizeContextName(dc.Context)
		configsByContext[contextName] = append(configsByContext[contextName], dc)
	}

	for contextName, groupedConfigs := range configsByContext {
		entry := contextCLIs[contextName]
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
				req.Metadata, m.notifier, gate.removalPredicate(contextName)); err != nil {
				req.Logger.Error("failed to clean up obsolete auto-discovered containers for context",
					slog.String("context", docker.DisplayContextName(contextName)),
					slog.Bool("swarm_mode", swarmMode),
					logger.ErrAttr(err))
			}
		}
	}

	return m.handleDeployWithContexts(ctx, req, contextCLIs, gate)
}

// handleDeploy deploys req. It is used by reconciliation, whose requests are
// only gated by sync windows for stacks that would not be restored to a
// revision known to be deployed, see stackRevision.restoresDeployed.
func (m *Manager) handleDeploy(ctx context.Context, req DeployRequest) error {
	contextCLIs := buildDeployContextCLIs(ctx, m.contexts, req.DeployConfigs)

	return m.handleDeployWithContexts(ctx, req, contextCLIs, m.newSyncWindowGate(req, time.Now()))
}

func (m *Manager) handleDeployWithContexts(ctx context.Context, req DeployRequest, contextCLIs map[string]deployContextCLI, gate *syncWindowGate) error {
	// Deployments run concurrently, grouped by repository and reference, and
	// limited by this manager's deployment limiter.
	var wg sync.WaitGroup

	resultCh := make(chan error, len(req.DeployConfigs))
	gitChanges := stages.NewGitChangeCache()
	gitAncestry := stages.NewGitAncestryCache()

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

			// A panic here (e.g. from a lower-level library bug) must never take
			// down the whole process: it would abort every other concurrently
			// running deployment too. Recover, log it, and report this stack's
			// deployment as failed instead.
			defer func() {
				if recovered := recover(); recovered != nil {
					logger.LogRecoveredPanic(deployLog, "stack deployment", recovered)

					resultCh <- fmt.Errorf("panic during deployment of stack %q: %v", dc.Name, recovered)
				}
			}()

			contextName := docker.NormalizeContextName(dc.Context)

			entry, ok := contextCLIs[contextName]
			if !ok || entry.err != nil {
				err := entry.err
				if !ok || err == nil {
					err = fmt.Errorf("no docker client available for context %q", docker.DisplayContextName(contextName))
				}

				gate.record(dc, err)

				resultCh <- err

				return
			}

			err := m.handleOneDeploy(ctx, req, deployLog, entry.cli, entry.swarmMode, dc, gitChanges, gitAncestry, gate)

			gate.record(dc, err)

			resultCh <- err
		}(deployCfg)
	}

	// Wait for all deployments to complete
	wg.Wait()
	close(resultCh)

	results := make([]error, 0, len(req.DeployConfigs))
	for e := range resultCh {
		results = append(results, e)
	}

	return summarizeDeployResults(results, len(req.DeployConfigs))
}

// summarizeDeployResults combines the results of the stacks of one request
// into the request's result. total is the number of stacks in the request.
//
// If no stack was deployed and every stack was skipped, filtered out or
// deferred by a sync window, the result is a merged
// *stages.SyncWindowBlockedError if any stack was deferred, and
// stages.ErrSkipDeployment otherwise.
func summarizeDeployResults(results []error, total int) error {
	var (
		errs            []error
		blocked         []*stages.SyncWindowBlockedError
		successCount    int
		skipCount       int
		filterSkipCount int
		handoverCount   int
	)

	for _, e := range results {
		if e == nil {
			successCount++
			continue
		}

		if errors.Is(e, stages.ErrWebhookFilterMismatch) {
			filterSkipCount++
			continue
		}

		if blockedErr, ok := errors.AsType[*stages.SyncWindowBlockedError](e); ok {
			blocked = append(blocked, blockedErr)
			continue
		}

		if errors.Is(e, stages.ErrSkipDeployment) {
			skipCount++
			continue
		}

		if errors.Is(e, selfupdate.ErrHandover) {
			handoverCount++
			continue
		}

		errs = append(errs, e)
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	if handoverCount > 0 {
		return selfupdate.ErrHandover
	}

	if successCount == 0 && total > 0 {
		if filterSkipCount == total {
			return stages.ErrWebhookFilterMismatch
		}

		if skipCount+filterSkipCount+len(blocked) == total {
			if len(blocked) > 0 {
				return stages.MergeSyncWindowBlocked(blocked)
			}

			return stages.ErrSkipDeployment
		}
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
	deploymentDockerCli command.Cli, swarmAvailable bool, dc *deployConfig.Config, gitChanges *stages.GitChangeCache,
	gitAncestry *stages.GitAncestryCache, gate *syncWindowGate,
) error {
	swarmMode, err := dc.ResolveSwarmMode(swarmAvailable)
	if err != nil {
		return fmt.Errorf("failed to resolve swarm mode for deployment %q on docker context %q: %w",
			dc.Name, docker.DisplayContextName(dc.Context), err)
	}

	stageMgr, err := stages.NewStageManager(
		stages.Dependencies{
			AppConfig:       m.appConfig,
			SecretProvider:  m.secretProvider,
			Notifier:        m.notifier,
			SchedulerHolds:  m,
			Contexts:        m.contexts,
			LeftoverTracker: m.leftoverTracker,
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
			GitChanges:   gitChanges,
			GitAncestry:  gitAncestry,
			ProjectSkips: m.projectSkips,
		},
	)
	if err != nil {
		return err
	}

	if !stageMgr.MatchesWebhookEventFilter() {
		return stages.ErrWebhookFilterMismatch
	}

	if dc.Destroy.Enabled {
		// Destroy has no pre-deploy stage to confirm there is work to do, so
		// check for the stack directly: a stack that is already gone must not
		// be reported as deferred on every run.
		if gate.blocks(dc) && destroyHasNothingToRemove(ctx, deployLog, deploymentDockerCli.Client(), swarmMode, dc) {
			deployLog.Debug("stack to destroy does not exist, skipping destruction")

			return stages.ErrSkipDeployment
		}

		if err := gate.admit(deployLog, dc, nil); err != nil {
			return err
		}

		release, admissionErr := m.acquireDeploymentPhase(ctx, deployLog, req.Repository.Name, phaseDeployment, m.limiter, true)
		if admissionErr != nil {
			return admissionErr
		}
		defer release()

		return stageMgr.RunStages(ctx, nil)
	}

	releasePreDeploy, err := m.acquireDeploymentPhase(ctx, deployLog, req.Repository.Name, phasePreDeploy, m.preDeployLimiter, true)
	if err != nil {
		return err
	}
	defer func() {
		if releasePreDeploy != nil {
			releasePreDeploy()
		}
	}()

	return stageMgr.RunStages(ctx, func(ctx context.Context) (func(), error) {
		releasePreDeploy()
		releasePreDeploy = nil

		// Pre-deploy confirmed that the stack must change, so this is the
		// point where a sync window defers it: before any notification,
		// commit status or deployment admission.
		rev := stackRevision{
			revision: stageMgr.Repository.Revision,
			deployed: stageMgr.DeployState.DeployedCommit,
			own:      stageMgr.ResolvedOwnReference(),
		}

		if err := gate.admitStack(deployLog, dc, rev, func(description string) {
			stageMgr.PostCommitStatusWithOutcome(ctx, commitstatus.StatePending, description, commitstatus.OutcomeDeferred)
		}); err != nil {
			return nil, err
		}

		return m.acquireQueuedDeploymentPhase(ctx, stageMgr)
	})
}

// acquireQueuedDeploymentPhase acquires a deploymentPhase (phaseDeployment) from the manager's limiter
// and posts a queued commit status to the repository. It returns a release function that must be
// called to release the acquired phase, and an error if the acquisition failed.
func (m *Manager) acquireQueuedDeploymentPhase(ctx context.Context, stageMgr *stages.StageManager) (func(), error) {
	stageMgr.PostQueuedCommitStatus(ctx)

	return m.acquireDeploymentPhase(ctx, stageMgr.Log, stageMgr.Repository.Name, phaseDeployment, m.limiter, false)
}

// acquireDeploymentPhase acquires a deploymentPhase (phasePreDeploy or phaseDeployment) from the given limiter.
// It returns a release function that must be called to release the acquired phase,
// and an error if the acquisition failed.
func (m *Manager) acquireDeploymentPhase(
	ctx context.Context,
	deployLog *slog.Logger,
	repository string,
	phase deploymentPhase,
	limiter *DeployerLimiter,
	recordLegacyQueue bool,
) (func(), error) {
	if limiter == nil {
		return func() {}, nil
	}

	if phase == phaseDeployment {
		deployLog.Debug("queuing deployment")
	} else {
		deployLog.Debug("queuing pre-deployment")
	}

	startedAt := time.Now()
	release, err := limiter.acquire(ctx, repository)

	outcome := "admitted"
	if err != nil {
		outcome = "canceled"
	}

	elapsed := time.Since(startedAt).Seconds()
	metricRepository := resolveDeploymentQueueRepository(repository)

	prometheus.DeploymentAdmissionDuration.WithLabelValues(metricRepository, string(phase), outcome).Observe(elapsed)

	if recordLegacyQueue {
		prometheus.DeploymentQueueDuration.WithLabelValues(metricRepository, outcome).Observe(elapsed)
	}

	if err != nil {
		return nil, err
	}

	return release, nil
}

// resolveDeploymentQueueRepository returns a sanitized repository name for use in Prometheus metrics.
func resolveDeploymentQueueRepository(repository string) string {
	repository = strings.TrimSpace(repository)
	if repository == "" {
		return "unknown"
	}

	return repository
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
