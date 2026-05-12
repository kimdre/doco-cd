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
			name:      "standalone allows restart no for scheduled restart mode",
			swarmMode: false,
			project: &types.Project{
				Services: types.Services{
					"ok": {
						Name:    "ok",
						Restart: "no",
						Labels: map[string]string{
							docoCDJobLabelNames.JobEnabled:  "true",
							docoCDJobLabelNames.JobSchedule: "*/10 * * * *",
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name:      "standalone rejects restart always for scheduled restart mode",
			swarmMode: false,
			project: &types.Project{
				Services: types.Services{
					"bad": {
						Name:    "bad",
						Restart: "always",
						Labels: map[string]string{
							docoCDJobLabelNames.JobEnabled:  "true",
							docoCDJobLabelNames.JobSchedule: "*/10 * * * *",
						},
					},
				},
			},
			wantErr: true,
		},
		{
			name:      "standalone allows one_off with restart unset",
			swarmMode: false,
			project: &types.Project{
				Services: types.Services{
					"ok-one-off": {
						Name: "ok-one-off",
						Labels: map[string]string{
							docoCDJobLabelNames.JobEnabled:       "true",
							docoCDJobLabelNames.JobSchedule:      "*/10 * * * *",
							docoCDJobLabelNames.JobExecutionMode: string(JobExecutionModeOneOff),
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name:      "swarm rejects restart policy condition any",
			swarmMode: true,
			project: &types.Project{
				Services: types.Services{
					"bad-job": {
						Name: "bad-job",
						Deploy: &types.DeployConfig{
							RestartPolicy: &types.RestartPolicy{
								Condition: "any",
							},
						},
						Labels: map[string]string{
							docoCDJobLabelNames.JobEnabled:       "true",
							docoCDJobLabelNames.JobSchedule:      "0 * * * *",
							docoCDJobLabelNames.JobExecutionMode: string(JobExecutionModeOneOff),
						},
					},
				},
			},
			wantErr: true,
		},
		{
			name:      "swarm allows restart policy none",
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
							docoCDJobLabelNames.JobExecutionMode: string(JobExecutionModeOneOff),
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name:      "swarm rejects empty restart policy condition when policy is set",
			swarmMode: true,
			project: &types.Project{
				Services: types.Services{
					"bad-empty": {
						Name: "bad-empty",
						Deploy: &types.DeployConfig{
							RestartPolicy: &types.RestartPolicy{},
						},
						Labels: map[string]string{
							docoCDJobLabelNames.JobEnabled:  "true",
							docoCDJobLabelNames.JobSchedule: "*/5 * * * *",
						},
					},
				},
			},
			wantErr: true,
		},
		{
			name:      "swarm allows unset restart policy",
			swarmMode: true,
			project: &types.Project{
				Services: types.Services{
					"ok-unset": {
						Name:   "ok-unset",
						Deploy: &types.DeployConfig{},
						Labels: map[string]string{
							docoCDJobLabelNames.JobEnabled:  "true",
							docoCDJobLabelNames.JobSchedule: "*/5 * * * *",
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
