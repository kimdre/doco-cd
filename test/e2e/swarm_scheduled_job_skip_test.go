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

	swarmTypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/docker"
)

// TestSwarmScheduledJobPollSkip checks that polls do not redeploy a restart-mode
// Swarm job service. doco-cd deploys the service at 0 replicas, and a run scales it up.
// Both states differ from the Compose scale, but they are not drift.
func TestSwarmScheduledJobPollSkip(t *testing.T) {
	t.Parallel()

	for _, target := range []string{"explicit", "discovery"} {
		t.Run(target, func(t *testing.T) {
			t.Parallel()

			const (
				stack      = "e2e-swarm-job-skip"
				jobService = stack + "_job"
				deployed   = "completed stage: post-deploy"
				mismatch   = "swarm service status mismatch"
				fullSkip   = "no changes detected, skipping deployment"
				cachedSkip = "unchanged Git subtree and deployed project, skipping full pre-deploy"
			)

			h := NewHarness(t, "swarm-scheduled-job-skip")
			h.EnableRemoteSwarmContext()
			h.EnableAPI("e2e-swarm-job-skip-api-key")
			h.TrackStack(stack)

			if target == "discovery" {
				h.SetPollTarget(target)
			}

			h.Start()

			remoteDocker := h.remoteDockerClient()
			seenPolls := make(map[string]bool)
			// waitForPoll waits for a completed poll that logged message for the stack.
			// When the poll must skip, a deployment or a replica mismatch fails the test.
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

						if message != deployed && (entry.Message == deployed || entry.Message == mismatch) {
							t.Fatalf("unchanged poll did not skip %s: %s", stack, line)
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
			inspectJob := func() swarmTypes.Service {
				t.Helper()

				result, err := remoteDocker.ServiceInspect(h.ctx, jobService, client.ServiceInspectOptions{})
				if err != nil {
					t.Fatalf("inspect job service: %v", err)
				}

				return result.Service
			}
			replicas := func(svc swarmTypes.Service) uint64 {
				t.Helper()

				if svc.Spec.Mode.Replicated == nil || svc.Spec.Mode.Replicated.Replicas == nil {
					t.Fatalf("job service is not a replicated service: %#v", svc.Spec.Mode)
				}

				return *svc.Spec.Mode.Replicated.Replicas
			}
			// assertUnchanged fails when a poll updated the job service.
			assertUnchanged := func(before swarmTypes.Service, wantReplicas uint64) {
				t.Helper()

				current := inspectJob()
				if current.Version.Index != before.Version.Index ||
					current.Spec.Labels[docker.DocoCDLabels.Deployment.Timestamp] != before.Spec.Labels[docker.DocoCDLabels.Deployment.Timestamp] {
					t.Fatalf("unchanged poll updated the job service: version %d -> %d, labels %#v",
						before.Version.Index, current.Version.Index, current.Spec.Labels)
				}

				if got := replicas(current); got != wantReplicas {
					t.Fatalf("job service replicas = %d, want %d", got, wantReplicas)
				}
			}

			waitForPoll(0, deployed)

			initial := inspectJob()
			if got := replicas(initial); got != 0 {
				t.Fatalf("deployed job service replicas = %d, want 0", got)
			}

			if got := initial.Spec.TaskTemplate.ContainerSpec.Labels[docker.DocoCDJobLabels.JobRestartReplicas]; got != "1" {
				t.Fatalf("job service restart replicas label = %q, want 1", got)
			}

			// Phase 1: the idle job service at 0 replicas is not drift.
			waitForPoll(h.LogMark(), fullSkip)
			assertUnchanged(initial, 0)

			skipMessage := fullSkip
			if target == "discovery" {
				skipMessage = cachedSkip
				waitForPoll(h.LogMark(), cachedSkip)
				assertUnchanged(initial, 0)
			}

			// Phase 2: a restart-mode run scales the job service to 1 replica and leaves it there.
			job := getScheduledJobByName(t, h, "remote", stack, jobService)
			runID := triggerScheduledTimeoutJob(t, h, "remote", stack, job.Name)

			h.WaitFor(2*time.Minute, "scheduled job run succeeds", func() bool {
				var detail struct {
					Content controlplane.Run `json:"content"`
				}
				if err := json.Unmarshal(h.APIRequest(http.MethodGet, "/v1/api/run/"+url.PathEscape(runID), http.StatusOK), &detail); err != nil {
					t.Fatalf("decode scheduled run detail: %v", err)
				}

				switch detail.Content.Status {
				case controlplane.RunStatusAccepted, controlplane.RunStatusRunning:
					return false
				case controlplane.RunStatusSucceeded:
					return true
				default:
					t.Fatalf("scheduled job run did not succeed: %#v", detail.Content)
					return false
				}
			})

			var afterRun swarmTypes.Service

			h.WaitFor(time.Minute, "job service scaled up by its run", func() bool {
				afterRun = inspectJob()
				return replicas(afterRun) == 1
			})

			for range 2 {
				waitForPoll(h.LogMark(), skipMessage)
				assertUnchanged(afterRun, 1)
			}

			// Phase 3: any other replica count is drift, and the poll restores the idle state.
			spec := afterRun.Spec
			scaled := uint64(2)
			spec.Mode.Replicated.Replicas = &scaled

			mark := h.LogMark()
			if _, err := remoteDocker.ServiceUpdate(h.ctx, afterRun.ID, client.ServiceUpdateOptions{
				Version: afterRun.Version,
				Spec:    spec,
			}); err != nil {
				t.Fatalf("externally scale job service: %v", err)
			}

			waitForPoll(mark, deployed)

			restored := inspectJob()
			if got := replicas(restored); got != 0 {
				t.Fatalf("redeployed job service replicas = %d, want 0", got)
			}

			waitForPoll(h.LogMark(), skipMessage)
			assertUnchanged(restored, 0)

			t.Logf("%s deployment completed %d distinct polls", target, len(seenPolls))
		})
	}
}
