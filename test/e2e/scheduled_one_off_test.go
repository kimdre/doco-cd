//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/restapi"
)

const (
	scheduledOneOffRecoveryStack      = "e2e-scheduled-one-off-recovery"
	scheduledOneOffSwarmRecoveryStack = "e2e-scheduled-one-off-swarm-recovery"
	scheduledOneOffJobService         = "backup"
	scheduledOneOffAppService         = "app"
)

func TestScheduledOneOff_ManualRunBeforeFirstSchedule(t *testing.T) {
	requireStandaloneSuite(t)
	t.Parallel()

	const stack = "e2e-scheduled-one-off-status"

	h := NewHarness(t, "scheduled-one-off-status")
	h.EnableRemoteContext()
	h.EnableAPI("e2e-scheduled-job-api-key")
	h.SetPollInterval(time.Minute)
	h.Start()
	h.WaitForLog(deployCompletedLog, 2*time.Minute)

	var appID string

	h.WaitFor(2*time.Minute, "initial dependency container", func() bool {
		appID = h.RemoteComposeContainerID(stack, scheduledOneOffAppService)
		return appID != ""
	})

	getJob := func() scheduledOneOffJob {
		t.Helper()
		return getScheduledOneOffJob(t, h, "remote", stack)
	}

	initialJob := getJob()
	if initialJob.Status != "created" || initialJob.LastRun != nil || initialJob.NextRunAt == nil {
		t.Fatalf("initial job = %#v, want an unexecuted created source with a future schedule", initialJob)
	}

	source, err := h.remoteDockerClient().ContainerInspect(h.ctx, initialJob.Name, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect created source job: %v", err)
	}

	setGate := scheduledOneOffCompletionGate(t, h, appID)
	t.Cleanup(func() {
		if err := setGate("touch /state/complete"); err != nil {
			t.Errorf("release manual job completion gate during cleanup: %v", err)
		}
	})

	if err := setGate("rm -f /state/complete && touch /state/ready"); err != nil {
		t.Fatalf("prepare manual job completion gate: %v", err)
	}

	query := url.Values{"context": {"remote"}, "stack": {stack}, "wait": {"false"}}

	var accepted struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(h.APIRequest(http.MethodPost,
		"/v1/api/job/"+url.PathEscape(initialJob.Name)+"/run?"+query.Encode(), http.StatusAccepted), &accepted); err != nil {
		t.Fatalf("decode manual trigger response: %v", err)
	}

	h.WaitFor(10*time.Second, "manual-first one-off execution reported as running", func() bool {
		job := getJob()

		return job.Status == "running" && job.LastRun != nil &&
			job.LastRun.JobID == accepted.JobID && job.LastRun.Status == controlplane.RunStatusRunning
	})
	oneOffID := waitForRunningScheduledOneOff(t, h, stack)

	// Keep the API-owned execution alive through the worker's one-minute recovery sweep.
	holdUntil := time.Now().Add(65 * time.Second)
	h.WaitFor(75*time.Second, "manual-first execution survives the recovery sweep", func() bool {
		job := getJob()
		if job.Status != "running" || job.LastRun == nil || job.LastRun.JobID != accepted.JobID ||
			job.LastRun.Status != controlplane.RunStatusRunning || h.RemoteOneOffContainerID(stack, scheduledOneOffJobService) != oneOffID {
			t.Fatalf("manual execution changed during recovery sweep: %#v", job)
		}

		return !time.Now().Before(holdUntil)
	})

	if err := setGate("touch /state/complete"); err != nil {
		t.Fatalf("release manual job completion gate: %v", err)
	}

	var completed scheduledOneOffJob

	h.WaitFor(30*time.Second, "manual-first execution completed and cleaned up", func() bool {
		completed = getJob()

		return completed.Status == "exited (0)" && completed.LastRunAt != nil && completed.LastRun != nil &&
			completed.LastRun.JobID == accepted.JobID && completed.LastRun.Status == controlplane.RunStatusSucceeded &&
			completed.LastRun.FinishedAt != nil && h.RemoteOneOffContainerID(stack, scheduledOneOffJobService) == ""
	})
	h.WaitFor(30*time.Second, "dependency restored after manual-first execution", func() bool {
		return h.RemoteComposeContainerID(stack, scheduledOneOffAppService) == appID
	})
	assertScheduledOneOffRunDetail(t, h, completed, accepted.JobID)

	stableUntil := time.Now().Add(3 * time.Second)
	h.WaitFor(10*time.Second, "manual-first terminal status remains stable", func() bool {
		job := getJob()
		if job.Status != "exited (0)" || job.LastRun == nil || job.LastRun.Status != controlplane.RunStatusSucceeded {
			t.Fatalf("completed manual execution lost its result: %#v", job)
		}

		if job.NextRunAt == nil || !job.NextRunAt.Equal(*initialJob.NextRunAt) {
			t.Fatalf("manual execution changed its future schedule: %v, want %v", job.NextRunAt, initialJob.NextRunAt)
		}

		return !time.Now().Before(stableUntil)
	})

	unchanged, err := h.remoteDockerClient().ContainerInspect(h.ctx, initialJob.Name, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect source after manual-first execution: %v", err)
	}

	if unchanged.Container.ID != source.Container.ID || !reflect.DeepEqual(unchanged.Container.State, source.Container.State) {
		t.Fatalf("manual one-off changed created source: before %#v, after %#v", source.Container.State, unchanged.Container.State)
	}
}

