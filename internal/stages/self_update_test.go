package stages

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/selfupdate"
)

// TestHandoverIsNeitherSuccessNorFailure pins the rule the whole feature rests
// on: the successor reports the deployment once it is healthy, so the instance
// that handed over must not record a failure. A failure record would make the
// next poll force-recreate the stack and fight the handover.
func TestHandoverIsNeitherSuccessNorFailure(t *testing.T) {
	sm := newFailureTestManager(t, "app-handover")

	handoverErr := fmt.Errorf("failed to deploy stack: %w", selfupdate.ErrHandover)

	if !errors.Is(handoverErr, selfupdate.ErrHandover) {
		t.Fatal("a wrapped handover must stay recognisable")
	}

	err := sm.handleStageFailure(context.Background(), StageDeploy, slog.Default(), handoverErr)
	if err == nil {
		t.Fatal("handleStageFailure must return the error it was given")
	}

	if notification.WasNotified(err) {
		t.Error("a handover must not send a failure notification")
	}

	if failure, ok := sm.lastDeploymentFailure(); ok {
		t.Errorf("a handover was recorded as a deployment failure: %+v", failure)
	}
}

func TestSelfUpdatePoisonedSkipsOnlyTheFailedCommit(t *testing.T) {
	store := selfupdate.NewStore(t.TempDir())

	previous := docker.SelfUpdateConfig()

	t.Cleanup(func() { docker.ConfigureSelfUpdate(previous) })

	docker.ConfigureSelfUpdate(docker.SelfUpdateOptions{
		Enabled: true,
		Store:   store,
		Identity: selfupdate.Identity{
			OK:      true,
			Project: "doco-cd",
			Service: "app",
		},
	})

	sm := newTestStageManager(t)
	sm.DeployConfig.Name = "doco-cd"

	err := store.AddPoison(selfupdate.Poison{
		Context:     "",
		Stack:       "doco-cd",
		CommitSHA:   "bad",
		ProjectHash: "hash",
		Reason:      "the successor never became healthy",
	})
	if err != nil {
		t.Fatalf("add poison: %v", err)
	}

	tests := []struct {
		name     string
		commit   string
		hash     string
		wantSkip bool
	}{
		{name: "the failed commit is skipped", commit: "bad", hash: "hash", wantSkip: true},
		{name: "a new commit is not skipped", commit: "good", hash: "hash"},
		{name: "a changed project is not skipped", commit: "bad", hash: "other"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			skip, err := selfUpdatePoisoned(sm.DeployConfig, tt.commit, tt.hash, slog.Default())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if skip != tt.wantSkip {
				t.Errorf("skip = %v, want %v", skip, tt.wantSkip)
			}
		})
	}
}

func TestSelfUpdatePoisonedIgnoresOtherStacks(t *testing.T) {
	store := selfupdate.NewStore(t.TempDir())

	previous := docker.SelfUpdateConfig()

	t.Cleanup(func() { docker.ConfigureSelfUpdate(previous) })

	docker.ConfigureSelfUpdate(docker.SelfUpdateOptions{
		Enabled:  true,
		Store:    store,
		Identity: selfupdate.Identity{OK: true, Project: "doco-cd", Service: "app"},
	})

	sm := newTestStageManager(t)
	sm.DeployConfig.Name = "my-app"

	skip, err := selfUpdatePoisoned(sm.DeployConfig, "bad", "hash", slog.Default())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if skip {
		t.Error("a poisoned self-update must not block another stack")
	}
}
