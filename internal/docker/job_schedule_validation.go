package docker

import (
	"fmt"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
)

func validateScheduledJobPolicies(project *types.Project, swarmMode bool) error {
	for serviceName, svc := range project.Services {
		_, enabled, err := ParseJobScheduleLabels(svc.Labels)
		if err != nil {
			return fmt.Errorf("service %s: %w", serviceName, err)
		}

		if !enabled {
			continue
		}

		if swarmMode {
			if svc.Deploy == nil || svc.Deploy.RestartPolicy == nil {
				continue
			}

			condition := strings.ToLower(strings.TrimSpace(svc.Deploy.RestartPolicy.Condition))
			if condition == "none" {
				continue
			}

			return fmt.Errorf("service %s: deploy.restart_policy.condition=%q is not allowed for scheduled services; use none or unset restart_policy", serviceName, svc.Deploy.RestartPolicy.Condition)
		}

		restart := strings.ToLower(strings.TrimSpace(svc.Restart))
		if restart == "" || restart == "no" {
			continue
		}

		return fmt.Errorf("service %s: restart=%q is not allowed for scheduled services; use no or unset", serviceName, svc.Restart)
	}

	return nil
}
