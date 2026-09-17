//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// TestSelfUpdateBootstrap proves the bootstrap path adopts doco-cd into GitOps:
// one `apply-self --bootstrap` run leaves exactly one container that carries
// full compose and doco-cd labels, and the next polls see no drift.
func TestSelfUpdateBootstrap(t *testing.T) {
	t.Parallel()

	h := NewHarness(t, "self-update-bootstrap")
	h.EnableSelfUpdate("e2e-self-bootstrap", "doco-cd")
	h.Start()

	t.Cleanup(func() {
		if t.Failed() {
			h.dumpSelfState()
		}
	})

	if code := h.BootstrapExitCode(); code != 0 {
		t.Fatalf("bootstrap exit code = %d, want 0", code)
	}

	if logs := h.BootstrapLogs(); !strings.Contains(logs, "self-update: bootstrap completed") {
		t.Errorf("bootstrap did not log completion")
	}

	all := h.SelfContainers(true)
	if len(all) != 1 {
		t.Fatalf("want exactly 1 self container after bootstrap, got %d", len(all))
	}

	id := all[0].ID
	labels := h.ContainerLabels(id)
	head := h.RepoHead()

	wantLabels := map[string]string{
		"com.docker.compose.project":          "e2e-self-bootstrap",
		"com.docker.compose.service":          "doco-cd",
		"com.docker.compose.container-number": "1",
		"cd.doco.deployment.name":             "e2e-self-bootstrap",
		"cd.doco.deployment.target.sha":       head,
		"cd.doco.metadata.manager":            "doco-cd",
	}

	for key, want := range wantLabels {
		if got := labels[key]; got != want {
			t.Errorf("label %s = %q, want %q", key, got, want)
		}
	}

	if labels["com.docker.compose.config-hash"] == "" {
		t.Error("container has no compose config-hash label")
	}

	if !h.ContainerHealthy(id) {
		t.Error("the managed container is not healthy")
	}

	// Adoption must be stable: the container the bootstrap created has to match
	// what the running instance computes, or every poll would hand over.
	mark := h.LogMark()
	h.WaitForLogOccurrencesAfter("no changes detected, skipping deployment", mark, 2, 90*time.Second)

	if got := h.SelfContainerID(); got != id {
		t.Errorf("self container changed without a commit: %s -> %s", id, got)
	}

	h.assertNoSelfLeftovers()
}

// TestSelfUpdateScaleOut proves the zero-downtime path: doco-cd starts a second
// container from the new config, health-checks it while still serving, then
// hands over and the successor removes the predecessor.
func TestSelfUpdateScaleOut(t *testing.T) {
	t.Parallel()

	h := NewHarness(t, "self-update-scaleout")
	h.EnableSelfUpdate("e2e-scaleout", "doco-cd")
	h.Start()

	t.Cleanup(func() {
		if t.Failed() {
			h.dumpSelfState()
		}
	})

	oldID := h.SelfContainerID()
	if oldID == "" {
		t.Fatal("no running self container after bootstrap")
	}

	stopWatch := h.WatchAlive(oldID)
	mark := h.LogMark()

	h.ReplaceInWorktree("deploy/compose.yaml", ":v1", ":v2")
	h.RepoPush("bump the doco-cd image")

	h.WaitForLogAfter("self-update: strategy selected", mark, 2*time.Minute)

	if logs := h.logsSince(mark); !strings.Contains(logs, `"strategy":"scale_out"`) {
		t.Errorf("wanted the scale_out strategy, log says otherwise")
	}

	h.WaitForLogAfter("self-update: successor healthy, handing over", mark, 3*time.Minute)

	// The predecessor must stay healthy right up to the point it reports that
	// it finished its work. Anything earlier is real downtime.
	h.WaitForLogAfter("self-update: drained", mark, 2*time.Minute)

	if downtime := stopWatch(); len(downtime) > 0 {
		t.Errorf("predecessor stopped running %d times before it drained, first at %s", len(downtime), downtime[0])
	}

	h.WaitFor(3*time.Minute, "exactly one running self container, and it is new", func() bool {
		running := h.SelfContainers(false)

		return len(running) == 1 && running[0].ID != oldID
	})

	newID := h.SelfContainerID()

	if got := h.ContainerImage(newID); !strings.HasSuffix(got, ":v2") {
		t.Errorf("successor image = %q, want a :v2 tag", got)
	}

	labels := h.ContainerLabels(newID)
	if got := labels["com.docker.compose.container-number"]; got != "2" {
		t.Errorf("successor container-number = %q, want %q", got, "2")
	}

	if got := labels["cd.doco.deployment.target.sha"]; got != h.RepoHead() {
		t.Errorf("successor commit label = %q, want %q", got, h.RepoHead())
	}

	h.WaitForLogAfter("self-update finalised", mark, 2*time.Minute)

	h.WaitFor(time.Minute, "the predecessor is gone", func() bool {
		return len(h.SelfContainers(true)) == 1
	})

	if n := h.LogCountAfter("self-update finalised", mark); n != 1 {
		t.Errorf("finalised %d times, want exactly 1", n)
	}

	if n := len(h.SelfAppliers(true)); n != 0 {
		t.Errorf("found %d applier containers in the scale-out path, want 0", n)
	}

	// The handover must settle: a stable stack reports no drift on later polls.
	mark2 := h.LogMark()
	h.WaitForLogOccurrencesAfter("no changes detected, skipping deployment", mark2, 2, 90*time.Second)

	if got := h.SelfContainerID(); got != newID {
		t.Errorf("self container changed again without a commit: %s -> %s", newID, got)
	}

	h.assertNoSelfLeftovers()
}

