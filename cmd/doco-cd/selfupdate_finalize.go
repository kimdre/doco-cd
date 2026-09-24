package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/selfupdate"
)

const (
	// finalizeWaitLogInterval reports a stalled handover without terminating
	// another stack's in-flight deployment or the applier's health gate.
	finalizeWaitLogInterval = 3 * time.Minute
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
	runs *controlplane.Runs,
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
		if record.State == selfupdate.StateApplyDrained {
			current, recoveryErr := failStoppedSelfApplier(ctx, apiClient, store, *record)
			if recoveryErr != nil {
				return nil, recoveryErr
			}

			record = &current
		}

		if record.State == selfupdate.StateApplyDrained || record.State == selfupdate.StateApplied ||
			record.State == selfupdate.StateFinalising {
			// The applier can act on the durable drain acknowledgement at any
			// moment, or the successor may already be serving. Close admission
			// before this restarted process serves.
			runs.Drain()

			current, loadErr := store.Load(record.ID)
			if loadErr != nil {
				return nil, loadErr
			}

			if current.State != record.State {
				return nil, fmt.Errorf("applier handover changed to %s during predecessor startup; restarting to restore admission", current.State)
			}

			record = &current
		}

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
	case selfupdate.StateApplying, selfupdate.StateApplyReady, selfupdate.StateApplyDrained:
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

		if record.State.RolledBackOrFailed() {
			return nil
		}

		if record.State.InApplierPhase() {
			return fmt.Errorf("self-update applier exited while record %s is still %s", record.ID, record.State)
		}
	case selfupdate.StateHandover, selfupdate.StateStarted, selfupdate.StateDrained:
		current, ready, err := waitForDrain(ctx, log, store, record)
		if err != nil {
			return err
		}

		if !ready {
			return nil
		}

		record = current
	}

	if record.State == selfupdate.StateApplied || record.State == selfupdate.StateDrained {
		updated, err := store.Update(record, selfupdate.StateFinalising, selfupdate.ActorSuccessor)
		if err != nil {
			return err
		}

		record = updated
	}

	if record.State != selfupdate.StateFinalising {
		return fmt.Errorf("self-update record %s is not ready for finalisation: %s", record.ID, record.State)
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
			return fmt.Errorf("record failed self-update before clearing its journal: %w", err)
		}

		if err := removeSelfUpdateLeftovers(ctx, log, apiClient, record); err != nil {
			return err
		}

		return store.Remove(record.ID)

	case selfupdate.StateStaged:
		// A failed journal write can leave a clone that was created before its
		// ID could be saved. Find it by this attempt's label before dropping
		// the only recovery record.
		if err := removeStagedSelfAppliers(ctx, apiClient, record); err != nil {
			return err
		}

		return store.Remove(record.ID)

	case selfupdate.StateStarted:
		if record.Successor.ID == "" {
			return fmt.Errorf("self-update record %s has no successor id", record.ID)
		}

		timeout := time.Duration(record.Deploy.TimeoutSeconds) * time.Second
		if timeout <= 0 {
			timeout = 90 * time.Second
		}

		if err := selfupdate.WaitHealthy(ctx, apiClient, record.Successor.ID, timeout, log.Logger); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			if _, removeErr := apiClient.ContainerRemove(context.WithoutCancel(ctx), record.Successor.ID, client.ContainerRemoveOptions{Force: true}); removeErr != nil &&
				!errdefs.IsNotFound(removeErr) {
				return fmt.Errorf("remove unhealthy self-update successor %s: %w", record.Successor.ID, removeErr)
			}

			record.Error = fmt.Sprintf("successor did not recover after predecessor restarted: %v", err)
			if saveErr := store.Save(record); saveErr != nil {
				return saveErr
			}

			aborted, updateErr := store.Update(record, selfupdate.StateAborted, selfupdate.ActorPredecessor)
			if updateErr != nil {
				return updateErr
			}

			return finalizeAsPredecessor(ctx, log, apiClient, notifier, store, aborted)
		}

		updated, err := store.Update(record, selfupdate.StateHandover, selfupdate.ActorPredecessor)
		if err != nil {
			return err
		}

		record = updated

		fallthrough

	case selfupdate.StateHandover, selfupdate.StateDrained:
		// A successor exists and may still be coming up. Hand over again rather
		// than racing it: the coordinator drains and waits to be stopped.
		log.Info("self-update: resuming handover after a restart", slog.String("id", record.ID))
		selfupdate.RequestDrain()

		return nil

	case selfupdate.StateApplying, selfupdate.StateApplyReady, selfupdate.StateApplyDrained:
		// The applier waits for a durable drain acknowledgment. Blocking boot
		// until it exits would deadlock the two processes.
		current, err := failStoppedSelfApplier(ctx, apiClient, store, record)
		if err != nil {
			return err
		}

		if current.State.RolledBackOrFailed() {
			return finalizeAsPredecessor(ctx, log, apiClient, notifier, store, current)
		}

		selfupdate.RequestDrain()

		return nil

	case selfupdate.StateApplied, selfupdate.StateFinalising:
		// A successor may already be accepting traffic. The coordinator exits
		// this drained predecessor rather than re-opening admission.
		selfupdate.RequestDrain()
		return nil

	default:
		return nil
	}
}

