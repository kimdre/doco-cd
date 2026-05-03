package reconciliation

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/moby/moby/api/types/events"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/notification"
)

func TestGetDeployConfigGroupByEvent(t *testing.T) {
	t.Parallel()

	dc1 := config.DefaultDeployConfig("stack-1", "main")
	dc1.Reconciliation.Enabled = true
	dc1.Reconciliation.Events = []string{"die", "destroy"}

	dc2 := config.DefaultDeployConfig("stack-2", "main")
	dc2.Reconciliation.Enabled = true
	dc2.Reconciliation.Events = []string{"unhealthy"}

	dc3 := config.DefaultDeployConfig("stack-3", "main")
	dc3.Reconciliation.Enabled = false
	dc3.Reconciliation.Events = []string{"die"}

	grouped := getDeployConfigGroupByEvent([]*config.DeployConfig{dc1, dc2, dc3})

	if len(grouped["die"]) != 1 || grouped["die"][0].Name != "stack-1" {
		t.Fatalf("expected die event to include only stack-1, got %#v", grouped["die"])
	}

	if len(grouped["destroy"]) != 1 || grouped["destroy"][0].Name != "stack-1" {
		t.Fatalf("expected destroy event to include only stack-1, got %#v", grouped["destroy"])
	}

	if len(grouped["unhealthy"]) != 1 || grouped["unhealthy"][0].Name != "stack-2" {
		t.Fatalf("expected unhealthy event to include only stack-2, got %#v", grouped["unhealthy"])
	}

	if _, ok := grouped["stop"]; ok {
		t.Fatalf("did not expect stop event group, got %#v", grouped["stop"])
	}
}

func TestNormalizeReconciliationEventAction(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		" DIE ":                    "die",
		"remove":                   "destroy",
		" delete ":                 "destroy",
		" UPDATE ":                 "update",
		"health_status: unhealthy": "unhealthy",
		"health_status: healthy":   "health_status: healthy",
	}

	for input, want := range tests {
		got := normalizeReconciliationEventAction(input)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("expected normalized action %q for input %q, got %q", want, input, got)
		}
	}
}

func TestDockerEventFiltersForActions(t *testing.T) {
	t.Parallel()

	got := dockerEventFiltersForActions([]string{" die ", "destroy", "unhealthy", "health_status: unhealthy", "die", "delete"}, false)
	slices.Sort(got)

	want := []string{"destroy", "die", "health_status: unhealthy"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected docker event filters %v, got %v", want, got)
	}
}

func TestDockerEventFiltersForActions_Swarm(t *testing.T) {
	t.Parallel()

	got := dockerEventFiltersForActions([]string{"destroy", "delete"}, true)
	slices.Sort(got)

	want := []string{"remove"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected docker event filters %v, got %v", want, got)
	}
}

func TestDockerEventFiltersForActions_SwarmUpdate(t *testing.T) {
	t.Parallel()

	got := dockerEventFiltersForActions([]string{"update"}, true)
	slices.Sort(got)

	want := []string{"update"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected docker event filters %v, got %v", want, got)
	}
}

func TestDockerEventsSinceValue(t *testing.T) {
	t.Parallel()

	if got := dockerEventsSinceValue(time.Time{}); got != "" {
		t.Fatalf("expected empty since value for zero cursor, got %q", got)
	}

	cursor := time.Unix(1735689600, 500).UTC()
	if got := dockerEventsSinceValue(cursor); got != "1735689600" {
		t.Fatalf("expected unix-seconds since value, got %q", got)
	}
}

func TestDockerEventTime(t *testing.T) {
	t.Parallel()

	t.Run("prefers time nano", func(t *testing.T) {
		t.Parallel()

		event := events.Message{Time: 100, TimeNano: 200_000_000_300}
		want := time.Unix(0, 200_000_000_300).UTC()

		if got := dockerEventTime(event); !got.Equal(want) {
			t.Fatalf("expected %s, got %s", want, got)
		}
	})

	t.Run("falls back to seconds", func(t *testing.T) {
		t.Parallel()

		event := events.Message{Time: 1710000000}
		want := time.Unix(1710000000, 0).UTC()

		if got := dockerEventTime(event); !got.Equal(want) {
			t.Fatalf("expected %s, got %s", want, got)
		}
	})

	t.Run("zero when event has no time", func(t *testing.T) {
		t.Parallel()

		if got := dockerEventTime(events.Message{}); !got.IsZero() {
			t.Fatalf("expected zero time, got %s", got)
		}
	})
}

