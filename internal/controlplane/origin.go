package controlplane

import (
	"context"

	"github.com/kimdre/doco-cd/internal/syncwindow"
)

type deploymentOriginKey struct{}

// WithDeploymentOrigin returns a context whose deployments are attributed to
// origin, which decides how sync windows apply to them. It is carried on the
// context because poll runs reach Deployment.Deploy through the PollRunner,
// which does not know what started the run.
func WithDeploymentOrigin(ctx context.Context, origin syncwindow.Origin) context.Context {
	return context.WithValue(ctx, deploymentOriginKey{}, origin)
}

// DeploymentOrigin returns the origin set by WithDeploymentOrigin, or
// syncwindow.OriginAutomatic if there is none.
func DeploymentOrigin(ctx context.Context) syncwindow.Origin {
	if origin, ok := ctx.Value(deploymentOriginKey{}).(syncwindow.Origin); ok && origin != "" {
		return origin
	}

	return syncwindow.OriginAutomatic
}
