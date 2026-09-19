package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/selfupdate"
)

const (
	// finalizeDrainWait bounds how long a successor waits for the predecessor
	// to report that it finished its in-flight work.
	finalizeDrainWait = 3 * time.Minute
	// finalizePollInterval is how often the successor re-reads the journal.
	finalizePollInterval = time.Second
)

// resolveSelfIdentity derives this process's compose identity. A failure is not
// fatal: doco-cd then runs with self-update unavailable.
func resolveSelfIdentity(ctx context.Context, log *logger.Logger, apiClient client.APIClient) selfupdate.Identity {
	containerID, err := getAppContainerID()
	if err != nil {
		log.Debug("could not resolve own container id, self-update is unavailable", logger.ErrAttr(err))

		return selfupdate.Identity{}
	}

	identity, err := selfupdate.Detect(ctx, apiClient, containerID)
	if err != nil {
		log.Warn("could not detect own compose identity, self-update is unavailable", logger.ErrAttr(err))

		return selfupdate.Identity{}
	}

	if identity.OK {
		log.Debug("self-update identity resolved",
			slog.String("container_id", identity.ContainerID),
			slog.String("project", identity.Project),
			slog.String("service", identity.Service),
			slog.Int("number", identity.Number),
		)
	}

	return identity
}

// finalizeSelfUpdate converges a handover left behind by an earlier process.
//
// A successor must not block here: its health gate is what the predecessor is
// waiting for, and the HTTP server only starts later in the boot. So the
// successor's side runs in the background and this call returns at once. While
// a record is unresolved, deployCompose refuses to start another handover.
func finalizeSelfUpdate(
	ctx context.Context,
	log *logger.Logger,
	apiClient client.APIClient,
	notifier *notification.Notifier,
	store *selfupdate.Store,
	identity selfupdate.Identity,
) (func(), error) {
	record, err := store.Active()
	if err != nil {
		return nil, err
	}

	if record == nil {
		return nil, nil
	}

	role := resolveSelfUpdateRole(*record, identity.ContainerID)

	log.Info("self-update: pending handover found",
		slog.String("id", record.ID),
		slog.String("state", string(record.State)),
		slog.String("strategy", string(record.Strategy)),
		slog.String("role", string(role)),
	)

	switch role {
	case roleSuccessor:
		pending := *record

		return func() {
			if err := finalizeAsSuccessor(ctx, log, apiClient, notifier, store, pending); err != nil {
				log.Error("self-update: failed to finalize the handover", logger.ErrAttr(err))
			}
		}, nil
	case rolePredecessor:
		return nil, finalizeAsPredecessor(ctx, log, apiClient, notifier, store, *record)
	default:
		// Neither container of the record is us: the record belongs to a stack
		// state that no longer exists, so it must not block this boot.
		log.Warn("self-update: record does not match this container, quarantining it", slog.String("id", record.ID))

		return nil, store.Quarantine(record.ID)
	}
}

type selfUpdateRole string

const (
	rolePredecessor selfUpdateRole = "predecessor"
	roleSuccessor   selfUpdateRole = "successor"
	roleUnknown     selfUpdateRole = "unknown"
)

func resolveSelfUpdateRole(record selfupdate.Record, ownID string) selfUpdateRole {
	switch ownID {
	case "":
		return roleUnknown
	case record.Predecessor.ID, record.Restored.ID:
		return rolePredecessor
	case record.Successor.ID:
		return roleSuccessor
	}

	// A successor created by the applier is not known to the record until the
	// applier writes it, so fall back to "not the predecessor" for a running
	// self container.
	if record.Predecessor.ID != "" && ownID != record.Predecessor.ID {
		return roleSuccessor
	}

	return roleUnknown
}

