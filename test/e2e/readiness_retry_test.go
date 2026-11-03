//go:build e2e

package e2e

import (
	"encoding/json"
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

func TestFailedDeployReadinessRetry(t *testing.T) {
	requireStandaloneSuite(t)
	t.Parallel()

	const stack = "e2e-readiness-retry"

	h := NewHarness(t, "readiness-retry")
	h.EnableAPI("readiness-retry-test-key")
	h.Start()

	ids := make(map[string]string)
	inspect := func(service string) client.ContainerInspectResult {
		t.Helper()

		result, err := h.docker.ContainerInspect(h.ctx, ids[service], client.ContainerInspectOptions{})
		if err != nil {
			t.Fatalf("inspect %s: %v", service, err)
		}

		if result.Container.State == nil || !result.Container.State.Running {
			t.Fatalf("%s must remain running: %+v", service, result.Container.State)
		}

		return result
	}
	healthyStates := make(map[string]client.ContainerInspectResult)
	assertIdentity := func() {
		t.Helper()

		result, err := h.docker.ContainerList(h.ctx, client.ContainerListOptions{
			All: true, Filters: client.Filters{}.Add("label", api.ProjectLabel+"="+stack),
		})
		if err != nil {
			t.Fatalf("list project containers: %v", err)
		}

		inventory := make(map[string][]string)

		for _, current := range result.Items {
			if current.Labels[api.HookLabel] == "" {
				service := current.Labels[api.ServiceLabel]
				inventory[service] = append(inventory[service], current.ID)
			}
		}

		for service, want := range ids {
			if got := inventory[service]; len(got) != 1 || got[0] != want {
				t.Fatalf("readiness retry replaced %s: got %v, want %s", service, got, want)
			}

			if before, ok := healthyStates[service]; ok {
				current := inspect(service)
				if current.Container.State.StartedAt != before.Container.State.StartedAt {
					t.Fatalf("readiness retry restarted healthy service %s", service)
				}
			}
		}
	}
	seenPolls := make(map[string]bool)
	waitForPoll := func(mark int, message string, status controlplane.RunStatus) {
		t.Helper()

		h.WaitFor(2*time.Minute, "completed poll: "+message, func() bool {
			assertIdentity()

			for line := range strings.Lines(h.logsSince(mark)) {
				var entry struct {
					Message string `json:"msg"`
					Deploy  struct {
						JobID string `json:"job_id"`
						Stack string `json:"stack"`
					} `json:"deploy"`
				}

				if err := json.Unmarshal([]byte(line), &entry); err != nil ||
					entry.Message != message || entry.Deploy.Stack != stack ||
					entry.Deploy.JobID == "" || seenPolls[entry.Deploy.JobID] {
					continue
				}

				var response struct {
					Content controlplane.Run `json:"content"`
				}

				if err := json.Unmarshal(h.APIRequest(http.MethodGet,
					"/v1/api/run/"+url.PathEscape(entry.Deploy.JobID), http.StatusOK), &response); err != nil {
					t.Fatalf("decode poll run: %v", err)
				}

				run := response.Content
				if run.Status == controlplane.RunStatusAccepted || run.Status == controlplane.RunStatusRunning {
					continue
				}

				if run.JobID != entry.Deploy.JobID || run.Trigger != controlplane.RunTriggerPoll ||
					run.Status != status || run.FinishedAt == nil ||
					!slices.Contains(run.Deployments, controlplane.RunTarget{Stack: stack, Context: "default"}) {
					t.Fatalf("unexpected completed poll: %+v", run)
				}

				seenPolls[run.JobID] = true

				return true
			}

			return false
		})
	}

	waitForPoll(0, "completed stage: post-deploy", controlplane.RunStatusSucceeded)

	mark := h.LogMark()
	h.CopyScenarioDir("update")
	h.RepoPush("add a service with a failing healthcheck")
	waitForPoll(mark, "deployment failed", controlplane.RunStatusFailed)

	revision, err := h.repo.Head()
	if err != nil {
		t.Fatalf("read source revision: %v", err)
	}

	for _, service := range []string{"healthy", "consumer", "slow"} {
		ids[service] = h.ComposeContainerID(stack, service)
		if ids[service] == "" {
			t.Fatalf("%s must exist after the failed deployment", service)
		}

		current := inspect(service)
		if service != "slow" {
			healthyStates[service] = current
		}
	}

	assertHealth := func(service, want string) {
		t.Helper()

		state := inspect(service).Container.State
		if state.Health == nil || string(state.Health.Status) != want {
			t.Fatalf("%s health = %+v, want %s", service, state.Health, want)
		}
	}

	assertHealth("healthy", "healthy")
	assertHealth("slow", "unhealthy")

	for range 2 {
		waitForPoll(h.LogMark(), "deployment failed", controlplane.RunStatusFailed)
		assertIdentity()
		assertHealth("healthy", "healthy")
		assertHealth("slow", "unhealthy")
		inspect("consumer")
	}

	// A readiness retry starts a stopped container without replacing it.
	if _, err := h.docker.ContainerStop(h.ctx, ids["slow"], client.ContainerStopOptions{Timeout: new(1)}); err != nil {
		t.Fatalf("stop unhealthy service: %v", err)
	}

	waitForPoll(h.LogMark(), "deployment failed", controlplane.RunStatusFailed)
	assertIdentity()
	assertHealth("slow", "unhealthy")

	// Change runtime health without a new revision or a container restart.
	mark = h.LogMark()
	h.ExecOutput(ids["slow"], "touch", "/tmp/ready")
	waitForPoll(mark, "completed stage: post-deploy", controlplane.RunStatusSucceeded)
	assertIdentity()
	assertHealth("slow", "healthy")

	current := inspect("slow")
	if current.Container.Config.Labels[docker.DocoCDLabels.Deployment.CommitSHA] != revision.Hash().String() {
		t.Fatal("readiness recovery must succeed on the same source revision")
	}

	for range 2 {
		waitForPoll(h.LogMark(), "no changes detected, skipping deployment", controlplane.RunStatusSucceeded)
	}
}
