package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/docker/cli/cli/command"
	dockerswarmtypes "github.com/moby/moby/api/types/swarm"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/restapi"
)

type StackActionResult struct {
	Service string `json:"service"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
}

var (
	ErrStackNotFound             = errors.New("stack not found")
	ErrNoApplicableStackServices = errors.New("no applicable services found")
)

type StackServiceNotFoundError struct {
	Service string
}

func (e *StackServiceNotFoundError) Error() string {
	return "service not found: " + e.Service
}

type StackServiceActionError struct {
	Service string
	Cause   error
}

func (e *StackServiceActionError) Error() string {
	return fmt.Sprintf("stack action failed for service %s: %v", e.Service, e.Cause)
}

func (e *StackServiceActionError) Unwrap() error {
	return e.Cause
}

func GetStackServices(ctx context.Context, dockerCLI command.Cli, stack string) ([]dockerswarmtypes.Service, error) {
	if dockerCLI == nil {
		return nil, errors.New("docker cli is required")
	}

	services, err := swarm.GetStackServices(ctx, dockerCLI.Client(), stack)
	if err != nil {
		return nil, err
	}

	if len(services) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrStackNotFound, stack)
	}

	return services, nil
}

func RunStackAction(
	ctx context.Context,
	dockerCLI command.Cli,
	stack string,
	action string,
	service string,
	replicas int,
	wait bool,
	log *slog.Logger,
) ([]StackActionResult, error) {
	services, err := GetStackServices(ctx, dockerCLI, stack)
	if err != nil {
		return nil, err
	}

	return RunStackActionOnServices(ctx, dockerCLI, services, stack, action, service, replicas, wait, log)
}

func RunStackActionOnServices(
	ctx context.Context,
	dockerCLI command.Cli,
	services []dockerswarmtypes.Service,
	stack string,
	action string,
	service string,
	replicas int,
	wait bool,
	log *slog.Logger,
) ([]StackActionResult, error) {
	if action == "scale" && replicas < 0 {
		return nil, errors.New("'replicas' parameter is required and must be a non-negative integer")
	}

	if action != "scale" && action != "restart" && action != "run" {
		return nil, fmt.Errorf("%w: %s", restapi.ErrInvalidAction, action)
	}

	results := make([]StackActionResult, 0, len(services))
	matched := false
	succeeded := false

	fullServiceName := ""
	if service != "" {
		fullServiceName = stack + "_" + service
	}

	for _, svc := range services {
		svcName := svc.Spec.Name
		if fullServiceName != "" && svcName != fullServiceName {
			continue
		}

		matched = true

		result := StackActionResult{Service: svcName, Status: "ok"}

		var err error

		switch action {
		case "scale":
			log.Info("scaling service", slog.String("service", svcName), slog.Int("replicas", replicas))

			err = swarm.ScaleService(ctx, dockerCLI, svcName, uint64(replicas), wait, false) // #nosec G115 -- replicas is validated as non-negative above.
			if errors.Is(err, swarm.ErrNotReplicatedService) {
				result.Status = "skipped"
				result.Reason = swarm.ErrNotReplicatedService.Error()
			}
		case "restart":
			if svc.Spec.Mode.ReplicatedJob != nil || svc.Spec.Mode.GlobalJob != nil {
				result.Status = "skipped"
				result.Reason = docker.ErrJobServiceRestartNotSupported.Error()
				err = docker.ErrJobServiceRestartNotSupported
			} else {
				log.Info("restarting service", slog.String("service", svcName))

				err = docker.RestartService(ctx, dockerCLI.Client(), svcName)
				if errors.Is(err, docker.ErrJobServiceRestartNotSupported) {
					result.Status = "skipped"
					result.Reason = docker.ErrJobServiceRestartNotSupported.Error()
				}
			}
		case "run":
			log.Info("retriggering job service", slog.String("service", svcName))

			err = docker.RerunJobService(ctx, dockerCLI.Client(), svcName)
			if errors.Is(err, docker.ErrNotAJobService) {
				result.Status = "skipped"
				result.Reason = docker.ErrNotAJobService.Error()
			}
		}

		if err != nil && result.Status != "skipped" {
			return results, &StackServiceActionError{Service: svcName, Cause: err}
		}

		if result.Status == "skipped" {
			log.Debug("skipping service for stack action", slog.String("service", svcName), slog.String("action", action), slog.String("reason", result.Reason))
		}

		results = append(results, result)
		if result.Status == "ok" {
			succeeded = true
		}
	}

	if !matched {
		return results, &StackServiceNotFoundError{Service: fullServiceName}
	}

	if !succeeded {
		return results, ErrNoApplicableStackServices
	}

	return results, nil
}

func RemoveStack(ctx context.Context, dockerCLI command.Cli, stack string, log *slog.Logger) error {
	if dockerCLI == nil {
		return errors.New("docker cli is required")
	}

	log.Info("removing stack", slog.String("stack", stack))

	return docker.RemoveSwarmStack(ctx, dockerCLI, stack)
}
