package scheduler

import (
	"slices"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/notification"
)

type recordingSender struct {
	levels   []notification.Level
	metadata []notification.Metadata
}

func (s *recordingSender) Send(level notification.Level, _, _ string, metadata notification.Metadata, _ ...notification.SendOption) error {
	s.levels = append(s.levels, level)
	s.metadata = append(s.metadata, metadata)

	return nil
}

func TestSendRunNotificationUsesInjectedSender(t *testing.T) {
	t.Parallel()

	sender := &recordingSender{}
	s := &scheduler{notifier: sender}
	job := scheduledJob{
		name:    "backup",
		mode:    scheduledJobModeSwarm,
		context: "remote",
		labels: map[string]string{
			docker.DocoCDLabels.Source.Name:             "acme/repo",
			docker.DocoCDLabels.Deployment.Name:         "app",
			docker.DocoCDLabels.Deployment.ConfigTarget: "prod",
			docker.DocoCDLabels.Deployment.CommitSHA:    "abc123",
		},
	}

	s.sendRunNotification(job, docker.JobScheduleConfig{NotifyOn: docker.JobNotifyAll}, "run-1", true, "completed", "done")

	if len(sender.levels) != 1 || sender.levels[0] != notification.Success {
		t.Fatalf("notification levels = %v, want [success]", sender.levels)
	}

	if len(sender.metadata) != 1 {
		t.Fatalf("notification count = %d, want 1", len(sender.metadata))
	}

	got := sender.metadata[0]
	if got.Repository != "acme/repo" || got.Stack != "app" || got.Context != "remote" || got.JobID != "run-1" {
		t.Fatalf("notification metadata = %#v", got)
	}
}

