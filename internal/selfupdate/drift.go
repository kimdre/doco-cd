package selfupdate

import (
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
)

// DriftSnapshot is the state that must be recoverable if Compose replaces a
// network shared by the predecessor and other project services.
type DriftSnapshot struct {
	Networks   map[string]network.Inspect           `json:"networks"`
	Containers map[string]container.InspectResponse `json:"containers"`
}
