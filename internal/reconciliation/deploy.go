package reconciliation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/stages"
	"github.com/kimdre/doco-cd/internal/test"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func Deploy(ctx context.Context,
	jobLog *slog.Logger,
	appConfig *config.AppConfig,
	dataMountPoint container.MountPoint,
	dockerCli command.Cli,
	secretProvider *secretprovider.SecretProvider,
	metadata notification.Metadata,
	jobTrigger stages.JobTrigger,
	repoData stages.RepositoryData,
	deployConfigs []*config.DeployConfig,
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
	appConfig *config.AppConfig,
	dataMountPoint container.MountPoint,
	dockerCli command.Cli,
	secretProvider *secretprovider.SecretProvider,
	metadata notification.Metadata,
	jobTrigger stages.JobTrigger,
	repoData stages.RepositoryData,
	deployConfigs []*config.DeployConfig,
	payload *webhook.ParsedPayload,
	testName string,
) error {
	if err := cleanupObsoleteAutoDiscoveredContainers(ctx, jobLog,
		dockerCli, string(repoData.CloneURL),
		deployConfigs,
		metadata); err != nil {
		return fmt.Errorf("failed to clean up obsolete auto-discovered containers: %w", err)
	}

	return handleDeploy(ctx, jobLog, appConfig,
		dataMountPoint, dockerCli, secretProvider, metadata.JobID, jobTrigger,
		repoData, deployConfigs, payload, testName, metadata)
}

func handleDeploy(ctx context.Context,
	jobLog *slog.Logger,
	appConfig *config.AppConfig,
	dataMountPoint container.MountPoint,
	dockerCli command.Cli,
	secretProvider *secretprovider.SecretProvider,
	jobID string,
	jobTrigger stages.JobTrigger,
	repoData stages.RepositoryData,
	deployConfigs []*config.DeployConfig,
	payload *webhook.ParsedPayload,
	testName string,
	metadata notification.Metadata,
) error {
	// We'll run each deployment concurrently but grouped by repo+reference and limited by the global deployerLimiter.
	var wg sync.WaitGroup

	resultCh := make(chan error, len(deployConfigs))

	for _, deployConfig := range deployConfigs {
		deployLog := jobLog.
			WithGroup("deploy").
			With(
				slog.String("stack", deployConfig.Name),
				slog.String("reference", deployConfig.Reference))

		// Used to make test deployments unique and prevent conflicts between tests when running in parallel.
		// It is not used in production.
		if testName != "" {
			deployConfig.Name = test.ConvertTestName(testName)
		}

		reconciliationHandler.startStackDeployment(repoData.Name, deployConfig.Name)

		wg.Add(1)

		go func(dc *config.DeployConfig) {
			defer wg.Done()
			defer reconciliationHandler.finishStackDeployment(repoData.Name, dc.Name)

			err := handleOneDeploy(ctx, deployLog,
				appConfig, dataMountPoint, dockerCli, secretProvider,
				dc, jobID, jobTrigger, repoData, payload, metadata)

			resultCh <- err
		}(deployConfig)
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
	appConfig *config.AppConfig, dataMountPoint container.MountPoint,
	dockerCli command.Cli,
	secretProvider *secretprovider.SecretProvider,
	dc *config.DeployConfig,
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

	stageMgr := stages.NewStageManager(
		jobID,
		jobTrigger,
		deployLog,
		failNotifyFunc,
		&repoData,
		&stages.Docker{
			Cmd:            dockerCli,
			DataMountPoint: dataMountPoint,
		},
		payLad,
		appConfig,
		dc,
		secretProvider,
		metadata,
	)

	err := stageMgr.RunStages(ctx)
	if err != nil {
		return err
	}

	return nil
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
