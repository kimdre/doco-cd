package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/selfupdate"
)

// handoverGrace is how long the predecessor waits to be stopped before it
// checks whether the successor is still alive.
const handoverGrace = 2 * time.Minute

// handoverExitCode is a self-exit, so Docker's restart policy brings a clean
// predecessor back when a handover could not complete.
const handoverExitCode = 1

type selfUpdateCoordinatorDeps struct {
	appConfig *app.Config
	client    client.APIClient
	store     *selfupdate.Store
	identity  selfupdate.Identity
	runs      *controlplane.Runs
	notifier  *notification.Notifier
	stopWork  context.CancelFunc
}

// runSelfUpdateCoordinator handles recovery requests without draining and
// waits for applier preflight before closing admission for a handover.
func runSelfUpdateCoordinator(ctx context.Context, log *logger.Logger, deps selfUpdateCoordinatorDeps) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-selfupdate.DrainRequests():
		}

		if handleDrainRequest(ctx, log, deps) {
			return
		}
	}
}

// handleDrainRequest converges the active handover for one drain request. It
// returns true when the coordinator must stop: on shutdown or once this
// process drained. Failures after draining exit the process instead.
func handleDrainRequest(ctx context.Context, log *logger.Logger, deps selfUpdateCoordinatorDeps) bool {
	record, err := deps.store.Active()
	if err != nil {
		log.Error("self-update: cannot read the handover record", logger.ErrAttr(err))

		return !retrySelfUpdateRecovery(ctx)
	}

	if record == nil {
		return false
	}

	if record.State == selfupdate.StateStaged || record.State.Unsuccessful() {
		return recoverOrRetry(ctx, log, deps, *record, "self-update: failed to recover handover; retrying")
	}

	if record.Strategy != selfupdate.StrategyScaleOut && record.Strategy != selfupdate.StrategyApplier {
		log.Error("self-update: refusing to drain for an unknown handover strategy",
			slog.String("strategy", string(record.Strategy)), slog.String("id", record.ID))

		return false
	}

	if record.Strategy == selfupdate.StrategyApplier &&
		(record.State == selfupdate.StateApplying || record.State == selfupdate.StateApplyReady) {
		current, waitErr := waitForApplierReady(ctx, log, deps.client, deps.store, *record)
		if waitErr != nil {
			if ctx.Err() != nil {
				return true
			}

			log.Error("self-update: cannot wait for applier preflight", logger.ErrAttr(waitErr))

			return !retrySelfUpdateRecovery(ctx)
		}

		if current.State.RolledBackOrFailed() {
			return recoverOrRetry(ctx, log, deps, current, "self-update: failed to recover applier; retrying")
		}

		record = &current
	}

	if record.Strategy == selfupdate.StrategyScaleOut &&
		record.State != selfupdate.StateHandover && record.State != selfupdate.StateDrained {
		log.Error("self-update: scale-out handover is not ready to drain", slog.String("state", string(record.State)))

		return false
	}

	log.Info("self-update: draining", slog.String("id", record.ID))

	// Stop lifecycle work first; an unrelated in-flight deployment must
	// finish before either strategy can modify this container.
	deps.stopWork()
	deps.runs.Drain()

	current, err := deps.store.Load(record.ID)
	if err != nil {
		log.Error("self-update: cannot confirm handover after draining", logger.ErrAttr(err))
		os.Exit(handoverExitCode)
	}

	if current.State.Unsuccessful() {
		recoverAndExit(ctx, log, deps, current, "self-update: failed to recover after draining")
	}

	if current.Strategy == selfupdate.StrategyApplier && current.State == selfupdate.StateApplyReady {
		if current.Error != "" {
			log.Warn("self-update: applier is recovering; restarting to restore admission",
				slog.String("id", current.ID), slog.String("reason", current.Error))
			os.Exit(handoverExitCode)
		}

		current = acknowledgeApplierDrain(ctx, log, deps, current)
	}

	if current.Strategy == selfupdate.StrategyScaleOut &&
		current.State != selfupdate.StateHandover && current.State != selfupdate.StateDrained {
		log.Error("self-update: scale-out handover changed while draining", slog.String("state", string(current.State)))
		os.Exit(handoverExitCode)
	}

	log.Info("self-update: drained",
		slog.String("id", current.ID),
		slog.String("strategy", string(current.Strategy)),
	)

	switch current.Strategy {
	case selfupdate.StrategyScaleOut:
		waitToBeReplaced(ctx, log, deps, current)
	case selfupdate.StrategyApplier:
		if current.State != selfupdate.StateApplyDrained {
			log.Error("self-update: applier handover lost its drain state", slog.String("state", string(current.State)))
			os.Exit(handoverExitCode)
		}

		selfupdate.MaybeCrash(deps.appConfig.DataMountPath, string(selfupdate.StateApplyDrained), log.Logger)
		waitForApplierHandover(ctx, log, deps, current)
	}

	return true
}

