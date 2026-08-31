package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
)

// resolveDockerContext returns a named context or the injected default Docker CLI.
func (h *Handler) resolveDockerContext(ctx context.Context, contextName string) (docker.ContextClient, error) {
	contextName = docker.NormalizeContextName(contextName)
	if h.contexts != nil {
		contextClient, err := h.contexts.Get(ctx, contextName)
		if err != nil {
			return docker.ContextClient{}, fmt.Errorf("failed to resolve Docker context %q: %w", docker.DisplayContextName(contextName), err)
		}

		return contextClient, nil
	}

	if contextName != "" {
		return docker.ContextClient{}, fmt.Errorf("unknown Docker context: %s", docker.DisplayContextName(contextName))
	}

	if h.dockerCli == nil {
		return docker.ContextClient{}, errors.New("docker cli is required")
	}

	return docker.ContextClient{Name: "", Cli: h.dockerCli}, nil
}

// resolveSwarmDockerContext resolves a context and verifies that it supports Swarm operations.
func (h *Handler) resolveSwarmDockerContext(ctx context.Context, contextName string) (docker.ContextClient, error) {
	contextClient, err := h.resolveDockerContext(ctx, contextName)
	if err != nil {
		return docker.ContextClient{}, err
	}

	if err := requireDockerSwarm(ctx, contextClient); err != nil {
		return docker.ContextClient{}, err
	}

	return contextClient, nil
}

// requireDockerSwarm checks named-context state or probes the default daemon directly.
func requireDockerSwarm(ctx context.Context, contextClient docker.ContextClient) error {
	enabled := contextClient.SwarmMode
	if contextClient.Name == "" {
		var err error

		enabled, err = swarm.ResolveModeEnabled(ctx, contextClient.Cli.Client())
		if err != nil {
			return fmt.Errorf("failed to check Docker Swarm mode: %w", err)
		}
	}

	if !enabled {
		return errors.New("swarm features are disabled or the Docker daemon is not an active swarm manager")
	}

	return nil
}
