//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/docker"
)

const (
	scheduledTimeoutStack        = "e2e-scheduled-job-timeout"
	scheduledTimeoutSwarmStack   = "e2e-scheduled-job-timeout-swarm"
	scheduledTimeoutAPIKey       = "e2e-scheduled-job-api-key" // #nosec G101 -- this is a fixed test credential.
	scheduledTimeoutRestartJob   = "report"
	scheduledTimeoutOneOff       = 5 * time.Second
	scheduledTimeoutRestart      = 3 * time.Second
	scheduledTimeoutResultWindow = 90 * time.Second
)

func TestScheduledJobTimeout_StopsStandaloneJobs(t *testing.T) {
	requireStandaloneSuite(t)
	t.Parallel()

	h := NewHarness(t, "scheduled-job-timeout")
	h.EnableRemoteContext()
	h.EnableAPI(scheduledTimeoutAPIKey)
	h.SetPollInterval(time.Minute)
	h.Start()
	h.WaitForLog(deployCompletedLog, 2*time.Minute)

	var appID string

	h.WaitFor(2*time.Minute, "initial dependency container", func() bool {
		appID = h.RemoteComposeContainerID(scheduledTimeoutStack, scheduledOneOffAppService)
		return appID != ""
	})

	// The phases share one harness and run in order. Harness helpers fail the parent test,
	// so the phases do not use subtests.
	assertStandaloneOneOffTimeout(t, h, appID)
	assertStandaloneRestartTimeout(t, h)
	assertStandaloneTimeoutRecovery(t, h, appID)
}

func assertStandaloneOneOffTimeout(t *testing.T, h *Harness, appID string) {
	t.Helper()

	job := getScheduledOneOffJob(t, h, "remote", scheduledTimeoutStack)
	if job.Timeout != scheduledTimeoutOneOff.String() {
		t.Fatalf("one-off job timeout = %q, want %q", job.Timeout, scheduledTimeoutOneOff)
	}

	runID := triggerScheduledTimeoutJob(t, h, "remote", scheduledTimeoutStack, job.Name)
	waitForScheduledTimeoutFailure(t, h, runID, scheduledTimeoutOneOff)

	h.WaitFor(30*time.Second, "timed-out one-off container cleanup", func() bool {
		return h.RemoteOneOffContainerID(scheduledTimeoutStack, scheduledOneOffJobService) == ""
	})
	h.WaitFor(30*time.Second, "dependency restored after timed-out one-off job", func() bool {
		return h.RemoteComposeContainerID(scheduledTimeoutStack, scheduledOneOffAppService) == appID
	})

	job = getScheduledOneOffJob(t, h, "remote", scheduledTimeoutStack)
	if job.LastRun == nil || job.LastRun.JobID != runID || job.LastRun.Status != controlplane.RunStatusFailed {
		t.Fatalf("one-off job last_run = %#v, want failed run %q", job.LastRun, runID)
	}

	nextRunID := triggerScheduledTimeoutJob(t, h, "remote", scheduledTimeoutStack, job.Name)
	waitForScheduledTimeoutFailure(t, h, nextRunID, scheduledTimeoutOneOff)
}

func assertStandaloneRestartTimeout(t *testing.T, h *Harness) {
	t.Helper()

	name := scheduledTimeoutStack + "-" + scheduledTimeoutRestartJob + "-1"

	job := getScheduledJobByName(t, h, "remote", scheduledTimeoutStack, name)
	if job.Timeout != scheduledTimeoutRestart.String() {
		t.Fatalf("restart job timeout = %q, want %q", job.Timeout, scheduledTimeoutRestart)
	}

	runID := triggerScheduledTimeoutJob(t, h, "remote", scheduledTimeoutStack, name)
	waitForScheduledTimeoutFailure(t, h, runID, scheduledTimeoutRestart)

	inspect, err := h.remoteDockerClient().ContainerInspect(h.ctx, name, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect timed-out restart job: %v", err)
	}

	if inspect.Container.State == nil || inspect.Container.State.Running {
		t.Fatalf("timed-out restart job is still running: %#v", inspect.Container.State)
	}
}

func assertStandaloneTimeoutRecovery(t *testing.T, h *Harness, appID string) {
	t.Helper()

	job := getScheduledOneOffJob(t, h, "remote", scheduledTimeoutStack)
	runID := triggerScheduledTimeoutJob(t, h, "remote", scheduledTimeoutStack, job.Name)

	h.WaitFor(30*time.Second, "one-off workload starts before forced restart", func() bool {
		return h.RemoteOneOffContainerID(scheduledTimeoutStack, scheduledOneOffJobService) != ""
	})
	h.KillAndRestartDaemon()
	waitForScheduledTimeoutFailure(t, h, runID, scheduledTimeoutOneOff)

	h.WaitFor(30*time.Second, "recovered timed-out container cleanup", func() bool {
		return h.RemoteOneOffContainerID(scheduledTimeoutStack, scheduledOneOffJobService) == ""
	})
	h.WaitFor(30*time.Second, "dependency restored after recovered timeout", func() bool {
		return h.RemoteComposeContainerID(scheduledTimeoutStack, scheduledOneOffAppService) == appID
	})
}

