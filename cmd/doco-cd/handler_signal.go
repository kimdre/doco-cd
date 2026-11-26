package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/kimdre/doco-cd/internal/common/types/clone"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/controlplane"
	log "github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/stages"
)

// pollSignalTrigger runs a batch of poll configurations once and waits for the result.
type pollSignalTrigger func(ctx context.Context, configs []poll.Config, triggerReason string, jobLog *slog.Logger) (string, error)

// notifyPollSignals subscribes to the poll trigger signals.
// The channel holds one signal. The runtime drops a signal when the channel is full.
// Thus, signals that arrive during a poll run cause at most one more run.
// Subscribe early, because the default action of SIGHUP stops the process.
func notifyPollSignals() (<-chan os.Signal, func()) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGUSR1)

	return sigCh, func() {
		signal.Stop(sigCh)
	}
}

// drainPollSignal discards a pending signal from startup.
func drainPollSignal(sigCh <-chan os.Signal, logger *slog.Logger) {
	select {
	case sig, ok := <-sigCh:
		if ok {
			logger.Debug("ignored poll trigger signal received during startup", slog.String("signal", sig.String()))
		}
	default:
	}
}

// runPollSignalLoop runs all enabled poll configurations once for each received signal.
// It uses the same filter as StartPoll, so configurations with polling disabled stay disabled.
// The loop runs until ctx is cancelled. A failed poll run does not stop the loop.
func runPollSignalLoop(
	ctx context.Context,
	sigCh <-chan os.Signal,
	configs []poll.Config,
	trigger pollSignalTrigger,
	logger *slog.Logger,
) {
	enabled := make([]poll.Config, 0, len(configs))

	for _, cfg := range configs {
		if pollJobEnabled(cfg) {
			enabled = append(enabled, cfg)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case sig, ok := <-sigCh:
			// Cancellation wins over a signal that arrived at the same time.
			if !ok || ctx.Err() != nil {
				return
			}

			triggerPollsFromSignal(ctx, sig, enabled, trigger, logger)
		}
	}
}

// triggerPollsFromSignal runs one poll batch for a received signal and logs the result.
func triggerPollsFromSignal(
	ctx context.Context,
	sig os.Signal,
	configs []poll.Config,
	trigger pollSignalTrigger,
	logger *slog.Logger,
) {
	sigLog := logger.With(slog.String("signal", sig.String()))

	if len(configs) == 0 {
		sigLog.Info("received poll trigger signal, but no enabled poll configurations exist")

		return
	}

	sigLog.Info("received poll trigger signal, triggering poll run", slog.Int("poll_configs", len(configs)))

	// The trigger changes the configurations in place. Copies keep the
	// configured values unchanged for the next signal.
	batch := make([]poll.Config, len(configs))
	for i := range configs {
		batch[i] = *clone.New(&configs[i])
	}

	jobID, err := trigger(ctx, batch, pollTriggerSignal, sigLog)
	logPollSignalResult(sigLog.With(slog.String("job_id", jobID)), err)
}

// logPollSignalResult logs the outcome of a poll run triggered by a signal.
// Skipped and deferred runs are expected outcomes and are not logged as errors.
func logPollSignalResult(logger *slog.Logger, err error) {
	if err == nil {
		logger.Info("poll run triggered by signal complete")

		return
	}

	// A signal is a manual action, so the operator must see why nothing was deployed.
	if errors.Is(err, stages.ErrDeploymentPaused) {
		logger.Info("poll jobs skipped by deployment pause", slog.String("reason", err.Error()))

		return
	}

	if errors.Is(err, stages.ErrRetrySuppressed) {
		logger.Debug("poll jobs skipped by deployment retry policy", slog.String("reason", err.Error()))

		return
	}

	if blocked, ok := errors.AsType[*stages.SyncWindowBlockedError](err); ok {
		logger.Info("poll jobs deferred by sync window", slog.String("reason", blocked.Error()))

		return
	}

	if controlplane.IsLifecycleCancellation(err) {
		logger.Debug("poll run triggered by signal cancelled", log.ErrAttr(err))

		return
	}

	logger.Error("poll run triggered by signal failed", log.ErrAttr(err))
}
