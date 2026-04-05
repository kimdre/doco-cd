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

func TestShouldCheckImageUpdates(t *testing.T) {
	t.Run("checks image updates when force image pull enabled", func(t *testing.T) {
		if !shouldCheckImageUpdates(true, false) {
			t.Fatal("expected pre-deploy image updates to be checked")
		}
	})

	t.Run("skips image updates when force recreate enabled", func(t *testing.T) {
		if shouldCheckImageUpdates(true, true) {
			t.Fatal("expected pre-deploy image updates to be skipped when force recreate is enabled")
		}
	})
}
