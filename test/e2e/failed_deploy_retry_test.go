//go:build e2e

package e2e

import (
	"testing"
	"time"
)

// TestFailedDeployRetry covers discussion #1702: a deployment that fails
// AFTER containers were created (post_start hook exits 1) must be recorded,
// retried on every poll with a full recreate, and the record must clear on
// the first success. Before the fix the daemon logged "no changes detected,
// skipping deployment" forever and the failure was reported exactly once.
//
// Ported from scenarios/failed-deploy-retry/run.sh.
func TestFailedDeployRetry(t *testing.T) {
	t.Parallel()

	h := NewHarness(t, "failed-deploy-retry")
	if h.isSwarmMode() {
		t.Skip("post_start hooks are not supported by Docker Swarm")
	}

	h.Start()

	h.WaitForLog("deployment failed", 120*time.Second)

	cidAfterFail := h.ContainerID("e2e-retry", "app")
	if cidAfterFail == "" {
		t.Fatal("app container must exist after the failed deploy (hook fails after start)")
	}

	// the regression: same commit must NOT be skipped as already deployed
	h.WaitForLog("last deployment attempt failed, retrying", 60*time.Second)
	h.WaitForContainerRecreate("e2e-retry", "app", cidAfterFail, 90*time.Second)

	// fix the hook and push. Success must clear the record: the first "no
	// changes" skip in the whole log proves both the successful deploy and
	// the cleared record - with the record present the daemon never skips.
	h.ReplaceInWorktree(".doco-cd.yml", `HOOK_EXIT: "1"`, `HOOK_EXIT: "0"`)
	h.RepoPush("fix the hook")

	h.WaitForLog("no changes detected, skipping deployment", 120*time.Second)

	cidOK := h.ContainerID("e2e-retry", "app")
	if cidOK == "" {
		t.Fatal("app container must run after the successful deploy")
	}

	time.Sleep(25 * time.Second) // two poll cycles

	if got := h.ContainerID("e2e-retry", "app"); got != cidOK {
		t.Fatalf("stack must stay untouched once the deploy succeeded: got %q, want %q", got, cidOK)
	}
}
