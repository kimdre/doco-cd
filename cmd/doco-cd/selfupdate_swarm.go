package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/common/lifecycle"
	"github.com/kimdre/doco-cd/internal/docker"
	swarmInternal "github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/selfupdate"
)

// runSwarmSelfUpdateFinalizer stays active to recover reporting after rollback or task restart.
// Health checks run after startup and never block the HTTP server from starting.
func runSwarmSelfUpdateFinalizer(ctx context.Context, log *logger.Logger, cli command.Cli, reporter *selfUpdateReporter, identity selfupdate.Identity) {
	for {
		handover, err := selfupdate.LoadSwarmHandover(ctx, cli.Client(), identity.SwarmServiceID)
		if err == nil && handover != nil {
			err = finalizeSwarmSelfUpdate(ctx, log, cli, reporter, identity, *handover)
		}

		if ctx.Err() != nil {
			return
		}

		if err != nil {
			log.Warn("self-update: Swarm confirmation or reporting will be retried", logger.ErrAttr(err))
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func swarmTaskAlive(ctx context.Context, apiClient client.APIClient, taskID string) (bool, error) {
	result, err := apiClient.TaskInspect(ctx, taskID, client.TaskInspectOptions{})
	if errdefs.IsNotFound(err) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("inspect Swarm reporting task: %w", err)
	}

	return selfupdate.SwarmTaskAlive(result.Task), nil
}

func finalizeSwarmSelfUpdate(ctx context.Context, log *logger.Logger, cli command.Cli, reporter *selfUpdateReporter, identity selfupdate.Identity, handover selfupdate.SwarmHandover) error {
	// Start-first can overlap both processes. Only take over after the predecessor task stops.
	if handover.Record.State != selfupdate.StateFinalising || handover.OwnerTaskID != identity.SwarmTaskID {
		alive, err := swarmTaskAlive(ctx, cli.Client(), handover.PredecessorTaskID)
		if err != nil || alive {
			return err
		}
	}

	if handover.Record.State == selfupdate.StateFinalising {
		if handover.OwnerTaskID != identity.SwarmTaskID {
			alive, err := swarmTaskAlive(ctx, cli.Client(), handover.OwnerTaskID)
			if err != nil || alive {
				return err
			}

			var resultErr error
			if handover.Record.Error != "" {
				resultErr = errors.New(handover.Record.Error)
				if handover.Record.TimedOut {
					resultErr = lifecycle.MarkTimedOut(resultErr)
				}
			}

			if err := selfupdate.ClaimSwarmHandover(ctx, cli.Client(), handover, identity.SwarmTaskID, resultErr); err != nil {
				return err
			}
		}
	} else {
		service, err := cli.Client().ServiceInspect(ctx, handover.ServiceID, client.ServiceInspectOptions{})
		if err != nil {
			return fmt.Errorf("inspect Swarm self-update service: %w", err)
		}

		resultErr := selfupdate.SwarmUpdateError(handover, service.Service)
		if resultErr == nil {
			task, err := cli.Client().TaskInspect(ctx, identity.SwarmTaskID, client.TaskInspectOptions{})
			if err != nil {
				return fmt.Errorf("inspect Swarm successor task: %w", err)
			}

			matches, err := selfupdate.SwarmTaskMatches(handover, service.Service, task.Task)
			if err != nil {
				return err
			}

			if !matches {
				if time.Until(handover.Record.CreatedAt.Add(handover.Record.Deploy.HealthTimeout())) > 0 {
					return nil
				}

				resultErr = lifecycle.MarkTimedOut(errors.New("self-update: no matching Swarm successor became healthy"))
			} else {
				resultErr = confirmSwarmSelfUpdate(ctx, log, cli, identity, handover)
			}
		}

		// Application shutdown is not a deployment timeout and leaves the result unresolved.
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if err := selfupdate.ClaimSwarmHandover(ctx, cli.Client(), handover, identity.SwarmTaskID, resultErr); err != nil {
			return err
		}

		handover.Record.Error = ""
		if resultErr != nil {
			handover.Record.Error = resultErr.Error()
			handover.Record.TimedOut = lifecycle.IsTimeout(resultErr)
		}
	}

	return reportSwarmSelfUpdate(ctx, log, cli.Client(), reporter, handover)
}

func confirmSwarmSelfUpdate(ctx context.Context, log *logger.Logger, cli command.Cli, identity selfupdate.Identity, handover selfupdate.SwarmHandover) error {
	remaining := time.Until(handover.Record.CreatedAt.Add(handover.Record.Deploy.HealthTimeout()))
	if remaining <= 0 {
		return lifecycle.MarkTimedOut(errors.New("self-update: Swarm confirmation timed out"))
	}

	waitCtx, cancel := context.WithTimeout(ctx, remaining)
	defer cancel()

	err := swarmInternal.WaitOnServicesWithTimeout(waitCtx, cli, handover.Record.Deploy.Services, remaining)
	if err == nil {
		err = selfupdate.WaitHealthy(waitCtx, cli.Client(), identity.ContainerID, time.Until(handover.Record.CreatedAt.Add(handover.Record.Deploy.HealthTimeout())), log.Logger)
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
		if err == nil {
			err = waitCtx.Err()
		}

		return lifecycle.MarkTimedOut(fmt.Errorf("self-update: Swarm confirmation timed out: %w", err))
	}

	if err != nil {
		return err
	}

	// Recheck after convergence and health, since rollback can change the service during either wait.
	service, err := cli.Client().ServiceInspect(ctx, handover.ServiceID, client.ServiceInspectOptions{})
	if err != nil {
		return fmt.Errorf("recheck Swarm self-update service: %w", err)
	}

	if err := selfupdate.SwarmUpdateError(handover, service.Service); err != nil {
		return err
	}

	task, err := cli.Client().TaskInspect(ctx, identity.SwarmTaskID, client.TaskInspectOptions{})
	if err != nil {
		return fmt.Errorf("recheck Swarm successor task: %w", err)
	}

	matches, err := selfupdate.SwarmTaskMatches(handover, service.Service, task.Task)
	if err != nil {
		return err
	}

	if !matches {
		return errors.New("self-update: Swarm successor no longer runs the pending update")
	}

	return nil
}

func reportSwarmSelfUpdate(ctx context.Context, log *logger.Logger, apiClient client.APIClient, reporter *selfUpdateReporter, handover selfupdate.SwarmHandover) error {
	if handover.Record.Error != "" {
		if err := reporter.reportFailure(ctx, log, handover.Record); err != nil {
			return err
		}
	} else {
		if err := reporter.reportSuccess(ctx, log, handover.Record); err != nil {
			return err
		}
	}

	if err := docker.RecordSwarmHandoverOutcome(handover); err != nil {
		return err
	}

	if err := selfupdate.RemoveSwarmHandover(ctx, apiClient, handover); err != nil {
		return err
	}

	if err := docker.RecordSwarmHandoverMetrics(handover); err != nil {
		return err
	}

	log.Info("self-update: Swarm handover finalised",
		slog.String("id", handover.Record.ID),
		slog.String("stack", handover.Record.Stack),
		slog.String("commit", handover.Record.Source.CommitSHA),
		slog.String("error", handover.Record.Error),
	)

	return nil
}
