//go:build e2e

package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/controlplane"
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

	getJob := func() scheduledOneOffJob {
		t.Helper()
		return getScheduledOneOffJob(t, h, "remote", stack)
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

	var job scheduledOneOffJob

	h.WaitFor(90*time.Second, "scheduled one-off execution reported as running", func() bool {
		job = getJob()
		return job.Status == "running" && job.LastRun != nil && job.LastRun.Status == controlplane.RunStatusRunning
	})

	automaticRunID := job.LastRun.JobID
	if automaticRunID == "" {
		t.Fatal("automatic execution has no tracked run ID")
	}

	h.WaitFor(30*time.Second, "successful scheduled one-off result reported by job API", func() bool {
		job = getJob()

		return job.Status == "exited (0)" && job.LastRunAt != nil &&
			job.LastRun != nil && job.LastRun.JobID == automaticRunID && job.LastRun.Status == controlplane.RunStatusSucceeded &&
			h.RemoteOneOffContainerID(stack, scheduledOneOffJobService) == ""
	})
	assertScheduledOneOffRunDetail(t, h, job, automaticRunID)

	scheduledLastRun := *job.LastRunAt

	if !sourceFailed() {
		t.Fatal("scheduled one-off execution changed the source container's exit status")
	}

	runQuery := url.Values{"context": {"remote"}, "stack": {stack}, "wait": {"false"}}

	var accepted struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(h.APIRequest(http.MethodPost, "/v1/api/job/"+url.PathEscape(sourceName)+"/run?"+runQuery.Encode(), http.StatusAccepted), &accepted); err != nil {
		t.Fatalf("decode manual trigger response: %v", err)
	}

	if accepted.JobID == "" || accepted.JobID == automaticRunID {
		t.Fatalf("manual run ID = %q, want a new ID distinct from %q", accepted.JobID, automaticRunID)
	}

	h.WaitFor(10*time.Second, "manual one-off execution reported as running", func() bool {
		job = getJob()

		return job.Status == "running" && job.LastRun != nil &&
			job.LastRun.JobID == accepted.JobID && job.LastRun.Status == controlplane.RunStatusRunning
	})
	h.WaitFor(30*time.Second, "successful manual one-off result reported by job API", func() bool {
		job = getJob()

		return job.Status == "exited (0)" && job.LastRunAt != nil && job.LastRunAt.After(scheduledLastRun) &&
			job.LastRun != nil && job.LastRun.JobID == accepted.JobID && job.LastRun.Status == controlplane.RunStatusSucceeded &&
			h.RemoteOneOffContainerID(stack, scheduledOneOffJobService) == ""
	})
	assertScheduledOneOffRunDetail(t, h, job, accepted.JobID)

	if !sourceFailed() {
		t.Fatal("manual one-off execution changed the source container's exit status")
	}
}

func TestScheduledOneOff_RecoversAfterForcedDaemonTermination(t *testing.T) {
	t.Parallel()

	h := NewHarness(t, "scheduled-one-off-recovery")
	h.EnableRemoteContext()
	h.EnableAPI("e2e-scheduled-job-api-key")
	h.SetPollInterval(time.Minute)
	h.Start()

	h.WaitFor(2*time.Minute, "initial dependency container", func() bool {
		return h.RemoteComposeContainerID(scheduledOneOffRecoveryStack, scheduledOneOffAppService) != ""
	})

	oneOffID := waitForRunningScheduledOneOff(t, h, scheduledOneOffRecoveryStack)

	var job scheduledOneOffJob

	h.WaitFor(30*time.Second, "tracked one-off run before daemon termination", func() bool {
		job = getScheduledOneOffJob(t, h, "remote", scheduledOneOffRecoveryStack)
		return job.LastRun != nil && job.LastRun.JobID != "" && job.LastRun.Status == controlplane.RunStatusRunning
	})

	runID := job.LastRun.JobID

	startedAt := job.LastRun.StartedAt
	if startedAt == nil {
		t.Fatal("tracked one-off run has no original start time")
	}

	h.WaitFor(30*time.Second, "dependency stopped before daemon termination", func() bool {
		return h.RemoteComposeContainerID(scheduledOneOffRecoveryStack, scheduledOneOffAppService) == ""
	})

	h.KillAndRestartDaemon()

	h.WaitFor(30*time.Second, "replacement daemon retains running one-off job", func() bool {
		return h.RemoteOneOffContainerID(scheduledOneOffRecoveryStack, scheduledOneOffJobService) == oneOffID &&
			h.RemoteContainerRunning(oneOffID)
	})
	h.WaitFor(30*time.Second, "replacement daemon restores the original tracked run ID", func() bool {
		job = getScheduledOneOffJob(t, h, "remote", scheduledOneOffRecoveryStack)

		return job.LastRun != nil && job.LastRun.JobID == runID && job.LastRun.StartedAt != nil &&
			job.LastRun.StartedAt.Equal(*startedAt)
	})

	h.WaitFor(2*time.Minute, "recovered one-off job artifact cleanup", func() bool {
		return h.RemoteOneOffContainerID(scheduledOneOffRecoveryStack, scheduledOneOffJobService) == ""
	})

	h.WaitFor(30*time.Second, "dependency restored by recovered one-off job", func() bool {
		return h.RemoteComposeContainerID(scheduledOneOffRecoveryStack, scheduledOneOffAppService) != ""
	})
	h.WaitFor(30*time.Second, "recovered one-off run succeeded", func() bool {
		job = getScheduledOneOffJob(t, h, "remote", scheduledOneOffRecoveryStack)
		return job.LastRun != nil && job.LastRun.JobID == runID && job.LastRun.Status == controlplane.RunStatusSucceeded
	})
	assertScheduledOneOffRunDetail(t, h, job, runID)
}

