//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	swarmTypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/controlplane"
)

func TestDeploymentPauseCompose(t *testing.T) {
	requireStandaloneSuite(t)

	const (
		stack   = "e2e-deployment-pause-compose"
		sibling = stack + "-sibling"
	)

	h := NewHarness(t, "deployment-pause-compose")
	h.EnableRemoteContext()
	h.EnableAPI("deployment-pause-compose-test-key")
	h.SetPollInterval(24 * time.Hour)
	h.Start()

	waitVersion := func(project, version string) string {
		t.Helper()

		var id string

		h.WaitFor(2*time.Minute, project+" app at version "+version, func() bool {
			id = h.RemoteComposeContainerID(project, "app")
			if id == "" {
				return false
			}

			result, err := h.remoteDocker.ContainerInspect(h.ctx, id, client.ContainerInspectOptions{})

			return err == nil && result.Container.Config != nil &&
				deploymentPauseVersion(result.Container.Config.Env) == version
		})

		return id
	}
	snapshot := func(id string) string {
		t.Helper()

		result, err := h.remoteDocker.ContainerInspect(h.ctx, id, client.ContainerInspectOptions{})
		if err != nil {
			t.Fatalf("inspect paused Compose container: %v", err)
		}

		if result.Container.Config == nil || result.Container.State == nil || !result.Container.State.Running {
			t.Fatalf("paused container %s is not running with a configuration", id)
		}

		config, err := json.Marshal(struct {
			Config     *container.Config
			HostConfig *container.HostConfig
		}{result.Container.Config, result.Container.HostConfig})
		if err != nil {
			t.Fatalf("encode paused Compose configuration: %v", err)
		}

		return id + ":" + string(config)
	}
	assertSnapshot := func(want string) {
		t.Helper()

		id := h.RemoteComposeContainerID(stack, "app")
		if id == "" || snapshot(id) != want {
			t.Fatal("pause changed the running Compose container ID or configuration")
		}
	}
	stopAndSignal := func(id string) {
		t.Helper()

		timeout := 0
		if _, err := h.remoteDocker.ContainerStop(h.ctx, id, client.ContainerStopOptions{Timeout: &timeout}); err != nil {
			t.Fatalf("stop Compose app to introduce drift: %v", err)
		}
		// An explicit update event avoids replaying Compose's own stop/destroy
		// events when a source deployment replaces its reconciliation listener.
		if _, err := h.remoteDocker.ContainerUpdate(h.ctx, id, client.ContainerUpdateOptions{
			RestartPolicy: &container.RestartPolicy{Name: container.RestartPolicyOnFailure, MaximumRetryCount: 1},
		}); err != nil {
			t.Fatalf("signal stopped Compose app drift: %v", err)
		}
	}

	initialID := waitVersion(stack, "1")
	waitVersion(sibling, "1")
	h.WaitForLog("reconciliation event listeners ready", 2*time.Minute)

	initialSnapshot := snapshot(initialID)

	var initialRun string

	h.WaitFor(time.Minute, "scheduled job runs before pausing", func() bool {
		job := getScheduledOneOffJob(t, h, "remote", stack)
		if job.LastRun == nil || job.LastRun.Status != controlplane.RunStatusSucceeded {
			return false
		}

		initialRun = job.LastRun.JobID

		return true
	})
	sourceName := getScheduledOneOffJob(t, h, "remote", stack).Name

	jobSource, err := h.remoteDocker.ContainerInspect(h.ctx, sourceName, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect scheduled source before pausing: %v", err)
	}

	mark, runID := deploymentPauseCommit(h)

	waitVersion(sibling, "2")
	deploymentPauseWaitDeployment(t, h, mark, runID, sibling, stack)

	deploymentPauseManualRun(t, h)
	assertSnapshot(initialSnapshot)

	pausedAt := time.Now()

	h.WaitFor(time.Minute, "existing scheduled job continues while deployment is paused", func() bool {
		job := getScheduledOneOffJob(t, h, "remote", stack)

		return job.LastRun != nil && job.LastRun.JobID != initialRun &&
			job.LastRun.Status == controlplane.RunStatusSucceeded &&
			job.LastRun.StartedAt != nil && job.LastRun.StartedAt.After(pausedAt)
	})

	pausedSource, err := h.remoteDocker.ContainerInspect(h.ctx, sourceName, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect scheduled source while paused: %v", err)
	}

	if pausedSource.Container.ID != jobSource.Container.ID ||
		!reflect.DeepEqual(pausedSource.Container.Config, jobSource.Container.Config) {
		t.Fatal("deployment pause recreated or reconfigured the existing scheduled job")
	}

	assertSnapshot(initialSnapshot)

	mark = h.LogMark()

	stopAndSignal(initialID)

	recoveredID := waitVersion(stack, "1")
	h.WaitForLogLineAfter(mark, 2*time.Minute, `"msg":"reconciliation completed"`, `"stack":"`+stack+`"`)

	recoveredSnapshot := snapshot(recoveredID)

	deploymentPauseManualRun(t, h)
	assertSnapshot(recoveredSnapshot)

	h.ReplaceInWorktree(".doco-cd.yml", "pause: true", "pause: false")
	mark = h.LogMark()
	h.RepoPush("resume deployment at the latest source")
	runID = deploymentPauseTrigger(h)

	latestID := waitVersion(stack, "2")
	if latestID == initialID || latestID == recoveredID {
		t.Fatal("resuming did not deploy the latest Compose configuration")
	}

	waitVersion(sibling, "2")
	deploymentPauseWaitDeployment(t, h, mark, runID, stack, "")
}

