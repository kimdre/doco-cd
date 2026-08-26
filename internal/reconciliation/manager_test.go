package reconciliation

import (
	"testing"

	"github.com/docker/compose/v5/pkg/api"
)

// TestSchedulerStopHold_IsolatedAcrossContexts verifies that MarkSchedulerStopHeld/
// UnmarkSchedulerStopHeld/isServiceSchedulerStopHeld key holds by Docker context, so
// the same compose project/service name on two different Docker contexts are tracked
// independently and never suppress each other's reconciliation events.
func TestSchedulerStopHold_IsolatedAcrossContexts(t *testing.T) {
	r := newReconciliation()

	attrs := map[string]string{
		api.ProjectLabel: "proj",
		api.ServiceLabel: "db",
	}

	r.markSchedulerStopHeld("", "proj", "db")

	if !r.isServiceSchedulerStopHeld("", attrs) {
		t.Fatal("expected service to be held stopped on the default context")
	}

	if r.isServiceSchedulerStopHeld("remote", attrs) {
		t.Fatal("expected hold on the default context to not be visible on the \"remote\" context")
	}

	r.markSchedulerStopHeld("remote", "proj", "db")

	if !r.isServiceSchedulerStopHeld("remote", attrs) {
		t.Fatal("expected service to be held stopped on the \"remote\" context")
	}

	// Releasing the default context's hold must not affect the remote context's hold.
	// (unmarkSchedulerStopHeld keeps a short grace-period entry alive after the
	// last release, so isServiceSchedulerStopHeld still reports true briefly —
	// see schedulerStopHoldGracePeriod — but this must remain scoped to the
	// default context and not leak into the remote context's independent hold.)
	r.unmarkSchedulerStopHeld("", "proj", "db")

	if !r.isServiceSchedulerStopHeld("remote", attrs) {
		t.Fatal("expected remote context hold to remain held after releasing the unrelated default context hold")
	}
}

// TestSchedulerHeldServiceKey_DiffersByContext ensures the internal key builder used
// for the scheduler stop-hold map includes the Docker context, so same-named
// project/service pairs on different contexts never collide in the map.
func TestSchedulerHeldServiceKey_DiffersByContext(t *testing.T) {
	defaultKey := schedulerHeldServiceKey("", "proj", "db")
	explicitDefaultKey := schedulerHeldServiceKey("default", "proj", "db")
	remoteKey := schedulerHeldServiceKey("remote", "proj", "db")

	if defaultKey != explicitDefaultKey {
		t.Fatalf("schedulerHeldServiceKey() produced different keys for default context aliases: %q and %q", defaultKey, explicitDefaultKey)
	}

	if defaultKey == remoteKey {
		t.Fatalf("schedulerHeldServiceKey() produced the same key %q for different contexts", defaultKey)
	}
}

func TestStackDeploymentKey_NormalizesDefaultContext(t *testing.T) {
	defaultKey := stackDeploymentKey("repo", "", "stack")
	explicitDefaultKey := stackDeploymentKey("repo", "default", "stack")
	remoteKey := stackDeploymentKey("repo", "remote", "stack")

	if defaultKey != explicitDefaultKey {
		t.Fatalf("stackDeploymentKey() produced different keys for default context aliases: %q and %q", defaultKey, explicitDefaultKey)
	}

	if defaultKey == remoteKey {
		t.Fatalf("stackDeploymentKey() produced the same key %q for different contexts", defaultKey)
	}
}