// recoverOrRetry reports and cleans up an unsuccessful handover while this
// process still serves, scheduling another attempt on failure. It returns true
// on shutdown.
func recoverOrRetry(ctx context.Context, log *logger.Logger, deps selfUpdateCoordinatorDeps, record selfupdate.Record, msg string) bool {
	if err := finalizeAsPredecessor(ctx, log, deps.client, deps.notifier, deps.store, record); err != nil {
		log.Error(msg, logger.ErrAttr(err))

		return !retrySelfUpdateRecovery(ctx)
	}

	return false
}

// recoverAndExit reports and cleans up an unsuccessful handover, then exits:
// a drained process cannot reopen admission, so the restart policy brings a
// clean one back.
func recoverAndExit(ctx context.Context, log *logger.Logger, deps selfUpdateCoordinatorDeps, record selfupdate.Record, msg string) {
	if err := finalizeAsPredecessor(ctx, log, deps.client, deps.notifier, deps.store, record); err != nil {
		log.Error(msg, logger.ErrAttr(err))
	}

	os.Exit(handoverExitCode)
}

// acknowledgeApplierDrain durably records that this process drained, which
// permits the applier to change containers. On failure it exits, recovering
// first if the applier already gave up.
func acknowledgeApplierDrain(ctx context.Context, log *logger.Logger, deps selfUpdateCoordinatorDeps, record selfupdate.Record) selfupdate.Record {
	updated, err := deps.store.Update(record, selfupdate.StateApplyDrained, selfupdate.ActorPredecessor)
	if err == nil {
		return updated
	}

	log.Error("self-update: cannot acknowledge applier drain", logger.ErrAttr(err))

	if errors.Is(err, selfupdate.ErrStaleRecord) {
		latest, loadErr := deps.store.Load(record.ID)

		switch {
		case loadErr != nil:
			log.Error("self-update: cannot reload the updated handover", logger.ErrAttr(loadErr))
		case latest.State.Unsuccessful():
			recoverAndExit(ctx, log, deps, latest, "self-update: failed to recover the updated handover")
		}
	}

	os.Exit(handoverExitCode)

	return record
}

// retrySelfUpdateRecovery schedules another attempt unless shutdown was requested.
func retrySelfUpdateRecovery(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(time.Second):
		selfupdate.RequestDrain()
		return true
	}
}

// waitForApplierReady keeps the predecessor serving until preflight completes
// or the applier reaches a terminal state.
func waitForApplierReady(
	ctx context.Context, log *logger.Logger, apiClient client.APIClient, store *selfupdate.Store, record selfupdate.Record,
) (selfupdate.Record, error) {
	var current selfupdate.Record

	err := pollUntil(ctx, func() {
		log.Warn("self-update: still waiting for applier preflight", slog.String("id", record.ID))
	}, func() (bool, error) {
		var err error

		current, err = store.Load(record.ID)
		if err != nil {
			return false, fmt.Errorf("read applier readiness: %w", err)
		}

		if current.State == selfupdate.StateApplying || current.State == selfupdate.StateApplyReady {
			current, err = failStoppedSelfApplier(ctx, apiClient, store, current)
			if err != nil {
				return false, err
			}
		}

		switch current.State {
		case selfupdate.StateApplyReady:
			return current.Error == "", nil
		case selfupdate.StateApplyDrained, selfupdate.StateFailed, selfupdate.StateRolledBack:
			return true, nil
		case selfupdate.StateApplying:
			return false, nil
		default:
			return false, fmt.Errorf("unexpected state while waiting for applier readiness: %s", current.State)
		}
	})

	return current, err
}

