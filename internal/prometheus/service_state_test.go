package prometheus

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestServiceStateCollector(t *testing.T) {
	t.Cleanup(func() { SetServiceStateSource(nil) })

	states := []ServiceState{
		{Context: "default", Stack: "web", Service: "app", DesiredTasks: 3, RunningTasks: 2, UpdateState: "paused"},
		{Context: "default", Stack: "web", Service: "db", DesiredTasks: 1, RunningTasks: 1},
	}

	SetServiceStateSource(func(context.Context) []ServiceState { return states })

	expected := `
# HELP doco_cd_service_desired_tasks Number of tasks Docker Swarm wants to run for a deployed service
# TYPE doco_cd_service_desired_tasks gauge
doco_cd_service_desired_tasks{context="default",service="app",stack="web"} 3
doco_cd_service_desired_tasks{context="default",service="db",stack="web"} 1
# HELP doco_cd_service_running_tasks Number of tasks of a deployed Docker Swarm service that are running
# TYPE doco_cd_service_running_tasks gauge
doco_cd_service_running_tasks{context="default",service="app",stack="web"} 2
doco_cd_service_running_tasks{context="default",service="db",stack="web"} 1
`

	if err := testutil.CollectAndCompare(serviceStates, strings.NewReader(expected),
		"doco_cd_service_desired_tasks", "doco_cd_service_running_tasks"); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		service, state string
		want           float64
	}{
		{"app", "paused", 1},
		{"app", "updating", 0},
		{"app", "rollback_completed", 0},
		{"db", "completed", 0},
	} {
		if got := testutil.ToFloat64(ServiceUpdateState.WithLabelValues("default", "web", tc.service, tc.state)); got != tc.want {
			t.Errorf("update_state{service=%q,state=%q} = %v, want %v", tc.service, tc.state, got, tc.want)
		}
	}
}

func TestServiceStateCollectorUnknownStateAndRemoval(t *testing.T) {
	t.Cleanup(func() { SetServiceStateSource(nil) })

	SetServiceStateSource(func(context.Context) []ServiceState {
		return []ServiceState{{Context: "default", Stack: "s", Service: "x", UpdateState: "future_state"}}
	})

	if n := testutil.CollectAndCount(serviceStates, "doco_cd_service_update_state"); n != len(knownServiceUpdateStates)+1 {
		t.Fatalf("got %d update_state series, want %d", n, len(knownServiceUpdateStates)+1)
	}

	SetServiceStateSource(func(context.Context) []ServiceState { return nil })

	if n := testutil.CollectAndCount(serviceStates); n != 0 {
		t.Fatalf("got %d series after the services were removed, want 0", n)
	}
}

func TestServiceStateCollectorWithoutSource(t *testing.T) {
	SetServiceStateSource(nil)

	if n := testutil.CollectAndCount(serviceStates); n != 0 {
		t.Fatalf("got %d series without a source, want 0", n)
	}
}
