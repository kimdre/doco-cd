package reconciliation

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/events"

	"github.com/kimdre/doco-cd/internal/lock"
	"github.com/kimdre/doco-cd/internal/notification"
)

// TestDeploySkipsWhenContextCancelledBeforeRepoLockAcquisition verifies that
// a reconciliation deploy with an already-cancelled context returns without
// deploying and without leaking the repository lock. The queued-waiter
// cancellation paths of LockContext are covered in internal/lock.
func TestDeploySkipsWhenContextCancelledBeforeRepoLockAcquisition(t *testing.T) {
	t.Parallel()

	repoName := t.Name()
	j := newJob(jobInfo{metadata: notification.Metadata{Repository: repoName}}, nil)

	var output bytes.Buffer

	jobLog := slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	j.deploy(ctx, jobLog, nil, "die", events.Message{}, "trace-id", "")

	logged := output.String()
	if !strings.Contains(logged, "reconciliation skipped") {
		t.Fatalf("expected skip log line, got %q", logged)
	}

	if strings.Contains(logged, "reconciliation started") {
		t.Fatalf("cancelled deploy must not start reconciliation: %q", logged)
	}

	repoLock := lock.GetRepoLock(repoName)
	if !repoLock.TryLock("verify") {
		t.Fatal("repository lock was leaked by cancelled deploy")
	}

	repoLock.Unlock()
}
