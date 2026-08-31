package scheduler

import (
	"testing"

	"github.com/kimdre/doco-cd/internal/docker"
)

func TestJobKeyPrefix(t *testing.T) {
	t.Parallel()

	if got := jobKeyPrefix(""); got != "" {
		t.Fatalf("jobKeyPrefix(\"\") = %q, want empty (default context keeps unprefixed keys)", got)
	}

	if got, want := jobKeyPrefix("remote"), "remote::"; got != want {
		t.Fatalf("jobKeyPrefix(%q) = %q, want %q", "remote", got, want)
	}
}
func TestNewSchedulerForMode_NormalizesContextAndCarriesMode(t *testing.T) {
	t.Parallel()

	s := newSchedulerForMode(docker.ContextClient{Name: "Default", SwarmMode: true}, scheduledJobModeSwarm, nil, nil, nil)
	if s.contextName != "" {
		t.Fatalf("newSchedulerForMode() contextName = %q, want empty string for the default context", s.contextName)
	}

	if s.mode != scheduledJobModeSwarm {
		t.Fatalf("newSchedulerForMode() mode = %q, want %q", s.mode, scheduledJobModeSwarm)
	}

	remote := newSchedulerForMode(docker.ContextClient{Name: "remote", SwarmMode: false}, scheduledJobModeContainer, nil, nil, nil)
	if remote.contextName != "remote" {
		t.Fatalf("newSchedulerForMode() contextName = %q, want %q", remote.contextName, "remote")
	}

	if remote.mode != scheduledJobModeContainer {
		t.Fatalf("newSchedulerForMode() mode = %q, want %q", remote.mode, scheduledJobModeContainer)
	}
}