func TestIsRestartReconciliationAction(t *testing.T) {
	t.Parallel()

	tests := map[string]bool{
		"unhealthy": true,
		"oom":       true,
		"kill":      true,
		"stop":      true,
		"die":       false,
		"destroy":   false,
		"update":    false,
	}

	for action, want := range tests {
		t.Run(action, func(t *testing.T) {
			t.Parallel()

			got := isRestartReconciliationAction(action)
			if got != want {
				t.Fatalf("expected restart routing %t for action %q, got %t", want, action, got)
			}
		})
	}
}

func TestIsRestartFollowupAction(t *testing.T) {
	t.Parallel()

	tests := map[string]bool{
		"die":       true,
		"stop":      true,
		"kill":      true,
		"unhealthy": false,
		"oom":       false,
		"destroy":   false,
	}

	for action, want := range tests {
		t.Run(action, func(t *testing.T) {
			t.Parallel()

			got := isRestartFollowupAction(action)
			if got != want {
				t.Fatalf("expected follow-up suppression %t for action %q, got %t", want, action, got)
			}
		})
	}
}

func TestRestartFollowupSuppressionWindow(t *testing.T) {
	t.Parallel()

	if got := restartFollowupSuppressionWindow(30); got != 40*time.Second {
		t.Fatalf("expected suppression window 40s, got %s", got)
	}

	if got := restartFollowupSuppressionWindow(0); got != 10*time.Second {
		t.Fatalf("expected suppression window 10s, got %s", got)
	}
}

func TestEvaluateUnhealthyRestartLimit(t *testing.T) {
	t.Parallel()

	now := time.Now()
	window := 10 * time.Second

	t.Run("allow when below limit", func(t *testing.T) {
		t.Parallel()

		history := []time.Time{now.Add(-9 * time.Second), now.Add(-3 * time.Second)}

		suppressed, updated := evaluateUnhealthyRestartLimit(history, now, 3, window)
		if suppressed {
			t.Fatal("expected restart to be allowed")
		}

		if len(updated) != 3 {
			t.Fatalf("expected history size 3, got %d", len(updated))
		}
	})

	t.Run("suppress when limit reached in window", func(t *testing.T) {
		t.Parallel()

		history := []time.Time{now.Add(-9 * time.Second), now.Add(-3 * time.Second), now.Add(-1 * time.Second)}

		suppressed, updated := evaluateUnhealthyRestartLimit(history, now, 3, window)
		if !suppressed {
			t.Fatal("expected restart to be suppressed")
		}

		if len(updated) != 3 {
			t.Fatalf("expected history size 3, got %d", len(updated))
		}
	})

	t.Run("prunes entries outside window", func(t *testing.T) {
		t.Parallel()

		history := []time.Time{now.Add(-25 * time.Second), now.Add(-15 * time.Second), now.Add(-1 * time.Second)}

		suppressed, updated := evaluateUnhealthyRestartLimit(history, now, 3, window)
		if suppressed {
			t.Fatal("expected restart to be allowed after old entries are pruned")
		}

		if len(updated) != 2 {
			t.Fatalf("expected history size 2 after prune+append, got %d", len(updated))
		}
	})
}

func TestCloneDeployConfigsWithForcedRecreate(t *testing.T) {
	t.Parallel()

	dc1 := config.DefaultDeployConfig("stack-a", "main")
	dc1.ForceRecreate = false
	dc2 := config.DefaultDeployConfig("stack-b", "main")
	dc2.ForceRecreate = false

	cloned := cloneDeployConfigsWithForcedRecreate([]*config.DeployConfig{dc1, dc2})

	if len(cloned) != 2 {
		t.Fatalf("expected 2 cloned deploy configs, got %d", len(cloned))
	}

	for i, dc := range cloned {
		if dc == nil {
			t.Fatalf("expected cloned deploy config at index %d to be non-nil", i)
		}

		if !dc.ForceRecreate {
			t.Fatalf("expected ForceRecreate to be true for cloned config at index %d", i)
		}
	}

	if dc1.ForceRecreate || dc2.ForceRecreate {
		t.Fatal("expected source deploy configs to remain unmodified")
	}
}

