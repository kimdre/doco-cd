package docker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/containerd/errdefs"
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
	if log == nil {
		log = slog.Default()
	}

	apiClient := dockerCli.Client()

	record, err := opts.Store.Load(opts.JournalID)
	if err != nil {
		return err
	}

	if !record.State.InApplierPhase() {
		if record.State.ApplierFinished() {
			return nil
		}

		return fmt.Errorf("self-update record %s is in state %q, the applier cannot resume it",
			record.ID, record.State)
	}

	if record.DriftStarted || record.Error != "" {
		return finishSelfApplyFailure(ctx, apiClient, opts.Store, record,
			pendingSelfApplyFailure(record), log)
	}

	var (
		project      *types.Project
		deployConfig *deploy.Config
		service      api.Compose
		unlockSource func()
	)

	for attempt := 0; attempt <= applierRestartRetries; attempt++ {
		err = nil
		if record.Deploy.NetworkDrift {
			err = ensureSelfApplierPreflightNetworks(ctx, apiClient, record)
		}

		if err == nil {
			project, deployConfig, service, unlockSource, err = prepareSelfApply(ctx, dockerCli, opts, record)
		}

		if err == nil {
			break
		}

		log.Warn("self-update: could not prepare applier",
			slog.Int("attempt", attempt+1), slog.Any("error", err))

		if attempt < applierRestartRetries {
			select {
			case <-ctx.Done():
				attempt = applierRestartRetries
			case <-time.After(time.Second):
			}
		}
	}

	if err != nil {
		return finishSelfApplyFailure(ctx, apiClient, opts.Store, record, err, log)
	}

	defer func() {
		if unlockSource != nil {
			unlockSource()
		}
	}()

	if err := validateSelfUpdateVolumes(ctx, apiClient, project); err != nil {
		return finishSelfApplyFailure(ctx, apiClient, opts.Store, record, err, log)
	}

	if record.Deploy.NetworkDrift {
		if record.Drift == nil {
			return finishSelfApplyFailure(ctx, apiClient, opts.Store, record,
				errors.New("network drift has no recoverable project snapshot"), log)
		}

		if _, _, _, _, err = selfDriftStartServices(project, record.Drift); err != nil {
			return finishSelfApplyFailure(ctx, apiClient, opts.Store, record,
				fmt.Errorf("select services for the network update: %w", err), log)
		}
	}

	// Other in-flight deployments may need the same source lock while the
	// predecessor drains. Release it for the wait and reacquire it before
	// Compose can read from the source or change any containers.
	unlockSource()
	unlockSource = nil

	record, err = readyAndWaitSelfApply(ctx, apiClient, opts.Store, record)
	if err != nil {
		if record.State.ApplierFinished() {
			return nil
		}

		if !record.State.InApplierPhase() {
			return err
		}

		if record.Error != "" {
			err = pendingSelfApplyFailure(record)
		}

		return finishSelfApplyFailure(ctx, apiClient, opts.Store, record, err, log)
	}

	unlockSource, err = lockSelfApplySource(record, opts)
	if err != nil {
		return finishSelfApplyFailure(ctx, apiClient, opts.Store, record,
			fmt.Errorf("relock the cached source after predecessor drain: %w", err), log)
	}

	if record.Deploy.NetworkDrift {
		// Detaching itself is a recoverable network operation. Persist rollback
		// intent first so a restart, even before Compose Create, never needs
		// service DNS to decide how to restore the previous project.
		record.DriftStarted = true
		if err := opts.Store.Save(record); err != nil {
			return finishSelfApplyFailure(ctx, apiClient, opts.Store, record,
				fmt.Errorf("record network rollback before detaching applier: %w", err), log)
		}

		if err := detachSelfApplierProjectNetworks(ctx, apiClient, record); err != nil {
			return finishSelfApplyFailure(ctx, apiClient, opts.Store, record, err, log)
		}
	}

	log.Info("self-update: applying", slog.String("stack", record.Stack), slog.String("service", record.Service))

	var applyErr error
	if record.Deploy.NetworkDrift {
		applyErr = applySelfDriftProject(ctx, dockerCli, apiClient, service, project,
			&record, opts.Store, deployConfig, opts.DataMountPath, log)
	} else {
		applyErr = applySelfService(ctx, apiClient, service, project, record, deployConfig, log)
	}

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

	return finishSelfApplyFailure(ctx, apiClient, opts.Store, record, applyErr, log)
}

// pendingSelfApplyFailure reconstructs the original failure reason for a
// restarted applier without accumulating previous rollback errors.
func pendingSelfApplyFailure(record selfupdate.Record) error {
	reason := record.Error
	if reason == "" {
		reason = "applier restarted during the network update; restoring the previous stack"
	} else {
		reason, _, _ = strings.Cut(reason, "; restore also failed: ")
	}

	return errors.New(reason)
}

