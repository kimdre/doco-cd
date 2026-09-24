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

		record, err := deps.store.Active()
		if err != nil {
			log.Error("self-update: cannot read the handover record", logger.ErrAttr(err))

			if !retrySelfUpdateRecovery(ctx) {
				return
			}

			continue
		}

		if record == nil {
			continue
		}

		if record.State == selfupdate.StateStaged || record.State == selfupdate.StateFailed ||
			record.State == selfupdate.StateRolledBack || record.State == selfupdate.StateAborted {
			if err := finalizeAsPredecessor(ctx, log, deps.client, deps.notifier, deps.store, *record); err != nil {
				log.Error("self-update: failed to recover handover; retrying", logger.ErrAttr(err))

				if !retrySelfUpdateRecovery(ctx) {
					return
				}
			}

			continue
		}

		if record.Strategy != selfupdate.StrategyScaleOut && record.Strategy != selfupdate.StrategyApplier {
			log.Error("self-update: refusing to drain for an unknown handover strategy",
				slog.String("strategy", string(record.Strategy)), slog.String("id", record.ID))

			continue
		}

		if record.Strategy == selfupdate.StrategyApplier &&
			(record.State == selfupdate.StateApplying || record.State == selfupdate.StateApplyReady) {
			current, waitErr := waitForApplierReady(ctx, log, deps.client, deps.store, *record)
			if waitErr != nil {
				if ctx.Err() != nil {
					return
				}

				log.Error("self-update: cannot wait for applier preflight", logger.ErrAttr(waitErr))

				if !retrySelfUpdateRecovery(ctx) {
					return
				}

				continue
			}

			record = &current
			if record.State == selfupdate.StateFailed || record.State == selfupdate.StateRolledBack {
				if err := finalizeAsPredecessor(ctx, log, deps.client, deps.notifier, deps.store, *record); err != nil {
					log.Error("self-update: failed to recover applier; retrying", logger.ErrAttr(err))

					if !retrySelfUpdateRecovery(ctx) {
						return
					}
				}

				continue
			}
		}

		if record.Strategy == selfupdate.StrategyScaleOut &&
			record.State != selfupdate.StateHandover && record.State != selfupdate.StateDrained {
			log.Error("self-update: scale-out handover is not ready to drain", slog.String("state", string(record.State)))
			continue
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

		record = &current
		if record.State == selfupdate.StateFailed || record.State == selfupdate.StateRolledBack ||
			record.State == selfupdate.StateAborted {
			if err = finalizeAsPredecessor(ctx, log, deps.client, deps.notifier, deps.store, *record); err != nil {
				log.Error("self-update: failed to recover after draining", logger.ErrAttr(err))
			}

			os.Exit(handoverExitCode)
		}

		if record.Strategy == selfupdate.StrategyApplier && record.State == selfupdate.StateApplyReady {
			if record.Error != "" {
				log.Warn("self-update: applier is recovering; restarting to restore admission",
					slog.String("id", record.ID), slog.String("reason", record.Error))
				os.Exit(handoverExitCode)
			}

			current, err = deps.store.Update(*record, selfupdate.StateApplyDrained, selfupdate.ActorPredecessor)
			if err != nil {
				if errors.Is(err, selfupdate.ErrStaleRecord) {
					latest, loadErr := deps.store.Load(record.ID)
					if loadErr == nil && (latest.State == selfupdate.StateFailed ||
						latest.State == selfupdate.StateRolledBack || latest.State == selfupdate.StateAborted) {
						if cleanupErr := finalizeAsPredecessor(ctx, log, deps.client, deps.notifier, deps.store, latest); cleanupErr != nil {
							log.Error("self-update: failed to recover the updated handover", logger.ErrAttr(cleanupErr))
						}
					} else if loadErr != nil {
						log.Error("self-update: cannot reload the updated handover", logger.ErrAttr(loadErr))
					}
				}

				log.Error("self-update: cannot acknowledge applier drain", logger.ErrAttr(err))
				os.Exit(handoverExitCode)
			}

			record = &current
		}

		if record.Strategy == selfupdate.StrategyScaleOut &&
			record.State != selfupdate.StateHandover && record.State != selfupdate.StateDrained {
			log.Error("self-update: scale-out handover changed while draining", slog.String("state", string(record.State)))
			os.Exit(handoverExitCode)
		}

		log.Info("self-update: drained",
			slog.String("id", record.ID),
			slog.String("strategy", string(record.Strategy)),
		)

		switch record.Strategy {
		case selfupdate.StrategyScaleOut:
			waitToBeReplaced(ctx, log, deps, *record)
		case selfupdate.StrategyApplier:
			if record.State != selfupdate.StateApplyDrained {
				log.Error("self-update: applier handover lost its drain state", slog.String("state", string(record.State)))
				os.Exit(handoverExitCode)
			}

			selfupdate.MaybeCrash(deps.appConfig.DataMountPath, string(selfupdate.StateApplyDrained), log.Logger)
			waitForApplierHandover(ctx, log, deps, *record)
		}

		return
	}
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
	poll := time.NewTicker(finalizePollInterval)
	defer poll.Stop()

	progress := time.NewTicker(finalizeWaitLogInterval)
	defer progress.Stop()

	for {
		current, err := store.Load(record.ID)
		if err != nil {
			return selfupdate.Record{}, fmt.Errorf("read applier readiness: %w", err)
		}

		if current.State == selfupdate.StateApplying || current.State == selfupdate.StateApplyReady {
			current, err = failStoppedSelfApplier(ctx, apiClient, store, current)
			if err != nil {
				return selfupdate.Record{}, err
			}
		}

		switch current.State {
		case selfupdate.StateApplyReady:
			if current.Error == "" {
				return current, nil
			}
		case selfupdate.StateApplyDrained, selfupdate.StateFailed, selfupdate.StateRolledBack:
			return current, nil
		case selfupdate.StateApplying:
		default:
			return current, fmt.Errorf("unexpected state while waiting for applier readiness: %s", current.State)
		}

		select {
		case <-ctx.Done():
			return selfupdate.Record{}, ctx.Err()
		case <-poll.C:
		case <-progress.C:
			log.Warn("self-update: still waiting for applier preflight", slog.String("id", record.ID))
		}
	}
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

		if current.State == selfupdate.StateFailed || current.State == selfupdate.StateRolledBack {
			if err := finalizeAsPredecessor(ctx, log, deps.client, deps.notifier, deps.store, current); err != nil {
				log.Error("self-update: failed to clean up the applier", logger.ErrAttr(err))
			}

			os.Exit(handoverExitCode)
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

		if !result.Container.State.Running &&
			!result.Container.State.Restarting && (result.Container.State.ExitCode == 0 || result.Container.State.Dead) {
			log.Warn("self-update: the applier exited without replacing this container, exiting for recovery",
				slog.String("applier_id", record.Applier.ID),
				slog.Int("exit_code", result.Container.State.ExitCode))

			os.Exit(handoverExitCode)
		}
	}
}
