//go:build e2e

package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/scheduler"
)

const (
	scheduledOneOffStack              = "e2e-scheduled-one-off"
	scheduledOneOffRecoveryStack      = "e2e-scheduled-one-off-recovery"
	scheduledOneOffSwarmRecoveryStack = "e2e-scheduled-one-off-swarm-recovery"
	scheduledOneOffJobService         = "backup"
	scheduledOneOffAppService         = "app"
)

func TestScheduledOneOff(t *testing.T) {
	t.Parallel()

	h := NewHarness(t, "scheduled-one-off")
	h.EnableRemoteContext()
	h.SetPollInterval(time.Minute)
	h.Start()

	h.WaitFor(2*time.Minute, "initial dependency container", func() bool {
		return h.RemoteComposeContainerID(scheduledOneOffStack, scheduledOneOffAppService) != ""
	})

	waitForRunningScheduledOneOff(t, h, scheduledOneOffStack)

	h.WaitFor(30*time.Second, "dependency stopped while one-off job runs", func() bool {
		return h.RemoteComposeContainerID(scheduledOneOffStack, scheduledOneOffAppService) == ""
	})

	h.WaitFor(2*time.Minute, "one-off job artifact cleanup", func() bool {
		return h.RemoteOneOffContainerID(scheduledOneOffStack, scheduledOneOffJobService) == ""
	})

	h.WaitFor(30*time.Second, "dependency restored after one-off job", func() bool {
		return h.RemoteComposeContainerID(scheduledOneOffStack, scheduledOneOffAppService) != ""
	})
}

func TestScheduledOneOff_StatusUsesLatestExecution(t *testing.T) {
	t.Parallel()

	const stack = "e2e-scheduled-one-off-status"

	h := NewHarness(t, "scheduled-one-off-status")
	h.EnableRemoteContext()
	h.EnableAPI("e2e-scheduled-job-api-key")
	h.SetPollInterval(time.Minute)
	h.Start()
	h.WaitForLog(deployCompletedLog, 2*time.Minute)

	query := url.Values{"context": {"remote"}, "stack": {stack}}
	getJob := func() scheduler.JobInfo {
		t.Helper()

		var response struct {
			Content []scheduler.JobInfo `json:"content"`
		}

		body := h.APIRequest(http.MethodGet, "/v1/api/jobs?"+query.Encode(), http.StatusOK)
		if err := json.Unmarshal(body, &response); err != nil {
			t.Fatalf("decode job API response: %v", err)
		}

		if len(response.Content) != 1 {
			t.Fatalf("expected one scheduled job, got %#v", response.Content)
		}

		return response.Content[0]
	}

	remoteDocker := h.remoteDockerClient()
	sourceName := getJob().Name

	if _, err := remoteDocker.ContainerStart(h.ctx, sourceName, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("start source job container: %v", err)
	}

	sourceFailed := func() bool {
		t.Helper()

		result, err := remoteDocker.ContainerInspect(h.ctx, sourceName, client.ContainerInspectOptions{})
		if err != nil {
			t.Fatalf("inspect source job container: %v", err)
		}

		state := result.Container.State

		return state != nil && !state.Running && state.ExitCode == 7
	}
	h.WaitFor(10*time.Second, "source job container exited with code 7", sourceFailed)

	if got := getJob().Status; got != "exited (7)" {
		t.Fatalf("job status before one-off execution = %q, want exited (7)", got)
	}

	appID := h.RemoteComposeContainerID(stack, scheduledOneOffAppService)

	exitCode, output, err := h.remoteDaemon.Exec(h.ctx, []string{"docker", "exec", appID, "touch", "/state/ready"})
	if err != nil {
		t.Fatalf("restore job dependency: %v", err)
	}

	body, err := io.ReadAll(output)
	if err != nil {
		t.Fatalf("read dependency command output: %v", err)
	}

	if exitCode != 0 {
		t.Fatalf("restore job dependency exited with code %d: %s", exitCode, body)
	}

	var job scheduler.JobInfo

	h.WaitFor(90*time.Second, "scheduled one-off execution reported as running", func() bool {
		return getJob().Status == "running"
	})
	h.WaitFor(30*time.Second, "successful scheduled one-off result reported by job API", func() bool {
		job = getJob()

		return job.Status == "exited (0)" && job.LastRunAt != nil &&
			h.RemoteOneOffContainerID(stack, scheduledOneOffJobService) == ""
	})

	scheduledLastRun := *job.LastRunAt

	if !sourceFailed() {
		t.Fatal("scheduled one-off execution changed the source container's exit status")
	}

	runQuery := url.Values{"context": {"remote"}, "stack": {stack}, "wait": {"false"}}
	h.APIRequest(http.MethodPost, "/v1/api/job/"+url.PathEscape(sourceName)+"/run?"+runQuery.Encode(), http.StatusAccepted)
	h.WaitFor(10*time.Second, "manual one-off execution reported as running", func() bool {
		return getJob().Status == "running"
	})
	h.WaitFor(30*time.Second, "successful manual one-off result reported by job API", func() bool {
		job = getJob()

		return job.Status == "exited (0)" && job.LastRunAt != nil && job.LastRunAt.After(scheduledLastRun) &&
			h.RemoteOneOffContainerID(stack, scheduledOneOffJobService) == ""
	})

	if !sourceFailed() {
		t.Fatal("manual one-off execution changed the source container's exit status")
	}
}