func TestStackDeploymentInProgressTracking(t *testing.T) {
	t.Parallel()

	r := newReconciliation()
	repo := "github.com/example/repo"
	stack := "stack-a"

	if r.isStackDeploymentInProgress(repo, stack) {
		t.Fatal("expected stack deployment to be initially not in progress")
	}

	r.startStackDeployment(repo, stack)

	if !r.isStackDeploymentInProgress(repo, stack) {
		t.Fatal("expected stack deployment to be marked in progress")
	}

	// Reference counting should keep stack marked as in-progress until all marks are cleared.
	r.startStackDeployment(repo, stack)
	r.finishStackDeployment(repo, stack)

	if !r.isStackDeploymentInProgress(repo, stack) {
		t.Fatal("expected stack deployment to remain in progress after one of two marks is cleared")
	}

	r.finishStackDeployment(repo, stack)

	if r.isStackDeploymentInProgress(repo, stack) {
		t.Fatal("expected stack deployment to be cleared after all marks are removed")
	}
}

func TestRestartOptionsFromDeployConfig(t *testing.T) {
	t.Parallel()

	t.Run("defaults", func(t *testing.T) {
		t.Parallel()

		opts := restartOptionsFromDeployConfig(nil)
		if opts.Timeout == nil || *opts.Timeout != 10 {
			t.Fatalf("expected default restart timeout 10, got %+v", opts.Timeout)
		}

		if opts.Signal != "" {
			t.Fatalf("expected empty restart signal, got %q", opts.Signal)
		}
	})

	t.Run("from deploy config", func(t *testing.T) {
		t.Parallel()

		dc := config.DefaultDeployConfig("stack-a", "main")
		dc.Reconciliation.RestartTimeout = 30
		dc.Reconciliation.RestartSignal = "SIGQUIT"

		opts := restartOptionsFromDeployConfig(dc)
		if opts.Timeout == nil || *opts.Timeout != 30 {
			t.Fatalf("expected restart timeout 30, got %+v", opts.Timeout)
		}

		if opts.Signal != "SIGQUIT" {
			t.Fatalf("expected restart signal SIGQUIT, got %q", opts.Signal)
		}
	})
}

func TestRestartNotificationMetadata(t *testing.T) {
	t.Parallel()

	metadata := restartNotificationMetadata(notification.Metadata{
		Repository: "owner/repo",
		Stack:      "stack-a",
		JobID:      "job-1",
	}, "unhealthy", "container", "1234567890abcdef", "stack-a-web-1", "trace-1")

	if metadata.Repository != "owner/repo" || metadata.Stack != "stack-a" || metadata.JobID != "job-1" {
		t.Fatalf("expected base metadata to be preserved, got %#v", metadata)
	}

	if metadata.ReconciliationEvent != "unhealthy" {
		t.Fatalf("expected reconciliation event unhealthy, got %q", metadata.ReconciliationEvent)
	}

	if metadata.TraceID != "trace-1" {
		t.Fatalf("expected trace id trace-1, got %q", metadata.TraceID)
	}

	if metadata.AffectedActorKind != "container" {
		t.Fatalf("expected actor kind container, got %q", metadata.AffectedActorKind)
	}

	if metadata.AffectedActorID != "1234567890ab" {
		t.Fatalf("expected short actor id 1234567890ab, got %q", metadata.AffectedActorID)
	}

	if metadata.AffectedActorName != "stack-a-web-1" {
		t.Fatalf("expected actor name stack-a-web-1, got %q", metadata.AffectedActorName)
	}
}

func TestRestartNotificationActorKindTitle(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"container": "Container",
		"service":   "Service",
		"":          "",
	}

	for input, want := range tests {
		if got := restartNotificationActorKindTitle(input); got != want {
			t.Fatalf("expected title %q for %q, got %q", want, input, got)
		}
	}
}

