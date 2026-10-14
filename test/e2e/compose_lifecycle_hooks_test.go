//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/docker"
)

// TestComposeLifecycleHooks verifies that doco-cd runs the Compose lifecycle hooks of a service
// when it stops and starts the service for a scheduled job and when reconciliation restarts it.
// The hooks of the db service write their names to a shared log.
func TestComposeLifecycleHooks(t *testing.T) {
	requireStandaloneSuite(t)
	t.Parallel()

	const (
		stack   = "e2e-compose-lifecycle-hooks"
		service = "db"
	)

	h := NewHarness(t, "compose-lifecycle-hooks")
	h.EnableAPI("e2e-compose-lifecycle-hooks-api-key")
	h.SetPollInterval(24 * time.Hour)
	h.TrackVolume(stack + "_marks")
	h.Start()
	h.WaitForLog(deployCompletedLog, 2*time.Minute)
	h.WaitForLog("reconciliation event listeners ready", 2*time.Minute)

	dbID := h.ComposeContainerID(stack, service)
	if dbID == "" {
		t.Fatal("db container is not running after the deployment")
	}

	// The label lets doco-cd skip the definition load for services without hooks.
	inspect, err := h.docker.ContainerInspect(h.ctx, dbID, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect db container: %v", err)
	}

	if got := inspect.Container.Config.Labels[docker.DocoCDLabels.Deployment.LifecycleHooks]; got != "pre_start,post_start,pre_stop" {
		t.Fatalf("db lifecycle hooks label = %q, want all phases", got)
	}

	waitForHookLog := func(desc string, want ...string) {
		t.Helper()

		var got string

		defer func() {
			if t.Failed() {
				t.Logf("last hook log: %q", got)
			}
		}()

		h.WaitFor(time.Minute, desc, func() bool {
			if !h.ContainerRunning(dbID) {
				return false
			}

			got = strings.Join(strings.Fields(h.ExecOutput(dbID, "cat", "/marks/log")), " ")

			return got == strings.Join(want, " ")
		})
	}

	waitForHookLog("hooks of the deployment", "pre_start", "post_start")

	// Reconciliation restarts the unhealthy container like `docker compose restart`.
	// The pre_stop hook removes the marker, so the restarted container is healthy.
	// This runs before the job, because reconciliation ignores events shortly after stop_services.
	mark := h.LogMark()
	h.ExecOutput(dbID, "touch", "/marks/unhealthy")
	h.WaitForLogAfter("container restarted successfully", mark, time.Minute)

	waitForHookLog("hooks of the reconciliation restart",
		"pre_start", "post_start", "pre_stop", "post_start")

	if id := h.ComposeContainerID(stack, service); id != dbID {
		t.Fatalf("reconciliation recreated the db container: %s -> %s", dbID, id)
	}

	job := getScheduledOneOffJob(t, h, "default", stack)
	query := url.Values{"context": {"default"}, "stack": {stack}, "wait": {"false"}}

	var accepted struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(h.APIRequest(http.MethodPost,
		"/v1/api/job/"+url.PathEscape(job.Name)+"/run?"+query.Encode(), http.StatusAccepted), &accepted); err != nil {
		t.Fatalf("decode manual run response: %v", err)
	}

	h.WaitFor(2*time.Minute, "scheduled job run completed", func() bool {
		var response struct {
			Content controlplane.Run `json:"content"`
		}
		if err := json.Unmarshal(h.APIRequest(http.MethodGet,
			"/v1/api/run/"+url.PathEscape(accepted.JobID), http.StatusOK), &response); err != nil {
			t.Fatalf("decode manual run: %v", err)
		}

		run := response.Content
		if run.Status == controlplane.RunStatusAccepted || run.Status == controlplane.RunStatusRunning {
			return false
		}

		if run.Status != controlplane.RunStatusSucceeded {
			t.Fatalf("scheduled job run = %#v, want %s", run, controlplane.RunStatusSucceeded)
		}

		return true
	})

	// stop_services runs pre_stop before the stop, and the start runs pre_start and post_start again.
	waitForHookLog("hooks of stop_services",
		"pre_start", "post_start", "pre_stop", "post_start", "pre_stop", "job", "pre_start", "post_start")

	runners, err := h.docker.ContainerList(h.ctx, client.ContainerListOptions{
		All: true,
		Filters: client.Filters{}.
			Add("label", api.ProjectLabel+"="+stack).
			Add("label", api.HookLabel+"=pre_start"),
	})
	if err != nil {
		t.Fatalf("list pre_start hook runners: %v", err)
	}

	if len(runners.Items) != 0 {
		t.Fatalf("pre_start hook runners remain after the job: %#v", runners.Items)
	}
}