func TestScheduledOneOff_StatusUsesLatestExecution(t *testing.T) {
	requireStandaloneSuite(t)
	t.Parallel()

	const stack = "e2e-scheduled-one-off-status"

	h := NewHarness(t, "scheduled-one-off-status")
	h.EnableRemoteContext()
	h.EnableAPI("e2e-scheduled-job-api-key")
	h.SetPollInterval(time.Minute)
	h.Start()
	h.WaitForLog(deployCompletedLog, 2*time.Minute)

	// Arm a single calendar occurrence after startup, rather than racing a
	// repeating timer while checking the automatic result and manual execution.
	schedule, scheduledAt := scheduledOneOffCalendarSchedule(time.Now())
	h.ReplaceInWorktree("deploy/compose.yaml", `"@every 24h"`, fmt.Sprintf("%q", schedule))
	h.RepoPush("e2e: arm one automatic scheduled execution")
	pollID := triggerScheduledOneOffFixturePoll(t, h)
	h.WaitFor(2*time.Minute, "calendar schedule deployment completed", func() bool {
		var response struct {
			Content controlplane.Run `json:"content"`
		}
		if err := json.Unmarshal(h.APIRequest(http.MethodGet, "/v1/api/run/"+url.PathEscape(pollID), http.StatusOK), &response); err != nil {
			t.Fatalf("decode calendar schedule deployment: %v", err)
		}

		if response.Content.Status == controlplane.RunStatusFailed || response.Content.Status == controlplane.RunStatusSkipped {
			t.Fatalf("calendar schedule deployment did not succeed: %#v", response.Content)
		}

		return response.Content.Status == controlplane.RunStatusSucceeded && response.Content.FinishedAt != nil
	})

	var appID string

	h.WaitFor(2*time.Minute, "initial dependency container", func() bool {
		appID = h.RemoteComposeContainerID(stack, scheduledOneOffAppService)
		return appID != ""
	})

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

	failedSource, err := remoteDocker.ContainerInspect(h.ctx, sourceName, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect failed source job container: %v", err)
	}

	assertSourceUnchanged := func() {
		t.Helper()

		result, inspectErr := remoteDocker.ContainerInspect(h.ctx, sourceName, client.ContainerInspectOptions{})
		if inspectErr != nil {
			t.Fatalf("inspect source job container after one-off execution: %v", inspectErr)
		}

		if result.Container.ID != failedSource.Container.ID || !reflect.DeepEqual(result.Container.State, failedSource.Container.State) {
			t.Fatalf("one-off execution changed the failed source container: before %#v, after %#v",
				failedSource.Container.State, result.Container.State)
		}
	}

	if got := getJob().Status; got != "exited (7)" {
		t.Fatalf("job status before one-off execution = %q, want exited (7)", got)
	}

	setGate := scheduledOneOffCompletionGate(t, h, appID)

	t.Cleanup(func() {
		if err := setGate("touch /state/complete"); err != nil {
			t.Errorf("release scheduled job completion gate during cleanup: %v", err)
		}
	})

	if err := setGate("rm -f /state/complete && touch /state/ready"); err != nil {
		t.Fatalf("prepare automatic job completion gate: %v", err)
	}

	if !time.Now().Before(scheduledAt) {
		t.Fatalf("missed automatic calendar occurrence at %s while preparing the job", scheduledAt)
	}

	waitForStoppedDependency := func(oneOffID, phase string) {
		t.Helper()
		h.WaitFor(30*time.Second, phase+" dependency stopped while one-off job runs", func() bool {
			return !h.RemoteContainerRunning(appID) &&
				h.RemoteComposeContainerID(stack, scheduledOneOffAppService) == "" &&
				h.RemoteOneOffContainerID(stack, scheduledOneOffJobService) == oneOffID &&
				h.RemoteContainerRunning(oneOffID)
		})
	}
	waitForCleanupAndRestoration := func(phase string) {
		t.Helper()
		h.WaitFor(2*time.Minute, phase+" one-off job artifact cleanup", func() bool {
			return h.RemoteOneOffContainerID(stack, scheduledOneOffJobService) == ""
		})
		h.WaitFor(30*time.Second, phase+" dependency restored after one-off job", func() bool {
			return h.RemoteComposeContainerID(stack, scheduledOneOffAppService) == appID
		})
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

	automaticOneOffID := waitForRunningScheduledOneOff(t, h, stack)
	waitForStoppedDependency(automaticOneOffID, "automatic")

	h.WaitFor(30*time.Second, "automatic execution fenced from another timer occurrence", func() bool {
		job = getJob()

		return job.Schedule == schedule && job.NextRunAt != nil && job.NextRunAt.After(time.Now().Add(24*time.Hour)) &&
			job.LastRun != nil && job.LastRun.JobID == automaticRunID && job.LastRun.Status == controlplane.RunStatusRunning
	})

	if err := setGate("touch /state/complete"); err != nil {
		t.Fatalf("release automatic job completion gate: %v", err)
	}

	h.WaitFor(30*time.Second, "successful scheduled one-off result reported by job API", func() bool {
		job = getJob()

		return job.Status == "exited (0)" && job.LastRunAt != nil &&
			job.LastRun != nil && job.LastRun.JobID == automaticRunID && job.LastRun.Status == controlplane.RunStatusSucceeded &&
			h.RemoteOneOffContainerID(stack, scheduledOneOffJobService) == ""
	})
	waitForCleanupAndRestoration("automatic")
	assertScheduledOneOffRunDetail(t, h, job, automaticRunID)

	scheduledLastRun := *job.LastRunAt

	assertSourceUnchanged()

	if err := setGate("rm -f /state/complete"); err != nil {
		t.Fatalf("reset manual job completion gate: %v", err)
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

	manualOneOffID := waitForRunningScheduledOneOff(t, h, stack)
	if manualOneOffID == automaticOneOffID {
		t.Fatalf("manual one-off container ID = %q, want a new artifact", manualOneOffID)
	}

	waitForStoppedDependency(manualOneOffID, "manual")

	if err := setGate("touch /state/complete"); err != nil {
		t.Fatalf("release manual job completion gate: %v", err)
	}

	h.WaitFor(30*time.Second, "successful manual one-off result reported by job API", func() bool {
		job = getJob()

		return job.Status == "exited (0)" && job.LastRunAt != nil && job.LastRunAt.After(scheduledLastRun) &&
			job.LastRun != nil && job.LastRun.JobID == accepted.JobID && job.LastRun.Status == controlplane.RunStatusSucceeded &&
			h.RemoteOneOffContainerID(stack, scheduledOneOffJobService) == ""
	})
	waitForCleanupAndRestoration("manual")
	assertScheduledOneOffRunDetail(t, h, job, accepted.JobID)

	assertSourceUnchanged()
}

func scheduledOneOffCompletionGate(t *testing.T, h *Harness, appID string) func(string) error {
	t.Helper()

	app, err := h.remoteDockerClient().ContainerInspect(h.ctx, appID, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect job dependency: %v", err)
	}

	var gateVolume string

	for _, mount := range app.Container.Mounts {
		if mount.Destination == "/state" && mount.Name != "" {
			gateVolume = mount.Name
			break
		}
	}

	if gateVolume == "" || app.Container.Config == nil {
		t.Fatal("job dependency has no shared gate volume or image configuration")
	}

	// The dependency is stopped during each run, so gate commands need their own container.
	return func(command string) error {
		ctx, cancel := context.WithTimeout(h.ctx, 30*time.Second)
		defer cancel()

		exitCode, output, execErr := h.remoteDaemon.Exec(ctx, []string{
			"docker", "run", "--rm", "--network=none", "--pull=never",
			"--mount", "type=volume,src=" + gateVolume + ",dst=/state",
			app.Container.Config.Image, "sh", "-c", command,
		})
		if execErr != nil {
			return fmt.Errorf("run gate command: %w", execErr)
		}

		body, readErr := io.ReadAll(output)
		if readErr != nil {
			return fmt.Errorf("read gate command output: %w", readErr)
		}

		if exitCode != 0 {
			return fmt.Errorf("gate command exited with code %d: %s", exitCode, body)
		}

		return nil
	}
}

func scheduledOneOffCalendarSchedule(now time.Time) (string, time.Time) {
	scheduledAt := now.UTC().Add(90 * time.Second).Truncate(time.Minute)
	schedule := fmt.Sprintf("CRON_TZ=UTC %d %d %d %d *",
		scheduledAt.Minute(), scheduledAt.Hour(), scheduledAt.Day(), scheduledAt.Month())

	return schedule, scheduledAt
}

func TestScheduledOneOffCalendarSchedule(t *testing.T) {
	t.Parallel()

	for _, now := range []time.Time{
		time.Date(2026, time.October, 11, 17, 30, 0, 0, time.UTC),
		time.Date(2026, time.October, 11, 17, 30, 59, 0, time.FixedZone("offset", -7*60*60)),
		time.Date(2026, time.December, 31, 23, 59, 0, 0, time.UTC),
		time.Date(2028, time.February, 29, 12, 0, 0, 0, time.UTC),
	} {
		t.Run(now.Format(time.RFC3339), func(t *testing.T) {
			expression, scheduledAt := scheduledOneOffCalendarSchedule(now)

			schedule, err := docker.ParseJobScheduleExpression(expression)
			if err != nil {
				t.Fatalf("parse calendar schedule %q: %v", expression, err)
			}

			if got := schedule.Next(now); !got.Equal(scheduledAt) {
				t.Fatalf("first occurrence = %s, want %s", got, scheduledAt)
			}

			if delay := scheduledAt.Sub(now); delay <= 30*time.Second || delay > 90*time.Second {
				t.Fatalf("first occurrence delay = %s, want (30s, 90s]", delay)
			}

			if next := schedule.Next(scheduledAt); !next.After(scheduledAt.Add(24 * time.Hour)) {
				t.Fatalf("next occurrence = %s, want beyond the test window after %s", next, scheduledAt)
			}
		})
	}
}

func triggerScheduledOneOffFixturePoll(t *testing.T, h *Harness) string {
	t.Helper()

	endpoint, err := h.daemon.Endpoint(h.ctx, "http")
	if err != nil {
		t.Fatalf("get daemon API endpoint for schedule deployment: %v", err)
	}

	ctx, cancel := context.WithTimeout(h.ctx, 15*time.Second)
	defer cancel()

	body := fmt.Appendf(nil, `[{"url":"http://gitserver/%s.git","reference":"refs/heads/main"}]`, h.scenario)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/v1/api/poll/run?wait=false", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create calendar schedule deployment request: %v", err)
	}

	req.Header.Set(restapi.KeyHeader, h.apiKey)
	req.Header.Set("Content-Type", "application/json")

	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("trigger calendar schedule deployment: %v", err)
	}
	defer response.Body.Close()

	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read calendar schedule deployment response: %v", err)
	}

	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("calendar schedule deployment response = %d, want 202: %s", response.StatusCode, data)
	}

	var accepted struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(data, &accepted); err != nil {
		t.Fatalf("decode calendar schedule deployment response: %v", err)
	}

	if accepted.JobID == "" {
		t.Fatal("calendar schedule deployment has no tracked run ID")
	}

	return accepted.JobID
}

