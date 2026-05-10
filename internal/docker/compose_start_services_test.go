package docker

import (
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
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
					docoCDJobLabelNames.JobExecutionMode: string(JobExecutionModeOneShot),
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
