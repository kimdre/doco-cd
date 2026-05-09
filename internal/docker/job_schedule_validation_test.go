package docker

import (
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
)

func TestValidateScheduledJobPolicies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		swarmMode bool
		project   *types.Project
		wantErr   bool
	}{
		{
			name:      "standalone allows restart no",
			swarmMode: false,
			project: &types.Project{
				Services: types.Services{
					"ok": {
						Name:    "ok",
						Restart: "no",
						Labels: map[string]string{
							docoCDJobLabelNames.JobEnabled:       "true",
							docoCDJobLabelNames.JobSchedule:      "*/10 * * * *",
							docoCDJobLabelNames.JobExecutionMode: string(JobExecutionModeOneShot),
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name:      "standalone rejects restart always",
			swarmMode: false,
			project: &types.Project{
				Services: types.Services{
					"bad": {
						Name:    "bad",
						Restart: "always",
						Labels: map[string]string{
							docoCDJobLabelNames.JobEnabled:       "true",
							docoCDJobLabelNames.JobSchedule:      "*/10 * * * *",
							docoCDJobLabelNames.JobExecutionMode: string(JobExecutionModeOneShot),
						},
					},
				},
			},
			wantErr: true,
		},
		{
			name:      "swarm job-mode rejects restart policy condition any",
			swarmMode: true,
			project: &types.Project{
				Services: types.Services{
					"bad-job": {
						Name: "bad-job",
						Deploy: &types.DeployConfig{
							Mode: "replicated-job",
							RestartPolicy: &types.RestartPolicy{
								Condition: "any",
							},
						},
						Labels: map[string]string{
							docoCDJobLabelNames.JobEnabled:       "true",
							docoCDJobLabelNames.JobSchedule:      "0 * * * *",
							docoCDJobLabelNames.JobExecutionMode: string(JobExecutionModeOneShot),
						},
					},
				},
			},
			wantErr: true,
		},
		{
			name:      "swarm job-mode allows restart policy none",
			swarmMode: true,
			project: &types.Project{
				Services: types.Services{
					"ok-job": {
						Name: "ok-job",
						Deploy: &types.DeployConfig{
							Mode: "global-job",
							RestartPolicy: &types.RestartPolicy{
								Condition: "none",
							},
						},
						Labels: map[string]string{
							docoCDJobLabelNames.JobEnabled:       "true",
							docoCDJobLabelNames.JobSchedule:      "@every 1h",
							docoCDJobLabelNames.JobExecutionMode: string(JobExecutionModeOneShot),
						},
					},
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateScheduledJobPolicies(tt.project, tt.swarmMode)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateScheduledJobPolicies() err=%v wantErr=%v", err, tt.wantErr)
			}
		})
	}
}
