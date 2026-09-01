package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/kimdre/doco-cd/internal/docker"
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

// requireDockerSwarm checks the capability resolved by ContextRegistry.
func requireDockerSwarm(_ context.Context, contextClient docker.ContextClient) error {
	enabled := contextClient.SwarmMode

	if !enabled {
		return errors.New("swarm features are disabled or the Docker daemon is not an active swarm manager")
	}

	return nil
}