// TestSelfUpdateApplier proves the fallback path: a container_name rules out
// scale-out, so doco-cd clones itself into a throwaway applier that performs
// the replacement from outside and then disappears.
func TestSelfUpdateApplier(t *testing.T) {
	t.Parallel()

	h := NewHarness(t, "self-update-applier")
	h.EnableSelfUpdate("e2e-applier", "doco-cd")
	h.Start()

	t.Cleanup(func() {
		if t.Failed() {
			h.dumpSelfState()
		}
	})

	oldID := h.SelfContainerID()
	if oldID == "" {
		t.Fatal("no running self container after bootstrap")
	}

	mark := h.LogMark()

	h.ReplaceInWorktree("deploy/compose.yaml", ":v1", ":v2")
	h.RepoPush("bump the doco-cd image")

	h.WaitForLogAfter("self-update: strategy selected", mark, 2*time.Minute)

	logs := h.logsSince(mark)
	if !strings.Contains(logs, `"strategy":"applier"`) {
		t.Errorf("wanted the applier strategy, log says otherwise")
	}

	if !strings.Contains(logs, "container_name is set") {
		t.Errorf("the applier strategy was not attributed to container_name")
	}

	h.WaitFor(2*time.Minute, "an applier container appears", func() bool {
		return len(h.SelfAppliers(true)) == 1
	})

	applierID := h.SelfAppliers(true)[0].ID

	if labels := h.ContainerLabels(applierID); labels["com.docker.compose.project"] != "" {
		t.Error("the applier carries compose labels and could be mistaken for a replica")
	}

	// The successor removes the applier as part of finalising, so "gone" is as
	// good an outcome as "exited 0" - both mean it did its job and stopped.
	h.WaitFor(3*time.Minute, "the applier finished and did not fail", func() bool {
		code, stopped := h.ContainerExitCode(applierID)
		if stopped {
			if code != 0 {
				t.Errorf("applier exited with code %d", code)
			}

			return true
		}

		return !h.ContainerExists(applierID)
	})

	h.WaitFor(2*time.Minute, "exactly one running self container, and it is new", func() bool {
		running := h.SelfContainers(false)

		return len(running) == 1 && running[0].ID != oldID && h.ContainerHealthy(running[0].ID)
	})

	newID := h.SelfContainerID()

	if got := h.ContainerImage(newID); !strings.HasSuffix(got, ":v2") {
		t.Errorf("successor image = %q, want a :v2 tag", got)
	}

	if names := h.SelfContainers(false)[0].Names; len(names) == 0 || names[0] != "/e2e-self-applier-doco-cd" {
		t.Errorf("successor container name = %v, want the fixture's container_name", names)
	}

	h.WaitForLogAfter("self-update finalised", mark, 2*time.Minute)

	h.WaitFor(2*time.Minute, "the applier is cleaned up", func() bool {
		return len(h.SelfAppliers(true)) == 0
	})

	if n := len(h.SelfContainers(true)); n != 1 {
		t.Errorf("stack has %d self containers at rest, want 1", n)
	}

	mark2 := h.LogMark()
	h.WaitForLogOccurrencesAfter("no changes detected, skipping deployment", mark2, 2, 90*time.Second)

	if got := h.SelfContainerID(); got != newID {
		t.Errorf("self container changed again without a commit: %s -> %s", newID, got)
	}

	h.assertNoSelfLeftovers()
}

