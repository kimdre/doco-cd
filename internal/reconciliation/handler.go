package reconciliation

import (
	"context"
	"errors"
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

func HandleDeploys(ctx context.Context,
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

		wg.Add(1)

		go func(dc *config.DeployConfig) {
			defer wg.Done()

			err := handleOneDeploy(ctx, deployLog,
				appConfig, dataMountPoint, dockerCli, secretProvider,
				dc, jobID, jobTrigger, repoData, payload)

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
			Client:         dockerCli.Client(),
			DataMountPoint: dataMountPoint,
		},
		payLad,
		appConfig,
		dc,
		secretProvider,
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