// readyAndWaitSelfApply commits preflight readiness before waiting for the
// predecessor to finish draining. Only apply_drained permits container changes.
func readyAndWaitSelfApply(
	ctx context.Context,
	apiClient client.APIClient,
	store *selfupdate.Store,
	record selfupdate.Record,
) (selfupdate.Record, error) {
	for record.State == selfupdate.StateApplying {
		if record.Error != "" {
			return record, fmt.Errorf("applier recovery is pending: %s", record.Error)
		}

		if err := ctx.Err(); err != nil {
			return record, err
		}

		updated, err := store.Update(record, selfupdate.StateApplyReady, selfupdate.ActorApplier)
		if errors.Is(err, selfupdate.ErrStaleRecord) {
			record, err = store.Load(record.ID)
			if err != nil {
				return record, fmt.Errorf("reload applier readiness: %w", err)
			}

			continue
		}

		if err != nil {
			return record, fmt.Errorf("record applier readiness: %w", err)
		}

		record = updated
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	predecessorStopped := false

	for {
		current, err := store.Load(record.ID)
		if err != nil {
			return record, fmt.Errorf("check predecessor drain: %w", err)
		}

		record = current
		if record.Error != "" {
			return record, fmt.Errorf("applier recovery is pending: %s", record.Error)
		}

		if record.State == selfupdate.StateApplyDrained {
			return record, nil
		}

		if record.State != selfupdate.StateApplyReady {
			return record, fmt.Errorf("predecessor drain stopped in state %s", record.State)
		}

		if predecessorStopped {
			return record, errors.New("predecessor stopped before recording applier drain")
		}

		previous, err := apiClient.ContainerInspect(ctx, record.Predecessor.ID, client.ContainerInspectOptions{})
		if errdefs.IsNotFound(err) || (err == nil && (previous.Container.State == nil || !previous.Container.State.Running)) {
			// The predecessor may have recorded its drain and stopped after
			// the load above. Re-read the journal before treating it as lost.
			predecessorStopped = true
			continue
		}

		select {
		case <-ctx.Done():
			return record, ctx.Err()
		case <-ticker.C:
		}
	}
}

// lockSelfApplySource holds the cached repository stable across applier retries.
func lockSelfApplySource(record selfupdate.Record, opts ApplySelfOptions) (func(), error) {
	ref, err := composeScheduledServiceRefFromLabels(record.Labels)
	if err != nil {
		return nil, err
	}

	sourceRepoPath, err := resolveScheduledSourceRepoPath(ref, opts.Scheduled.ComposeLoad.DataMountPath)
	if err != nil {
		return nil, err
	}

	return lockScheduledSource(ref, opts.Scheduled.ComposeLoad.DataMountPath, sourceRepoPath)
}

// FailSelfUpdate recovers from applier setup errors before ApplySelfUpdate.
// Its caller must exit nonzero on recovery failure so Docker retries.
func FailSelfUpdate(
	ctx context.Context,
	dockerCli command.Cli,
	store *selfupdate.Store,
	journalID string,
	cause error,
	log *slog.Logger,
) error {
	if cause == nil {
		return errors.New("self-update applier failure has no cause")
	}

	record, err := store.Load(journalID)
	if err != nil {
		return errors.Join(cause, fmt.Errorf("load self-update record for recovery: %w", err))
	}

	if !record.State.InApplierPhase() {
		if record.State.ApplierFinished() {
			return nil
		}

		return errors.Join(cause, fmt.Errorf("cannot recover self-update in state %s", record.State))
	}

	if log == nil {
		log = slog.Default()
	}

	return finishSelfApplyFailure(ctx, dockerCli.Client(), store, record, cause, log)
}

// prepareSelfApply locks and loads the desired stack before the applier
// announces readiness to the predecessor.
func prepareSelfApply(
	ctx context.Context,
	dockerCli command.Cli,
	opts ApplySelfOptions,
	record selfupdate.Record,
) (*types.Project, *deploy.Config, api.Compose, func(), error) {
	ref, err := composeScheduledServiceRefFromLabels(record.Labels)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("rebuild the compose reference for the self stack: %w", err)
	}

	// Take the same source lock as the poll path and release it on every retry.
	sourceRepoPath, err := resolveScheduledSourceRepoPath(ref, opts.Scheduled.ComposeLoad.DataMountPath)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("resolve the cached source for the self stack: %w", err)
	}

	unlockSource, err := lockScheduledSource(ref, opts.Scheduled.ComposeLoad.DataMountPath, sourceRepoPath)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("lock the cached source for the self stack: %w", err)
	}

	release := true
	defer func() {
		if release {
			unlockSource()
		}
	}()

	project, deployConfig, err := loadComposeScheduledProjectAll(ctx, dockerCli, ref, opts.SecretProvider, opts.Scheduled)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("load the self stack: %w", err)
	}

	if !record.Deploy.NetworkDrift {
		project, err = project.WithSelectedServices([]string{record.Service}, types.IgnoreDependencies)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("select the self service: %w", err)
		}
	}

	payload := selfApplierPayload(record)
	timestamp := time.Now().UTC().Format(time.RFC3339)
	addComposeServiceLabels(project, deployConfig, payload, record.Source.SourceURL,
		ref.WorkingDir, app.Version, timestamp, ComposeVersion, record.Source.CommitSHA, record.Source.ProjectHash)

	if record.Deploy.NetworkDrift {
		addComposeVolumeLabels(project, deployConfig, payload, record.Source.SourceURL,
			app.Version, timestamp, ComposeVersion, record.Source.CommitSHA, record.Source.ProjectHash)
	}

	service, err := compose.NewComposeService(dockerCli)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("initialise Compose for self-update: %w", err)
	}

	release = false

	return project, deployConfig, service, unlockSource, nil
}

