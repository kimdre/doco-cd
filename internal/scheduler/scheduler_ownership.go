package scheduler

import (
	"fmt"

	"github.com/kimdre/doco-cd/internal/docker"
)

// OwnershipOptions determines which jobs this instance schedules on each context.
type OwnershipOptions struct {
	InstanceID           string
	RequireOwnerContexts []string
}

// decision returns whether this instance is allowed to schedule a job with the given
// owner label on the given context, and if not, a reason why.
func (o OwnershipOptions) decision(contextName, owner string) (bool, string) {
	if owner != "" {
		if owner != o.InstanceID {
			return false, fmt.Sprintf("job is owned by scheduler instance %q", owner)
		}

		return true, ""
	}

	for _, name := range o.RequireOwnerContexts {
		if docker.NormalizeContextName(name) == docker.NormalizeContextName(contextName) {
			return false, "job has no owner on a context requiring ownership"
		}
	}

	return true, ""
}
