package main

import (
	"context"
	"testing"

	"github.com/kimdre/doco-cd/internal/controlplane"
)

func TestHandleErrorPreservesLifecycleCancellation(t *testing.T) {
	t.Parallel()

	err := handleError{msg: "deployment failed", err: context.Canceled, httpStatusCode: 500}
	if !controlplane.IsLifecycleCancellation(err) {
		t.Fatalf("handleError does not preserve cancellation identity: %v", err)
	}
}
