package docker

import (
	"context"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/api"

	"github.com/kimdre/doco-cd/internal/common/types/set"
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

	services, err := getStartServicesForDeploy(project, set.New[string](), set.New[string]())
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

	if _, err := getStartServicesForDeploy(project, set.New[string](), set.New[string]()); err == nil {
		t.Fatalf("expected error for invalid schedule labels")
	}
}

func TestGetAutostartDisabledServices(t *testing.T) {
	t.Parallel()

	project := &types.Project{
		Services: types.Services{
			"default": {Name: "default"},
			"enabled": {
				Name: "enabled",
				Labels: map[string]string{
					DocoCDLabels.Deployment.Autostart: "true",
				},
			},
			"disabled": {
				Name: "disabled",
				Labels: map[string]string{
					DocoCDLabels.Deployment.Autostart: "false",
				},
			},
			"custom-disabled": {
				Name: "custom-disabled",
				CustomLabels: map[string]string{
					DocoCDLabels.Deployment.Autostart: " FALSE ",
				},
			},
		},
	}

	disabled, err := getAutostartDisabledServices(project)
	if err != nil {
		t.Fatalf("getAutostartDisabledServices() failed: %v", err)
	}

	if !disabled.Contains("disabled") || !disabled.Contains("custom-disabled") {
		t.Fatalf("expected disabled services, got %v", disabled.ToSlice())
	}

	if disabled.Contains("default") || disabled.Contains("enabled") {
		t.Fatalf("unexpected disabled services: %v", disabled.ToSlice())
	}
}

func TestGetAutostartDisabledServices_InvalidValue(t *testing.T) {
	t.Parallel()

	project := &types.Project{
		Services: types.Services{
			"bad": {
				Name: "bad",
				Labels: map[string]string{
					DocoCDLabels.Deployment.Autostart: "sometimes",
				},
			},
		},
	}

	if _, err := getAutostartDisabledServices(project); err == nil {
		t.Fatalf("expected invalid autostart label to fail")
	}
}

func TestGetStartServicesForDeploy_PreservesAutostartState(t *testing.T) {
	t.Parallel()

	project := &types.Project{
		Services: types.Services{
			"api":     {Name: "api"},
			"running": {Name: "running"},
			"stopped": {Name: "stopped"},
		},
	}

	services, err := getStartServicesForDeploy(
		project,
		set.New[string]("running", "stopped"),
		set.New[string]("running"),
	)
	if err != nil {
		t.Fatalf("getStartServicesForDeploy() failed: %v", err)
	}

	got := set.New[string](services...)
	if !got.Contains("api") || !got.Contains("running") {
		t.Fatalf("expected normal and previously running services to start, got %v", services)
	}

	if got.Contains("stopped") {
		t.Fatalf("previously stopped service must not start, got %v", services)
	}
}

func TestGetRunningServices(t *testing.T) {
	t.Parallel()

	services := getRunningServices([]api.ContainerSummary{
		{
			State:  "running",
			Labels: map[string]string{api.ServiceLabel: "api"},
		},
		{
			State:  "exited",
			Labels: map[string]string{api.ServiceLabel: "stopped"},
		},
		{
			State:  "running",
			Labels: map[string]string{},
		},
	})

	if !services.Contains("api") || services.Contains("stopped") || services.Contains("") {
		t.Fatalf("unexpected running services: %v", services.ToSlice())
	}
}

func TestGetStartServicesForDeploy_IncludesCompletedDependencyServices(t *testing.T) {
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

	services, err := getStartServicesForDeploy(project, set.New[string](), set.New[string]())
	if err != nil {
		t.Fatalf("getStartServicesForDeploy() failed: %v", err)
	}

	startSet := set.New[string](services...)

	if !startSet.Contains("init") || !startSet.Contains("api") || !startSet.Contains("db") {
		t.Fatalf("expected one-shot, dependent, and normal services to be started: %v", services)
	}
}

func TestGetOneShotServices(t *testing.T) {
	t.Parallel()

	project := &types.Project{
		Services: types.Services{
			"init": {
				Name: "init",
			},
			"post-init": {
				Name: "post-init",
				Labels: map[string]string{
					DocoCDLabels.Deployment.OneShot: "true",
				},
			},
			"explicitly-long-running": {
				Name: "explicitly-long-running",
				Labels: map[string]string{
					DocoCDLabels.Deployment.OneShot: "false",
				},
			},
			"default-restart-policy": {
				Name: "default-restart-policy",
			},
			"no-restart-policy": {
				Name:    "no-restart-policy",
				Restart: "no",
			},
			"api": {
				Name: "api",
				DependsOn: types.DependsOnConfig{
					"init": {
						Condition: types.ServiceConditionCompletedSuccessfully,
					},
				},
			},
		},
	}

	services, err := getOneShotServices(project)
	if err != nil {
		t.Fatalf("getOneShotServices() failed: %v", err)
	}

	if !services.Contains("init") || !services.Contains("post-init") {
		t.Fatalf("expected dependency and labeled one-shot services, got %v", services.ToSlice())
	}

	if services.Contains("explicitly-long-running") || services.Contains("default-restart-policy") ||
		services.Contains("no-restart-policy") || services.Contains("api") {
		t.Fatalf("unexpected one-shot services: %v", services.ToSlice())
	}
}