// finishSelfApplyFailure retries rollback against fresh journal state if
// another process advances the handover during recovery.
func finishSelfApplyFailure(
	ctx context.Context,
	apiClient client.APIClient,
	store *selfupdate.Store,
	record selfupdate.Record,
	applyErr error,
	log *slog.Logger,
) error {
	for attempt := 0; attempt <= applierRestartRetries; attempt++ {
		err := finishSelfApplyFailureOnce(ctx, apiClient, store, record, applyErr, log)
		if !errors.Is(err, selfupdate.ErrStaleRecord) {
			return err
		}

		log.Warn("self-update: journal changed during recovery; retrying with the latest state",
			slog.Int("attempt", attempt+1), slog.Any("error", err))
	}

	return fmt.Errorf("self-update recovery journal continued to change: %w", selfupdate.ErrStaleRecord)
}

// finishSelfApplyFailureOnce records the failure and restores the predecessor
// from the latest journal state.
func finishSelfApplyFailureOnce(
	ctx context.Context,
	apiClient client.APIClient,
	store *selfupdate.Store,
	record selfupdate.Record,
	applyErr error,
	log *slog.Logger,
) error {
	log.Error("self-update: apply failed, restoring the previous stack", slog.Any("error", applyErr))

	latest, err := store.Load(record.ID)
	if err != nil {
		return errors.Join(applyErr, fmt.Errorf("reload self-update record before recovery: %w", err))
	}

	if latest.State.ApplierFinished() {
		return nil
	}

	if !latest.State.InApplierPhase() {
		return fmt.Errorf("cannot recover self-update after it entered state %s: %w", latest.State, applyErr)
	}

	if latest.Error != "" {
		applyErr = pendingSelfApplyFailure(latest)
	}

	record = latest
	record.Error = applyErr.Error()

	state := selfupdate.StateFailed

	var restoreErr error

	if record.DriftStarted {
		for attempt := 0; attempt <= applierRestartRetries; attempt++ {
			record.Restored, restoreErr = restoreSelfDriftProject(ctx, apiClient, record, log)
			if restoreErr == nil {
				state = selfupdate.StateRolledBack
				break
			}

			log.Warn("self-update: network rollback did not complete",
				slog.Int("attempt", attempt+1), slog.Any("error", restoreErr))

			if attempt < applierRestartRetries {
				time.Sleep(time.Second)
			}
		}
	} else {
		current, err := apiClient.ContainerInspect(context.WithoutCancel(ctx), record.Predecessor.ID, client.ContainerInspectOptions{})
		if err == nil && current.Container.State != nil && current.Container.State.Running {
			record.Restored = record.Predecessor
		} else {
			record.Restored, restoreErr = restoreSelfPredecessor(ctx, apiClient, store, record, log)
			if restoreErr == nil {
				state = selfupdate.StateRolledBack
			}
		}
	}

	if restoreErr != nil {
		record.Error = fmt.Sprintf("%s; restore also failed: %v", applyErr, restoreErr)
		// A stopped predecessor cannot restart itself after an API stop.
		// Keep the in-progress journal and fail the applier process so Docker
		// retries recovery rather than abandoning the last living actor.
		if err := store.Save(record); err != nil {
			return errors.Join(restoreErr, fmt.Errorf("save failed self-update recovery: %w", err))
		}

		return fmt.Errorf("self-update recovery is incomplete: %w", restoreErr)
	}

	if err := store.Save(record); err != nil {
		return err
	}

	if _, err := store.Update(record, state, selfupdate.ActorApplier); err != nil {
		return err
	}

	if state == selfupdate.StateRolledBack {
		log.Info("self-update: rolled back", slog.String("stack", record.Stack))

		return nil
	}

	// The failure is durable and the predecessor finalises/poisons it on
	// recovery. Exiting successfully avoids Docker re-running a terminal
	// record and leaving an exited clone stuck in a restart loop.
	return nil
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
	if err := validateSelfUpdateVolumes(ctx, apiClient, project); err != nil {
		return err
	}

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
