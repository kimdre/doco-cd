package docker

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	swarmTypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/common/types/set"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
)

// waitForRunningJobs checks if there are any running scheduled jobs that are configured to be waited for before deployment,
// and waits until they are finished or the timeout is reached.
func waitForRunningJobs(
	ctx context.Context,
	dockerCli command.Cli,
	deployConfig *deploy.Config,
	project *types.Project,
	log *slog.Logger,
	swarmMode bool,
) error {
	jobServices, err := getScheduledJobServicesToWait(project, deployConfig.WaitRunningJobs)
	if err != nil {
		return err
	}

	if len(jobServices) == 0 {
		return nil
	}

	timeout := time.Duration(deployConfig.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	deadline := time.Now().Add(timeout)

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	lastWaitLogAt := time.Time{}

	for {
		running, err := getRunningScheduledJobServices(ctx, dockerCli, deployConfig.Name, jobServices, swarmMode)
		if err != nil {
			return fmt.Errorf("failed to inspect running scheduled jobs: %w", err)
		}

		if len(running) == 0 {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for running scheduled jobs to finish: %s", timeout, strings.Join(running, ", "))
		}

		now := time.Now()
		if lastWaitLogAt.IsZero() || now.Sub(lastWaitLogAt) >= 5*time.Second {
			log.Info("waiting for running scheduled jobs to finish before deployment",
				slog.Any("jobs", running),
			)

			lastWaitLogAt = now
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func getScheduledJobServicesToWait(project *types.Project, defaultWait bool) (set.Set[string], error) {
	ret := set.New[string]()

	if project == nil {
		return ret, nil
	}

	for _, svc := range project.Services {
		enabledRaw, ok := svc.Labels[DocoCDJobLabels.JobEnabled]
		if !ok {
			continue
		}

		enabled, err := strconv.ParseBool(strings.TrimSpace(enabledRaw))
		if err != nil || !enabled {
			continue
		}

		waitForService := defaultWait

		if waitRaw, waitLabelSet := svc.Labels[DocoCDJobLabels.JobWaitRunning]; waitLabelSet {
			waitForService, err = strconv.ParseBool(strings.TrimSpace(waitRaw))
			if err != nil {
				return nil, fmt.Errorf("invalid %s label value %q on service %s", DocoCDJobLabels.JobWaitRunning, waitRaw, svc.Name)
			}
		}

		if !waitForService {
			continue
		}

		ret.Add(svc.Name)
	}

	return ret, nil
}

func getRunningScheduledJobServices(
	ctx context.Context,
	dockerCli command.Cli,
	stackName string,
	configuredJobServices set.Set[string],
	swarmMode bool,
) ([]string, error) {
	runningSet := set.New[string]()

	if swarmMode {
		services, err := swarm.GetStackServices(ctx, dockerCli.Client(), stackName)
		if err != nil {
			return nil, err
		}

		for _, svc := range services {
			if svc.Spec.TaskTemplate.ContainerSpec == nil {
				continue
			}

			serviceName := strings.TrimSpace(svc.Spec.TaskTemplate.ContainerSpec.Labels[api.ServiceLabel])
			if serviceName == "" || !configuredJobServices.Contains(serviceName) {
				continue
			}

			tasks, taskErr := dockerCli.Client().TaskList(ctx, client.TaskListOptions{
				Filters: make(client.Filters).Add("service", svc.ID),
			})
			if taskErr != nil {
				return nil, taskErr
			}

			for _, task := range tasks.Items {
				if task.DesiredState == swarmTypes.TaskStateRunning && task.Status.State == swarmTypes.TaskStateRunning {
					runningSet.Add(serviceName)
					break
				}
			}
		}
	} else {
		containers, err := GetLabeledContainers(ctx, dockerCli.Client(), api.ProjectLabel, stackName, true)
		if err != nil {
			return nil, err
		}

		for _, cont := range containers {
			serviceName := strings.TrimSpace(cont.Labels[api.ServiceLabel])
			if serviceName == "" || !configuredJobServices.Contains(serviceName) {
				continue
			}

			if cont.State == "running" {
				runningSet.Add(serviceName)
			}
		}
	}

	running := runningSet.ToSlice()
	slices.Sort(running)

	return running, nil
}

func getAutostartDisabledServices(project *types.Project) (set.Set[string], error) {
	disabled := set.New[string]()
	if project == nil {
		return disabled, nil
	}

	for serviceName, svc := range project.Services {
		raw, ok := getServiceSchedulerLabels(svc)[DocoCDLabels.Deployment.Autostart]
		if !ok {
			continue
		}

		enabled, err := strconv.ParseBool(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("service %s: invalid %s label value %q",
				serviceName, DocoCDLabels.Deployment.Autostart, raw)
		}

		if !enabled {
			disabled.Add(serviceName)
		}
	}

	return disabled, nil
}

func getRunningServices(containers []api.ContainerSummary) set.Set[string] {
	running := set.New[string]()

	for _, cont := range containers {
		if strings.EqualFold(strings.TrimSpace(string(cont.State)), "running") {
			if serviceName := strings.TrimSpace(cont.Labels[api.ServiceLabel]); serviceName != "" {
				running.Add(serviceName)
			}
		}
	}

	return running
}

func getStartServicesForDeploy(project *types.Project, autostartDisabledServices, runningServices set.Set[string]) ([]string, error) {
	startServices := make([]string, 0, len(project.Services))
	completedDependencyServices := getServiceCompletedDependencies(project)

	for serviceName, svc := range project.Services {
		if completedDependencyServices.Contains(serviceName) ||
			(svc.Name != "" && completedDependencyServices.Contains(svc.Name)) {
			continue
		}

		labels := getServiceSchedulerLabels(svc)
		_, hasScheduleLabel := labels[docoCDJobLabelNames.JobEnabled]

		_, enabled, err := ParseJobScheduleLabels(labels)
		if err != nil {
			return nil, fmt.Errorf("service %s: %w", serviceName, err)
		}

		if enabled || hasScheduleLabel {
			continue
		}

		if svc.GetScale() == 0 {
			continue
		}

		if autostartDisabledServices.Contains(serviceName) && !runningServices.Contains(serviceName) {
			continue
		}

		startServices = append(startServices, serviceName)
	}

	return startServices, nil
}

// getServiceCompletedDependencies returns services referenced via depends_on with
// condition=service_completed_successfully. These are init-style one-shot services
// that should be started as dependencies but not treated as long-running start targets.
func getServiceCompletedDependencies(project *types.Project) set.Set[string] {
	completed := set.New[string]()

	if project == nil {
		return completed
	}

	for _, svc := range project.Services {
		for depName, dep := range svc.DependsOn {
			if strings.TrimSpace(dep.Condition) == types.ServiceConditionCompletedSuccessfully {
				completed.Add(depName)
			}
		}
	}

	return completed
}

func getJobServices(project *types.Project) (set.Set[string], error) {
	jobServices := set.New[string]()

	if project == nil {
		return jobServices, nil
	}

	for serviceName, svc := range project.Services {
		labels := getServiceSchedulerLabels(svc)

		_, enabled, err := ParseJobScheduleLabels(labels)
		if err != nil {
			return nil, fmt.Errorf("service %s: %w", serviceName, err)
		}

		if !enabled {
			continue
		}

		if svc.Name != "" {
			jobServices.Add(svc.Name)
		} else {
			jobServices.Add(serviceName)
		}
	}

	return jobServices, nil
}

func getNonJobServices(startServices []string, jobServices set.Set[string]) set.Set[string] {
	nonJobServices := set.New[string]()

	for _, serviceName := range startServices {
		if jobServices.Contains(serviceName) {
			continue
		}

		nonJobServices.Add(serviceName)
	}

	return nonJobServices
}

// projectForStart returns a copy containing only services that may start during deployment.
// Docker Compose's Start ignores StartOptions.Services and starts every service in the project.
func projectForStart(project *types.Project, jobServices, stoppedAutostartServices set.Set[string]) (*types.Project, error) {
	nonJobServiceNames := make([]string, 0, len(project.Services))

	for serviceName, svc := range project.Services {
		if jobServices.Contains(serviceName) || (svc.Name != "" && jobServices.Contains(svc.Name)) ||
			stoppedAutostartServices.Contains(serviceName) ||
			(svc.Name != "" && stoppedAutostartServices.Contains(svc.Name)) {
			continue
		}

		nonJobServiceNames = append(nonJobServiceNames, serviceName)
	}

	// WithSelectedServices treats an empty selection as "all services", which
	// would re-include job services. Return an explicit empty-service project
	// instead so nothing is started.
	if len(nonJobServiceNames) == 0 {
		emptyProject := *project
		emptyProject.Services = types.Services{}

		return &emptyProject, nil
	}

	// IgnoreDependencies keeps the selection to exactly the non-job services and
	// strips any depends_on edges pointing at excluded (job) services, so a job
	// service is never pulled back in as a dependency.
	startProject, err := project.WithSelectedServices(nonJobServiceNames, types.IgnoreDependencies)
	if err != nil {
		return nil, fmt.Errorf("failed to select services to start: %w", err)
	}

	return startProject, nil
}

type serviceStartStatus struct {
	running   bool
	unhealthy bool
	terminal  string
}

func assessStartedServiceStates(containers []api.ContainerSummary, targetServices set.Set[string]) (bool, []string, error) {
	statusByService := make(map[string]serviceStartStatus, targetServices.Len())
	for svc := range targetServices {
		statusByService[svc] = serviceStartStatus{}
	}

	for _, cont := range containers {
		serviceName := strings.TrimSpace(cont.Labels[api.ServiceLabel])
		if serviceName == "" || !targetServices.Contains(serviceName) {
			continue
		}

		status := statusByService[serviceName]

		state := strings.ToLower(strings.TrimSpace(string(cont.State)))
		health := strings.ToLower(strings.TrimSpace(string(cont.Health)))

		switch state {
		case "running":
			switch health {
			case "", "healthy":
				status.running = true
			case "unhealthy":
				status.unhealthy = true
			}
		// "restarting" exists only after a container died and restart policy
		// kicked in - right after deploy that is a crash, not a slow start
		case "restarting", "exited", "dead":
			status.terminal = state
		}

		statusByService[serviceName] = status
	}

	waiting := make([]string, 0, len(statusByService))
	for serviceName, status := range statusByService {
		if status.unhealthy {
			return false, nil, fmt.Errorf("service %s is unhealthy", serviceName)
		}

		if status.terminal != "" && !status.running {
			return false, nil, fmt.Errorf("service %s has a %s container", serviceName, status.terminal)
		}

		if !status.running {
			waiting = append(waiting, serviceName)
		}
	}

	slices.Sort(waiting)

	return len(waiting) == 0, waiting, nil
}

// startReadyStableSamples is how many consecutive ready samples (1s apart)
// services must hold before start counts as successful. A service that
// crashes moments after start spends most of each crash cycle in a short
// "running" window - one lucky sample used to mark the deploy successful,
// and every following poll redeployed the crashed container as drift.
const startReadyStableSamples = 3

// projectContainerLister abstracts container listing so the wait loop can be
// tested without a Docker daemon.
type projectContainerLister func(ctx context.Context) ([]api.ContainerSummary, error)

func waitForStartedServices(ctx context.Context, dockerCli command.Cli, projectName string,
	startServices []string, jobServices set.Set[string], timeout time.Duration,
) error {
	listContainers := func(ctx context.Context) ([]api.ContainerSummary, error) {
		return GetProjectContainers(ctx, dockerCli, projectName)
	}

	return waitForStartedServicesWith(ctx, listContainers, startServices, jobServices, timeout)
}

func waitForStartedServicesWith(ctx context.Context, listContainers projectContainerLister,
	startServices []string, jobServices set.Set[string], timeout time.Duration,
) error {
	nonJobServices := getNonJobServices(startServices, jobServices)
	if nonJobServices.Len() == 0 {
		return nil
	}

	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	deadline := time.Now().Add(timeout)

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	readyStreak := 0

	for {
		containers, err := listContainers(ctx)
		if err != nil {
			return fmt.Errorf("failed to inspect project containers: %w", err)
		}

		ready, waiting, stateErr := assessStartedServiceStates(containers, nonJobServices)
		if stateErr != nil {
			return stateErr
		}

		if ready {
			readyStreak++
			if readyStreak >= startReadyStableSamples {
				return nil
			}
		} else {
			readyStreak = 0

			if time.Now().After(deadline) {
				return fmt.Errorf("timed out after %s waiting for services to start: %s", timeout, strings.Join(waiting, ", "))
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func getServiceSchedulerLabels(svc types.ServiceConfig) map[string]string {
	if len(svc.CustomLabels) == 0 {
		return svc.Labels
	}

	labels := make(map[string]string, len(svc.Labels))
	maps.Copy(labels, svc.Labels)

	maps.Copy(labels, svc.CustomLabels)

	return labels
}