func TestGetOneShotServices_InvalidLabel(t *testing.T) {
	t.Parallel()

	project := &types.Project{
		Services: types.Services{
			"invalid": {
				Name: "invalid",
				Labels: map[string]string{
					DocoCDLabels.Deployment.OneShot: "sometimes",
				},
			},
		},
	}

	if _, err := getOneShotServices(project); err == nil {
		t.Fatalf("expected invalid one-shot label to fail")
	}
}

func TestGetOneShotServices_RejectsContinuousRestartPolicy(t *testing.T) {
	t.Parallel()

	project := &types.Project{
		Services: types.Services{
			"invalid": {
				Name:    "invalid",
				Restart: "unless-stopped",
				Labels: map[string]string{
					DocoCDLabels.Deployment.OneShot: "true",
				},
			},
		},
	}

	if _, err := getOneShotServices(project); err == nil {
		t.Fatalf("expected incompatible one-shot restart policy to fail")
	}
}

func TestAddOneShotServiceLabels(t *testing.T) {
	t.Parallel()

	project := &types.Project{
		Services: types.Services{
			"init": {
				Name:         "init",
				CustomLabels: types.Labels{"existing": "value"},
			},
			"api": {
				Name: "api",
			},
		},
	}

	addOneShotServiceLabels(project, set.New[string]("init"))

	initService := project.Services["init"]
	if initService.CustomLabels[DocoCDLabels.Deployment.OneShot] != "true" ||
		initService.CustomLabels["existing"] != "value" {
		t.Fatalf("unexpected init service labels: %v", initService.CustomLabels)
	}

	if _, exists := project.Services["api"].CustomLabels[DocoCDLabels.Deployment.OneShot]; exists {
		t.Fatalf("ordinary service must not receive the one-shot label")
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

	startProject, err := projectForStart(project, jobServices, set.New[string]())
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

	startProject, err := projectForStart(project, jobServices, set.New[string]())
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

	startProject, err := projectForStart(project, jobServices, set.New[string]())
	if err != nil {
		t.Fatalf("projectForStart() failed: %v", err)
	}

	if len(startProject.Services) != 0 {
		t.Fatalf("expected no services to start when only job services exist, got: %v", startProject.ServiceNames())
	}
}

func TestProjectForStart_ExcludesStoppedAutostartServices(t *testing.T) {
	t.Parallel()

	project := &types.Project{
		Name: "stack",
		Services: types.Services{
			"api": {
				Name: "api",
				DependsOn: types.DependsOnConfig{
					"on-demand": {Condition: "service_started"},
				},
			},
			"on-demand": {
				Name: "on-demand",
			},
		},
	}

	startProject, err := projectForStart(
		project,
		set.New[string](),
		set.New[string]("on-demand"),
	)
	if err != nil {
		t.Fatalf("projectForStart() failed: %v", err)
	}

	if _, ok := startProject.Services["on-demand"]; ok {
		t.Fatalf("stopped autostart service must be excluded from start project")
	}

	if _, ok := startProject.Services["api"].DependsOn["on-demand"]; ok {
		t.Fatalf("dependency on stopped autostart service must be stripped")
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

	ready, waiting, err := assessStartedServiceStates(containers, set.New[string]("api"), set.New[string]())
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

	_, _, err := assessStartedServiceStates(containers, set.New[string]("api"), set.New[string]())
	if err == nil {
		t.Fatalf("expected error when target service has an exited container")
	}
}

func TestAssessStartedServiceStates_FailsWhenRestarting(t *testing.T) {
	t.Parallel()

	containers := []api.ContainerSummary{
		{
			State: "restarting",
			Labels: map[string]string{
				api.ServiceLabel: "api",
			},
		},
	}

	_, _, err := assessStartedServiceStates(containers, set.New[string]("api"), set.New[string]())
	if err == nil {
		t.Fatalf("expected error when target service has a restarting container")
	}
}

func TestAssessStartedServiceStates_AcceptsCompletedOneShot(t *testing.T) {
	t.Parallel()

	containers := []api.ContainerSummary{
		runningContainer("api"),
		{
			State:    "exited",
			ExitCode: 0,
			Labels: map[string]string{
				api.ServiceLabel: "post-init",
			},
		},
	}

	ready, waiting, err := assessStartedServiceStates(
		containers,
		set.New[string]("api", "post-init"),
		set.New[string]("post-init"),
	)
	if err != nil {
		t.Fatalf("assessStartedServiceStates() returned unexpected error: %v", err)
	}

	if !ready || len(waiting) != 0 {
		t.Fatalf("expected services to be ready, waiting: %v", waiting)
	}
}

func TestAssessStartedServiceStates_WaitsForRunningOneShot(t *testing.T) {
	t.Parallel()

	ready, waiting, err := assessStartedServiceStates(
		[]api.ContainerSummary{runningContainer("post-init")},
		set.New[string]("post-init"),
		set.New[string]("post-init"),
	)
	if err != nil {
		t.Fatalf("assessStartedServiceStates() returned unexpected error: %v", err)
	}

	if ready || len(waiting) != 1 || waiting[0] != "post-init" {
		t.Fatalf("expected to wait for running one-shot service, got ready=%t waiting=%v", ready, waiting)
	}
}

func TestAssessStartedServiceStates_FailsWhenOneShotExitsNonZero(t *testing.T) {
	t.Parallel()

	_, _, err := assessStartedServiceStates(
		[]api.ContainerSummary{{
			State:    "exited",
			ExitCode: 12,
			Labels: map[string]string{
				api.ServiceLabel: "post-init",
			},
		}},
		set.New[string]("post-init"),
		set.New[string]("post-init"),
	)
	if err == nil {
		t.Fatalf("expected nonzero one-shot exit to fail")
	}
}

func TestAssessStartedServiceStates_RequiresAllOneShotContainersToComplete(t *testing.T) {
	t.Parallel()

	for _, containers := range [][]api.ContainerSummary{
		{
			{State: "exited", ExitCode: 0, Labels: map[string]string{api.ServiceLabel: "post-init"}},
			runningContainer("post-init"),
		},
		{
			runningContainer("post-init"),
			{State: "exited", ExitCode: 0, Labels: map[string]string{api.ServiceLabel: "post-init"}},
		},
	} {
		ready, waiting, err := assessStartedServiceStates(
			containers,
			set.New[string]("post-init"),
			set.New[string]("post-init"),
		)
		if err != nil {
			t.Fatalf("assessStartedServiceStates() returned unexpected error: %v", err)
		}

		if ready || len(waiting) != 1 || waiting[0] != "post-init" {
			t.Fatalf("expected to wait for every one-shot container, got ready=%t waiting=%v", ready, waiting)
		}
	}
}

func TestAssessStartedServiceStates_FailsOneShotRegardlessOfContainerOrder(t *testing.T) {
	t.Parallel()

	for _, containers := range [][]api.ContainerSummary{
		{
			{State: "exited", ExitCode: 0, Labels: map[string]string{api.ServiceLabel: "post-init"}},
			{State: "exited", ExitCode: 1, Labels: map[string]string{api.ServiceLabel: "post-init"}},
		},
		{
			{State: "exited", ExitCode: 1, Labels: map[string]string{api.ServiceLabel: "post-init"}},
			{State: "exited", ExitCode: 0, Labels: map[string]string{api.ServiceLabel: "post-init"}},
		},
	} {
		_, _, err := assessStartedServiceStates(
			containers,
			set.New[string]("post-init"),
			set.New[string]("post-init"),
		)
		if err == nil {
			t.Fatalf("expected any nonzero one-shot exit to fail")
		}
	}
}

// scriptedLister returns one containers sample per call, holding the last
// sample once the script is exhausted.
func scriptedLister(t *testing.T, samples [][]api.ContainerSummary, calls *int) projectContainerLister {
	t.Helper()

	return func(_ context.Context) ([]api.ContainerSummary, error) {
		i := *calls
		if i >= len(samples) {
			i = len(samples) - 1
		}

		*calls++

		return samples[i], nil
	}
}

func runningContainer(service string) api.ContainerSummary {
	return api.ContainerSummary{
		State: "running",
		Labels: map[string]string{
			api.ServiceLabel: service,
		},
	}
}

func TestWaitForStartedServices_FailsOnCrashLoop(t *testing.T) {
	t.Parallel()

	// crash-right-after-start: first sample catches the brief "running"
	// window, the next one sees the restart-policy backoff
	calls := 0
	lister := scriptedLister(t, [][]api.ContainerSummary{
		{runningContainer("api")},
		{{State: "restarting", Labels: map[string]string{api.ServiceLabel: "api"}}},
	}, &calls)

	err := waitForStartedServicesWith(
		t.Context(), lister, []string{"api"}, set.New[string](), set.New[string](), 30*time.Second,
	)
	if err == nil {
		t.Fatalf("expected crashlooping service to fail the start wait")
	}
}

func TestWaitForStartedServices_RequiresStableReadiness(t *testing.T) {
	t.Parallel()

	// a not-yet-running sample resets the streak: success must take the
	// full startReadyStableSamples consecutive ready samples after the dip
	calls := 0
	lister := scriptedLister(t, [][]api.ContainerSummary{
		{runningContainer("api")},
		{{State: "created", Labels: map[string]string{api.ServiceLabel: "api"}}},
		{runningContainer("api")},
		{runningContainer("api")},
		{runningContainer("api")},
	}, &calls)

	err := waitForStartedServicesWith(
		t.Context(), lister, []string{"api"}, set.New[string](), set.New[string](), 30*time.Second,
	)
	if err != nil {
		t.Fatalf("waitForStartedServicesWith() returned unexpected error: %v", err)
	}

	if calls != 5 {
		t.Fatalf("expected success on the 5th sample (streak reset by the dip), got %d calls", calls)
	}
}

func TestWaitForStartedServices_SucceedsAfterStableSamples(t *testing.T) {
	t.Parallel()

	calls := 0
	lister := scriptedLister(t, [][]api.ContainerSummary{
		{runningContainer("api")},
	}, &calls)

	err := waitForStartedServicesWith(
		t.Context(), lister, []string{"api"}, set.New[string](), set.New[string](), 30*time.Second,
	)
	if err != nil {
		t.Fatalf("waitForStartedServicesWith() returned unexpected error: %v", err)
	}

	if calls != startReadyStableSamples {
		t.Fatalf("expected exactly %d samples before success, got %d", startReadyStableSamples, calls)
	}
}

func TestWaitForStartedServices_AcceptsOneShotCompletedBeforeFirstPoll(t *testing.T) {
	t.Parallel()

	calls := 0
	lister := scriptedLister(t, [][]api.ContainerSummary{{
		runningContainer("api"),
		{
			State:    "exited",
			ExitCode: 0,
			Labels: map[string]string{
				api.ServiceLabel: "post-init",
			},
		},
	}}, &calls)

	err := waitForStartedServicesWith(
		t.Context(),
		lister,
		[]string{"api", "post-init"},
		set.New[string](),
		set.New[string]("post-init"),
		30*time.Second,
	)
	if err != nil {
		t.Fatalf("waitForStartedServicesWith() returned unexpected error: %v", err)
	}

	if calls != startReadyStableSamples {
		t.Fatalf("expected exactly %d samples before success, got %d", startReadyStableSamples, calls)
	}
}

func TestWaitForStartedServices_AllowsOneShotRestartRetry(t *testing.T) {
	t.Parallel()

	calls := 0
	lister := scriptedLister(t, [][]api.ContainerSummary{
		{{State: "exited", ExitCode: 1, Labels: map[string]string{api.ServiceLabel: "post-init"}}},
		{{State: "restarting", Labels: map[string]string{api.ServiceLabel: "post-init"}}},
		{{State: "running", Labels: map[string]string{api.ServiceLabel: "post-init"}}},
		{{State: "exited", ExitCode: 0, Labels: map[string]string{api.ServiceLabel: "post-init"}}},
	}, &calls)

	err := waitForStartedServicesWith(
		t.Context(),
		lister,
		[]string{"post-init"},
		set.New[string](),
		set.New[string]("post-init"),
		30*time.Second,
	)
	if err != nil {
		t.Fatalf("waitForStartedServicesWith() returned unexpected error: %v", err)
	}
}

func TestWaitForStartedServices_FailsStableOneShotError(t *testing.T) {
	t.Parallel()

	calls := 0
	lister := scriptedLister(t, [][]api.ContainerSummary{{
		{
			State:    "exited",
			ExitCode: 1,
			Labels: map[string]string{
				api.ServiceLabel: "post-init",
			},
		},
	}}, &calls)

	err := waitForStartedServicesWith(
		t.Context(),
		lister,
		[]string{"post-init"},
		set.New[string](),
		set.New[string]("post-init"),
		30*time.Second,
	)
	if err == nil {
		t.Fatalf("expected stable one-shot failure to fail")
	}

	if calls != oneShotFailureStableSamples {
		t.Fatalf("expected failure after %d samples, got %d", oneShotFailureStableSamples, calls)
	}
}
