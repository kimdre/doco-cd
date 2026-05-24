package docker

import (
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/api"

	"github.com/kimdre/doco-cd/internal/utils/set"
)

func TestGetStartServicesForDeploy(t *testing.T) {
	t.Parallel()

	project := &types.Project{
		Services: types.Services{
			"api": {
				Name: "api",
			},
			"scaled-down": {
				Name:  "scaled-down",
				Scale: new(0),
			},
			"disabled": {
				Name: "disabled",
				Labels: map[string]string{
					docoCDJobLabelNames.JobEnabled: "false",
				},
			},
			"web": {
				Name: "web",
				Labels: map[string]string{
					docoCDJobLabelNames.JobEnabled:  "true",
					docoCDJobLabelNames.JobSchedule: "*/5 * * * *",
				},
			},
			"custom": {
				Name: "custom",
				CustomLabels: map[string]string{
					docoCDJobLabelNames.JobEnabled:  "true",
					docoCDJobLabelNames.JobSchedule: "@every 30m",
				},
			},
			"job": {
				Name: "job",
				Labels: map[string]string{
					docoCDJobLabelNames.JobEnabled:       "true",
					docoCDJobLabelNames.JobSchedule:      "@hourly",
					docoCDJobLabelNames.JobExecutionMode: string(JobExecutionModeOneOff),
				},
			},
		},
	}

	services, err := getStartServicesForDeploy(project)
	if err != nil {
		t.Fatalf("getStartServicesForDeploy() failed: %v", err)
	}

	if len(services) != 1 || services[0] != "api" {
		t.Fatalf("unexpected start services: %v", services)
	}
}

func TestGetStartServicesForDeploy_InvalidLabels(t *testing.T) {
	t.Parallel()

	project := &types.Project{
		Services: types.Services{
			"bad": {
				Name: "bad",
				Labels: map[string]string{
					docoCDJobLabelNames.JobEnabled:  "true",
					docoCDJobLabelNames.JobSchedule: "not-a-valid-schedule",
				},
			},
		},
	}

	if _, err := getStartServicesForDeploy(project); err == nil {
		t.Fatalf("expected error for invalid schedule labels")
	}
}

func TestGetJobServices(t *testing.T) {
	t.Parallel()

	project := &types.Project{
		Services: types.Services{
			"job": {
				Name: "job",
				Labels: map[string]string{
					docoCDJobLabelNames.JobEnabled:  "true",
					docoCDJobLabelNames.JobSchedule: "*/5 * * * *",
				},
			},
			"disabled-job": {
				Name: "disabled-job",
				Labels: map[string]string{
					docoCDJobLabelNames.JobEnabled: "false",
				},
			},
			"api": {
				Name: "api",
			},
		},
	}

	jobServices, err := getJobServices(project)
	if err != nil {
		t.Fatalf("getJobServices() failed: %v", err)
	}

	if !jobServices.Contains("job") {
		t.Fatalf("expected job service to be included: %v", jobServices.ToSlice())
	}

	if jobServices.Contains("disabled-job") || jobServices.Contains("api") {
		t.Fatalf("unexpected service marked as job: %v", jobServices.ToSlice())
	}
}

func TestAssessStartedServiceStates_IgnoresExitedJobContainer(t *testing.T) {
	t.Parallel()

	containers := []api.ContainerSummary{
		{
			State: "running",
			Labels: map[string]string{
				api.ServiceLabel: "api",
			},
		},
		{
			State: "exited",
			Labels: map[string]string{
				api.ServiceLabel: "backup",
			},
		},
	}

	allReady, waiting, err := assessStartedServiceStates(containers, set.New[string]("api"))
	if err != nil {
		t.Fatalf("assessStartedServiceStates() returned unexpected error: %v", err)
	}

	if !allReady {
		t.Fatalf("expected all target services to be ready, waiting: %v", waiting)
	}
}

func TestAssessStartedServiceStates_FailsWhenNonJobExited(t *testing.T) {
	t.Parallel()

	containers := []api.ContainerSummary{
		{
			State: "exited",
			Labels: map[string]string{
				api.ServiceLabel: "api",
			},
		},
	}

	_, _, err := assessStartedServiceStates(containers, set.New[string]("api"))
	if err == nil {
		t.Fatalf("expected error when target service has an exited container")
	}
}
