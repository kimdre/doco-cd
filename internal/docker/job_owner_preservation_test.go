package docker

import (
	"maps"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	composetypes "github.com/docker/cli/cli/compose/types"
	"github.com/docker/compose/v5/pkg/api"
	swarmTypes "github.com/moby/moby/api/types/swarm"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func TestPreserveComposeJobOwnersOnAutomaticRedeploy(t *testing.T) {
	jobLabels := func(owner string) map[string]string {
		labels := map[string]string{DocoCDJobLabels.JobEnabled: "true", DocoCDJobLabels.JobSchedule: "@hourly"}
		if owner != "" {
			labels[DocoCDJobLabels.JobOwner] = owner
		}

		return labels
	}
	container := func(service, owner string) api.ContainerSummary {
		labels := jobLabels(owner)
		labels[api.ServiceLabel] = service

		return api.ContainerSummary{Labels: labels}
	}

	project := &types.Project{Name: "stack", Services: types.Services{
		"backup":   {Name: "backup", Labels: jobLabels("")},
		"rotate":   {Name: "rotate", Labels: jobLabels("")},
		"override": {Name: "override", Labels: jobLabels("instance-c")},
		"unowned":  {Name: "unowned", Labels: jobLabels("")},
		"new":      {Name: "new", Labels: jobLabels("")},
		"web":      {Name: "web"},
	}}
	containers := []api.ContainerSummary{
		container("backup", "instance-a"),
		container("rotate", "instance-d"),
		container("override", "instance-a"),
		container("override", "instance-b"),
		container("unowned", ""),
		container("web", "instance-a"),
	}
	ephemeral := container("backup", "instance-b")
	ephemeral.Labels[DocoCDJobLabels.JobEphemeral] = "true"
	containers = append(containers, ephemeral)
	oneOff := container("backup", "instance-b")
	oneOff.Labels[api.OneoffLabel] = "True"
	containers = append(containers, oneOff)

	if err := preserveComposeJobOwners(project, containers); err != nil {
		t.Fatal(err)
	}

	addComposeServiceLabels(project, &deploy.Config{Name: "stack"}, &webhook.ParsedPayload{},
		"", "/repo", "dev", "2026-01-01T00:00:00Z", ComposeVersion, "", "", "")

	for service, want := range map[string]string{
		"backup": "instance-a", "rotate": "instance-d", "override": "instance-c",
		"unowned": "", "new": "", "web": "",
	} {
		got := getServiceSchedulerLabels(project.Services[service])[DocoCDJobLabels.JobOwner]
		if got != want {
			t.Errorf("service %q owner = %q, want %q", service, got, want)
		}
	}
}

func TestPreserveComposeJobOwnersRejectsConflicts(t *testing.T) {
	project := &types.Project{Services: types.Services{
		"backup": {Name: "backup", Labels: types.Labels{DocoCDJobLabels.JobEnabled: "true"}},
	}}

	containers := []api.ContainerSummary{
		{Labels: map[string]string{api.ServiceLabel: "backup", DocoCDJobLabels.JobEnabled: "true", DocoCDJobLabels.JobOwner: "instance-a"}},
		{Labels: map[string]string{api.ServiceLabel: "backup", DocoCDJobLabels.JobEnabled: "true", DocoCDJobLabels.JobOwner: "instance-b"}},
	}
	if err := preserveComposeJobOwners(project, containers); err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("expected conflicting owners to prevent automatic redeploy, got %v", err)
	}
}

