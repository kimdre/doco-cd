package reconciliation

import (
	"reflect"
	"slices"
	"testing"

	"github.com/kimdre/doco-cd/internal/config"
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
		" DIE ":                        "die",
		"health_status:unhealthy":      "unhealthy",
		" health_status:   unhealthy ": "unhealthy",
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

	got := dockerEventFiltersForActions([]string{" die ", "destroy", "unhealthy", "health_status: unhealthy", "die"})
	slices.Sort(got)

	want := []string{"destroy", "die", "health_status"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected docker event filters %v, got %v", want, got)
	}
}
