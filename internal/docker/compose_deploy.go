package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"

	"github.com/kimdre/doco-cd/internal/common/types/set"
	"github.com/kimdre/doco-cd/internal/common/types/slice"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker/registryauth"
)

// deployCompose deploys a project as specified by the Docker Compose specification (LoadCompose).
func deployCompose(ctx context.Context, dockerCli command.Cli, project *types.Project,
	deployConfig *deploy.Config, recreateMode string, services []string,
	needSignal []SignalService, setPhase func(string),
) error {
	var (
		err          error
		beforeImages map[string]api.ImageSummary // Images used by stack before deployment
		afterImages  map[string]api.ImageSummary // Images used by stack after deployment
	)

	service, err := compose.NewComposeService(dockerCli)
	if err != nil {
		return err
	}

	if len(needSignal) > 0 {
		setDeploymentPhase(setPhase, "signaling services")

		if err := ComposeSignal(ctx, dockerCli, project, needSignal); err != nil {
			return err
		}
	}

	if deployConfig.PruneImages {
		beforeImages, err = service.Images(ctx, project.Name, api.ImagesOptions{})
		if err != nil {
			// No such image error is okay since we wanted to remove the image anyway
			if !strings.Contains(strings.ToLower(err.Error()), ErrNoSuchImage.Error()) {
				return fmt.Errorf("failed to get existing images: %w", err)
			}
		}
	}

	if deployConfig.ForceImagePull {
		for i, s := range project.Services {
			s.PullPolicy = types.PullPolicyAlways
			project.Services[i] = s
		}
	}

	setDeploymentPhase(setPhase, "pulling images")

	err = service.Pull(ctx, project, api.PullOptions{
		Quiet:           true,
		IgnoreBuildable: true,
	})
	if err != nil {
		imageRefs := make([]string, 0, len(project.Services))
		for _, svc := range project.Services {
			if svc.Image == "" {
				continue
			}

			imageRefs = append(imageRefs, svc.Image)
		}

		hint := registryauth.BuildFailureHint(dockerCli.ConfigFile(), imageRefs, err)
		if hint != "" {
			return fmt.Errorf("failed to pull images: %w; %s", err, hint)
		}

		return fmt.Errorf("failed to pull images: %w", err)
	}

	if recreateMode == "" {
		recreateMode = api.RecreateDiverged
	}

	// Convert deployConfig.BuildOpts.Args to types.MappingWithEquals
	buildArgs := make(types.MappingWithEquals)
	for k, v := range deployConfig.BuildOpts.Args {
		buildArgs[k] = &v
	}

	buildOpts := api.BuildOptions{
		Pull:     deployConfig.BuildOpts.ForceImagePull,
		Quiet:    deployConfig.BuildOpts.Quiet,
		Progress: "auto",
		Args:     buildArgs,
		NoCache:  deployConfig.BuildOpts.NoCache,
	}

	setDeploymentPhase(setPhase, "building images")

	err = service.Build(ctx, project, buildOpts)
	if err != nil {
		return err
	}

	createOpts := api.CreateOptions{
		Services:             services,
		RemoveOrphans:        deployConfig.RemoveOrphans,
		Recreate:             recreateMode,
		RecreateDependencies: api.RecreateDiverged,
		QuietPull:            true,
	}

	autostartDisabledServices, err := getAutostartDisabledServices(project)
	if err != nil {
		return err
	}

	runningServices := set.New[string]()

	if autostartDisabledServices.Len() > 0 {
		containers, err := GetProjectContainers(ctx, dockerCli, project.Name)
		if err != nil {
			return fmt.Errorf("failed to inspect existing services before deployment: %w", err)
		}

		runningServices = getRunningServices(containers)
	}

	startServices, err := getStartServicesForDeploy(project, autostartDisabledServices, runningServices)
	if err != nil {
		return err
	}

	jobServices, err := getJobServices(project)
	if err != nil {
		return err
	}

	stoppedAutostartServices := autostartDisabledServices.Difference(runningServices)

	// Remove mismatched recreatable volumes (tmpfs, NFS, CIFS mounts) before create.
	// Docker Compose then recreates them with the desired configuration during service.Create.
	setDeploymentPhase(setPhase, "preparing deployment resources")

	if err = removeMismatchedRecreatableVolumes(ctx, dockerCli.Client(), deployConfig.Name, project); err != nil {
		return fmt.Errorf("failed to remove mismatched recreatable volumes: %w", err)
	}

	setDeploymentPhase(setPhase, "creating services")

	err = service.Create(ctx, project, createOpts)
	if err != nil {
		return err
	}

	if len(startServices) > 0 {
		setDeploymentPhase(setPhase, "starting services")

		// Docker Compose's Start ignores StartOptions.Services and starts every
		// service in the passed project (including containers in the "created" or
		// "exited" state), so narrow the project to services allowed to start.
		startProject, err := projectForStart(project, jobServices, stoppedAutostartServices)
		if err != nil {
			return err
		}

		startOpts := api.StartOptions{
			Project:  startProject,
			Wait:     false,
			Services: startServices,
		}

		err = service.Start(ctx, startProject.Name, startOpts)
		if err != nil {
			if !errors.Is(err, ErrNoContainerToStart) {
				return err
			}
		}

		setDeploymentPhase(setPhase, "waiting for services to start")

		err = waitForStartedServices(ctx, dockerCli, project.Name, startServices, jobServices,
			time.Duration(deployConfig.Timeout)*time.Second)
		if err != nil {
			return err
		}
	}

	if deployConfig.PruneImages {
		setDeploymentPhase(setPhase, "pruning unused images")

		afterImages, err = service.Images(ctx, project.Name, api.ImagesOptions{})
		if err != nil {
			// No such image error is okay since we wanted to remove the image anyway
			if !strings.Contains(strings.ToLower(err.Error()), ErrNoSuchImage.Error()) {
				return fmt.Errorf("failed to get images after deployment: %w", err)
			}
		}

		// Determine unused images by comparing image SHAs used by services before and after the deployment

		var ids []string

		for svc, beforeImg := range beforeImages {
			afterImg, exists := afterImages[svc]
			if !exists || beforeImg.ID != afterImg.ID {
				ids = append(ids, beforeImg.ID)
			}
		}

		_, err = pruneImages(ctx, dockerCli, slice.Unique(ids))
		if err != nil {
			return fmt.Errorf("failed to prune images: %w", err)
		}
	}

	return nil
}
