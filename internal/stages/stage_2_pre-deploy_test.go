package stages

import (
	"testing"

	"github.com/kimdre/doco-cd/internal/docker"
)

func TestShouldSkipDeployment(t *testing.T) {
	t.Run("skips when nothing changed and force recreate disabled", func(t *testing.T) {
		if !shouldSkipDeployment(false, nil, docker.IgnoredInfo{}, false, false) {
			t.Fatal("expected deployment to be skipped")
		}
	})

	t.Run("does not skip when force recreate enabled", func(t *testing.T) {
		if shouldSkipDeployment(false, nil, docker.IgnoredInfo{}, false, true) {
			t.Fatal("expected deployment to proceed when force recreate is enabled")
		}
	})
}
