package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"

	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/reconciliation"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/selfupdate"
	"github.com/kimdre/doco-cd/internal/source"
)

// ErrApplySelfUsage is returned for a malformed apply-self invocation.
var ErrApplySelfUsage = errors.New("usage: doco-cd apply-self <journal-id> | doco-cd apply-self --bootstrap")

// parseApplySelfArgs splits the two apply-self modes.
func parseApplySelfArgs(args []string) (bootstrap bool, journalID string, err error) {
	if len(args) != 1 {
		return false, "", ErrApplySelfUsage
	}

	if args[0] == "--bootstrap" {
		return true, "", nil
	}

	if args[0] == "" || args[0][0] == '-' {
		return false, "", ErrApplySelfUsage
	}

	return false, args[0], nil
}

// runApplySelf is the entry point of the throwaway clone that finishes a
// self-update, and of the one-shot bootstrap that first adopts doco-cd into
// GitOps. Neither mode serves HTTP or starts the scheduler.
func runApplySelf(ctx context.Context, log *logger.Logger, c *app.Config, args []string) error {
	bootstrap, journalID, err := parseApplySelfArgs(args)
	if err != nil {
		log.Log(ctx, logger.LevelCritical, "invalid apply-self arguments", logger.ErrAttr(err))

		return err
	}

	dockerCli, err := docker.CreateDockerCli(c.DockerQuietDeploy)
	if err != nil {
		log.Log(ctx, logger.LevelCritical, "failed to create docker client", logger.ErrAttr(err))

		return err
	}

	defer func() {
		_ = dockerCli.Client().Close()
	}()

	if bootstrap {
		return runSelfBootstrap(ctx, log, c, dockerCli)
	}

	failBeforeApply := func(cause error) error {
		log.Error("self-update: applier initialization failed", logger.ErrAttr(cause))

		return docker.FailSelfUpdate(ctx, dockerCli, selfupdate.NewStore(c.DataMountPath), journalID, cause, log.Logger)
	}

	dataMountPoint, err := bootstrapDataMountPoint(ctx, c, dockerCli)
	if err != nil {
		return failBeforeApply(err)
	}

	// Compose files are resolved by their host path, which only resolves inside
	// the container through this symlink.
	if err = CreateMountpointSymlink(dataMountPoint); err != nil {
		return failBeforeApply(fmt.Errorf("create the data mount symlink: %w", err))
	}

	secretProvider, err := secretprovider.Initialize(ctx, c.SecretProvider, app.Version)
	if err != nil {
		return failBeforeApply(fmt.Errorf("initialize the secret provider: %w", err))
	}

	if secretProvider != nil {
		defer secretProvider.Close()
	}

	log.Info("self-update: applier started", slog.String("journal_id", journalID))

	err = docker.ApplySelfUpdate(ctx, dockerCli, docker.ApplySelfOptions{
		Store:          selfupdate.NewStore(c.DataMountPath),
		JournalID:      journalID,
		SecretProvider: secretProvider,
		Scheduled:      docker.NewScheduledComposeOptions(c),
		Log:            log.Logger,
		DataMountPath:  c.DataMountPath,
	})
	if err != nil {
		log.Error("self-update: applier failed", logger.ErrAttr(err))

		return err
	}

	return nil
}

// runSelfBootstrap deploys every configured poll target once, then exits. It is
// how the first doco-cd container is created with correct compose and doco-cd
// labels, so later deployments recognise the stack as their own.
func runSelfBootstrap(ctx context.Context, log *logger.Logger, c *app.Config, dockerCli command.Cli) error {
	if len(c.PollConfig) == 0 {
		return errors.New("bootstrap needs POLL_CONFIG to know what to deploy")
	}

	notifier, err := notification.New(notification.Config{
		APIURL:                string(c.AppriseApiURL),
		NotifyURLs:            c.AppriseNotifyUrls,
		NotifyLevel:           c.AppriseNotifyLevel,
		BodyTemplate:          c.AppriseNotifyBodyTemplate,
		FailureRepeatInterval: c.AppriseNotifyRepeatInterval,
	})
	if err != nil {
		return fmt.Errorf("create the notifier: %w", err)
	}

	contexts := docker.NewContextRegistry(dockerCli, docker.ContextRegistryOptions{
		Quiet:         c.DockerQuietDeploy,
		SwarmFeatures: c.DockerSwarmFeatures,
	})

	secretProvider, err := secretprovider.Initialize(ctx, c.SecretProvider, app.Version)
	if err != nil {
		return fmt.Errorf("initialize the secret provider: %w", err)
	}

	if secretProvider != nil {
		defer secretProvider.Close()
	}

	dataMountPoint, err := bootstrapDataMountPoint(ctx, c, dockerCli)
	if err != nil {
		return err
	}

	// Compose files are resolved by their host path, which only resolves inside
	// the container through this symlink.
	if err = CreateMountpointSymlink(dataMountPoint); err != nil {
		return fmt.Errorf("create the data mount symlink: %w", err)
	}

	reconciliationManager, err := reconciliation.NewManager(reconciliation.Dependencies{
		AppConfig:                c,
		DataMountPoint:           dataMountPoint,
		DockerCLI:                dockerCli,
		Contexts:                 contexts,
		SecretProvider:           secretProvider,
		Notifier:                 notifier,
		MaxConcurrentDeployments: c.MaxConcurrentDeployments,
	})
	if err != nil {
		return fmt.Errorf("create the reconciliation manager: %w", err)
	}

	defer reconciliationManager.Close()

	sourcePreparer, err := source.NewPreparer(source.Dependencies{AppConfig: c})
	if err != nil {
		return fmt.Errorf("create the source preparer: %w", err)
	}

	deployment, err := controlplane.NewDeployment(controlplane.DeploymentDependencies{
		SourcePreparer: sourcePreparer,
		Reconciler:     reconciliationManager,
		Contexts:       contexts,
		DataMountPoint: dataMountPoint,
	})
	if err != nil {
		return fmt.Errorf("create the deployment operation: %w", err)
	}

	for _, pollConfig := range c.PollConfig {
		metadata := notification.Metadata{}

		if err = RunPoll(ctx, pollConfig, c, log.Logger, metadata, "bootstrap", deployment, notifier); err != nil {
			log.Error("self-update: bootstrap deployment failed", logger.ErrAttr(err))

			return err
		}
	}

	log.Info("self-update: bootstrap completed")

	return nil
}

// bootstrapDataMountPoint resolves the data volume the way the main process
// does, so a bootstrap run writes to the same place the long-lived instance
// will read from.
func bootstrapDataMountPoint(ctx context.Context, c *app.Config, dockerCli command.Cli) (container.MountPoint, error) {
	return resolveDataMountPoint(
		c.DataHostPath,
		c.DataMountPath,
		func() (container.MountPoint, error) {
			return detectDataMountPoint(
				c.DataMountPath,
				getAppContainerID,
				func(containerID, destination string) (container.MountPoint, error) {
					return docker.GetMountPointByDestination(ctx, dockerCli.Client(), containerID, destination)
				},
			)
		},
	)
}