func TestDeploymentPauseSwarm(t *testing.T) {
	t.Parallel()

	const (
		stack   = "e2e-deployment-pause-swarm"
		sibling = stack + "-sibling"
	)

	h := NewHarness(t, "deployment-pause-swarm")
	h.EnableRemoteSwarmContext()
	h.EnableAPI("deployment-pause-swarm-test-key")
	h.SetPollInterval(24 * time.Hour)
	h.Start()

	inspect := func(project string) swarmTypes.Service {
		t.Helper()

		result, err := h.remoteDocker.ServiceInspect(h.ctx, project+"_app", client.ServiceInspectOptions{})
		if err != nil {
			t.Fatalf("inspect isolated Swarm service %s: %v", project, err)
		}

		return result.Service
	}
	waitVersion := func(project, version string) swarmTypes.Service {
		t.Helper()

		var service swarmTypes.Service

		h.WaitFor(3*time.Minute, project+" Swarm app at version "+version, func() bool {
			result, err := h.remoteDocker.ServiceInspect(h.ctx, project+"_app", client.ServiceInspectOptions{})
			if err != nil || result.Service.Spec.TaskTemplate.ContainerSpec == nil ||
				deploymentPauseVersion(result.Service.Spec.TaskTemplate.ContainerSpec.Env) != version {
				return false
			}

			if mode := result.Service.Spec.Mode.Replicated; mode == nil || mode.Replicas == nil || *mode.Replicas != 1 {
				return false
			}

			if status := result.Service.UpdateStatus; status != nil && status.State != swarmTypes.UpdateStateCompleted {
				return false
			}

			service = result.Service

			id := h.containerID(h.remoteDocker, true, project, "app")
			if id == "" {
				return false
			}

			container, err := h.remoteDocker.ContainerInspect(h.ctx, id, client.ContainerInspectOptions{})

			return err == nil && container.Container.Config != nil &&
				deploymentPauseVersion(container.Container.Config.Env) == version
		})

		return service
	}
	assertSnapshot := func(want swarmTypes.Service) {
		t.Helper()

		got := inspect(stack)
		if got.ID != want.ID || got.Version.Index != want.Version.Index || !reflect.DeepEqual(got.Spec, want.Spec) {
			t.Fatalf("pause changed the Swarm service ID, version, or Spec: before %#v, after %#v", want, got)
		}
	}

	initial := waitVersion(stack, "1")
	waitVersion(sibling, "1")
	h.WaitForLog("reconciliation event listeners ready", 2*time.Minute)

	mark, runID := deploymentPauseCommit(h)

	waitVersion(sibling, "2")
	deploymentPauseWaitDeployment(t, h, mark, runID, sibling, stack)

	deploymentPauseManualRun(t, h)
	assertSnapshot(initial)

	mark = h.LogMark()
	if _, err := h.remoteDocker.ServiceRemove(h.ctx, initial.ID, client.ServiceRemoveOptions{}); err != nil {
		t.Fatalf("remove paused Swarm service to introduce drift: %v", err)
	}

	recovered := waitVersion(stack, "1")
	if recovered.ID == initial.ID {
		t.Fatal("last-deployed recovery did not replace the missing Swarm service")
	}

	h.WaitForLogLineAfter(mark, 3*time.Minute, `"msg":"reconciliation completed"`, `"stack":"`+stack+`"`)
	recovered = inspect(stack)

	deploymentPauseManualRun(t, h)
	assertSnapshot(recovered)

	h.ReplaceInWorktree(".doco-cd.yml", "pause: true", "pause: false")
	mark = h.LogMark()
	h.RepoPush("resume deployment at the latest source")
	runID = deploymentPauseTrigger(h)

	latest := waitVersion(stack, "2")
	if latest.Version.Index == recovered.Version.Index {
		t.Fatal("resuming did not update the Swarm service version")
	}

	waitVersion(sibling, "2")
	deploymentPauseWaitDeployment(t, h, mark, runID, stack, "")
}