func TestScheduledOneOff_RecoversAfterForcedDaemonTermination(t *testing.T) {
	requireStandaloneSuite(t)
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
	waitForRecoveredScheduledRun(t, h, runID, *startedAt)
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
	waitForRecoveredScheduledRun(t, h, runID, *startedAt)
}

func waitForRecoveredScheduledRun(t *testing.T, h *Harness, runID string, startedAt time.Time) {
	t.Helper()

	h.WaitFor(30*time.Second, "recovered scheduled run succeeded", func() bool {
		var detail struct {
			Content controlplane.Run `json:"content"`
		}
		if err := json.Unmarshal(h.APIRequest(http.MethodGet, "/v1/api/run/"+url.PathEscape(runID), http.StatusOK), &detail); err != nil {
			t.Fatalf("decode recovered run detail: %v", err)
		}

		run := detail.Content
		if run.JobID != runID || run.Trigger != controlplane.RunTriggerScheduledJob ||
			run.StartedAt == nil || !run.StartedAt.Equal(startedAt) {
			t.Fatalf("recovered run identity or start time changed: %#v", run)
		}

		if run.Status == controlplane.RunStatusFailed {
			t.Fatalf("recovered scheduled run failed: %s", run.Message)
		}

		return run.Status == controlplane.RunStatusSucceeded && run.FinishedAt != nil
	})
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

	jobJSON, err := selectScheduledOneOffJob(response.Content, stack)
	if err != nil {
		t.Fatalf("select scheduled job for %s/%s: %v", contextName, stack, err)
	}

	var job scheduledOneOffJob
	if err := json.Unmarshal(jobJSON, &job.ScheduledJobInfo); err != nil {
		t.Fatalf("decode scheduled job: %v", err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(jobJSON, &fields); err != nil {
		t.Fatalf("decode scheduled job fields: %v", err)
	}

	var present bool

	job.lastRunJSON, present = fields["last_run"]
	if !present {
		t.Fatal("scheduled job response must include last_run, even when null")
	}

	return job
}

func selectScheduledOneOffJob(jobs []json.RawMessage, stack string) (json.RawMessage, error) {
	var selected json.RawMessage

	for _, data := range jobs {
		var job controlplane.ScheduledJobInfo
		if err := json.Unmarshal(data, &job); err != nil {
			return nil, fmt.Errorf("decode scheduled job: %w", err)
		}

		name := stack + "-" + scheduledOneOffJobService + "-1"
		if job.Mode == "swarm" {
			name = stack + "_" + scheduledOneOffJobService
		}

		if job.Stack != stack || job.Name != name {
			continue
		}

		if selected != nil {
			return nil, fmt.Errorf("multiple backup jobs for %s: %s", stack, jobs)
		}

		selected = data
	}

	if selected == nil {
		return nil, fmt.Errorf("backup job not found for %s: %s", stack, jobs)
	}

	return selected, nil
}

func TestSelectScheduledOneOffJob(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"container", "swarm"} {
		t.Run(mode, func(t *testing.T) {
			name := "stack-backup-1"
			if mode == "swarm" {
				name = "stack_backup"
			}

			backup := json.RawMessage(fmt.Sprintf(`{"name":%q,"stack":"stack","mode":%q,"last_run":null}`, name, mode))
			app := json.RawMessage(`{"name":"stack_app","stack":"stack","mode":"swarm","last_run":null}`)

			for _, jobs := range [][]json.RawMessage{{backup}, {app, backup}, {backup, app}} {
				selected, err := selectScheduledOneOffJob(jobs, "stack")
				if err != nil || string(selected) != string(backup) {
					t.Fatalf("selected = %s, error = %v", selected, err)
				}
			}

			for _, jobs := range [][]json.RawMessage{nil, {app}, {backup, backup}, {json.RawMessage(`invalid`)}} {
				if _, err := selectScheduledOneOffJob(jobs, "stack"); err == nil {
					t.Fatalf("invalid job list accepted: %s", jobs)
				}
			}
		})
	}
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
