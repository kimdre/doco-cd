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

func Deploy(ctx context.Context,
	jobLog *slog.Logger,
	appConfig *app.Config,
	dataMountPoint container.MountPoint,
	dockerCli command.Cli,
	secretProvider *secretprovider.SecretProvider,
	metadata notification.Metadata,
	jobTrigger stages.JobTrigger,
	repoData stages.RepositoryData,
	deployConfigs []*deployConfig.Config,
	payload *webhook.ParsedPayload,
	testName string,
) error {
	err := deploy(ctx, jobLog, appConfig,
		dataMountPoint, dockerCli, secretProvider, metadata,
		jobTrigger, repoData, deployConfigs, payload, testName)

	// Skip long-lived reconciliation listeners for test-triggered deployments.
	// Test runs use testName only to make stacks unique and do not need background
	// Docker event watchers that can outlive the test and race with TempDir cleanup.
	if testName == "" {
		reconciliationHandler.addJob(ctx, jobInfo{
			appConfig:      appConfig,
			dataMountPoint: dataMountPoint,
			dockerCli:      dockerCli,
			secretProvider: secretProvider,
			jobLog:         jobLog,
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

func deploy(ctx context.Context,
	jobLog *slog.Logger,
	appConfig *app.Config,
	dataMountPoint container.MountPoint,
	dockerCli command.Cli,
	secretProvider *secretprovider.SecretProvider,
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
		contextName := strings.TrimSpace(dc.Context)
		configsByContext[contextName] = append(configsByContext[contextName], dc)
	}

	dockerQuiet := false
	if appConfig != nil {
		dockerQuiet = appConfig.DockerQuietDeploy
	}

	for contextName, groupedConfigs := range configsByContext {
		cleanupCli, closeFn, err := dockerCliForContext(dockerCli, dockerQuiet, contextName)
		if err != nil {
			return err
		}

		if err := cleanupObsoleteAutoDiscoveredContainers(ctx, jobLog,
			cleanupCli, repoData.SourceUrl,
			groupedConfigs,
			metadata); err != nil {
			if closeFn != nil {
				closeFn()
			}

			return fmt.Errorf("failed to clean up obsolete auto-discovered containers: %w", err)
		}

		if closeFn != nil {
			closeFn()
		}
	}

	return handleDeploy(ctx, jobLog, appConfig,
		dataMountPoint, dockerCli, secretProvider, metadata.JobID, jobTrigger,
		repoData, deployConfigs, payload, testName, metadata)
}

func handleDeploy(ctx context.Context,
	jobLog *slog.Logger,
	appConfig *app.Config,
	dataMountPoint container.MountPoint,
	dockerCli command.Cli,
	secretProvider *secretprovider.SecretProvider,
	jobID string,
	jobTrigger stages.JobTrigger,
	repoData stages.RepositoryData,
	deployConfigs []*deployConfig.Config,
	payload *webhook.ParsedPayload,
	testName string,
	metadata notification.Metadata,
) error {
	// We'll run each deployment concurrently but grouped by repo+reference and limited by the global deployerLimiter.
	var wg sync.WaitGroup

	resultCh := make(chan error, len(deployConfigs))

	for _, deployCfg := range deployConfigs {
		deployLog := jobLog.
			WithGroup("deploy").
			With(slog.String("stack", deployCfg.Name))

		if repoData.Source != config.SourceTypeOCI {
			deployLog = deployLog.With(slog.String("reference", deployCfg.Reference))
		}

		// Used to make test deployments unique and prevent conflicts between tests when running in parallel.
		// It is not used in production.
		if testName != "" {
			deployCfg.Name = test.ConvertTestName(testName)
		}

		reconciliationHandler.startStackDeployment(repoData.Name, deployCfg.Name)

		wg.Add(1)

		go func(dc *deployConfig.Config) {
			defer wg.Done()
			defer reconciliationHandler.finishStackDeployment(repoData.Name, dc.Name)

			err := handleOneDeploy(ctx, deployLog,
				appConfig, dataMountPoint, dockerCli, secretProvider,
				dc, jobID, jobTrigger, repoData, payload, metadata)

			resultCh <- err
		}(deployCfg)
	}

	// Wait for all deployments to complete
	wg.Wait()
	close(resultCh)

	var errs []error

	for e := range resultCh {
		if e != nil {
			errs = append(errs, e)
			// keep looping to drain channel
		}
	}

	return errors.Join(errs...)
}

func handleOneDeploy(ctx context.Context, deployLog *slog.Logger,
	appConfig *app.Config, dataMountPoint container.MountPoint,
	dockerCli command.Cli,
	secretProvider *secretprovider.SecretProvider,
	dc *deployConfig.Config,
	jobID string,
	jobTrigger stages.JobTrigger,
	repoData stages.RepositoryData,
	payLad *webhook.ParsedPayload,
	metadata notification.Metadata,
) error {
	if deployerLimiter != nil {
		deployLog.Debug("queuing deployment")

		unlock, lErr := deployerLimiter.acquire(ctx, repoData.Name, NormalizeReference(dc.Reference))
		if lErr != nil {
			return lErr
		}
		defer unlock()
	}

	dockerQuiet := false
	if appConfig != nil {
		dockerQuiet = appConfig.DockerQuietDeploy
	}

	deploymentDockerCli, closeFn, err := dockerCliForContext(dockerCli, dockerQuiet, dc.Context)
	if err != nil {
		return err
	}

	if closeFn != nil {
		defer closeFn()
	}

	swarmMode, err := dockerSwarm.ResolveModeEnabled(ctx, deploymentDockerCli.Client())
	if err != nil {
		return fmt.Errorf("failed to check if docker host is running in swarm mode: %w", err)
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

func dockerCliForContext(baseCli command.Cli, quiet bool, contextName string) (command.Cli, func(), error) {
	contextName = strings.TrimSpace(contextName)
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