func TestScheduledJobTimeout_StopsSwarmOneOffJob(t *testing.T) {
	t.Parallel()

	h := NewHarness(t, "scheduled-job-timeout-swarm")
	h.EnableRemoteSwarmContext()
	h.EnableAPI(scheduledTimeoutAPIKey)
	h.SetPollInterval(time.Minute)
	h.Start()
	h.WaitForLog(deployCompletedLog, 2*time.Minute)

	h.WaitFor(2*time.Minute, "initial Swarm dependency service", func() bool {
		return h.containerID(h.remoteDockerClient(), true, scheduledTimeoutSwarmStack, scheduledOneOffAppService) != ""
	})

	job := getScheduledOneOffJob(t, h, "remote", scheduledTimeoutSwarmStack)
	if job.Timeout != scheduledTimeoutOneOff.String() {
		t.Fatalf("Swarm one-off job timeout = %q, want %q", job.Timeout, scheduledTimeoutOneOff)
	}

	runID := triggerScheduledTimeoutJob(t, h, "remote", scheduledTimeoutSwarmStack, job.Name)
	waitForScheduledTimeoutFailure(t, h, runID, scheduledTimeoutOneOff)

	containers, err := h.remoteDockerClient().ContainerList(h.ctx, client.ContainerListOptions{
		Filters: client.Filters{}.Add("label", docker.DocoCDJobLabels.JobRunID+"="+runID),
	})
	if err != nil {
		t.Fatalf("list timed-out Swarm task containers: %v", err)
	}

	if len(containers.Items) != 0 {
		t.Fatalf("timed-out Swarm task containers still running: %v", containers.Items)
	}

	h.WaitFor(time.Minute, "timed-out Swarm one-off service cleanup", func() bool {
		services, err := h.remoteDockerClient().ServiceList(h.ctx, client.ServiceListOptions{
			Filters: client.Filters{}.Add("label", docker.DocoCDJobLabels.JobRunID+"="+runID),
		})
		if err != nil {
			t.Fatalf("list timed-out Swarm services: %v", err)
		}

		return len(services.Items) == 0
	})
	h.WaitFor(time.Minute, "Swarm dependency restored after timed-out one-off job", func() bool {
		return h.containerID(h.remoteDockerClient(), true, scheduledTimeoutSwarmStack, scheduledOneOffAppService) != ""
	})
}

func triggerScheduledTimeoutJob(t *testing.T, h *Harness, contextName, stack, name string) string {
	t.Helper()

	query := url.Values{"context": {contextName}, "stack": {stack}, "wait": {"false"}}

	var accepted struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(h.APIRequest(http.MethodPost,
		"/v1/api/job/"+url.PathEscape(name)+"/run?"+query.Encode(), http.StatusAccepted), &accepted); err != nil {
		t.Fatalf("decode manual trigger response for %s: %v", name, err)
	}

	if accepted.JobID == "" {
		t.Fatalf("manual trigger for %s returned no run ID", name)
	}

	return accepted.JobID
}

func waitForScheduledTimeoutFailure(t *testing.T, h *Harness, runID string, timeout time.Duration) {
	t.Helper()

	var run controlplane.Run

	h.WaitFor(scheduledTimeoutResultWindow, "scheduled run fails with timeout", func() bool {
		var detail struct {
			Content controlplane.Run `json:"content"`
		}
		if err := json.Unmarshal(h.APIRequest(http.MethodGet, "/v1/api/run/"+url.PathEscape(runID), http.StatusOK), &detail); err != nil {
			t.Fatalf("decode scheduled run detail: %v", err)
		}

		run = detail.Content
		if run.Status == controlplane.RunStatusSucceeded {
			t.Fatalf("scheduled run %s succeeded, want timeout failure", runID)
		}

		return run.Status == controlplane.RunStatusFailed && run.FinishedAt != nil
	})

	want := "exceeded timeout of " + timeout.String()
	if !strings.Contains(run.Message, want) {
		t.Fatalf("scheduled run message = %q, want it to contain %q", run.Message, want)
	}

	if run.StartedAt == nil {
		t.Fatalf("timed-out scheduled run %s has no start time", runID)
	}

	if elapsed := run.FinishedAt.Sub(*run.StartedAt); elapsed < timeout {
		t.Fatalf("scheduled run finished after %s, want at least the %s timeout", elapsed, timeout)
	}
}

func getScheduledJobByName(t *testing.T, h *Harness, contextName, stack, name string) controlplane.ScheduledJobInfo {
	t.Helper()

	query := url.Values{"context": {contextName}, "stack": {stack}}

	var response struct {
		Content []controlplane.ScheduledJobInfo `json:"content"`
	}
	if err := json.Unmarshal(h.APIRequest(http.MethodGet, "/v1/api/jobs?"+query.Encode(), http.StatusOK), &response); err != nil {
		t.Fatalf("decode job API response: %v", err)
	}

	job, err := selectScheduledJobByName(response.Content, stack, name)
	if err != nil {
		t.Fatalf("select scheduled job %s for %s/%s: %v", name, contextName, stack, err)
	}

	return job
}

func selectScheduledJobByName(jobs []controlplane.ScheduledJobInfo, stack, name string) (controlplane.ScheduledJobInfo, error) {
	for _, job := range jobs {
		if job.Stack == stack && job.Name == name {
			return job, nil
		}
	}

	return controlplane.ScheduledJobInfo{}, fmt.Errorf("job %s not found in %d jobs", name, len(jobs))
}