// waitToBeReplaced marks the record drained and blocks until the successor
// stops this container. It gives up only if the successor disappeared.
func waitToBeReplaced(ctx context.Context, log *logger.Logger, deps selfUpdateCoordinatorDeps, record selfupdate.Record) {
	if record.State == selfupdate.StateHandover {
		updated, err := deps.store.Update(record, selfupdate.StateDrained, selfupdate.ActorPredecessor)
		if err != nil {
			log.Error("self-update: failed to mark the handover drained", logger.ErrAttr(err))
			os.Exit(handoverExitCode)
		}

		record = updated
	}

	selfupdate.MaybeCrash(deps.appConfig.DataMountPath, string(selfupdate.StateDrained), log.Logger)

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(handoverGrace):
		}

		result, err := deps.client.ContainerInspect(ctx, record.Successor.ID, client.ContainerInspectOptions{})
		if err != nil && !errdefs.IsNotFound(err) {
			log.Warn("self-update: could not inspect the successor; retaining the handover",
				slog.String("successor_id", record.Successor.ID), logger.ErrAttr(err))

			continue
		}

		if errdefs.IsNotFound(err) || result.Container.State == nil || !result.Container.State.Running {
			log.Error("self-update: the successor is gone, aborting the handover",
				slog.String("successor_id", record.Successor.ID))

			abortHandover(ctx, log, deps, record)

			return
		}

		log.Warn("self-update: still waiting to be replaced by the successor",
			slog.String("successor_id", record.Successor.ID))
	}
}

// abortHandover removes a dead successor and restarts this container, which
// comes back with admission open.
func abortHandover(ctx context.Context, log *logger.Logger, deps selfUpdateCoordinatorDeps, record selfupdate.Record) {
	removeCtx := context.WithoutCancel(ctx)

	if record.Successor.ID != "" {
		if _, err := deps.client.ContainerRemove(removeCtx, record.Successor.ID, client.ContainerRemoveOptions{Force: true}); err != nil &&
			!errdefs.IsNotFound(err) {
			log.Error("self-update: failed to remove the dead successor; retaining the handover for recovery", logger.ErrAttr(err))
			os.Exit(handoverExitCode)
		}
	}

	record.Error = "the successor stopped before it could take over"

	if err := deps.store.Save(record); err != nil {
		log.Warn("self-update: failed to save the abort reason", logger.ErrAttr(err))
	}

	if _, err := deps.store.Update(record, selfupdate.StateAborted, selfupdate.ActorPredecessor); err != nil {
		log.Warn("self-update: failed to mark the handover aborted", logger.ErrAttr(err))
	}

	// A drained process cannot re-open admission, so exit and let the restart
	// policy bring a clean one back. The finaliser reports on the next boot.
	os.Exit(handoverExitCode)
}

// waitForApplierHandover blocks while the applier replaces this container. The
// process normally dies inside this wait.
func waitForApplierHandover(ctx context.Context, log *logger.Logger, deps selfUpdateCoordinatorDeps, record selfupdate.Record) {
	if record.Applier.ID == "" {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}

		current, err := deps.store.Load(record.ID)
		if err != nil {
			log.Error("self-update: cannot confirm applier handover, exiting for recovery", logger.ErrAttr(err))
			os.Exit(handoverExitCode)
		}

		if current.State.RolledBackOrFailed() {
			recoverAndExit(ctx, log, deps, current, "self-update: failed to clean up the applier")
		}

		result, err := deps.client.ContainerInspect(ctx, record.Applier.ID, client.ContainerInspectOptions{})
		if err != nil {
			log.Warn("self-update: the applier is gone, exiting so a clean instance can recover",
				slog.String("applier_id", record.Applier.ID))
			os.Exit(handoverExitCode)
		}

		if result.Container.State == nil {
			log.Error("self-update: the applier has no container state, exiting for recovery",
				slog.String("applier_id", record.Applier.ID))
			os.Exit(handoverExitCode)
		}

		if applierFinished(result.Container.State) {
			log.Warn("self-update: the applier exited without replacing this container, exiting for recovery",
				slog.String("applier_id", record.Applier.ID),
				slog.Int("exit_code", result.Container.State.ExitCode))

			os.Exit(handoverExitCode)
		}
	}
}
