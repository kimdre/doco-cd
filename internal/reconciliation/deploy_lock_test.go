package reconciliation

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/events"

	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/lock"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/stages"
)

func TestWaitingWebhookDoesNotHoldDeploymentSlots(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		cli := cleanupTestCLI{apiClient: &cleanupTestClient{}}
		manager := newTestManagerWithDependencies(t, Dependencies{DockerCLI: cli})
		dc := deployConfig.New("wait-webhook-"+uuid.NewString(), "main")
		req := pauseTestRequest(dc)
		req.JobTrigger = stages.JobTriggerWebhook

		key := lock.StackKey(dc.Context, dc.Name)
		if err := lock.LockStackContext(t.Context(), key, "scheduled job backup"); err != nil {
			t.Fatal(err)
		}

		defer lock.UnlockStack(key)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		done := make(chan error, 1)

		go func() {
			done <- manager.handleOneDeploy(ctx, req, req.Logger, cli, false, dc, nil, nil, nil)
		}()

		synctest.Wait()

		select {
		case err := <-done:
			t.Fatalf("webhook did not wait for the locked stack: %v", err)
		default:
		}

		if preDeploy, deploy := len(manager.preDeployLimiter.sem), len(manager.limiter.sem); preDeploy != 0 || deploy != 0 {
			t.Fatalf("waiting webhook used concurrency slots: pre-deploy=%d deploy=%d", preDeploy, deploy)
		}

		poll := req
		poll.JobTrigger = stages.JobTriggerPoll

		pollCtx, cancelPoll := context.WithTimeout(t.Context(), time.Second)
		defer cancelPoll()

		if err := manager.handleOneDeploy(pollCtx, poll, poll.Logger, cli, false, dc, nil, nil, nil); !errors.Is(err, stages.ErrStackLocked) {
			t.Fatalf("poll returned %v, want ErrStackLocked", err)
		}

		cancel()

		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("webhook returned %v, want context.Canceled", err)
		}
	})
}

func TestReconciliationKeepsRevisionOfStackSkippedByPoll(t *testing.T) {
	t.Parallel()

	old := deployConfig.New("busy", "main")
	busy := deployConfig.New("busy", "main")
	free := deployConfig.New("free", "main")
	discovered := deployConfig.New("discovered", "main")

	previous := &job{info: syncWindowTestRequest("deployed", "", old)}
	req := syncWindowTestRequest("polled", "", busy, free, discovered)

	deferred := deferLockedStacks(nil, []stackResult{
		{config: busy, err: stages.StackLocked("scheduled job backup")},
		{config: free},
		{config: discovered, err: stages.StackLocked("project action restart")},
	})
	info, carried, pinned := reconciliationJobInfo(req, deferred, previous)
	next := &job{info: info, carried: carried}

	if got := next.requestFor(old).Repository.Revision; got != "deployed" || len(info.DeployConfigs) != 2 || info.DeployConfigs[0] != old {
		t.Fatalf("skipped busy stack revision = %q, configs = %v; want old config and deployed revision", got, info.DeployConfigs)
	}

	if info.DeployConfigs[1] != free || next.requestFor(free).Repository.Revision != "polled" {
		t.Fatalf("successful free stack did not adopt the polled revision: %v", info.DeployConfigs)
	}

	if len(pinned) != 1 || pinned[0] != discovered {
		t.Fatalf("pinned configs = %v, want only the skipped stack with no prior deployment", pinned)
	}
}

// TestDeployDoesNotContendRepositoryLock verifies that job.deploy ignores the repository-wide lock.
// Redeploys use a per-stack lock instead.
func TestDeployDoesNotContendRepositoryLock(t *testing.T) {
	t.Parallel()

	repoName := t.Name()
	j := newJob(newTestManager(t), DeployRequest{Metadata: notification.Metadata{Repository: repoName}}, nil)

	var output bytes.Buffer

	jobLog := slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Hold the repository-wide lock for the whole call.
	// Before reconciliation-drop-repo-lock this would have made job.deploy skip
	// (context never even gets canceled here); now it must have no effect at all.
	repoLock := lock.GetRepoLock(repoName)
	if !repoLock.TryLock("held-by-test") {
		t.Fatal("failed to acquire repository lock for test setup")
	}

	defer repoLock.Unlock()

	done := make(chan struct{})

	go func() {
		defer close(done)

		j.deploy(t.Context(), jobLog, nil, "die", events.Message{}, "trace-id", "", false)
	}()

	select {
	case <-done:
		// Success: job.deploy completed while the repository lock was held elsewhere.
	case <-time.After(5 * time.Second):
		t.Fatal("job.deploy blocked while the repository lock was held by another owner; " +
			"it must not contend for that lock anymore")
	}

	logged := output.String()
	if !strings.Contains(logged, "reconciliation started") {
		t.Fatalf("expected reconciliation to start despite the repository lock being held, got %q", logged)
	}

	if !strings.Contains(logged, "reconciliation completed") {
		t.Fatalf("expected reconciliation to complete despite the repository lock being held, got %q", logged)
	}

	if strings.Contains(logged, "reconciliation skipped") {
		t.Fatalf("job.deploy must not skip because of the repository lock anymore, got %q", logged)
	}
}
