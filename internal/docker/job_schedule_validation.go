package docker

import (
	"fmt"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
)

func validateScheduledJobPolicies(project *types.Project, swarmMode bool) error {
	for serviceName, svc := range project.Services {
		cfg, enabled, err := ParseJobScheduleLabels(svc.Labels)
		if err != nil {
			return fmt.Errorf("service %s: %w", serviceName, err)
		}

		if !enabled || cfg.ExecutionMode != JobExecutionModeOneShot {
			continue
		}

		if swarmMode {
			if svc.Deploy == nil {
				continue
			}

			mode := strings.ToLower(strings.TrimSpace(svc.Deploy.Mode))
			if mode != "replicated-job" && mode != "global-job" {
				continue
			}

			if svc.Deploy.RestartPolicy == nil {
				continue
			}

			condition := strings.ToLower(strings.TrimSpace(svc.Deploy.RestartPolicy.Condition))
			if condition == "" || condition == "none" {
				continue
			}

			return fmt.Errorf("service %s: deploy.restart_policy.condition=%q is not allowed for swarm job-mode one-shot schedules; use none", serviceName, svc.Deploy.RestartPolicy.Condition)
		}

		restart := strings.ToLower(strings.TrimSpace(svc.Restart))
		if restart == "" || restart == "no" {
			continue
		}

		return fmt.Errorf("service %s: restart=%q is not allowed for standalone one-shot schedules; use no or unset", serviceName, svc.Restart)
	}

	return nil
}
