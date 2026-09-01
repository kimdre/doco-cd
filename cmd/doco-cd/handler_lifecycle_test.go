package main

import (
	"context"
	"testing"

	"github.com/kimdre/doco-cd/internal/controlplane"
)

func TestDeploymentErrorPreservesLifecycleCancellation(t *testing.T) {
	t.Parallel()

	err := controlplane.DeploymentError{Msg: "deployment failed", Err: context.Canceled, HTTPStatusCode: 500}
	if !controlplane.IsLifecycleCancellation(err) {
		t.Fatalf("DeploymentError does not preserve cancellation identity: %v", err)
	}
}