func TestScheduledOneOff_RecoversAfterForcedDaemonTermination(t *testing.T) {
	t.Parallel()

	h := NewHarness(t, "scheduled-one-off-recovery")
	h.EnableRemoteContext()
	h.SetPollInterval(time.Minute)
	h.Start()

	h.WaitFor(2*time.Minute, "initial dependency container", func() bool {
		return h.RemoteComposeContainerID(scheduledOneOffRecoveryStack, scheduledOneOffAppService) != ""
	})

	oneOffID := waitForRunningScheduledOneOff(t, h, scheduledOneOffRecoveryStack)

	h.WaitFor(30*time.Second, "dependency stopped before daemon termination", func() bool {
		return h.RemoteComposeContainerID(scheduledOneOffRecoveryStack, scheduledOneOffAppService) == ""
	})

	h.KillAndRestartDaemon()

	h.WaitFor(30*time.Second, "replacement daemon retains running one-off job", func() bool {
		return h.RemoteOneOffContainerID(scheduledOneOffRecoveryStack, scheduledOneOffJobService) == oneOffID &&
			h.RemoteContainerRunning(oneOffID)
	})

	h.WaitFor(2*time.Minute, "recovered one-off job artifact cleanup", func() bool {
		return h.RemoteOneOffContainerID(scheduledOneOffRecoveryStack, scheduledOneOffJobService) == ""
	})

	h.WaitFor(30*time.Second, "dependency restored by recovered one-off job", func() bool {
		return h.RemoteComposeContainerID(scheduledOneOffRecoveryStack, scheduledOneOffAppService) != ""
	})
}

func TestScheduledOneOff_SwarmRecoversAfterForcedDaemonTermination(t *testing.T) {
	// Scheduled-job discovery spans the shared Swarm cluster, so this test must
	// not run alongside other E2E scenarios that create scheduled services.
	h := NewHarness(t, "scheduled-one-off-swarm-recovery")
	if !h.isSwarmMode() {
		t.Skip("scheduled one-off Swarm recovery requires a Swarm manager")
	}

	h.SetPollInterval(time.Minute)
	h.Start()

	h.WaitFor(2*time.Minute, "initial Swarm dependency service", func() bool {
		return h.SwarmContainerID(scheduledOneOffSwarmRecoveryStack, scheduledOneOffAppService) != ""
	})

	oneOffID := waitForRunningSwarmScheduledOneOff(t, h, scheduledOneOffSwarmRecoveryStack)

	h.WaitFor(30*time.Second, "Swarm dependency stopped before daemon termination", func() bool {
		return h.SwarmContainerID(scheduledOneOffSwarmRecoveryStack, scheduledOneOffAppService) == ""
	})

	h.KillAndRestartDaemon()

	h.WaitFor(30*time.Second, "replacement daemon reuses running Swarm one-off service", func() bool {
		return h.SwarmOneOffServiceID(scheduledOneOffSwarmRecoveryStack) == oneOffID &&
			h.SwarmServiceHasRunningTask(oneOffID)
	})

	h.WaitFor(2*time.Minute, "recovered Swarm one-off service cleanup", func() bool {
		return h.SwarmOneOffServiceID(scheduledOneOffSwarmRecoveryStack) == ""
	})

	h.WaitFor(30*time.Second, "Swarm dependency restored by recovered one-off job", func() bool {
		return h.SwarmContainerID(scheduledOneOffSwarmRecoveryStack, scheduledOneOffAppService) != ""
	})
}

func waitForRunningScheduledOneOff(t *testing.T, h *Harness, stack string) string {
	t.Helper()

	var oneOffID string

	h.WaitFor(2*time.Minute, "running scheduled one-off job", func() bool {
		oneOffID = h.RemoteOneOffContainerID(stack, scheduledOneOffJobService)
		return oneOffID != "" && h.RemoteContainerRunning(oneOffID)
	})

	return oneOffID
}

func waitForRunningSwarmScheduledOneOff(t *testing.T, h *Harness, stack string) string {
	t.Helper()

	var oneOffID string

	h.WaitFor(2*time.Minute, "running scheduled Swarm one-off job", func() bool {
		oneOffID = h.SwarmOneOffServiceID(stack)
		return oneOffID != "" && h.SwarmServiceHasRunningTask(oneOffID)
	})

	return oneOffID
}