// TestSelfUpdateRollback proves a broken new version never takes the instance
// down: scale-out keeps the predecessor serving and throws the successor away,
// the applier restores the predecessor. Both then refuse to retry that commit.
func TestSelfUpdateRollback(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		scenario     string
		stack        string
		wantStrategy string
		// zeroDowntime is true where the predecessor never stops serving.
		zeroDowntime bool
	}{
		{
			name:         "scale out keeps serving",
			scenario:     "self-update-rollback-scaleout",
			stack:        "e2e-rollback-scaleout",
			wantStrategy: "scale_out",
			zeroDowntime: true,
		},
		{
			name:         "applier restores the previous container",
			scenario:     "self-update-rollback-applier",
			stack:        "e2e-rollback-applier",
			wantStrategy: "applier",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := NewHarness(t, tt.scenario)
			h.EnableSelfUpdate(tt.stack, "doco-cd")
			h.Start()

			t.Cleanup(func() {
				if t.Failed() {
					h.dumpSelfState()
				}
			})

			oldID := h.SelfContainerID()
			if oldID == "" {
				t.Fatal("no running self container after bootstrap")
			}

			var stopWatch func() []time.Time
			if tt.zeroDowntime {
				stopWatch = h.WatchAlive(oldID)
			}

			mark := h.LogMark()

			// HTTP_PORT does not fit a uint16, so the new container fails
			// config validation at boot and never reports healthy.
			h.ReplaceInWorktree("deploy/compose.yaml", `E2E_GENERATION: "1"`,
				"E2E_GENERATION: \"2\"\n      HTTP_PORT: \"99999\"")
			h.RepoPush("break the doco-cd config")

			h.WaitForLogAfter("self-update: strategy selected", mark, 2*time.Minute)

			if logs := h.logsSince(mark); !strings.Contains(logs, `"strategy":"`+tt.wantStrategy+`"`) {
				t.Errorf("wanted the %s strategy, log says otherwise", tt.wantStrategy)
			}

			if tt.zeroDowntime {
				h.WaitForLogAfter("self-update rolled back, this instance keeps running", mark, 4*time.Minute)

				if downtime := stopWatch(); len(downtime) > 0 {
					t.Errorf("predecessor went unhealthy %d times during a rollback, first at %s",
						len(downtime), downtime[0])
				}

				if got := h.SelfContainerID(); got != oldID {
					t.Errorf("self container changed during a rollback: %s -> %s", oldID, got)
				}
			} else {
				h.WaitForLogAfter("self-update: rolled back", mark, 4*time.Minute)

				h.WaitFor(2*time.Minute, "the previous container is serving again", func() bool {
					running := h.SelfContainers(false)

					return len(running) == 1 && h.ContainerHealthy(running[0].ID)
				})
			}

			h.WaitFor(2*time.Minute, "exactly one self container is left", func() bool {
				return len(h.SelfContainers(true)) == 1
			})

			if !h.RunsSelfImage(h.SelfContainerID(), "v1") {
				t.Errorf("surviving container image = %q, want the original v1 image",
					h.ContainerImage(h.SelfContainerID()))
			}

			// A failed self-update must be tried once, not on every poll.
			mark2 := h.LogMark()
			h.WaitForLogOccurrencesAfter("self-update poisoned, skipping until a new commit", mark2, 2, 90*time.Second)

			if n := h.LogCountAfter("self-update: strategy selected", mark); n != 1 {
				t.Errorf("self-update was attempted %d times for one broken commit, want 1", n)
			}

			h.assertNoSelfLeftovers()
		})
	}
}

