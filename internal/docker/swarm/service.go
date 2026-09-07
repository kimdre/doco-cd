package swarm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/command/service/progress"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
	"golang.org/x/sync/errgroup"

	"github.com/kimdre/doco-cd/internal/docker/jsonstream"
)

const (
	rollbackStatusObservationTimeout  = 20 * time.Second
	rollbackStatusObservationInterval = 250 * time.Millisecond
	jobTaskPollInterval               = 200 * time.Millisecond
)

// Service represents a service.
type Service struct {
	ID string
	swarm.Meta
	Spec         swarm.ServiceSpec
	PreviousSpec *swarm.ServiceSpec `json:",omitempty"`
	Endpoint     swarm.Endpoint
	UpdateStatus *swarm.UpdateStatus `json:",omitempty"`

	// ServiceStatus is an optional, extra field indicating the number of
	// desired and running tasks. It is provided primarily as a shortcut to
	// calculating these values client-side, which otherwise would require
	// listing all tasks for a service, an operation that could be
	// computation and network expensive.
	ServiceStatus *swarm.ServiceStatus `json:",omitempty"`

	// JobStatus is the status of a Service which is in one of ReplicatedJob or
	// GlobalJob modes. It is absent on Replicated and Global services.
	JobStatus *swarm.JobStatus `json:",omitempty"`
}

// waitOnService waits for the service to converge. It outputs a progress bar,
// if appropriate based on the CLI flags.
func waitOnService(ctx context.Context, dockerCli command.Cli, serviceID string) error {
	errChan := make(chan error, 1)

	pipeReader, pipeWriter := io.Pipe()
	defer pipeReader.Close() // nolint:errcheck

	go func() {
		errChan <- progress.ServiceProgress(ctx, dockerCli.Client(), serviceID, pipeWriter)

		defer pipeWriter.Close() //nolint:errcheck
	}()

	// Monitor the output of the progress reader for errors.
	progressErr := jsonstream.ErrorReader(ctx, pipeReader)
	if progressErr == nil {
		progressErr = <-errChan
	}

	serviceResult, err := dockerCli.Client().ServiceInspect(ctx, serviceID, client.ServiceInspectOptions{})
	if err != nil {
		if progressErr != nil {
			return progressErr
		}

		return fmt.Errorf("failed to inspect service %s update status: %w", serviceID, err)
	}

	rollbackErr := rollbackUpdateStatusError(serviceID, serviceResult.Service.Spec.Name, serviceResult.Service.UpdateStatus)
	if rollbackErr != nil {
		return rollbackErr
	}

	if progressErr != nil {
		delayedRollbackErr := waitForRollbackUpdateStatus(
			ctx,
			dockerCli.Client(),
			serviceID,
			serviceResult.Service.Spec.Name,
			rollbackStatusObservationTimeout,
		)
		if delayedRollbackErr != nil {
			return delayedRollbackErr
		}
	}

	return progressErr
}

// WaitOnJobService waits for one Swarm job execution to complete. Unlike
// ServiceProgress alone, it also reports terminal task failures, which do not
// converge and would otherwise leave callers blocked indefinitely.
//
// When previousJobIteration is provided, the service must first advance beyond
// that iteration before its progress is observed. This prevents a reused job
// service from reporting an earlier successful execution as the current run.
func WaitOnJobService(
	ctx context.Context,
	dockerCli command.Cli,
	serviceID string,
	previousJobIteration *uint64,
) error {
	iteration, err := waitForJobIteration(ctx, dockerCli.Client(), serviceID, previousJobIteration)
	if err != nil {
		return err
	}

	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	progressResult := make(chan error, 1)
	failureResult := make(chan error, 1)

	go func() {
		progressResult <- waitOnService(waitCtx, dockerCli, serviceID)
	}()

	go func() {
		failureResult <- waitForJobTaskFailure(waitCtx, dockerCli.Client(), serviceID, iteration)
	}()

	select {
	case err = <-progressResult:
		cancel()

		failureErr := <-failureResult
		if err != nil {
			if failureErr != nil && !errors.Is(failureErr, context.Canceled) {
				return failureErr
			}

			return err
		}

		if failureErr = ensureJobIteration(ctx, dockerCli.Client(), serviceID, iteration); failureErr != nil {
			return failureErr
		}

		if failureErr = jobTaskFailure(ctx, dockerCli.Client(), serviceID, iteration); failureErr != nil {
			return failureErr
		}

		return nil
	case err = <-failureResult:
		cancel()

		progressErr := <-progressResult

		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}

		return progressErr
	}
}

