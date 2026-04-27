package reconciliation

import (
	"reflect"
	"slices"
	"testing"

	"github.com/moby/moby/api/types/events"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
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
			name: "service name fallback",
			event: events.Message{Actor: events.Actor{
				Attributes: map[string]string{"name": "stack-a_web"},
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
