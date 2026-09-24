package scheduler

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/docker"
)

type ownershipTestCli struct {
	command.Cli
	apiClient client.APIClient
}

func (c ownershipTestCli) Client() client.APIClient { return c.apiClient }

type ownershipTestClient struct {
	client.APIClient
	containers []container.Summary
	services   []swarm.Service
}

func (c ownershipTestClient) ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
	return client.ContainerListResult{Items: c.containers}, nil
}

func (c ownershipTestClient) ServiceList(context.Context, client.ServiceListOptions) (client.ServiceListResult, error) {
	return client.ServiceListResult{Items: c.services}, nil
}

func TestRefreshJobs_OwnershipAcrossContextAliases(t *testing.T) {
	t.Parallel()

	labels := func(owner, service string) map[string]string {
		return map[string]string{
			docker.DocoCDJobLabels.JobEnabled:  "true",
			docker.DocoCDJobLabels.JobSchedule: "@every 2h",
			docker.DocoCDJobLabels.JobOwner:    owner,
			api.ProjectLabel:                   "shared",
			api.ServiceLabel:                   service,
		}
	}

	containers := []container.Summary{
		{ID: "container-a", Names: []string{"/backup"}, Labels: labels("host-a", "backup")},
		{ID: "container-b", Names: []string{"/cleanup"}, Labels: labels("host-b", "cleanup")},
		{ID: "container-local", Names: []string{"/local"}, Labels: map[string]string{
			docker.DocoCDJobLabels.JobEnabled: "true", docker.DocoCDJobLabels.JobSchedule: "@hourly",
			api.ProjectLabel: "shared", api.ServiceLabel: "local",
		}},
	}
	services := []swarm.Service{
		{ID: "swarm-a", Spec: swarm.ServiceSpec{Name: "shared_backup", TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Labels: labels("host-a", "backup")}}}},
		{ID: "swarm-b", Spec: swarm.ServiceSpec{Name: "shared_cleanup", TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Labels: labels("host-b", "cleanup")}}}},
		{ID: "swarm-local", Spec: swarm.ServiceSpec{Name: "shared_local", TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Labels: map[string]string{
			docker.DocoCDJobLabels.JobEnabled: "true", docker.DocoCDJobLabels.JobSchedule: "@hourly",
		}}}}},
	}
	fake := ownershipTestCli{apiClient: ownershipTestClient{containers: containers, services: services}}
	now := time.Date(2026, time.September, 24, 6, 0, 0, 0, time.UTC)

	for _, mode := range []scheduledJobMode{scheduledJobModeContainer, scheduledJobModeSwarm} {
		for _, tc := range []struct {
			context string
			id      string
			owned   string
		}{
			{context: "default", id: "host-a", owned: "backup"},
			{context: "docker-host", id: "host-b", owned: "cleanup"},
		} {
			name := string(mode) + "/" + tc.id
			t.Run(name, func(t *testing.T) {
				worker := newSchedulerForMode(
					docker.ContextClient{Name: tc.context, Cli: fake}, mode,
					nil, nil, nil, nil, nil, nil, docker.ScheduledComposeOptions{},
					OwnershipOptions{InstanceID: tc.id, RequireOwnerContexts: []string{tc.context}},
				)

				next, ok := worker.refreshJobs(t.Context(), now)
				if !ok || !next.After(now) || len(worker.states) != 1 {
					t.Fatalf("scheduled states = %v; next=%v, ok=%v", worker.states, next, ok)
				}

				for _, state := range worker.states {
					if state.cfg.Owner != tc.id {
						t.Fatalf("scheduled owner = %q, want %q", state.cfg.Owner, tc.id)
					}
				}

				jobs, err := worker.listJobs(t.Context(), "")
				if err != nil || len(jobs) != 3 {
					t.Fatalf("listJobs = %v, %v", jobs, err)
				}

				for _, job := range jobs {
					wantScheduled := job.Name == tc.owned || job.Name == "shared_"+tc.owned
					if job.Eligible != wantScheduled || (job.NextRunAt != nil) != wantScheduled {
						t.Errorf("job %q: eligible=%v next=%v, want eligible=%v", job.Name, job.Eligible, job.NextRunAt, wantScheduled)
					}
				}
			})
		}
	}

	// B's default context is not strict: it schedules its own job and an unowned local job.
	local := newSchedulerForMode(
		docker.ContextClient{Name: "default", Cli: fake}, scheduledJobModeContainer,
		nil, nil, nil, nil, nil, nil, docker.ScheduledComposeOptions{},
		OwnershipOptions{InstanceID: "host-b", RequireOwnerContexts: []string{"docker-host"}},
	)
	local.refreshJobs(t.Context(), now)

	if len(local.states) != 2 {
		t.Fatalf("B's local context scheduled %d jobs, want 2", len(local.states))
	}
}