// failStoppedSelfApplier recovers a missing, dead or cleanly exited applier.
// A nonzero exit with on-failure restart is transient and must not be stolen
// from Docker's restart loop.
func failStoppedSelfApplier(
	ctx context.Context, apiClient client.APIClient, store *selfupdate.Store, record selfupdate.Record,
) (selfupdate.Record, error) {
	stopped := record.Applier.ID == ""
	if !stopped {
		result, err := apiClient.ContainerInspect(ctx, record.Applier.ID, client.ContainerInspectOptions{})
		switch {
		case errdefs.IsNotFound(err):
			stopped = true
		case err != nil:
			return record, fmt.Errorf("inspect self-update applier %s: %w", record.Applier.ID, err)
		case result.Container.State == nil:
			return record, fmt.Errorf("self-update applier %s has no container state", record.Applier.ID)
		default:
			state := result.Container.State
			stopped = state.Dead || (!state.Running && !state.Restarting && state.ExitCode == 0)
		}
	}

	if !stopped {
		return record, nil
	}

	current, err := store.Load(record.ID)
	if err != nil {
		return record, err
	}

	if !current.State.InApplierPhase() {
		return current, nil
	}

	current.Error = "self-update applier stopped without completing the handover"

	return store.Update(current, selfupdate.StateFailed, selfupdate.ActorPredecessor)
}

// removeStagedSelfAppliers cleans up clones recorded in the journal or found
// by their attempt label after an incomplete staging operation.
func removeStagedSelfAppliers(ctx context.Context, apiClient client.APIClient, record selfupdate.Record) error {
	if record.Applier.ID != "" {
		if err := docker.RemoveSelfApplier(ctx, apiClient, record.Applier.ID); err != nil && !errdefs.IsNotFound(err) {
			return err
		}
	}

	appliers, err := docker.FindSelfAppliers(ctx, apiClient, record.Stack)
	if err != nil {
		return err
	}

	for _, applier := range appliers {
		if applier.ID == record.Applier.ID || applier.Labels[docker.SelfApplierLabel] != record.ID {
			continue
		}

		if err := docker.RemoveSelfApplier(ctx, apiClient, applier.ID); err != nil && !errdefs.IsNotFound(err) {
			return err
		}
	}

	return nil
}

// waitForDrain only permits takeover after the predecessor has recorded its
// completed drain. A removed or aborted record is not a successful handover.
func waitForDrain(ctx context.Context, log *logger.Logger, store *selfupdate.Store, record selfupdate.Record) (selfupdate.Record, bool, error) {
	poll := time.NewTicker(finalizePollInterval)
	defer poll.Stop()

	progress := time.NewTicker(finalizeWaitLogInterval)
	defer progress.Stop()

	for {
		current, err := store.Load(record.ID)
		if err != nil {
			if errors.Is(err, selfupdate.ErrNoRecord) {
				return selfupdate.Record{}, false, nil
			}

			return selfupdate.Record{}, false, err
		}

		switch current.State {
		case selfupdate.StateDrained, selfupdate.StateFinalising:
			return current, true, nil
		case selfupdate.StateRolledBack, selfupdate.StateFailed, selfupdate.StateAborted:
			return current, false, nil
		case selfupdate.StateStarted, selfupdate.StateHandover:
			// The predecessor is still health-checking or draining.
		default:
			return current, false, fmt.Errorf("unexpected self-update state while waiting for drain: %s", current.State)
		}

		select {
		case <-ctx.Done():
			return selfupdate.Record{}, false, ctx.Err()
		case <-poll.C:
		case <-progress.C:
			log.Warn("self-update: still waiting for the predecessor to finish draining",
				slog.String("id", record.ID),
				slog.String("state", string(current.State)))
		}
	}
}