// TestSelfUpdateRegression proves the feature does not change how other stacks
// are deployed: a self-managed doco-cd still reconciles a normal stack, and a
// commit that touches both still lands both.
func TestSelfUpdateRegression(t *testing.T) {
	t.Parallel()

	h := NewHarness(t, "self-update-regression")
	h.EnableSelfUpdate("e2e-regression", "doco-cd")
	h.Start()

	t.Cleanup(func() {
		if t.Failed() {
			h.dumpSelfState()
		}
	})

	selfID := h.SelfContainerID()

	h.WaitFor(2*time.Minute, "the app stack is deployed", func() bool {
		return h.ContainerID("e2e-regression-app", "app") != ""
	})

	appID := h.ContainerID("e2e-regression-app", "app")
	mark := h.LogMark()

	// A change to another stack must not touch doco-cd itself.
	h.ReplaceInWorktree("app/compose.yaml", "alpine:3.22", "alpine:3.21")
	h.RepoPush("bump the app image")

	h.WaitForContainerRecreate("e2e-regression-app", "app", appID, 3*time.Minute)

	if got := h.SelfContainerID(); got != selfID {
		t.Errorf("a deploy of another stack replaced doco-cd: %s -> %s", selfID, got)
	}

	if n := h.LogCountAfter("self-update: strategy selected", mark); n != 0 {
		t.Errorf("a deploy of another stack started %d self-updates, want 0", n)
	}

	// A commit that touches both must land both.
	appID = h.ContainerID("e2e-regression-app", "app")
	mark2 := h.LogMark()

	h.ReplaceInWorktree("app/compose.yaml", `"sleep", "600"`, `"sleep", "601"`)
	h.ReplaceInWorktree("deploy/compose.yaml", ":v1", ":v2")
	h.RepoPush("bump both stacks")

	h.WaitForLogAfter("self-update finalised", mark2, 4*time.Minute)

	h.WaitFor(3*time.Minute, "the app stack was deployed too", func() bool {
		id := h.ContainerID("e2e-regression-app", "app")

		return id != "" && id != appID
	})

	newSelfID := h.SelfContainerID()
	if newSelfID == selfID {
		t.Error("doco-cd was not replaced by the combined commit")
	}

	if !h.RunsSelfImage(newSelfID, "v2") {
		t.Errorf("successor image = %q, want v2", h.ContainerImage(newSelfID))
	}

	if got := h.ContainerLabels(h.ContainerID("e2e-regression-app", "app"))["cd.doco.deployment.target.sha"]; got != h.RepoHead() {
		t.Errorf("app container commit label = %q, want %q", got, h.RepoHead())
	}

	h.assertNoSelfLeftovers()
}

// TestSelfUpdateCrashRecovery kills the process at a chosen point of the
// handover and proves the next boot converges to a single healthy container.
func TestSelfUpdateCrashRecovery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		scenario string
		stack    string
	}{
		{name: "crash after the successor is healthy", scenario: "self-update-crash-handover", stack: "e2e-crash-handover"},
		{name: "crash after the applier applied", scenario: "self-update-crash-applied", stack: "e2e-crash-applied"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := NewHarness(t, tt.scenario)
			h.EnableSelfUpdate(tt.stack, "doco-cd")
			h.Start()

			t.Cleanup(func() {
				if t.Failed() {
					h.dumpSelfState()
				}
			})

			oldID := h.SelfContainerID()
			mark := h.LogMark()

			h.ReplaceInWorktree("deploy/compose.yaml", ":v1", ":v2")
			h.RepoPush("bump the doco-cd image")

			h.WaitForLogAfter("self-update: crash hook fired", mark, 3*time.Minute)

			h.WaitFor(5*time.Minute, "the stack converges to one new healthy container", func() bool {
				all := h.SelfContainers(true)

				return len(all) == 1 && h.ContainerHealthy(all[0].ID) &&
					all[0].ID != oldID && h.RunsSelfImage(all[0].ID, "v2")
			})

			h.WaitForLogAfter("self-update finalised", mark, 3*time.Minute)

			h.WaitFor(2*time.Minute, "the appliers are cleaned up", func() bool {
				return len(h.SelfAppliers(true)) == 0
			})

			// The crash must not leave a record that blocks later deployments.
			mark2 := h.LogMark()
			h.WaitForLogOccurrencesAfter("no changes detected, skipping deployment", mark2, 2, 90*time.Second)

			h.assertNoSelfLeftovers()
		})
	}
}
