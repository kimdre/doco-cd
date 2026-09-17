package main

import (
	"context"
	"log/slog"
	"os"
	"time"

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

// runSelfUpdateCoordinator drains this instance once a deploy has handed the
// stack over, then waits to be stopped by the successor. It is the only place
// that decides this process is finished.
func runSelfUpdateCoordinator(ctx context.Context, log *logger.Logger, deps selfUpdateCoordinatorDeps) {
	select {
	case <-ctx.Done():
		return
	case <-selfupdate.DrainRequests():
	}

	log.Info("self-update: draining")

	// Stop the scheduler and the cert watcher first, then let in-flight runs
	// finish. New work is refused from here on, but a deploy already running
	// for another stack must be allowed to complete.
	deps.stopWork()
	deps.runs.Drain()

	record, err := deps.store.Active()
	if err != nil || record == nil {
		log.Warn("self-update: no active record at drain time, staying up", logger.ErrAttr(err))

		return
	}

	log.Info("self-update: drained",
		slog.String("id", record.ID),
		slog.String("strategy", string(record.Strategy)),
	)

	selfupdate.MaybeCrash(deps.appConfig.DataMountPath, string(selfupdate.StateDrained), log.Logger)

	switch record.Strategy {
	case selfupdate.StrategyScaleOut:
		waitToBeReplaced(ctx, log, deps, *record)
	case selfupdate.StrategyApplier:
		waitForApplierHandover(ctx, log, deps, *record)
	}
}

// waitToBeReplaced marks the record drained and blocks until the successor
// stops this container. It gives up only if the successor disappeared.
func waitToBeReplaced(ctx context.Context, log *logger.Logger, deps selfUpdateCoordinatorDeps, record selfupdate.Record) {
	if record.State == selfupdate.StateHandover {
		updated, err := deps.store.Update(record, selfupdate.StateDrained, selfupdate.ActorPredecessor)
		if err != nil {
			log.Warn("self-update: failed to mark the handover drained", logger.ErrAttr(err))
		} else {
			record = updated
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(handoverGrace):
		}

		result, err := deps.client.ContainerInspect(ctx, record.Successor.ID, client.ContainerInspectOptions{})
		if err != nil || result.Container.State == nil || !result.Container.State.Running {
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
		if _, err := deps.client.ContainerRemove(removeCtx, record.Successor.ID, client.ContainerRemoveOptions{Force: true}); err != nil {
			log.Warn("self-update: failed to remove the dead successor", logger.ErrAttr(err))
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

		result, err := deps.client.ContainerInspect(ctx, record.Applier.ID, client.ContainerInspectOptions{})
		if err != nil {
			log.Warn("self-update: the applier is gone, exiting so a clean instance can recover",
				slog.String("applier_id", record.Applier.ID))
			os.Exit(handoverExitCode)
		}

		if result.Container.State != nil && !result.Container.State.Running {
			log.Warn("self-update: the applier exited without replacing this container, exiting for recovery",
				slog.String("applier_id", record.Applier.ID),
				slog.Int("exit_code", result.Container.State.ExitCode))

			os.Exit(handoverExitCode)
		}
	}
}
