//go:build e2e

package e2e

import (
	"testing"
	"time"
)

func TestMultiContextSameNameDeployment(t *testing.T) {
	t.Parallel()

	h := NewHarness(t, "multi-context")
	h.EnableRemoteContext()
	h.Start()

	h.WaitFor(2*time.Minute, "same-named stack deployed on default context", func() bool {
		return h.ContainerID("e2e-multi-context", "app") != ""
	})
	h.WaitFor(2*time.Minute, "same-named stack deployed on remote context", func() bool {
		return h.RemoteContainerID("e2e-multi-context", "app") != ""
	})
}
