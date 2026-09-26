package docker

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	composetypes "github.com/docker/cli/cli/compose/types"
	"github.com/docker/compose/v5/pkg/api"
	swarmTypes "github.com/moby/moby/api/types/swarm"
)

// isScheduledJob returns true if the labels indicate that the service is a scheduled job.
func isScheduledJob(labels map[string]string) bool {
	enabled, err := strconv.ParseBool(strings.TrimSpace(labels[DocoCDJobLabels.JobEnabled]))
	return err == nil && enabled
}

// preserveComposeJobOwners carries runtime owners into a reloaded project before
// automatic recreation. A newly authored Compose owner always wins.
func preserveComposeJobOwners(project *types.Project, containers []api.ContainerSummary) error {
	owners := make(map[string]string)
	seen := make(map[string]bool)

	for _, container := range containers {
		ephemeral, _ := strconv.ParseBool(strings.TrimSpace(container.Labels[DocoCDJobLabels.JobEphemeral]))

		oneOff, _ := strconv.ParseBool(strings.TrimSpace(container.Labels[api.OneoffLabel]))
		if !isScheduledJob(container.Labels) || ephemeral || oneOff {
			continue
		}

		serviceName := container.Labels[api.ServiceLabel]

		service, ok := project.Services[serviceName]
		if !ok || !isScheduledJob(service.Labels) || hasLabel(service.Labels, DocoCDJobLabels.JobOwner) ||
			hasLabel(service.CustomLabels, DocoCDJobLabels.JobOwner) {
			continue
		}

		owner := container.Labels[DocoCDJobLabels.JobOwner]
		if seen[serviceName] && owners[serviceName] != owner {
			return fmt.Errorf("conflicting existing job owners for compose service %s", serviceName)
		}

		seen[serviceName] = true
		owners[serviceName] = owner
	}

	for serviceName, service := range project.Services {
		if !isScheduledJob(service.Labels) || hasLabel(service.Labels, DocoCDJobLabels.JobOwner) ||
			hasLabel(service.CustomLabels, DocoCDJobLabels.JobOwner) {
			continue
		}

		if owner := owners[serviceName]; owner != "" {
			if service.CustomLabels == nil {
				service.CustomLabels = make(types.Labels)
			}

			service.CustomLabels[DocoCDJobLabels.JobOwner] = owner
			project.Services[serviceName] = service
		}
	}

	return nil
}

// preserveSwarmJobOwners reads the deployed task template, not service-spec
// metadata, because scheduled-job configuration is read from task labels.
func preserveSwarmJobOwners(stack *composetypes.Config, namespace string, existing []swarmTypes.Service) {
	owners := make(map[string]string, len(existing))
	for _, service := range existing {
		if service.Spec.TaskTemplate.ContainerSpec == nil {
			continue
		}

		labels := service.Spec.TaskTemplate.ContainerSpec.Labels
		if !isScheduledJob(labels) {
			continue
		}

		if owner := labels[DocoCDJobLabels.JobOwner]; owner != "" {
			owners[service.Spec.Name] = owner
		}
	}

	for i, service := range stack.Services {
		if !isScheduledJob(service.Labels) || hasLabel(service.Labels, DocoCDJobLabels.JobOwner) ||
			hasLabel(service.Deploy.Labels, DocoCDJobLabels.JobOwner) {
			continue
		}

		if owner := owners[namespace+"_"+service.Name]; owner != "" {
			if service.Labels == nil {
				service.Labels = make(composetypes.Labels)
			}

			service.Labels[DocoCDJobLabels.JobOwner] = owner
			stack.Services[i] = service
		}
	}
}

// validateComposeJobOwners returns an error if any scheduled job service
// in the project has an invalid owner label.
func validateComposeJobOwners(project *types.Project) error {
	for name, service := range project.Services {
		labels := getServiceSchedulerLabels(service)
		if !isScheduledJob(labels) {
			continue
		}

		if _, err := parseJobOwner(labels); err != nil {
			return fmt.Errorf("compose service %s: %w", name, err)
		}
	}

	return nil
}

// validateSwarmJobOwners returns an error if any scheduled job service
// in the stack has an invalid owner label.
func validateSwarmJobOwners(stack *composetypes.Config) error {
	for _, service := range stack.Services {
		if !isScheduledJob(service.Labels) {
			continue
		}

		if _, err := parseJobOwner(service.Labels); err != nil {
			return fmt.Errorf("swarm service %s: %w", service.Name, err)
		}
	}

	return nil
}