// finalizeAsSuccessor removes the predecessor and reports the deployment that
// the predecessor could not report itself.
func finalizeAsSuccessor(
	ctx context.Context,
	log *logger.Logger,
	apiClient client.APIClient,
	notifier *notification.Notifier,
	store *selfupdate.Store,
	record selfupdate.Record,
) error {
	switch record.State {
	case selfupdate.StateRolledBack, selfupdate.StateFailed, selfupdate.StateAborted:
		// The predecessor is the one that reports these, not us.
		return nil
	case selfupdate.StateApplying:
		if err := waitForApplier(ctx, log, apiClient, record); err != nil {
			return err
		}

		reloaded, err := store.Load(record.ID)
		if err != nil {
			if errors.Is(err, selfupdate.ErrNoRecord) {
				return nil
			}

			return err
		}

		record = reloaded

		if record.State == selfupdate.StateRolledBack || record.State == selfupdate.StateFailed {
			return nil
		}
	case selfupdate.StateHandover, selfupdate.StateStarted:
		if err := waitForDrain(ctx, log, store, record); err != nil {
			return err
		}
	}

	if record.State == selfupdate.StateHandover || record.State == selfupdate.StateDrained || record.State == selfupdate.StateApplied {
		updated, err := store.Update(record, selfupdate.StateFinalising, selfupdate.ActorSuccessor)
		if err != nil {
			return err
		}

		record = updated
	}

	if err := removeSelfUpdateLeftovers(ctx, log, apiClient, record); err != nil {
		return err
	}

	selfupdate.MaybeCrash(docker.SelfUpdateConfig().DataMountPath, string(selfupdate.StateFinalising), log.Logger)

	reportSelfUpdateSuccess(log, notifier, record)

	docker.RecordDeployStatus(record.Source.RepoName, record.Stack, record.Source.CommitSHA, record.Source.ProjectHash)

	if err := store.ClearPoison(record.Context, record.Stack); err != nil {
		log.Warn("self-update: failed to clear the poison entry", logger.ErrAttr(err))
	}

	log.Info("self-update finalised",
		slog.String("id", record.ID),
		slog.String("stack", record.Stack),
		slog.String("commit", record.Source.CommitSHA),
	)

	return store.Remove(record.ID)
}

// finalizeAsPredecessor handles the states where this container is the one that
// stays, after a rolled back or abandoned attempt.
func finalizeAsPredecessor(
	ctx context.Context,
	log *logger.Logger,
	apiClient client.APIClient,
	notifier *notification.Notifier,
	store *selfupdate.Store,
	record selfupdate.Record,
) error {
	switch record.State {
	case selfupdate.StateRolledBack, selfupdate.StateFailed, selfupdate.StateAborted:
		// The applier that did the rollback is about to be deleted, so the
		// instance that survived is the one that has to report it.
		log.Warn("self-update: rolled back",
			slog.String("id", record.ID),
			slog.String("stack", record.Stack),
			slog.String("state", string(record.State)),
			slog.String("reason", record.Error),
		)

		reportSelfUpdateFailure(log, notifier, record)

		if err := poisonSelfUpdate(store, record); err != nil {
			log.Warn("self-update: failed to record the poison entry", logger.ErrAttr(err))
		}

		if err := removeSelfUpdateLeftovers(ctx, log, apiClient, record); err != nil {
			return err
		}

		return store.Remove(record.ID)

	case selfupdate.StateStaged:
		// Nothing was created, so the attempt can simply be dropped.
		return store.Remove(record.ID)

	case selfupdate.StateStarted, selfupdate.StateHandover, selfupdate.StateDrained:
		// A successor exists and may still be coming up. Hand over again rather
		// than racing it: the coordinator drains and waits to be stopped.
		log.Info("self-update: resuming handover after a restart", slog.String("id", record.ID))
		selfupdate.RequestDrain()

		return nil

	case selfupdate.StateApplying:
		if err := waitForApplier(ctx, log, apiClient, record); err != nil {
			return err
		}

		reloaded, err := store.Load(record.ID)
		if err != nil {
			if errors.Is(err, selfupdate.ErrNoRecord) {
				return nil
			}

			return err
		}

		if reloaded.State == selfupdate.StateRolledBack || reloaded.State == selfupdate.StateFailed {
			return finalizeAsPredecessor(ctx, log, apiClient, notifier, store, reloaded)
		}

		return nil

	default:
		return nil
	}
}

// waitForDrain waits until the predecessor reports it finished its work, or
// until it is gone.
func waitForDrain(ctx context.Context, log *logger.Logger, store *selfupdate.Store, record selfupdate.Record) error {
	deadline := time.Now().Add(finalizeDrainWait)

	for time.Now().Before(deadline) {
		current, err := store.Load(record.ID)
		if err != nil {
			if errors.Is(err, selfupdate.ErrNoRecord) {
				return nil
			}

			return err
		}

		if current.State == selfupdate.StateDrained {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(finalizePollInterval):
		}
	}

	log.Warn("self-update: predecessor did not report a drain in time, continuing",
		slog.String("id", record.ID))

	return nil
}

