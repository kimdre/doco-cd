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

func TestGetStartServicesForDeploy_ExcludesCompletedDependencyServices(t *testing.T) {
	t.Parallel()

	project := &types.Project{
		Services: types.Services{
			"init": {
				Name: "init",
			},
			"db": {
				Name: "db",
			},
			"api": {
				Name: "api",
				DependsOn: types.DependsOnConfig{
					"init": {
						Condition: "service_completed_successfully",
					},
					"db": {
						Condition: "service_started",
					},
				},
			},
		},
	}

	services, err := getStartServicesForDeploy(project)
	if err != nil {
		t.Fatalf("getStartServicesForDeploy() failed: %v", err)
	}

	startSet := set.New[string](services...)

	if startSet.Contains("init") {
		t.Fatalf("did not expect completed dependency service in start targets: %v", services)
	}

	if !startSet.Contains("api") || !startSet.Contains("db") {
		t.Fatalf("expected dependent and normal services to be started: %v", services)
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

func TestProjectForStart_ExcludesJobServices(t *testing.T) {
	t.Parallel()

	project := &types.Project{
		Name: "stack",
		Services: types.Services{
			"api": {
				Name: "api",
			},
			"init": {
				Name: "init",
			},
			"web": {
				Name: "web",
				DependsOn: types.DependsOnConfig{
					"init": {Condition: "service_completed_successfully"},
				},
			},
			"job": {
				Name: "job",
				Labels: map[string]string{
					docoCDJobLabelNames.JobEnabled:  "true",
					docoCDJobLabelNames.JobSchedule: "*/5 * * * *",
				},
			},
		},
	}

	jobServices, err := getJobServices(project)
	if err != nil {
		t.Fatalf("getJobServices() failed: %v", err)
	}

	startProject, err := projectForStart(project, jobServices)
	if err != nil {
		t.Fatalf("projectForStart() failed: %v", err)
	}

	if _, ok := startProject.Services["job"]; ok {
		t.Fatalf("scheduled job service must be excluded from the start project")
	}

	for _, name := range []string{"api", "init", "web"} {
		if _, ok := startProject.Services[name]; !ok {
			t.Fatalf("expected non-job service %q to be retained in the start project", name)
		}
	}

	// The original project must be left untouched.
	if _, ok := project.Services["job"]; !ok {
		t.Fatalf("original project must not be mutated")
	}
}

func TestProjectForStart_DependencyOnJobServiceStripped(t *testing.T) {
	t.Parallel()

	project := &types.Project{
		Name: "stack",
		Services: types.Services{
			"api": {
				Name: "api",
				DependsOn: types.DependsOnConfig{
					"job": {Condition: "service_started"},
				},
			},
			"job": {
				Name: "job",
				Labels: map[string]string{
					docoCDJobLabelNames.JobEnabled:  "true",
					docoCDJobLabelNames.JobSchedule: "*/5 * * * *",
				},
			},
		},
	}

	jobServices, err := getJobServices(project)
	if err != nil {
		t.Fatalf("getJobServices() failed: %v", err)
	}

	startProject, err := projectForStart(project, jobServices)
	if err != nil {
		t.Fatalf("projectForStart() failed: %v", err)
	}

	if _, ok := startProject.Services["job"]; ok {
		t.Fatalf("job service must not be pulled in as a dependency")
	}

	if _, ok := startProject.Services["api"].DependsOn["job"]; ok {
		t.Fatalf("depends_on edge to job service must be stripped")
	}
}

func TestProjectForStart_OnlyJobServices(t *testing.T) {
	t.Parallel()

	project := &types.Project{
		Name: "stack",
		Services: types.Services{
			"job": {
				Name: "job",
				Labels: map[string]string{
					docoCDJobLabelNames.JobEnabled:  "true",
					docoCDJobLabelNames.JobSchedule: "*/5 * * * *",
				},
			},
		},
	}

	jobServices, err := getJobServices(project)
	if err != nil {
		t.Fatalf("getJobServices() failed: %v", err)
	}

	startProject, err := projectForStart(project, jobServices)
	if err != nil {
		t.Fatalf("projectForStart() failed: %v", err)
	}

	if len(startProject.Services) != 0 {
		t.Fatalf("expected no services to start when only job services exist, got: %v", startProject.ServiceNames())
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

	ready, waiting, err := assessStartedServiceStates(containers, set.New[string]("api"))
	if err != nil {
		t.Fatalf("assessStartedServiceStates() returned unexpected error: %v", err)
	}

	if !ready {
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