func deploymentPauseCommit(h *Harness) (int, string) {
	h.t.Helper()

	mark := h.LogMark()
	h.ReplaceInWorktree(".doco-cd.yml", "pause: false", "pause: true")

	for _, dir := range []string{"paused", "sibling"} {
		h.ReplaceInWorktree(dir+"/compose.yaml", `VERSION: "1"`, `VERSION: "2"`)
	}

	h.RepoPush("pause deployment while updating both entries to v2")

	return mark, deploymentPauseTrigger(h)
}

func deploymentPauseWaitDeployment(t *testing.T, h *Harness, mark int, runID, activeStack, pausedStack string) {
	t.Helper()

	h.WaitFor(3*time.Minute, "completed active deployment run for "+activeStack, func() bool {
		var response struct {
			Content controlplane.Run `json:"content"`
		}
		if err := json.Unmarshal(h.APIRequest(http.MethodGet, "/v1/api/run/"+url.PathEscape(runID),
			http.StatusOK), &response); err != nil {
			t.Fatalf("decode active deployment run: %v", err)
		}

		if response.Content.FinishedAt == nil {
			return false
		}

		if response.Content.Status != controlplane.RunStatusSucceeded {
			t.Fatalf("active deployment run = %#v, want succeeded", response.Content)
		}

		return true
	})

	h.WaitForLogLineAfter(mark, time.Minute, `"msg":"completed stage: post-deploy"`,
		`"job_id":"`+runID+`"`, `"stack":"`+activeStack+`"`)

	if pausedStack != "" {
		h.WaitForLogLineAfter(mark, time.Minute, `"msg":"deployment skipped"`, "deployment paused",
			`"job_id":"`+runID+`"`, `"stack":"`+pausedStack+`"`)
	}

	h.WaitForLogLineAfter(mark, 3*time.Minute, "reconciliation event listeners ready", `"job_id":"`+runID+`"`)
}

func deploymentPauseTrigger(h *Harness) string {
	h.t.Helper()

	body := fmt.Sprintf(`[{"url":"http://gitserver/%s.git","reference":"refs/heads/main"}]`, h.scenario)

	var accepted struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(h.APIRequestWithBody(http.MethodPost, "/v1/api/poll/run?wait=false",
		http.StatusAccepted, body), &accepted); err != nil {
		h.t.Fatalf("decode manual pause trigger: %v", err)
	}

	if accepted.JobID == "" {
		h.t.Fatal("manual pause trigger returned no tracked run ID")
	}

	return accepted.JobID
}

func deploymentPauseManualRun(t *testing.T, h *Harness) {
	t.Helper()

	runID := deploymentPauseTrigger(h)

	h.WaitFor(2*time.Minute, "manual trigger is skipped rather than bypassing deployment pause", func() bool {
		var response struct {
			Content controlplane.Run `json:"content"`
		}
		if err := json.Unmarshal(h.APIRequest(http.MethodGet, "/v1/api/run/"+url.PathEscape(runID),
			http.StatusOK), &response); err != nil {
			t.Fatalf("decode manual paused run: %v", err)
		}

		run := response.Content
		if run.FinishedAt == nil {
			return false
		}

		if run.Status != controlplane.RunStatusSkipped {
			t.Fatalf("manual paused run = %#v, want skipped", run)
		}

		if !strings.Contains(run.Message, "deployment paused") || !strings.Contains(run.Message, "e2e-"+h.scenario) {
			t.Fatalf("skipped manual run lost pause identity: %#v", run)
		}

		return true
	})
}

func deploymentPauseVersion(env []string) string {
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "VERSION="); ok {
			return value
		}
	}

	return ""
}