func TestSelectRestartDeployConfig(t *testing.T) {
	t.Parallel()

	if got := selectRestartDeployConfig(nil, nil); got != nil {
		t.Fatalf("expected nil for empty deploy config list, got %#v", got)
	}

	dc1 := config.DefaultDeployConfig("stack-a", "main")
	dc1.Internal.Hash = "hash-a"
	dc2 := config.DefaultDeployConfig("stack-b", "main")
	dc2.Internal.Hash = "hash-b"

	got := selectRestartDeployConfig([]*config.DeployConfig{nil, dc1, dc2}, nil)
	if got != dc1 {
		t.Fatalf("expected first non-nil deploy config to be selected")
	}

	got = selectRestartDeployConfig([]*config.DeployConfig{dc1, dc2}, map[string]string{
		docker.DocoCDLabels.Deployment.ConfigHash: "hash-b",
	})
	if got != dc2 {
		t.Fatalf("expected config hash match to be selected")
	}
}

func TestShouldSuppressRestartFollowupEvent(t *testing.T) {
	t.Parallel()

	containerID := "container-1"
	j := &job{
		restartSuppressUntil: map[string]time.Time{
			containerID: time.Now().Add(5 * time.Second),
		},
	}

	event := events.Message{Actor: events.Actor{ID: containerID}}

	if !j.shouldSuppressRestartFollowupEvent("die", event) {
		t.Fatal("expected die follow-up event to be suppressed")
	}

	if _, ok := j.restartSuppressUntil[containerID]; !ok {
		t.Fatal("expected suppression marker to remain active during suppression window")
	}
}

func TestShouldSuppressRestartFollowupEvent_MultipleFollowupEvents(t *testing.T) {
	t.Parallel()

	containerID := "container-1"
	j := &job{
		restartSuppressUntil: map[string]time.Time{
			containerID: time.Now().Add(5 * time.Second),
		},
	}

	event := events.Message{Actor: events.Actor{ID: containerID}}

	for _, action := range []string{"stop", "die", "kill"} {
		if !j.shouldSuppressRestartFollowupEvent(action, event) {
			t.Fatalf("expected %q follow-up event to be suppressed", action)
		}
	}

	if _, ok := j.restartSuppressUntil[containerID]; !ok {
		t.Fatal("expected suppression marker to remain until the suppression window expires")
	}
}

func TestShouldSuppressRestartFollowupEvent_Expired(t *testing.T) {
	t.Parallel()

	containerID := "container-1"
	j := &job{
		restartSuppressUntil: map[string]time.Time{
			containerID: time.Now().Add(-1 * time.Second),
		},
	}

	event := events.Message{Actor: events.Actor{ID: containerID}}

	if j.shouldSuppressRestartFollowupEvent("die", event) {
		t.Fatal("expected expired suppression marker to be ignored")
	}

	if _, ok := j.restartSuppressUntil[containerID]; ok {
		t.Fatal("expected expired suppression marker to be cleaned up")
	}
}

func TestShouldSuppressRestartFollowupEvent_NonFollowupAction(t *testing.T) {
	t.Parallel()

	containerID := "container-1"
	j := &job{
		restartSuppressUntil: map[string]time.Time{
			containerID: time.Now().Add(5 * time.Second),
		},
	}

	event := events.Message{Actor: events.Actor{ID: containerID}}

	if j.shouldSuppressRestartFollowupEvent("destroy", event) {
		t.Fatal("expected non-follow-up action not to be suppressed")
	}

	if _, ok := j.restartSuppressUntil[containerID]; !ok {
		t.Fatal("expected suppression marker to remain for unrelated actions")
	}
}

