package swarm

import (
	"github.com/docker/docker/api/types/swarm"
)

// Service represents a service.
type Service struct {
	ID string
	swarm.Meta
	Spec         swarm.ServiceSpec   `json:",omitempty"`
	PreviousSpec *swarm.ServiceSpec  `json:",omitempty"`
	Endpoint     swarm.Endpoint      `json:",omitempty"`
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
