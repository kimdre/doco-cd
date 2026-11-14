package options

import (
	"context"
	"log/slog"
	"time"

	"github.com/docker/cli/opts"
	"github.com/moby/moby/api/types/swarm"
)

// Deploy holds docker stack deploy options.
type Deploy struct {
	Composefiles     []string
	Namespace        string
	ResolveImage     string
	SendRegistryAuth bool
	Prune            bool
	Detach           bool
	Quiet            bool
	Environment      map[string]string
	Timeout          time.Duration
	Logger           *slog.Logger
	// ForceUpdateServices names the services whose unchanged tasks must be recreated.
	ForceUpdateServices []string
	// SelfService is this instance's full Swarm service name. An accepted update
	// can replace this process, so later cancellation is a handover.
	SelfService string
	// PrepareSelfUpdate persists reporting metadata before the manager can replace this task.
	PrepareSelfUpdate func(context.Context, swarm.Service, *swarm.ServiceSpec, []string) error
}

// Config holds docker stack config options.
type Config struct {
	Composefiles      []string
	SkipInterpolation bool
}

// List holds docker stack ls options.
type List struct {
	Format        string
	AllNamespaces bool
}

// PS holds docker stack ps options.
type PS struct {
	Filter    opts.FilterOpt
	Namespace string
	Format    string
	NoTrunc   bool
	NoResolve bool
	Quiet     bool
}

// Remove holds docker stack remove options.
type Remove struct {
	Namespaces []string
	Detach     bool
	Logger     *slog.Logger
}

// Services holds docker stack services options.
type Services struct {
	Quiet     bool
	Format    string
	Filter    opts.FilterOpt
	Namespace string
}