func TestScheduledOneOff_SwarmRecoversAfterForcedDaemonTermination(t *testing.T) {
	// Scheduled-job discovery spans the shared Swarm cluster, so this test must
	// not run alongside other E2E scenarios that create scheduled services.
	h := NewHarness(t, "scheduled-one-off-swarm-recovery")
	if !h.isSwarmMode() {
		t.Skip("scheduled one-off Swarm recovery requires a Swarm manager")
	}

	h.EnableAPI("e2e-scheduled-job-api-key")
	h.SetPollInterval(time.Minute)
	h.Start()

	h.WaitFor(2*time.Minute, "initial Swarm dependency service", func() bool {
		return h.SwarmContainerID(scheduledOneOffSwarmRecoveryStack, scheduledOneOffAppService) != ""
	})

	oneOffID := waitForRunningSwarmScheduledOneOff(t, h, scheduledOneOffSwarmRecoveryStack)

	var job scheduledOneOffJob

	h.WaitFor(30*time.Second, "tracked Swarm one-off run before daemon termination", func() bool {
		job = getScheduledOneOffJob(t, h, "default", scheduledOneOffSwarmRecoveryStack)
		return job.LastRun != nil && job.LastRun.JobID != "" && job.LastRun.Status == controlplane.RunStatusRunning
	})

	runID := job.LastRun.JobID

	startedAt := job.LastRun.StartedAt
	if startedAt == nil {
		t.Fatal("tracked Swarm one-off run has no original start time")
	}

	h.WaitFor(30*time.Second, "Swarm dependency stopped before daemon termination", func() bool {
		return h.SwarmContainerID(scheduledOneOffSwarmRecoveryStack, scheduledOneOffAppService) == ""
	})

	h.KillAndRestartDaemon()

	h.WaitFor(30*time.Second, "replacement daemon reuses running Swarm one-off service", func() bool {
		return h.SwarmOneOffServiceID(scheduledOneOffSwarmRecoveryStack) == oneOffID &&
			h.SwarmServiceHasRunningTask(oneOffID)
	})
	h.WaitFor(30*time.Second, "replacement daemon restores the original tracked Swarm run ID", func() bool {
		job = getScheduledOneOffJob(t, h, "default", scheduledOneOffSwarmRecoveryStack)

		return job.LastRun != nil && job.LastRun.JobID == runID && job.LastRun.StartedAt != nil &&
			job.LastRun.StartedAt.Equal(*startedAt)
	})

	h.WaitFor(2*time.Minute, "recovered Swarm one-off service cleanup", func() bool {
		return h.SwarmOneOffServiceID(scheduledOneOffSwarmRecoveryStack) == ""
	})

	h.WaitFor(30*time.Second, "Swarm dependency restored by recovered one-off job", func() bool {
		return h.SwarmContainerID(scheduledOneOffSwarmRecoveryStack, scheduledOneOffAppService) != ""
	})
	h.WaitFor(30*time.Second, "recovered Swarm one-off run succeeded", func() bool {
		job = getScheduledOneOffJob(t, h, "default", scheduledOneOffSwarmRecoveryStack)
		return job.LastRun != nil && job.LastRun.JobID == runID && job.LastRun.Status == controlplane.RunStatusSucceeded
	})
	assertScheduledOneOffRunDetail(t, h, job, runID)
}

type scheduledOneOffJob struct {
	controlplane.ScheduledJobInfo
	lastRunJSON json.RawMessage
}

func getScheduledOneOffJob(t *testing.T, h *Harness, contextName, stack string) scheduledOneOffJob {
	t.Helper()

	query := url.Values{"context": {contextName}, "stack": {stack}}

	var response struct {
		Content []json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(h.APIRequest(http.MethodGet, "/v1/api/jobs?"+query.Encode(), http.StatusOK), &response); err != nil {
		t.Fatalf("decode job API response: %v", err)
	}

	if len(response.Content) != 1 {
		t.Fatalf("expected one scheduled job for %s/%s, got %s", contextName, stack, response.Content)
	}

	var job scheduledOneOffJob
	if err := json.Unmarshal(response.Content[0], &job.ScheduledJobInfo); err != nil {
		t.Fatalf("decode scheduled job: %v", err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(response.Content[0], &fields); err != nil {
		t.Fatalf("decode scheduled job fields: %v", err)
	}

	var present bool

	job.lastRunJSON, present = fields["last_run"]
	if !present {
		t.Fatal("scheduled job response must include last_run, even when null")
	}

	return job
}

func assertScheduledOneOffRunDetail(t *testing.T, h *Harness, job scheduledOneOffJob, runID string) {
	t.Helper()

	if job.LastRun == nil || job.LastRun.JobID != runID || job.LastRun.Trigger != controlplane.RunTriggerScheduledJob {
		t.Fatalf("last_run does not identify scheduled execution %q: %#v", runID, job.LastRun)
	}

	var detail struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(h.APIRequest(http.MethodGet, "/v1/api/run/"+url.PathEscape(runID), http.StatusOK), &detail); err != nil {
		t.Fatalf("decode run detail: %v", err)
	}

	var nestedJSON, detailJSON any
	if err := json.Unmarshal(job.lastRunJSON, &nestedJSON); err != nil {
		t.Fatalf("decode last_run JSON: %v", err)
	}

	if err := json.Unmarshal(detail.Content, &detailJSON); err != nil {
		t.Fatalf("decode run detail content: %v", err)
	}

	if !reflect.DeepEqual(nestedJSON, detailJSON) {
		t.Fatalf("last_run %s differs from complete run detail %s", job.lastRunJSON, detail.Content)
	}
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
