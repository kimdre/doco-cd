//go:build e2e

package e2e

import (
	"testing"
	"time"

	"github.com/moby/moby/client"
)

func TestPostInitOneShot(t *testing.T) {
	t.Parallel()

	h := NewHarness(t, "post-init-one-shot")
	if h.isSwarmMode() {
		t.Skip("one-shot Compose services are not supported by Docker Swarm")
	}

	h.Start()

	h.WaitForLog(`"msg":"job completed successfully"`, 2*time.Minute)

	appID := h.ContainerID("e2e-post-init-one-shot", "app")
	if appID == "" || !h.ContainerRunning(appID) {
		t.Fatal("app container must be running after the successful deployment")
	}

	h.WaitFor(30*time.Second, "post-init service exit code 0", func() bool {
		containers, err := h.docker.ContainerList(h.ctx, client.ContainerListOptions{
			All:     true,
			Filters: h.containerFilters(false, "e2e-post-init-one-shot", "post-init"),
		})
		if err != nil || len(containers.Items) != 1 {
			return false
		}

		inspect, err := h.docker.ContainerInspect(h.ctx, containers.Items[0].ID, client.ContainerInspectOptions{})

		return err == nil && inspect.Container.State != nil && !inspect.Container.State.Running &&
			inspect.Container.State.ExitCode == 0
	})
}
