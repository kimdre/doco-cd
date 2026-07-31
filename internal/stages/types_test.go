package stages

import (
	"slices"
	"testing"

	"github.com/kimdre/doco-cd/internal/docker"
)

func TestChangedServiceNames(t *testing.T) {
	t.Parallel()

	t.Run("nil state returns nil", func(t *testing.T) {
		t.Parallel()

		var d *DeploymentState
		if got := d.changedServiceNames(); got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})

	t.Run("empty state returns empty", func(t *testing.T) {
		t.Parallel()

		if got := (&DeploymentState{}).changedServiceNames(); len(got) != 0 {
			t.Errorf("expected no names, got %v", got)
		}
	})

	t.Run("flattens, dedupes and sorts", func(t *testing.T) {
		t.Parallel()

		d := &DeploymentState{changedServices: []docker.Change{
			{Type: "mounts", Services: []string{"worker", "web"}},
			{Type: "mismatch", Services: []string{"web", "caddy"}},
		}}

		expected := []string{"caddy", "web", "worker"}
		if got := d.changedServiceNames(); !slices.Equal(got, expected) {
			t.Errorf("expected %v, got %v", expected, got)
		}
	})
}