// waitForApplier blocks until the applier container has exited.
func waitForApplier(ctx context.Context, log *logger.Logger, apiClient client.APIClient, record selfupdate.Record) error {
	if record.Applier.ID == "" {
		return nil
	}

	deadline := time.Now().Add(finalizeDrainWait)

	for time.Now().Before(deadline) {
		result, err := apiClient.ContainerInspect(ctx, record.Applier.ID, client.ContainerInspectOptions{})
		if err != nil {
			// A removed applier cannot report anything more, so there is
			// nothing left to wait for and nothing to propagate.
			return nil // nolint:nilerr
		}

		if result.Container.State == nil || !result.Container.State.Running {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(finalizePollInterval):
		}
	}

	log.Warn("self-update: applier did not exit in time", slog.String("applier_id", record.Applier.ID))

	return nil
}

// removeSelfUpdateLeftovers drops the predecessor and the applier, so the stack
// is back to exactly one container per service.
func removeSelfUpdateLeftovers(ctx context.Context, log *logger.Logger, apiClient client.APIClient, record selfupdate.Record) error {
	if record.Predecessor.ID != "" && record.Predecessor.ID != docker.SelfUpdateConfig().Identity.ContainerID {
		timeout := 10
		if _, err := apiClient.ContainerStop(ctx, record.Predecessor.ID, client.ContainerStopOptions{Timeout: &timeout}); err != nil {
			log.Debug("self-update: predecessor was already stopped", logger.ErrAttr(err))
		}

		if _, err := apiClient.ContainerRemove(ctx, record.Predecessor.ID, client.ContainerRemoveOptions{Force: true}); err != nil {
			log.Debug("self-update: predecessor was already removed", logger.ErrAttr(err))
		} else {
			log.Info("self-update: predecessor removed", slog.String("predecessor_id", record.Predecessor.ID))
		}
	}

	appliers, err := docker.FindSelfAppliers(ctx, apiClient, record.Stack)
	if err != nil {
		return err
	}

	for _, applier := range appliers {
		if err = docker.RemoveSelfApplier(ctx, apiClient, applier.ID); err != nil {
			log.Warn("self-update: failed to remove an applier container", logger.ErrAttr(err))
		}
	}

	return nil
}

func poisonSelfUpdate(store *selfupdate.Store, record selfupdate.Record) error {
	reason := record.Error
	if reason == "" {
		reason = "self-update ended in state " + string(record.State)
	}

	return store.AddPoison(selfupdate.Poison{
		Context:     record.Context,
		Stack:       record.Stack,
		CommitSHA:   record.Source.CommitSHA,
		ProjectHash: record.Source.ProjectHash,
		Reason:      reason,
	})
}

func selfUpdateMetadata(record selfupdate.Record) notification.Metadata {
	return notification.Metadata{
		Repository: record.Source.FullName,
		Stack:      record.Stack,
		Context:    record.Context,
		Target:     record.Source.ConfigTarget,
		Revision:   record.Source.CommitSHA,
		JobID:      record.Source.JobID,
	}
}

func reportSelfUpdateSuccess(log *logger.Logger, notifier *notification.Notifier, record selfupdate.Record) {
	if notifier == nil {
		return
	}

	err := notifier.Send(
		notification.Success,
		"Deployment completed",
		fmt.Sprintf("Successfully deployed stack %s (self-update, %s)", record.Stack, record.Strategy),
		selfUpdateMetadata(record),
	)
	if err != nil {
		log.Warn("self-update: failed to send the success notification", logger.ErrAttr(err))
	}
}

func reportSelfUpdateFailure(log *logger.Logger, notifier *notification.Notifier, record selfupdate.Record) {
	if notifier == nil {
		return
	}

	reason := record.Error
	if reason == "" {
		reason = "the new version did not become healthy"
	}

	err := notifier.Send(
		notification.Failure,
		"Deployment failed",
		fmt.Sprintf("Self-update of stack %s was rolled back: %s", record.Stack, reason),
		selfUpdateMetadata(record),
	)
	if err != nil {
		log.Warn("self-update: failed to send the failure notification", logger.ErrAttr(err))
	}
}
