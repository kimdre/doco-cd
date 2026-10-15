//go:build e2e

package e2e

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/docker"
)

func TestAutostartDrift(t *testing.T) {
	requireStandaloneSuite(t)

	for _, target := range []string{"explicit", "discovery"} {
		t.Run(target, func(t *testing.T) {
			t.Parallel()

			const (
				stack      = "e2e-autostart-drift"
				service    = "app"
				compose    = "stacks/" + stack + "/compose.yaml"
				deployed   = "completed stage: post-deploy"
				fullSkip   = "no changes detected, skipping deployment"
				cachedSkip = "unchanged Git subtree and deployed project, skipping full pre-deploy"
			)

			h := NewHarness(t, "autostart-drift")
			h.EnableRemoteContext()
			h.EnableAPI("e2e-autostart-drift-api-key")
			h.CaptureNotifications()
			h.TrackStack(stack)

			if target == "discovery" {
				h.SetPollTarget(target)
			}

			h.Start()

			remoteDocker := h.remoteDockerClient()
			seenPolls := make(map[string]bool)
			waitForPoll := func(mark int, message string) {
				t.Helper()

				h.WaitFor(2*time.Minute, "completed poll: "+message, func() bool {
					for line := range strings.Lines(h.logsSince(mark)) {
						var entry struct {
							Message string `json:"msg"`
							Deploy  struct {
								JobID string `json:"job_id"`
								Stack string `json:"stack"`
							} `json:"deploy"`
						}
						if err := json.Unmarshal([]byte(line), &entry); err != nil || entry.Deploy.Stack != stack {
							continue
						}

						if message != deployed && entry.Message == deployed {
							t.Fatalf("unchanged poll deployed %s instead of skipping: %s", stack, line)
						}

						if entry.Message != message || entry.Deploy.JobID == "" {
							continue
						}

						var response struct {
							Content controlplane.Run `json:"content"`
						}
						if err := json.Unmarshal(h.APIRequest(http.MethodGet,
							"/v1/api/run/"+url.PathEscape(entry.Deploy.JobID), http.StatusOK), &response); err != nil {
							t.Fatalf("decode poll run %s: %v", entry.Deploy.JobID, err)
						}

						run := response.Content
						if run.Status == controlplane.RunStatusAccepted || run.Status == controlplane.RunStatusRunning {
							return false
						}

						// Poll runs report success; the stack log proves the skip reason.
						if run.JobID != entry.Deploy.JobID || run.Trigger != controlplane.RunTriggerPoll ||
							run.Status != controlplane.RunStatusSucceeded || run.FinishedAt == nil {
							t.Fatalf("poll did not complete successfully: %#v", run)
						}

						if !slices.Contains(run.Deployments, controlplane.RunTarget{Stack: stack, Context: "remote"}) {
							t.Fatalf("poll did not process the isolated stack: %#v", run)
						}

						if seenPolls[run.JobID] {
							t.Fatalf("poll %s was counted more than once", run.JobID)
						}

						seenPolls[run.JobID] = true
						h.logf("poll %s completed: %s", run.JobID, message)

						return true
					}

					return false
				})
			}
			inspect := func() client.ContainerInspectResult {
				t.Helper()

				result, err := remoteDocker.ContainerList(h.ctx, client.ContainerListOptions{
					All: true,
					Filters: client.Filters{}.
						Add("label", api.ProjectLabel+"="+stack).
						Add("label", api.ServiceLabel+"="+service),
				})
				if err != nil {
					t.Fatalf("list autostart-disabled service: %v", err)
				}

				if len(result.Items) != 1 {
					t.Fatalf("service inventory = %#v, want exactly one container", result.Items)
				}

				current, err := remoteDocker.ContainerInspect(h.ctx, result.Items[0].ID, client.ContainerInspectOptions{})
				if err != nil {
					t.Fatalf("inspect autostart-disabled service: %v", err)
				}

				if current.Container.Config == nil || current.Container.State == nil || current.Container.HostConfig == nil {
					t.Fatalf("container has incomplete configuration or state: %#v", current.Container)
				}

				return current
			}
			assertState := func(current client.ContainerInspectResult, state string) {
				t.Helper()

				if string(current.Container.State.Status) != state || current.Container.State.Running != (state == "running") {
					t.Fatalf("container %s state = %#v, want %s", current.Container.ID, current.Container.State, state)
				}
			}
			assertLabels := func(current client.ContainerInspectResult, commit string) {
				t.Helper()

				labels := current.Container.Config.Labels
				for key, want := range map[string]string{
					docker.DocoCDLabels.Deployment.Name:      stack,
					docker.DocoCDLabels.Deployment.CommitSHA: commit,
				} {
					if labels[key] != want {
						t.Fatalf("deployment label %s = %q, want %q", key, labels[key], want)
					}
				}

				for _, key := range []string{
					docker.DocoCDLabels.Deployment.ComposeHash,
					docker.DocoCDLabels.Deployment.ConfigHash,
					docker.DocoCDLabels.Deployment.Timestamp,
				} {
					if labels[key] == "" {
						t.Fatalf("deployment metadata label %s is empty", key)
					}
				}

				if target == "discovery" && labels[docker.DocoCDLabels.Deployment.AutoDiscovery] != "true" {
					t.Fatalf("service was not auto-discovered: %#v", labels)
				}
			}
			notificationCount := 0
			assertNotifications := func() {
				t.Helper()

				if got := h.StackNotifications(stack); len(got) != notificationCount {
					t.Fatalf("deployment notifications = %#v, want exactly %d", got, notificationCount)
				}
			}
			waitForNotification := func() {
				t.Helper()

				next := h.NextNotification(stack, 30*time.Second)
				if !strings.Contains(next.Title, "Deployment completed") {
					t.Fatalf("notification = %#v, want a completed deployment", next)
				}

				notificationCount++

				assertNotifications()
			}
			assertUnchanged := func(before client.ContainerInspectResult, state string) {
				t.Helper()

				current := inspect()
				assertState(current, state)

				if current.Container.ID != before.Container.ID || current.Container.Created != before.Container.Created ||
					!maps.Equal(current.Container.Config.Labels, before.Container.Config.Labels) {
					t.Fatalf("unchanged poll changed container identity or deployment labels: before %#v, after %#v",
						before.Container, current.Container)
				}

				assertNotifications()
			}

			waitForPoll(0, deployed)
			waitForNotification()

			initialHead, err := h.repo.Head()
			if err != nil {
				t.Fatalf("read initial fixture HEAD: %v", err)
			}

			initial := inspect()
			assertState(initial, "created")
			assertLabels(initial, initialHead.Hash().String())

			if initial.Container.HostConfig.RestartPolicy.Name != "unless-stopped" ||
				initial.Container.Config.Labels["cd.doco.deployment.autostart"] != "false" {
				t.Fatalf("unexpected restart or autostart configuration: %#v", initial.Container)
			}

			waitForPoll(h.LogMark(), fullSkip)
			assertUnchanged(initial, "created")

			skipMessage := fullSkip
			if target == "discovery" {
				skipMessage = cachedSkip
				waitForPoll(h.LogMark(), cachedSkip)
				assertUnchanged(initial, "created")
			}

			// Simulate Sablier using only this fixture's container.
			if _, err := remoteDocker.ContainerStart(h.ctx, initial.Container.ID, client.ContainerStartOptions{}); err != nil {
				t.Fatalf("externally start service: %v", err)
			}

			h.WaitFor(15*time.Second, "externally started service is running", func() bool {
				return inspect().Container.State.Running
			})

			if _, err := remoteDocker.ContainerStop(h.ctx, initial.Container.ID, client.ContainerStopOptions{}); err != nil {
				t.Fatalf("externally stop service: %v", err)
			}

			assertUnchanged(initial, "exited")

			for poll := 1; poll <= 3; poll++ {
				waitForPoll(h.LogMark(), skipMessage)
				assertUnchanged(initial, "exited")
			}

			unchangedHead, err := h.repo.Head()
			if err != nil || unchangedHead.Hash() != initialHead.Hash() {
				t.Fatalf("fixture HEAD changed during unchanged polls: HEAD = %v, error = %v", unchangedHead, err)
			}

			mark := h.LogMark()
			if _, err := remoteDocker.ContainerRemove(h.ctx, initial.Container.ID, client.ContainerRemoveOptions{}); err != nil {
				t.Fatalf("remove stopped service: %v", err)
			}

			waitForPoll(mark, deployed)
			waitForNotification()

			replacement := inspect()
			assertState(replacement, "created")
			assertLabels(replacement, initialHead.Hash().String())

			if replacement.Container.ID == initial.Container.ID {
				t.Fatal("missing service was not replaced")
			}

			mark = h.LogMark()
			h.ReplaceInWorktree(compose, `VERSION: "1"`, `VERSION: "2"`)
			configCommit := h.RepoPush("e2e: change stopped service configuration")

			waitForPoll(mark, deployed)
			waitForNotification()

			changed := inspect()
			assertState(changed, "created")
			assertLabels(changed, configCommit.String())

			if changed.Container.ID == replacement.Container.ID ||
				changed.Container.Config.Labels[docker.DocoCDLabels.Deployment.ComposeHash] ==
					replacement.Container.Config.Labels[docker.DocoCDLabels.Deployment.ComposeHash] ||
				!slices.Contains(changed.Container.Config.Env, "VERSION=2") {
				t.Fatalf("stopped service configuration was not applied: %#v", changed.Container)
			}

			waitForPoll(h.LogMark(), fullSkip)
			assertUnchanged(changed, "created")

			mark = h.LogMark()
			h.ReplaceInWorktree(compose, `cd.doco.deployment.autostart: "false"`, `cd.doco.deployment.autostart: "true"`)
			enabledCommit := h.RepoPush("e2e: enable service autostart")

			waitForPoll(mark, deployed)
			waitForNotification()

			enabled := inspect()
			assertState(enabled, "running")
			assertLabels(enabled, enabledCommit.String())

			if enabled.Container.Config.Labels["cd.doco.deployment.autostart"] != "true" ||
				!slices.Contains(enabled.Container.Config.Env, "VERSION=2") {
				t.Fatalf("autostart enablement lost the service configuration: %#v", enabled.Container)
			}

			t.Logf("%s deployment completed %d distinct polls with %d deployment notifications",
				target, len(seenPolls), notificationCount)
		})
	}
}