func waitForJobIteration(
	ctx context.Context,
	apiClient client.APIClient,
	serviceID string,
	previousJobIteration *uint64,
) (uint64, error) {
	ticker := time.NewTicker(jobTaskPollInterval)
	defer ticker.Stop()

	for {
		result, err := apiClient.ServiceInspect(ctx, serviceID, client.ServiceInspectOptions{})
		if err != nil {
			return 0, fmt.Errorf("inspect job service %s: %w", serviceID, err)
		}

		if result.Service.Spec.Mode.ReplicatedJob == nil && result.Service.Spec.Mode.GlobalJob == nil {
			return 0, fmt.Errorf("service %s is not a Swarm job service", serviceID)
		}

		if result.Service.JobStatus != nil {
			iteration := result.Service.JobStatus.JobIteration.Index
			if previousJobIteration == nil || iteration > *previousJobIteration {
				return iteration, nil
			}
		}

		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-ticker.C:
		}
	}
}

func waitForJobTaskFailure(ctx context.Context, apiClient client.APIClient, serviceID string, iteration uint64) error {
	ticker := time.NewTicker(jobTaskPollInterval)
	defer ticker.Stop()

	for {
		if err := ensureJobIteration(ctx, apiClient, serviceID, iteration); err != nil {
			return err
		}

		if err := jobTaskFailure(ctx, apiClient, serviceID, iteration); err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func ensureJobIteration(ctx context.Context, apiClient client.APIClient, serviceID string, iteration uint64) error {
	result, err := apiClient.ServiceInspect(ctx, serviceID, client.ServiceInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect job service %s: %w", serviceID, err)
	}

	if result.Service.JobStatus == nil {
		return fmt.Errorf("job service %s no longer reports an active job iteration", serviceID)
	}

	if result.Service.JobStatus.JobIteration.Index != iteration {
		return fmt.Errorf(
			"job service %s advanced from iteration %d to %d while waiting",
			serviceID,
			iteration,
			result.Service.JobStatus.JobIteration.Index,
		)
	}

	return nil
}

func jobTaskFailure(ctx context.Context, apiClient client.APIClient, serviceID string, iteration uint64) error {
	tasks, err := apiClient.TaskList(ctx, client.TaskListOptions{
		Filters: make(client.Filters).Add("service", serviceID),
	})
	if err != nil {
		return fmt.Errorf("list tasks for job service %s: %w", serviceID, err)
	}

	return jobTaskFailureFromTasks(tasks.Items, iteration)
}

func jobTaskFailureFromTasks(tasks []swarm.Task, iteration uint64) error {
	for _, task := range tasks {
		if task.JobIteration == nil || task.JobIteration.Index != iteration {
			continue
		}

		switch task.Status.State {
		case swarm.TaskStateFailed, swarm.TaskStateRejected, swarm.TaskStateOrphaned,
			swarm.TaskStateShutdown, swarm.TaskStateRemove:
			return newJobTaskFailure(task)
		}
	}

	return nil
}

func newJobTaskFailure(task swarm.Task) error {
	details := strings.TrimSpace(task.Status.Err)
	if details == "" {
		details = strings.TrimSpace(task.Status.Message)
	}

	exitCode := ""
	if task.Status.ContainerStatus != nil {
		exitCode = fmt.Sprintf(" (exit code %d)", task.Status.ContainerStatus.ExitCode)
	}

	if details == "" {
		return fmt.Errorf("job task %s ended in %s%s", task.ID, task.Status.State, exitCode)
	}

	return fmt.Errorf("job task %s ended in %s%s: %s", task.ID, task.Status.State, exitCode, details)
}

// waitForRollbackUpdateStatus keeps observing a service update for a short time
// to catch rollback states that may appear after the first progress error.
func waitForRollbackUpdateStatus(ctx context.Context, apiClient client.APIClient, serviceID, serviceName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		serviceResult, err := apiClient.ServiceInspect(ctx, serviceID, client.ServiceInspectOptions{})
		if err == nil {
			rollbackErr := rollbackUpdateStatusError(serviceID, serviceName, serviceResult.Service.UpdateStatus)
			if rollbackErr != nil {
				return rollbackErr
			}

			if status := serviceResult.Service.UpdateStatus; status != nil && isTerminalNonRollbackUpdateState(status.State) {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(rollbackStatusObservationInterval):
		}
	}

	return nil
}

// rollbackUpdateStatusError returns an error when a service update finished in a rollback state.
func rollbackUpdateStatusError(serviceID, serviceName string, status *swarm.UpdateStatus) error {
	if status == nil || !isRollbackUpdateState(status.State) {
		return nil
	}

	target := strings.TrimSpace(serviceName)
	if target == "" {
		target = serviceID
	}

	message := strings.TrimSpace(status.Message)
	if message == "" {
		return fmt.Errorf("service %s entered rollback state %q", target, status.State)
	}

	return fmt.Errorf("service %s entered rollback state %q: %s", target, status.State, message)
}

// isRollbackUpdateState reports whether the update state indicates a rollback lifecycle.
func isRollbackUpdateState(state swarm.UpdateState) bool {
	return state == swarm.UpdateStateRollbackStarted ||
		state == swarm.UpdateStateRollbackPaused ||
		state == swarm.UpdateStateRollbackCompleted
}

func isTerminalNonRollbackUpdateState(state swarm.UpdateState) bool {
	return state == swarm.UpdateStateCompleted || state == swarm.UpdateStatePaused
}

// waitForNetwork waits for the network to be ready by repeatedly inspecting it until it succeeds or times out.
func waitForNetwork(ctx context.Context, apiClient client.NetworkAPIClient, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, err := apiClient.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
		if err == nil {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}

	return fmt.Errorf("timeout waiting for network %s to be ready", name)
}

// waitForSecret waits for the secret to be ready by repeatedly inspecting it until it succeeds or times out.
func waitForSecret(ctx context.Context, apiClient client.SecretAPIClient, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, err := apiClient.SecretInspect(ctx, name, client.SecretInspectOptions{})
		if err == nil {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}

	return fmt.Errorf("timeout waiting for secret %s to be ready", name)
}

// waitForConfig waits for the config to be ready by repeatedly inspecting it until it succeeds or times out.
func waitForConfig(ctx context.Context, apiClient client.ConfigAPIClient, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, err := apiClient.ConfigInspect(ctx, name, client.ConfigInspectOptions{})
		if err == nil {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}

	return fmt.Errorf("timeout waiting for config %s to be ready", name)
}

// waitForResources waits for the specified resources to be ready by concurrently inspecting them until they succeed or time out.
func waitForResources(ctx context.Context, apiClient client.APIClient, networks map[string]client.NetworkCreateOptions, secrets []swarm.SecretSpec, configs []swarm.ConfigSpec) error {
	const resourceWaitTimeout = 5 * time.Second

	g, ctx := errgroup.WithContext(ctx)

	for name := range networks {
		g.Go(func() error {
			return waitForNetwork(ctx, apiClient, name, resourceWaitTimeout)
		})
	}

	for _, secret := range secrets {
		g.Go(func() error {
			return waitForSecret(ctx, apiClient, secret.Name, resourceWaitTimeout)
		})
	}

	for _, config := range configs {
		g.Go(func() error {
			return waitForConfig(ctx, apiClient, config.Name, resourceWaitTimeout)
		})
	}

	return g.Wait()
}
