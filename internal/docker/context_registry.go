package docker

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/docker/cli/cli/command"
	contextstore "github.com/docker/cli/cli/context/store"
	"github.com/moby/moby/client"

	dockerSwarm "github.com/kimdre/doco-cd/internal/docker/swarm"
)

const DefaultContextName = "default"

var (
	ErrDockerContextNotFound = errors.New("docker context not found")
	ErrContextRegistryClosed = errors.New("docker context registry is closed")
)

type ContextClient struct {
	Name      string
	Cli       command.Cli
	SwarmMode bool
}

func (c ContextClient) DisplayName() string {
	return DisplayContextName(c.Name)
}

type ContextClientResult struct {
	ContextClient
	Err error
}

type ContextRegistry struct {
	mu sync.RWMutex

	baseCli command.Cli
	quiet   bool
	closed  bool

	available map[string]struct{}
	clients   map[string]command.Cli

	listContexts func() ([]contextstore.Metadata, error)
	createCli    func(bool, string) (command.Cli, error)
	resolveSwarm func(context.Context, client.APIClient) (bool, error)
}

func NewContextRegistry(baseCli command.Cli, quiet bool) *ContextRegistry {
	registry := &ContextRegistry{
		baseCli:      baseCli,
		quiet:        quiet,
		available:    map[string]struct{}{"": {}},
		clients:      make(map[string]command.Cli),
		createCli:    CreateDockerCliWithContext,
		resolveSwarm: dockerSwarm.ResolveModeEnabled,
	}

	if baseCli != nil {
		registry.listContexts = func() ([]contextstore.Metadata, error) {
			return baseCli.ContextStore().List()
		}
	}

	return registry
}

func NormalizeContextName(name string) string {
	name = strings.TrimSpace(name)
	if strings.EqualFold(name, DefaultContextName) {
		return ""
	}

	return name
}

func DisplayContextName(name string) string {
	name = NormalizeContextName(name)
	if name == "" {
		return DefaultContextName
	}

	return name
}

func (r *ContextRegistry) Refresh() error {
	if r == nil {
		return errors.New("docker context registry is required")
	}

	r.mu.RLock()
	closed := r.closed
	listContexts := r.listContexts
	r.mu.RUnlock()

	if closed {
		return ErrContextRegistryClosed
	}

	available := map[string]struct{}{"": {}}

	if listContexts != nil {
		contexts, err := listContexts()
		if err != nil {
			return fmt.Errorf("failed to list docker contexts: %w", err)
		}

		for _, metadata := range contexts {
			name := NormalizeContextName(metadata.Name)
			if name != "" {
				available[name] = struct{}{}
			}
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return ErrContextRegistryClosed
	}

	r.available = available

	return nil
}

func (r *ContextRegistry) Names() ([]string, error) {
	if err := r.Refresh(); err != nil {
		return nil, err
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.available))
	for name := range r.available {
		names = append(names, name)
	}

	sort.Slice(names, func(i, j int) bool {
		return DisplayContextName(names[i]) < DisplayContextName(names[j])
	})

	return names, nil
}

func (r *ContextRegistry) Get(ctx context.Context, name string) (ContextClient, error) {
	name = NormalizeContextName(name)
	if name != "" {
		if err := r.Refresh(); err != nil {
			return ContextClient{}, err
		}
	}

	cli, err := r.clientForKnownContext(name)
	if err != nil {
		return ContextClient{}, err
	}

	swarmMode, err := r.resolveSwarm(ctx, cli.Client())
	if err != nil {
		return ContextClient{}, fmt.Errorf("failed to check swarm mode for docker context %q: %w", DisplayContextName(name), err)
	}

	return ContextClient{Name: name, Cli: cli, SwarmMode: swarmMode}, nil
}

func (r *ContextRegistry) List(ctx context.Context) ([]ContextClientResult, error) {
	names, err := r.Names()
	if err != nil {
		return nil, err
	}

	results := make([]ContextClientResult, 0, len(names))
	for _, name := range names {
		cli, cliErr := r.clientForKnownContext(name)
		if cliErr != nil {
			results = append(results, ContextClientResult{
				ContextClient: ContextClient{Name: name},
				Err:           cliErr,
			})

			continue
		}

		swarmMode, swarmErr := r.resolveSwarm(ctx, cli.Client())
		if swarmErr != nil {
			swarmErr = fmt.Errorf("failed to check swarm mode for docker context %q: %w", DisplayContextName(name), swarmErr)
		}

		results = append(results, ContextClientResult{
			ContextClient: ContextClient{Name: name, Cli: cli, SwarmMode: swarmMode},
			Err:           swarmErr,
		})
	}

	return results, nil
}

func (r *ContextRegistry) clientForKnownContext(name string) (command.Cli, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return nil, ErrContextRegistryClosed
	}

	if _, ok := r.available[name]; !ok {
		return nil, fmt.Errorf("%w: %s", ErrDockerContextNotFound, DisplayContextName(name))
	}

	if name == "" {
		if r.baseCli == nil {
			return nil, errors.New("default docker cli is required")
		}

		return r.baseCli, nil
	}

	if cli := r.clients[name]; cli != nil {
		return cli, nil
	}

	cli, err := r.createCli(r.quiet, name)
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client for context %q: %w", name, err)
	}

	r.clients[name] = cli

	return cli, nil
}

func (r *ContextRegistry) Close() error {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}

	r.closed = true

	clients := make([]command.Cli, 0, len(r.clients))
	for _, cli := range r.clients {
		clients = append(clients, cli)
	}

	r.clients = nil
	r.mu.Unlock()

	var errs []error

	for _, cli := range clients {
		if cli == nil || cli.Client() == nil {
			continue
		}

		if err := cli.Client().Close(); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}
