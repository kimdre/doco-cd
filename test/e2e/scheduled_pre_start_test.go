//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/docker"
	internalgit "github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func TestScheduledPreStart_UnrelatedWebhooks(t *testing.T) {
	requireStandaloneSuite(t)
	t.Parallel()

	const (
		scenario = "scheduled-pre-start"
		stack    = "e2e-scheduled-pre-start"
		volume   = stack + "_job-state"
		cloneURL = "http://gitserver/" + scenario + ".git"
	)

	webhookSecret := rand.Text()

	h := NewHarness(t, scenario)
	h.EnableRemoteContext()
	h.EnableAPI("e2e-scheduled-pre-start-api-key")
	h.SetEnv("WEBHOOK_SECRET", webhookSecret)
	h.SetPollInterval(24 * time.Hour)
	h.TrackStack(stack)
	h.TrackVolume(volume)
	h.Start()
	h.WaitForLog(deployCompletedLog, 2*time.Minute)

	ctx, cancel := context.WithTimeout(h.ctx, 5*time.Minute)
	defer cancel()

	remoteDocker := h.remoteDockerClient()
	inventory := func() (sourceID, hookID string) {
		t.Helper()

		filters := client.Filters{}.
			Add("label", api.ProjectLabel+"="+stack).
			Add("label", api.ServiceLabel+"="+scheduledOneOffJobService)

		result, err := remoteDocker.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
		if err != nil {
			t.Fatalf("list scheduled source and pre_start hook: %v", err)
		}

		if len(result.Items) != 2 {
			t.Fatalf("scheduled source inventory = %#v, want exactly one source and one hook", result.Items)
		}

		for _, item := range result.Items {
			if item.Labels[api.HookLabel] != "" {
				if hookID != "" {
					t.Fatalf("multiple prepared hooks: %#v", result.Items)
				}

				hookID = item.ID
			} else {
				if sourceID != "" {
					t.Fatalf("multiple scheduled source containers: %#v", result.Items)
				}

				sourceID = item.ID
			}
		}

		if sourceID == "" || hookID == "" {
			t.Fatalf("missing source or hook in scheduled inventory: %#v", result.Items)
		}

		return sourceID, hookID
	}
	inspectCreated := func(id string) client.ContainerInspectResult {
		t.Helper()

		result, err := remoteDocker.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
		if err != nil {
			t.Fatalf("inspect scheduled source or hook %s: %v", id, err)
		}

		if result.Container.Config == nil || result.Container.State == nil ||
			result.Container.State.Status != "created" || result.Container.State.Running {
			t.Fatalf("container %s is not an unstarted Created container: %#v", id, result.Container)
		}

		return result
	}

	sourceID, hookID := inventory()
	source := inspectCreated(sourceID)
	hook := inspectCreated(hookID)
	idle := inspectCreated(stack + "-idle-1")

	initialHead, err := h.repo.Head()
	if err != nil {
		t.Fatalf("read initial fixture HEAD: %v", err)
	}

	labels := source.Container.Config.Labels

	expectedDiscovery := docker.MarshalAutoDiscoveryConfig(deploy.AutoDiscoveryConfig{
		Enabled:       true,
		Delete:        true,
		RemoveVolumes: false,
		RemoveImages:  true,
	})
	if labels[docker.DocoCDLabels.Deployment.AutoDiscovery] != "true" ||
		labels[docker.DocoCDLabels.Deployment.AutoDiscoveryConfig] != expectedDiscovery {
		t.Fatalf("source auto-discovery labels = %#v, want enabled discovery config %q", labels, expectedDiscovery)
	}

	for key, want := range map[string]string{
		docker.DocoCDLabels.Deployment.Name:      stack,
		docker.DocoCDLabels.Deployment.CommitSHA: initialHead.Hash().String(),
		docker.DocoCDLabels.Source.URL:           cloneURL,
		docker.DocoCDLabels.Source.Name:          internalgit.GetFullName(cloneURL),
	} {
		if got := labels[key]; got != want {
			t.Fatalf("source label %s = %q, want %q", key, got, want)
		}
	}

	for _, key := range []string{
		docker.DocoCDLabels.Deployment.ComposeHash,
		docker.DocoCDLabels.Deployment.ConfigHash,
		docker.DocoCDLabels.Deployment.Timestamp,
	} {
		if labels[key] == "" {
			t.Fatalf("source deployment metadata label %s is empty", key)
		}
	}

	if hook.Container.Config.Labels[docker.DocoCDLabels.Deployment.AutoDiscoveryConfig] != "" {
		t.Fatal("prepared hook unexpectedly carries the source auto-discovery config")
	}

	assertUnexecutedJob := func() {
		t.Helper()

		job := getScheduledOneOffJob(t, h, "remote", stack)
		if job.Status != "created" || job.LastRun != nil || job.NextRunAt == nil || !job.NextRunAt.After(time.Now()) {
			t.Fatalf("scheduled job = %#v, want an unexecuted Created source with a future schedule", job)
		}
	}
	assertUnexecutedJob()

	for revision := 1; revision <= 2; revision++ {
		before, hErr := h.repo.Head()
		if hErr != nil {
			t.Fatalf("read fixture HEAD before unrelated commit %d: %v", revision, hErr)
		}

		mark := h.LogMark()
		h.ReplaceInWorktree("notes.txt", fmt.Sprintf("revision %d", revision-1), fmt.Sprintf("revision %d", revision))

		after := h.RepoPush(fmt.Sprintf("e2e: unrelated change %d", revision))
		if after == before.Hash() {
			t.Fatalf("unrelated commit %d did not advance HEAD", revision)
		}

		payload := webhook.GithubPushPayload{
			Ref:    "refs/heads/main",
			Before: before.Hash().String(),
			After:  after.String(),
		}
		payload.Repository.Name = scenario
		payload.Repository.FullName = internalgit.GetFullName(cloneURL)
		payload.Repository.CloneURL = cloneURL
		payload.Repository.WebURL = cloneURL

		runID := sendScheduledPreStartWebhook(t, h, webhookSecret, payload)
		h.WaitFor(2*time.Minute, "unrelated webhook "+runID+" skipped the discovered stack", func() bool {
			var response struct {
				Content controlplane.Run `json:"content"`
			}
			if decodeErr := json.Unmarshal(h.APIRequest(http.MethodGet,
				"/v1/api/run/"+url.PathEscape(runID), http.StatusOK), &response); decodeErr != nil {
				t.Fatalf("decode webhook run %s: %v", runID, decodeErr)
			}

			run := response.Content
			if run.JobID != runID || run.Trigger != controlplane.RunTriggerWebhook ||
				run.Repository != payload.Repository.FullName || run.Revision != fmt.Sprintf("%s (%s)", payload.Ref, payload.After) {
				t.Fatalf("webhook run identity does not match the pushed commit: %#v", run)
			}

			if run.Status == controlplane.RunStatusAccepted || run.Status == controlplane.RunStatusRunning {
				return false
			}

			if run.Status != controlplane.RunStatusSkipped || run.Message != "deployment skipped" || run.FinishedAt == nil {
				t.Fatalf("unrelated webhook did not complete as an unchanged-deployment skip: %#v", run)
			}

			for _, target := range run.Deployments {
				if target.Stack == stack && target.Context == "remote" {
					return true
				}
			}

			t.Fatalf("webhook skipped without processing the discovered stack: %#v", run)

			return false
		})

		// The run API reports a generic skip message, so check its stack-specific reason too.
		h.WaitFor(15*time.Second, "unchanged-deployment reason for webhook "+runID, func() bool {
			for _, line := range strings.Split(h.logsSince(mark), "\n") {
				if strings.Contains(line, `"job_id":"`+runID+`"`) && strings.Contains(line, `"stack":"`+stack+`"`) &&
					(strings.Contains(line, "no changes detected, skipping deployment") ||
						strings.Contains(line, "unchanged Git subtree and deployed project, skipping full pre-deploy")) {
					return true
				}
			}

			return false
		})

		currentSourceID, currentHookID := inventory()
		if currentSourceID != sourceID || currentHookID != hookID {
			t.Fatalf("webhook %d recreated scheduled containers: source %s -> %s, hook %s -> %s",
				revision, sourceID, currentSourceID, hookID, currentHookID)
		}

		for _, initial := range []client.ContainerInspectResult{source, hook} {
			current := inspectCreated(initial.Container.ID)
			if current.Container.Created != initial.Container.Created ||
				!reflect.DeepEqual(current.Container.State, initial.Container.State) ||
				!reflect.DeepEqual(current.Container.Config.Labels, initial.Container.Config.Labels) {
				t.Fatalf("webhook %d changed container %s state or deployment metadata: before %#v, after %#v",
					revision, initial.Container.ID, initial.Container, current.Container)
			}
		}

		assertUnexecutedJob()
	}

	setState := scheduledOneOffCompletionGate(t, h, sourceID)
	previousState := source.Container.State

	for _, attempt := range []struct {
		name      string
		failHook  bool
		hookCount int
		runCount  int
	}{
		{name: "first run", hookCount: 1, runCount: 1},
		{name: "repeat run", hookCount: 2, runCount: 2},
		{name: "failed hook", failHook: true, hookCount: 3, runCount: 2},
		{name: "recovery after failed hook", hookCount: 4, runCount: 3},
	} {
		if attempt.failHook {
			if err := setState("touch /state/fail-hook"); err != nil {
				t.Fatalf("arm hook failure: %v", err)
			}
		} else if err := setState("rm -f /state/fail-hook"); err != nil {
			t.Fatalf("clear hook failure: %v", err)
		}

		query := url.Values{"context": {"remote"}, "stack": {stack}, "wait": {"false"}}

		var accepted struct {
			JobID string `json:"job_id"`
		}
		if err := json.Unmarshal(h.APIRequest(http.MethodPost,
			"/v1/api/job/"+url.PathEscape(getScheduledOneOffJob(t, h, "remote", stack).Name)+"/run?"+query.Encode(),
			http.StatusAccepted), &accepted); err != nil {
			t.Fatalf("decode manual run response: %v", err)
		}

		h.WaitFor(time.Minute, attempt.name+" completed", func() bool {
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

			wantStatus := controlplane.RunStatusSucceeded
			if attempt.failHook {
				wantStatus = controlplane.RunStatusFailed
			}

			if run.Status != wantStatus || run.FinishedAt == nil {
				t.Fatalf("%s: run = %#v, want %s", attempt.name, run, wantStatus)
			}

			return true
		})

		h.WaitFor(15*time.Second, attempt.name+" left the source stopped", func() bool {
			current, err := remoteDocker.ContainerInspect(ctx, sourceID, client.ContainerInspectOptions{})
			if err != nil {
				t.Fatalf("inspect source after %s: %v", attempt.name, err)
			}

			if current.Container.Created != source.Container.Created ||
				!reflect.DeepEqual(current.Container.Config.Labels, labels) {
				t.Fatalf("%s changed source identity or deployment metadata", attempt.name)
			}

			state := current.Container.State
			if state.Running {
				return false
			}

			if attempt.failHook {
				if !reflect.DeepEqual(state, previousState) {
					t.Fatalf("failed hook changed the workload state: before %#v, after %#v", previousState, state)
				}
			} else if state.Status != "exited" || state.ExitCode != 0 || state.StartedAt == previousState.StartedAt {
				t.Fatalf("%s did not execute the workload successfully: %#v", attempt.name, state)
			}

			previousState = state

			return true
		})

		if err := setState(fmt.Sprintf(
			`test "$(wc -l < /state/hooks)" -eq %d && test "$(wc -l < /state/runs)" -eq %d`,
			attempt.hookCount, attempt.runCount)); err != nil {
			t.Fatalf("%s did not run hooks before the workload: %v", attempt.name, err)
		}

		hooks, err := remoteDocker.ContainerList(ctx, client.ContainerListOptions{
			All: true,
			Filters: client.Filters{}.
				Add("label", api.ProjectLabel+"="+stack).
				Add("label", api.HookLabel+"=pre_start"),
		})
		if err != nil {
			t.Fatalf("list hooks after %s: %v", attempt.name, err)
		}

		wantHooks := 0
		if attempt.failHook {
			wantHooks = 1
		}

		if len(hooks.Items) != wantHooks {
			t.Fatalf("%s: retained hooks = %#v, want %d", attempt.name, hooks.Items, wantHooks)
		}

		currentIdle := inspectCreated(idle.Container.ID)
		if !reflect.DeepEqual(currentIdle.Container.State, idle.Container.State) {
			t.Fatalf("%s changed an unrelated autostart-disabled service", attempt.name)
		}
	}

	mark := h.LogMark()

	before, err := h.repo.Head()
	if err != nil {
		t.Fatalf("read HEAD before unsupported one-off config: %v", err)
	}

	h.ReplaceInWorktree("stacks/"+stack+"/compose.yaml", `execution_mode: "restart"`, `execution_mode: "one_off"`)
	after := h.RepoPush("e2e: reject one-off pre_start instead of silently skipping it")
	payload := webhook.GithubPushPayload{Ref: "refs/heads/main", Before: before.Hash().String(), After: after.String()}
	payload.Repository.Name = scenario
	payload.Repository.FullName = internalgit.GetFullName(cloneURL)
	payload.Repository.CloneURL = cloneURL
	payload.Repository.WebURL = cloneURL
	runID := sendScheduledPreStartWebhook(t, h, webhookSecret, payload)
	h.WaitForLogLineAfter(mark, time.Minute, runID, docker.ErrComposeOneOffPreStartUnsupported.Error())
	h.WaitFor(15*time.Second, "unsupported one-off configuration reported as failed", func() bool {
		var response struct {
			Content controlplane.Run `json:"content"`
		}
		if err := json.Unmarshal(h.APIRequest(http.MethodGet,
			"/v1/api/run/"+url.PathEscape(runID), http.StatusOK), &response); err != nil {
			t.Fatalf("decode rejected deployment: %v", err)
		}

		run := response.Content
		if run.Status == controlplane.RunStatusAccepted || run.Status == controlplane.RunStatusRunning {
			return false
		}

		if run.Status != controlplane.RunStatusFailed ||
			!strings.Contains(run.Message, docker.ErrComposeOneOffPreStartUnsupported.Error()) {
			t.Fatalf("unsupported one-off deployment was not rejected: %#v", run)
		}

		return run.FinishedAt != nil
	})

	current, err := remoteDocker.ContainerInspect(ctx, sourceID, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect source after rejected configuration: %v", err)
	}

	if !reflect.DeepEqual(current.Container.State, previousState) || !reflect.DeepEqual(current.Container.Config.Labels, labels) {
		t.Fatal("rejected one-off configuration changed the existing source")
	}
}

func sendScheduledPreStartWebhook(t *testing.T, h *Harness, secret string, payload webhook.GithubPushPayload) string {
	t.Helper()

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode scheduled pre_start webhook: %v", err)
	}

	ctx, cancel := context.WithTimeout(h.ctx, 15*time.Second)
	defer cancel()

	endpoint, err := h.daemon.Endpoint(ctx, "http")
	if err != nil {
		t.Fatalf("get daemon webhook endpoint: %v", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/v1/webhook", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create scheduled pre_start webhook request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", "sha256="+webhook.GenerateHMAC(body, secret))
	req.Header.Set("X-GitHub-Event", "push")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("send scheduled pre_start webhook: %v", err)
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read scheduled pre_start webhook response: %v", err)
	}

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("scheduled pre_start webhook status = %d, want 202; body = %s", resp.StatusCode, responseBody)
	}

	var accepted struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(responseBody, &accepted); err != nil {
		t.Fatalf("decode scheduled pre_start webhook response: %v", err)
	}

	if accepted.JobID == "" {
		t.Fatalf("scheduled pre_start webhook response has no job_id: %s", responseBody)
	}

	return accepted.JobID
}