func TestStackNameFromEvent(t *testing.T) {
	t.Parallel()

	candidates := []*config.DeployConfig{
		config.DefaultDeployConfig("stack-a", "main"),
		config.DefaultDeployConfig("stack-b", "main"),
	}

	tests := []struct {
		name  string
		event events.Message
		want  string
	}{
		{
			name: "deployment label",
			event: events.Message{Actor: events.Actor{
				Attributes: map[string]string{docker.DocoCDLabels.Deployment.Name: "stack-a"},
			}},
			want: "stack-a",
		},
		{
			name: "stack namespace label",
			event: events.Message{Actor: events.Actor{
				Attributes: map[string]string{swarm.StackNamespaceLabel: "stack-b"},
			}},
			want: "stack-b",
		},
		{
			name: "stack namespace label not in candidates",
			event: events.Message{Actor: events.Actor{
				Attributes: map[string]string{swarm.StackNamespaceLabel: "other-stack"},
			}},
			want: "",
		},
		{
			name: "service name fallback",
			event: events.Message{Actor: events.Actor{
				Attributes: map[string]string{"name": "stack-a_web"},
			}},
			want: "stack-a",
		},
		{
			name: "swarm service name attribute",
			event: events.Message{Actor: events.Actor{
				Attributes: map[string]string{"com.docker.swarm.service.name": "stack-b_api"},
			}},
			want: "stack-b",
		},
		{
			name: "swarm task name attribute",
			event: events.Message{Actor: events.Actor{
				Attributes: map[string]string{"com.docker.swarm.task.name": "stack-a_web.1.abc123"},
			}},
			want: "stack-a",
		},
		{
			name: "service attr fallback",
			event: events.Message{Actor: events.Actor{
				Attributes: map[string]string{"service": "stack-b_api"},
			}},
			want: "stack-b",
		},
		{
			name: "unknown service",
			event: events.Message{Actor: events.Actor{
				Attributes: map[string]string{"name": "other_web"},
			}},
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := stackNameFromEvent(tc.event, candidates)
			if got != tc.want {
				t.Fatalf("expected stack name %q, got %q", tc.want, got)
			}
		})
	}
}

func TestDeployConfigsByName(t *testing.T) {
	t.Parallel()

	dc1 := config.DefaultDeployConfig("stack-a", "main")
	dc2 := config.DefaultDeployConfig("stack-b", "main")
	dc3 := config.DefaultDeployConfig("stack-a", "main")

	got := deployConfigsByName([]*config.DeployConfig{dc1, dc2, dc3}, "stack-a")

	if len(got) != 2 {
		t.Fatalf("expected 2 deploy configs, got %d", len(got))
	}

	if got[0].Name != "stack-a" || got[1].Name != "stack-a" {
		t.Fatalf("expected only stack-a deploy configs, got %#v", got)
	}
}

func TestUniqueRedeployDCsFromGroupByEvent(t *testing.T) {
	t.Parallel()

	dcDie := config.DefaultDeployConfig("stack-die", "main")
	dcDestroy := config.DefaultDeployConfig("stack-destroy", "main")
	dcBoth := config.DefaultDeployConfig("stack-both", "main")       // registered under two redeploy events
	dcRestart := config.DefaultDeployConfig("stack-restart", "main") // only restart events → must be excluded

	grouped := map[string][]*config.DeployConfig{
		"die":       {dcDie, dcBoth},
		"destroy":   {dcDestroy, dcBoth}, // dcBoth appears again — must be deduplicated
		"unhealthy": {dcRestart},         // restart-oriented — must be excluded
		"stop":      {dcRestart},         // restart-oriented — must be excluded
	}

	got := uniqueRedeployDCsFromGroupByEvent(grouped)

	// Build a name set for order-independent assertion.
	names := make(map[string]struct{}, len(got))
	for _, dc := range got {
		names[dc.Name] = struct{}{}
	}

	if len(got) != 3 {
		t.Fatalf("expected 3 unique redeploy configs, got %d: %v", len(got), names)
	}

	for _, wantName := range []string{"stack-die", "stack-destroy", "stack-both"} {
		if _, ok := names[wantName]; !ok {
			t.Errorf("expected %q to be included, got %v", wantName, names)
		}
	}

	if _, ok := names["stack-restart"]; ok {
		t.Error("expected stack-restart to be excluded (only restart-oriented events)")
	}
}

func TestUniqueRedeployDCsFromGroupByEvent_EmptyWhenOnlyRestartEvents(t *testing.T) {
	t.Parallel()

	dc := config.DefaultDeployConfig("stack-a", "main")

	grouped := map[string][]*config.DeployConfig{
		"unhealthy": {dc},
		"oom":       {dc},
		"kill":      {dc},
		"stop":      {dc},
	}

	got := uniqueRedeployDCsFromGroupByEvent(grouped)
	if len(got) != 0 {
		t.Fatalf("expected empty result for all-restart events, got %d configs", len(got))
	}
}

func TestUniqueRedeployDCsFromGroupByEvent_EmptyInput(t *testing.T) {
	t.Parallel()

	got := uniqueRedeployDCsFromGroupByEvent(map[string][]*config.DeployConfig{})
	if len(got) != 0 {
		t.Fatalf("expected empty result for empty input, got %d", len(got))
	}
}
