package reconciliation

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/events"

	"github.com/kimdre/doco-cd/internal/lock"
	"github.com/kimdre/doco-cd/internal/notification"
)

// TestDeployDoesNotContendRepositoryLock verifies that job.deploy ignores the repository-wide lock.
// The only remaining conflict job.deploy relies on is the per-stack TryLock in
// reconcile plus docker.DeployStack's own per-stack lock.
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
