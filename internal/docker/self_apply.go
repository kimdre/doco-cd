package docker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/selfupdate"
	"github.com/kimdre/doco-cd/internal/webhook"
)

// ApplySelfOptions configures a single run of the self-update applier.
type ApplySelfOptions struct {
	Store          *selfupdate.Store
	JournalID      string
	SecretProvider secretprovider.SecretProvider
	Scheduled      ScheduledComposeOptions
	Log            *slog.Logger
	DataMountPath  string
}

// ApplySelfUpdate replaces the doco-cd container named by the journal record.
// It runs inside a throwaway clone, so it survives the predecessor being
// stopped halfway through its own recreate.
func ApplySelfUpdate(ctx context.Context, dockerCli command.Cli, opts ApplySelfOptions) error {
	log := opts.Log
	apiClient := dockerCli.Client()

	record, err := opts.Store.Load(opts.JournalID)
	if err != nil {
		return err
	}

	if record.State != selfupdate.StateApplying {
		return fmt.Errorf("self-update record %s is in state %q, the applier only runs for %q",
			record.ID, record.State, selfupdate.StateApplying)
	}

	ref, err := composeScheduledServiceRefFromLabels(record.Labels)
	if err != nil {
		return fmt.Errorf("rebuild the compose reference for the self stack: %w", err)
	}

	// Take the same locks the poll path and the managed-recreate path take, so
	// the reload cannot race a concurrent Prepare or a GC sweep of the artifact.
	sourceRepoPath, err := resolveScheduledSourceRepoPath(ref, opts.Scheduled.ComposeLoad.DataMountPath)
	if err != nil {
		return fmt.Errorf("resolve the cached source for the self stack: %w", err)
	}

	unlockSource, err := lockScheduledSource(ref, opts.Scheduled.ComposeLoad.DataMountPath, sourceRepoPath)
	if err != nil {
		return fmt.Errorf("lock the cached source for the self stack: %w", err)
	}
	defer unlockSource()

	project, deployConfig, err := loadComposeScheduledProjectAll(ctx, dockerCli, ref, opts.SecretProvider, opts.Scheduled)
	if err != nil {
		return fmt.Errorf("load the self stack: %w", err)
	}

	selfOnly, err := project.WithSelectedServices([]string{record.Service}, types.IgnoreDependencies)
	if err != nil {
		return fmt.Errorf("select the self service: %w", err)
	}

	// Without these the successor gets no compose or doco-cd labels, so nothing
	// would recognise it as the deployed stack afterwards.
	addComposeServiceLabels(
		selfOnly,
		deployConfig,
		selfApplierPayload(record),
		record.Source.SourceURL,
		ref.WorkingDir,
		app.Version,
		time.Now().UTC().Format(time.RFC3339),
		ComposeVersion,
		record.Source.CommitSHA,
		record.Source.ProjectHash,
	)

	service, err := compose.NewComposeService(dockerCli)
	if err != nil {
		return err
	}

	log.Info("self-update: applying", slog.String("stack", record.Stack), slog.String("service", record.Service))

	applyErr := applySelfService(ctx, apiClient, service, selfOnly, record, deployConfig, log)
	if applyErr == nil {
		record.Error = ""
		if err = opts.Store.Save(record); err != nil {
			return err
		}

		if _, err = opts.Store.Update(record, selfupdate.StateApplied, selfupdate.ActorApplier); err != nil {
			return err
		}

		log.Info("self-update: applied", slog.String("stack", record.Stack))

		selfupdate.MaybeCrash(opts.DataMountPath, string(selfupdate.StateApplied), log)

		return nil
	}

	log.Error("self-update: apply failed, restoring the previous container", slog.Any("error", applyErr))

	record.Error = applyErr.Error()

	restored, restoreErr := restoreSelfPredecessor(ctx, apiClient, opts.Store, record, log)

	state := selfupdate.StateRolledBack
	if restoreErr != nil {
		state = selfupdate.StateFailed
		record.Error = fmt.Sprintf("%s; restore also failed: %v", applyErr, restoreErr)
	} else {
		record.Restored = restored
	}

	if err = opts.Store.Save(record); err != nil {
		return err
	}

	if _, err = opts.Store.Update(record, state, selfupdate.ActorApplier); err != nil {
		return err
	}

	if state == selfupdate.StateRolledBack {
		log.Info("self-update: rolled back", slog.String("stack", record.Stack))

		return nil
	}

	return fmt.Errorf("self-update failed and could not be rolled back: %w", applyErr)
}

// applySelfService recreates the doco-cd service and waits for it to be healthy.
func applySelfService(
	ctx context.Context,
	apiClient client.APIClient,
	service api.Compose,
	project *types.Project,
	record selfupdate.Record,
	deployConfig *deploy.Config,
	log *slog.Logger,
) error {
	err := service.Create(ctx, project, api.CreateOptions{
		Services:             []string{record.Service},
		Recreate:             api.RecreateForce,
		RecreateDependencies: api.RecreateNever,
		IgnoreOrphans:        true,
		QuietPull:            true,
	})
	if err != nil {
		return fmt.Errorf("recreate the self service: %w", err)
	}

	successor, err := findSelfSuccessor(ctx, apiClient, &selfTarget{
		Project: record.Stack,
		Service: record.Service,
		Context: record.Context,
	}, record.Predecessor.ID)
	if err != nil {
		return err
	}

	if _, err = apiClient.ContainerStart(ctx, successor.ID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start the new container: %w", err)
	}

	timeout := time.Duration(record.Deploy.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = defaultSelfHealthTimeout
	}

	if deployConfig != nil && deployConfig.Timeout > 0 {
		timeout = time.Duration(deployConfig.Timeout) * time.Second
	}

	return selfupdate.WaitHealthy(ctx, apiClient, successor.ID, timeout, log)
}

// resolveScheduledSourceRepoPath exposes the cached source path for a service
// reference, so the applier locks exactly what the poll path locks.
func resolveScheduledSourceRepoPath(ref composeScheduledServiceRef, dataMountPath string) (string, error) {
	path, _, err := resolveScheduledSourceRepo(ref, dataMountPath)

	return path, err
}

// selfApplierPayload rebuilds the deploy payload from the journal, so the
// successor's labels match what the predecessor would have written.
func selfApplierPayload(record selfupdate.Record) *webhook.ParsedPayload {
	return &webhook.ParsedPayload{
		Source:   webhook.PayloadSource(SourceTypeLabelValue(record.Source.SourceType, record.Labels[DocoCDLabels.Source.Type])),
		Trigger:  record.Source.Trigger,
		Name:     record.Source.RepoName,
		FullName: record.Source.FullName,
		WebURL:   record.Source.SourceURL,
	}
}