func TestRefreshJobs_SkippedJobLogsOnce(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer

	fake := ownershipTestCli{apiClient: ownershipTestClient{containers: []container.Summary{
		{ID: "unowned", Names: []string{"/unowned"}, Labels: map[string]string{
			docker.DocoCDJobLabels.JobEnabled:  "true",
			docker.DocoCDJobLabels.JobSchedule: "@hourly",
		}},
	}}}
	worker := newSchedulerForMode(
		docker.ContextClient{Name: "docker-host", Cli: fake}, scheduledJobModeContainer,
		slog.New(slog.NewTextHandler(&output, nil)), nil, nil, nil, nil, nil,
		docker.ScheduledComposeOptions{},
		OwnershipOptions{InstanceID: "host-b", RequireOwnerContexts: []string{"docker-host"}},
	)
	now := time.Date(2026, time.September, 24, 6, 0, 0, 0, time.UTC)
	worker.refreshJobs(t.Context(), now)
	worker.refreshJobs(t.Context(), now.Add(time.Minute))

	if got := strings.Count(output.String(), "job not scheduled on this instance"); got != 1 {
		t.Fatalf("skipped job warning count = %d, want 1; output=%s", got, output.String())
	}
}

func TestNextScheduledRun_PreservesScheduleAlignment(t *testing.T) {
	t.Parallel()

	schedule, err := docker.ParseJobScheduleExpression("@every 1m")
	if err != nil {
		t.Fatalf("ParseJobScheduleExpression() failed: %v", err)
	}

	scheduledAt := time.Date(2026, time.May, 9, 12, 0, 0, 0, time.UTC)
	now := scheduledAt.Add(250 * time.Millisecond)

	got := nextScheduledRun(schedule, scheduledAt, now)

	want := scheduledAt.Add(time.Minute)
	if !got.Equal(want) {
		t.Fatalf("nextScheduledRun() = %s, want %s", got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
}

func TestNextScheduledRun_SkipsMissedRunsWithoutDrift(t *testing.T) {
	t.Parallel()

	schedule, err := docker.ParseJobScheduleExpression("@every 1m")
	if err != nil {
		t.Fatalf("ParseJobScheduleExpression() failed: %v", err)
	}

	scheduledAt := time.Date(2026, time.May, 9, 12, 0, 0, 0, time.UTC)
	now := scheduledAt.Add(3*time.Minute + 30*time.Second)

	got := nextScheduledRun(schedule, scheduledAt, now)

	want := time.Date(2026, time.May, 9, 12, 4, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("nextScheduledRun() = %s, want %s", got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
}

func TestGetNearestNextRun(t *testing.T) {
	t.Parallel()

	want := time.Date(2026, time.May, 9, 12, 1, 0, 0, time.UTC)

	got, ok := getNearestNextRun(map[string]scheduledJobState{
		"later": {
			nextRun: time.Date(2026, time.May, 9, 12, 5, 0, 0, time.UTC),
		},
		"earlier": {
			nextRun: want,
		},
		"zero": {},
	})
	if !ok {
		t.Fatalf("getNearestNextRun() reported no next run")
	}

	if !got.Equal(want) {
		t.Fatalf("getNearestNextRun() = %s, want %s", got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
}

func TestParseJobScheduleExpression_NextRunUsesLocalTimezone_Berlin(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("time.LoadLocation() failed: %v", err)
	}

	originalLocal := time.Local
	time.Local = berlin

	t.Cleanup(func() {
		time.Local = originalLocal
	})

	schedule, err := docker.ParseJobScheduleExpression("0 */6 * * *")
	if err != nil {
		t.Fatalf("ParseJobScheduleExpression() failed: %v", err)
	}

	now := time.Date(2026, time.May, 11, 0, 30, 0, 0, time.Local)
	got := schedule.Next(now)
	want := time.Date(2026, time.May, 11, 6, 0, 0, 0, time.Local)

	if !got.Equal(want) {
		t.Fatalf("schedule.Next() = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}