func TestPreserveSwarmJobOwnersOnRotation(t *testing.T) {
	job := func(name, owner string) composetypes.ServiceConfig {
		labels := composetypes.Labels{DocoCDJobLabels.JobEnabled: "true", DocoCDJobLabels.JobSchedule: "@hourly"}
		if owner != "" {
			labels[DocoCDJobLabels.JobOwner] = owner
		}

		return composetypes.ServiceConfig{Name: name, Labels: labels}
	}
	existing := func(name, owner string) swarmTypes.Service {
		labels := map[string]string{DocoCDJobLabels.JobEnabled: "true"}
		if owner != "" {
			labels[DocoCDJobLabels.JobOwner] = owner
		}

		return swarmTypes.Service{Spec: swarmTypes.ServiceSpec{
			Annotations:  swarmTypes.Annotations{Name: "stack_" + name},
			TaskTemplate: swarmTypes.TaskSpec{ContainerSpec: &swarmTypes.ContainerSpec{Labels: labels}},
		}}
	}
	deployOverride := job("deploy-override", "")
	deployOverride.Deploy.Labels = composetypes.Labels{DocoCDJobLabels.JobOwner: "instance-c"}
	stack := &composetypes.Config{Services: []composetypes.ServiceConfig{
		job("backup", ""), job("rotate", ""), job("override", "instance-c"),
		deployOverride, job("unowned", ""), job("new", ""),
		{Name: "web"},
	}}
	prior := []swarmTypes.Service{
		existing("backup", "instance-a"),
		existing("rotate", "instance-d"),
		existing("override", "instance-a"),
		existing("deploy-override", "instance-a"),
		existing("unowned", ""),
		existing("web", "instance-a"),
	}

	preserveSwarmJobOwners(stack, "stack", prior)
	addSwarmServiceLabels(stack, nil, &deploy.Config{Name: "stack"}, &webhook.ParsedPayload{},
		"", "/repo", "dev", "2026-01-01T00:00:00Z", "", "", "")

	want := []string{"instance-a", "instance-d", "instance-c", "instance-c", "", "", ""}
	for i, service := range stack.Services {
		if got := service.Labels[DocoCDJobLabels.JobOwner]; got != want[i] {
			t.Errorf("service %q task-template owner = %q, want %q", service.Name, got, want[i])
		}
	}

	before := maps.Clone(stack.Services[0].Labels)
	addSwarmServiceLabels(stack, nil, &deploy.Config{Name: "stack"}, &webhook.ParsedPayload{},
		"", "/repo", "dev", "2026-01-02T00:00:00Z", "", "", "")

	if !maps.Equal(stack.Services[0].Labels, before) {
		t.Error("rotation changed an unrelated scheduled job's task template")
	}
}

func TestValidateAutomaticJobOwners(t *testing.T) {
	tests := []struct {
		name    string
		labels  map[string]string
		wantErr bool
	}{
		{
			name:    "empty owner",
			labels:  map[string]string{DocoCDJobLabels.JobEnabled: "true", DocoCDJobLabels.JobOwner: ""},
			wantErr: true,
		},
		{
			name:    "malformed owner",
			labels:  map[string]string{DocoCDJobLabels.JobEnabled: "true", DocoCDJobLabels.JobOwner: "bad owner"},
			wantErr: true,
		},
		{
			name:   "valid owner",
			labels: map[string]string{DocoCDJobLabels.JobEnabled: "true", DocoCDJobLabels.JobOwner: "instance-a"},
		},
		{
			name:   "unowned job",
			labels: map[string]string{DocoCDJobLabels.JobEnabled: "true"},
		},
		{
			name:   "non-job owner",
			labels: map[string]string{DocoCDJobLabels.JobOwner: "bad owner"},
		},
		{
			name:   "unrelated legacy schedule",
			labels: map[string]string{DocoCDJobLabels.JobEnabled: "true", DocoCDJobLabels.JobSchedule: "invalid schedule"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			project := &types.Project{Services: types.Services{
				"backup": {Name: "backup", Labels: types.Labels(tc.labels)},
			}}
			stack := &composetypes.Config{Services: []composetypes.ServiceConfig{
				{Name: "backup", Labels: composetypes.Labels(tc.labels)},
			}}

			for name, validate := range map[string]func() error{
				"compose": func() error { return validateComposeJobOwners(project) },
				"swarm":   func() error { return validateSwarmJobOwners(stack) },
			} {
				t.Run(name, func(t *testing.T) {
					err := validate()
					if tc.wantErr {
						if err == nil || !strings.Contains(err.Error(), DocoCDJobLabels.JobOwner) {
							t.Fatalf("expected invalid owner error, got %v", err)
						}
					} else if err != nil {
						t.Fatalf("unexpected owner validation error: %v", err)
					}
				})
			}
		})
	}
}
