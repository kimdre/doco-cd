package selfupdate

import (
	"fmt"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
)

// Strategy is how doco-cd replaces its own container.
type Strategy string

const (
	// StrategyAuto picks scale_out unless a constraint forces applier.
	StrategyAuto Strategy = "auto"
	// StrategyScaleOut starts a second container, health-checks it, then hands over.
	StrategyScaleOut Strategy = "scale_out"
	// StrategyApplier delegates the replacement to a throwaway clone.
	StrategyApplier Strategy = "applier"
)

// Constraints are runtime facts the selector cannot read off the service.
type Constraints struct {
	// NetworkDrift is true when a project network must be recreated, which
	// would stop every attached container including this one.
	NetworkDrift bool
}

// Select decides the strategy and returns the reasons that ruled out scale-out.
func Select(svc types.ServiceConfig, sourceType, contextName string, requested Strategy, c Constraints) (Strategy, []string, error) {
	if strings.EqualFold(sourceType, "oci") {
		return "", nil, fmt.Errorf("%w: the self stack comes from an OCI source", ErrUnsupported)
	}

	if contextName != "" && contextName != "default" {
		return "", nil, fmt.Errorf("%w: the self stack must run on the default Docker context, got %q", ErrUnsupported, contextName)
	}

	if scale := svc.GetScale(); scale != 1 {
		return "", nil, fmt.Errorf("%w: the doco-cd service must run exactly one replica, got %d", ErrUnsupported, scale)
	}

	if !hasRestartPolicy(svc) {
		return "", nil, fmt.Errorf("%w: the doco-cd service needs a restart policy (always, unless-stopped or on-failure)", ErrUnsupported)
	}

	var reasons []string

	if svc.ContainerName != "" {
		reasons = append(reasons, "container_name is set")
	}

	if len(svc.Ports) > 0 {
		reasons = append(reasons, "the service publishes host ports")
	}

	if svc.NetworkMode == "host" {
		reasons = append(reasons, "network_mode is host")
	}

	if c.NetworkDrift {
		reasons = append(reasons, "a project network must be recreated")
	}

	switch requested {
	case StrategyApplier:
		return StrategyApplier, reasons, nil
	case StrategyScaleOut:
		if len(reasons) > 0 {
			return "", reasons, fmt.Errorf("%w: scale_out was requested but %s", ErrUnsupported, strings.Join(reasons, "; "))
		}

		return StrategyScaleOut, nil, nil
	case StrategyAuto, "":
		if len(reasons) > 0 {
			return StrategyApplier, reasons, nil
		}

		return StrategyScaleOut, nil, nil
	default:
		return "", nil, fmt.Errorf("%w: unknown strategy %q", ErrUnsupported, requested)
	}
}

// hasRestartPolicy reports whether Docker will bring the container back after a
// self-exit, which every recovery path depends on.
func hasRestartPolicy(svc types.ServiceConfig) bool {
	policy := svc.Restart

	if svc.Deploy != nil && svc.Deploy.RestartPolicy != nil && svc.Deploy.RestartPolicy.Condition != "" {
		switch svc.Deploy.RestartPolicy.Condition {
		case "any", "on-failure":
			return true
		}
	}

	switch {
	case policy == "always", policy == "unless-stopped":
		return true
	case strings.HasPrefix(policy, "on-failure"):
		return true
	default:
		return false
	}
}