// waitForApplier blocks until the applier has finished, rather than mistaking
// an on-failure restart between attempts for a completed handover.
func waitForApplier(ctx context.Context, log *logger.Logger, apiClient client.APIClient, record selfupdate.Record) error {
	if record.Applier.ID == "" {
		return fmt.Errorf("self-update record %s has no applier id", record.ID)
	}

	poll := time.NewTicker(finalizePollInterval)
	defer poll.Stop()

	progress := time.NewTicker(finalizeWaitLogInterval)
	defer progress.Stop()

	for {
		result, err := apiClient.ContainerInspect(ctx, record.Applier.ID, client.ContainerInspectOptions{})
		if err != nil {
			if errdefs.IsNotFound(err) {
				return nil
			}

			return fmt.Errorf("inspect self-update applier %s: %w", record.Applier.ID, err)
		}

		if result.Container.State == nil {
			return fmt.Errorf("self-update applier %s has no container state", record.Applier.ID)
		}

		state := result.Container.State
		if !state.Running && !state.Restarting && (state.ExitCode == 0 || state.Dead) {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-poll.C:
		case <-progress.C:
			log.Warn("self-update: still waiting for the applier",
				slog.String("applier_id", record.Applier.ID))
		}
	}
}

// removeSelfUpdateLeftovers drops the predecessor and the applier, so the stack
// is back to exactly one container per service.
func removeSelfUpdateLeftovers(ctx context.Context, log *logger.Logger, apiClient client.APIClient, record selfupdate.Record) error {
	if record.State == selfupdate.StateAborted {
		if err := removeAbortedSuccessor(ctx, apiClient, record); err != nil {
			return err
		}
	}

	if record.Predecessor.ID != "" && record.Predecessor.ID != docker.SelfUpdateConfig().Identity.ContainerID {
		timeout := 10
		if _, err := apiClient.ContainerStop(ctx, record.Predecessor.ID, client.ContainerStopOptions{Timeout: &timeout}); err != nil &&
			!errdefs.IsNotFound(err) && !errdefs.IsNotModified(err) {
			return fmt.Errorf("stop self-update predecessor %s: %w", record.Predecessor.ID, err)
		}

		if _, err := apiClient.ContainerRemove(ctx, record.Predecessor.ID, client.ContainerRemoveOptions{Force: true}); err != nil {
			if !errdefs.IsNotFound(err) {
				return fmt.Errorf("remove self-update predecessor %s: %w", record.Predecessor.ID, err)
			}
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
			if !errdefs.IsNotFound(err) {
				return err
			}
		}
	}

	return nil
}

// removeAbortedSuccessor discovers and removes an abandoned replacement even
// when its container ID was not persisted in the journal.
func removeAbortedSuccessor(ctx context.Context, apiClient client.APIClient, record selfupdate.Record) error {
	successorID := record.Successor.ID
	if successorID == "" {
		list, err := apiClient.ContainerList(ctx, client.ContainerListOptions{
			All: true,
			Filters: make(client.Filters).
				Add("label", api.ProjectLabel+"="+record.Stack).
				Add("label", api.ServiceLabel+"="+record.Service),
		})
		if err != nil {
			return fmt.Errorf("find aborted self-update successor: %w", err)
		}

		for _, c := range list.Items {
			if c.ID == record.Predecessor.ID {
				continue
			}

			if successorID != "" {
				return fmt.Errorf("multiple aborted self-update successors found for %s/%s", record.Stack, record.Service)
			}

			successorID = c.ID
		}
	}

	if successorID == "" {
		return nil
	}

	if _, err := apiClient.ContainerRemove(ctx, successorID, client.ContainerRemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("remove aborted self-update successor %s: %w", successorID, err)
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