func TestResolveStopServiceStacks(t *testing.T) {
	t.Parallel()

	refs := []docker.StopServiceRef{
		{Service: "db"},
		{Project: "other-project", Service: "cache"},
		{Project: "other-project", Service: "search"},
	}

	got := resolveStopServiceStacks(refs, "own-project")

	want := []string{"own-project", "other-project", "other-project"}
	if len(got) != len(want) {
		t.Fatalf("resolveStopServiceStacks() = %v, want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("resolveStopServiceStacks()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLockStacks_DeduplicatesAndLocksSortedOrder(t *testing.T) {
	t.Parallel()

	// Repeated/empty entries must not cause a self-deadlock (locking the same
	// stack twice) and must not be double-unlocked.
	unlock := lockStacks("", "zeta", "alpha", "alpha", "", "zeta")
	unlock()

	// If dedup/unlock bookkeeping were broken, acquiring the same stacks again
	// would deadlock (this call would hang forever), so a normal test timeout
	// failure would catch it.
	unlock2 := lockStacks("", "alpha", "zeta")
	unlock2()
}

func TestLockStacks_SameStackDifferentContextsDoNotBlock(t *testing.T) {
	t.Parallel()

	// Two different Docker contexts using the same stack name must not share
	// a lock (see lock.StackKey): locking "prod" on "context-a" must not block
	// locking "prod" on "context-b" concurrently.
	unlockA := lockStacks("context-a", "prod")

	done := make(chan struct{})

	go func() {
		defer close(done)

		unlockB := lockStacks("context-b", "prod")
		unlockB()
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("lockStacks() for the same stack name on a different context blocked; expected independent locks")
	}

	unlockA()
}

func TestStopHold_RefCounting(t *testing.T) {
	t.Parallel()

	s := &scheduler{stopHolds: map[stopHoldKey]*stopHoldState{}}

	// First holder must actually perform the stop.
	if isFirst := s.acquireStopHold(scheduledJobModeContainer, "proj", "db"); !isFirst {
		t.Fatal("expected first acquireStopHold() to report isFirst=true")
	}

	// A second concurrent holder of the same target must not stop it again.
	if isFirst := s.acquireStopHold(scheduledJobModeContainer, "proj", "db"); isFirst {
		t.Fatal("expected second acquireStopHold() to report isFirst=false")
	}

	// Releasing while another holder remains must not trigger a restart.
	if isLast, _ := s.releaseStopHold(scheduledJobModeContainer, "proj", "db"); isLast {
		t.Fatal("expected first releaseStopHold() to report isLast=false while a holder remains")
	}

	// The last holder releasing must trigger the actual restart.
	if isLast, _ := s.releaseStopHold(scheduledJobModeContainer, "proj", "db"); !isLast {
		t.Fatal("expected final releaseStopHold() to report isLast=true")
	}

	// The hold must be fully removed once released by the last holder.
	if _, ok := s.stopHolds[stopHoldKey{mode: scheduledJobModeContainer, project: "proj", service: "db"}]; ok {
		t.Fatal("expected stop hold to be removed after last release")
	}
}

func TestStopHold_SwarmReplicasSurviveUntilLastRelease(t *testing.T) {
	t.Parallel()

	s := &scheduler{stopHolds: map[stopHoldKey]*stopHoldState{}}

	// Two concurrent jobs both stop the same shared swarm service.
	if isFirst := s.acquireStopHold(scheduledJobModeSwarm, "stack", "shared"); !isFirst {
		t.Fatal("expected first acquireStopHold() to report isFirst=true")
	}

	// Only the first holder actually observes/records the original replica count.
	s.setStopHoldReplicas(scheduledJobModeSwarm, "stack", "shared", 3)

	if isFirst := s.acquireStopHold(scheduledJobModeSwarm, "stack", "shared"); isFirst {
		t.Fatal("expected second acquireStopHold() to report isFirst=false")
	}

	// The second job finishes first and releases its hold: since the first
	// job is still holding it, this must NOT restore the service yet.
	if isLast, _ := s.releaseStopHold(scheduledJobModeSwarm, "stack", "shared"); isLast {
		t.Fatal("expected release while a holder remains to report isLast=false")
	}

	// The first job finishes and releases its hold: now it must restore, and
	// the originally recorded replica count must still be intact.
	isLast, replicas := s.releaseStopHold(scheduledJobModeSwarm, "stack", "shared")
	if !isLast {
		t.Fatal("expected final release to report isLast=true")
	}

	if replicas != 3 {
		t.Fatalf("replicas = %d, want 3", replicas)
	}
}

func TestStopHold_IsolatedAcrossContexts(t *testing.T) {
	t.Parallel()

	// Two scheduler workers on different Docker contexts must not share a
	// stop hold for the same project/service name: a hold acquired on one
	// context must not be visible to (or block) the other context's worker.
	sDefault := &scheduler{contextName: "", stopHolds: map[stopHoldKey]*stopHoldState{}}
	sRemote := &scheduler{contextName: "remote", stopHolds: map[stopHoldKey]*stopHoldState{}}

	if isFirst := sDefault.acquireStopHold(scheduledJobModeContainer, "proj", "db"); !isFirst {
		t.Fatal("expected first acquireStopHold() on default context to report isFirst=true")
	}

	// The same project/service on a different context must be an independent
	// hold, so this must also report isFirst=true.
	if isFirst := sRemote.acquireStopHold(scheduledJobModeContainer, "proj", "db"); !isFirst {
		t.Fatal("expected first acquireStopHold() on remote context to report isFirst=true, holds leaked across contexts")
	}

	if isLast, _ := sRemote.releaseStopHold(scheduledJobModeContainer, "proj", "db"); !isLast {
		t.Fatal("expected releaseStopHold() on remote context to report isLast=true")
	}

	// Releasing the remote context's hold must not have touched the default
	// context's hold.
	if isLast, _ := sDefault.releaseStopHold(scheduledJobModeContainer, "proj", "db"); !isLast {
		t.Fatal("expected releaseStopHold() on default context to report isLast=true")
	}
}

func TestGetScheduledRunMetricLabels_IncludesContext(t *testing.T) {
	t.Parallel()

	cfg := docker.JobScheduleConfig{ExecutionMode: docker.JobExecutionModeOneOff}

	defaultJob := scheduledJob{name: "backup", mode: scheduledJobModeContainer, context: ""}
	if got, want := getScheduledRunMetricLabels(defaultJob, cfg, "stack"), []string{"default", "stack", "backup", "container", "one_off"}; !slices.Equal(got, want) {
		t.Fatalf("getScheduledRunMetricLabels() = %v, want %v", got, want)
	}

	remoteJob := scheduledJob{name: "backup", mode: scheduledJobModeContainer, context: "remote"}
	if got, want := getScheduledRunMetricLabels(remoteJob, cfg, "stack"), []string{"remote", "stack", "backup", "container", "one_off"}; !slices.Equal(got, want) {
		t.Fatalf("getScheduledRunMetricLabels() = %v, want %v", got, want)
	}
}
