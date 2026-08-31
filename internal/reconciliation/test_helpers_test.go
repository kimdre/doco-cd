package reconciliation

import "testing"

func newTestManager(t *testing.T) *Manager {
	t.Helper()

	manager, err := NewManager(Dependencies{})
	if err != nil {
		t.Fatalf("failed to create reconciliation manager: %v", err)
	}

	t.Cleanup(manager.Close)

	return manager
}

func newTestDeployerLimiter(t *testing.T, maxConcurrent uint) *DeployerLimiter {
	t.Helper()

	limiter := NewDeployerLimiter(maxConcurrent)
	t.Cleanup(limiter.Close)

	return limiter
}
