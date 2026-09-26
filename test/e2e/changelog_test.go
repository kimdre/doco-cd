//go:build e2e

package e2e

import (
	"slices"
	"strings"
	"testing"
	"time"
)

const changelogTimeout = 120 * time.Second

// TestChangelog verifies which commits a deploy notification lists, on the repository
// layouts real deployment repos have. The daemon runs with its data on a named volume, so
// its repository path inside the container differs from the one on the docker host,
// like any real install.
func TestChangelog(t *testing.T) {
	t.Parallel()

	// One stack, its image tag in .doco-cd.yml and bumped by CI. The roll commit touches
	// nothing but the deploy config, and that is the commit the notification must name.
	t.Run("deploy_config", func(t *testing.T) {
		t.Parallel()

		const stack = "e2e-changelog-deploy-config"

		h := NewHarness(t, "changelog-deploy-config")
		h.CaptureNotifications()
		h.Start()

		assertChangelog(t, h.NextNotification(stack, changelogTimeout), nil)

		h.ReplaceInWorktree("backend/VERSION", "v1", "v2")
		h.RepoPush("backend: v2")
		h.ReplaceInWorktree(".doco-cd.yml", `APP_TAG: "3.21"`, `APP_TAG: "3.22"`)
		h.RepoPush("deploy: roll app to 3.22")

		assertChangelog(t, h.NextNotification(stack, changelogTimeout), []string{"deploy: roll app to 3.22"})

		h.ReplaceInWorktree("deploy/app/compose.yaml", `["sleep", "infinity"]`, `["sleep", "86400"]`)
		h.RepoPush("compose: sleep a day")

		assertChangelog(t, h.NextNotification(stack, changelogTimeout), []string{"compose: sleep a day"})
	})

	// Per-box deploy configs with several stacks each, a stack whose compose is included
	// from shared/ of the same repository, and tag bumps landing as pull request merges.
	t.Run("pull_request", func(t *testing.T) {
		t.Parallel()

		const (
			app   = "e2e-changelog-box1-app"
			agent = "e2e-changelog-box1-agent"
		)

		h := NewHarness(t, "changelog-pull-request")
		h.SetPollTarget("box1")
		h.CaptureNotifications()
		h.Start()

		assertChangelog(t, h.NextNotification(app, changelogTimeout), nil)
		assertChangelog(t, h.NextNotification(agent, changelogTimeout), nil)

		// The other box's deploy config is not this stack's, its own is.
		h.ReplaceInWorktree(".doco-cd.box2.yml", `APP_TAG: "3.21"`, `APP_TAG: "3.22"`)
		h.RepoPush("box2: roll app to 3.22")
		h.ReplaceInWorktree(".doco-cd.box1.yml", `APP_TAG: "3.21"`, `APP_TAG: "3.22"`)
		h.RepoPush("box1: roll app to 3.22")

		assertChangelog(t, h.NextNotification(app, changelogTimeout), []string{"box1: roll app to 3.22"})

		// A Renovate branch forks, main moves on, then the pull request is merged. The
		// branch commit is the change, the merge and the unrelated main commit are not.
		h.RepoCheckout("renovate/agent", true)
		h.ReplaceInWorktree(".doco-cd.box1.yml", `AGENT_TAG: "3.21"`, `AGENT_TAG: "3.22"`)
		h.RepoPush("chore(deps): update agent to 3.22")
		h.RepoCheckout("main", false)
		h.ReplaceInWorktree("README.md", "# boxes", "# boxes, two of them")
		h.RepoPush("docs: count the boxes")
		h.RepoMerge("renovate/agent", "Merge pull request #1 from renovate/agent")

		// The agent last deployed before the app's roll, and both stacks share the deploy
		// config, so that roll is in the agent's range too. Listing it is the accepted
		// trade-off of attributing a whole deploy config file to each of its stacks.
		assertChangelog(t, h.NextNotification(agent, changelogTimeout), []string{
			"chore(deps): update agent to 3.22",
			"box1: roll app to 3.22",
		})

		if n := h.StackNotifications("e2e-changelog-box2-app"); len(n) != 0 {
			t.Fatalf("stack of another target was deployed: %+v", n)
		}
	})
}

// assertChangelog checks that n reports a completed deployment listing exactly want.
func assertChangelog(t *testing.T, n Notification, want []string) {
	t.Helper()

	if !strings.Contains(n.Title, "Deployment completed") {
		t.Fatalf("notification of %s is %q, want a completed deployment", n.Stack, n.Title)
	}

	if !slices.Equal(n.Commits, want) {
		t.Fatalf("changelog of %s\n got: %q\nwant: %q", n.Stack, n.Commits, want)
	}
}
