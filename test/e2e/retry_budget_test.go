//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	swarmTypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

func TestDeploymentRetryBudgetCompose(t *testing.T) {
	t.Parallel()

	const stack = "e2e-retry-budget"

	h := NewHarness(t, "retry-budget")
	h.EnableAPI("retry-budget-test-key")
	h.TrackVolume(stack + "_state")
	h.Start()
	h.WaitForLog(`"reason":"delay"`, 2*time.Minute)

	containerID := func() string { return h.ComposeContainerID(stack, "app") }

	first := containerID()
	if first == "" {
		t.Fatal("failed hook must leave its container running")
	}

	if got := h.ExecOutput(first, "sh", "-c", "wc -l < /state/attempts"); got != "1" {
		t.Fatalf("original deployment attempts = %q, want 1", got)
	}

	h.WaitForComposeContainerRecreate(stack, "app", first, 2*time.Minute)
	h.WaitForLog("budget_exhausted", 2*time.Minute)

	paused := containerID()
	if got := h.ExecOutput(paused, "sh", "-c", "wc -l < /state/attempts"); got != "2" {
		t.Fatalf("attempts after one allowed retry = %q, want 2", got)
	}

	h.AssertStays(25*time.Second, "exhausted budget preserves container", paused, containerID)

	if got := h.ExecOutput(paused, "sh", "-c", "wc -l < /state/attempts"); got != "2" {
		t.Fatalf("exhausted budget admitted extra attempts: %q", got)
	}

	body := `[{"url":"http://gitserver/retry-budget.git","reference":"main"}]`
	h.APIRequestWithBody(http.MethodPost, "/v1/api/poll/run", http.StatusInternalServerError, body)
	h.WaitForComposeContainerRecreate(stack, "app", paused, 2*time.Minute)

	paused = containerID()
	if got := h.ExecOutput(paused, "sh", "-c", "wc -l < /state/attempts"); got != "3" {
		t.Fatalf("failed manual retry attempts = %q, want 3", got)
	}

	h.AssertStays(25*time.Second, "failed manual retry does not refill the automatic budget", paused, containerID)

	// Fix the external condition without changing the watched source.
	h.ExecOutput(paused, "touch", "/state/ready")
	mark := h.LogMark()
	h.APIRequestWithBody(http.MethodPost, "/v1/api/poll/run", http.StatusOK, body)
	h.WaitForLogAfter(deployCompletedLog, mark, 2*time.Minute)
	h.WaitForComposeContainerRecreate(stack, "app", paused, 2*time.Minute)
	h.WaitForLogAfter("no changes detected, skipping deployment", mark, 2*time.Minute)

	if got := h.ExecOutput(containerID(), "sh", "-c", "wc -l < /state/attempts"); got != "4" {
		t.Fatalf("manual retry attempts = %q, want 4", got)
	}
}

func TestDeploymentRetryBudgetSwarmPreservesRollback(t *testing.T) {
	t.Parallel()

	const stack = "e2e-retry-budget-swarm"

	h := NewHarness(t, "retry-budget-swarm")
	h.EnableRemoteSwarmContext()
	h.Start()
	h.WaitForLog(deployCompletedLog, 3*time.Minute)

	inspect := func() swarmTypes.Service {
		result, err := h.remoteDocker.ServiceInspect(h.ctx, stack+"_app", client.ServiceInspectOptions{})
		if err != nil {
			t.Fatalf("inspect isolated Swarm service: %v", err)
		}

		return result.Service
	}
	working := inspect().Spec.TaskTemplate.ContainerSpec
	mark := h.LogMark()
	h.ReplaceInWorktree("compose.yaml", "tail -f /dev/null", "exit 1")
	h.RepoPush("break Swarm release")
	h.WaitForLogAfter("budget_exhausted", mark, 3*time.Minute)
	h.WaitFor(2*time.Minute, "Swarm rollback completed", func() bool {
		status := inspect().UpdateStatus

		return status != nil && status.State == swarmTypes.UpdateStateRollbackCompleted
	})

	restored := inspect()
	if !reflect.DeepEqual(restored.Spec.TaskTemplate.ContainerSpec.Command, working.Command) ||
		!reflect.DeepEqual(restored.Spec.TaskTemplate.ContainerSpec.Args, working.Args) ||
		restored.Spec.TaskTemplate.ContainerSpec.Image != working.Image {
		t.Fatal("Swarm did not restore the working service specification")
	}

	snapshot := func() string {
		service := inspect()

		spec, err := json.Marshal(service.Spec)
		if err != nil {
			t.Fatal(err)
		}

		tasks, err := h.remoteDocker.TaskList(h.ctx, client.TaskListOptions{
			Filters: client.Filters{}.Add("service", service.ID),
		})
		if err != nil {
			t.Fatal(err)
		}

		return fmt.Sprintf("%d:%s:%d", service.Version.Index, spec, len(tasks.Items))
	}
	h.AssertStays(25*time.Second, "paused revision leaves rolled-back service untouched", snapshot(), snapshot)

	mark = h.LogMark()
	h.ReplaceInWorktree("compose.yaml", "exit 1", "echo fixed; tail -f /dev/null")
	h.RepoPush("fix Swarm release")
	h.WaitForLogAfter(deployCompletedLog, mark, 3*time.Minute)

	if got := strings.Join(inspect().Spec.TaskTemplate.ContainerSpec.Args, " "); !strings.Contains(got, "echo fixed") {
		t.Fatalf("new revision failed to bypass the paused budget: %s", got)
	}
}
