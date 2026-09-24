package docker

import (
	"strings"
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
		{
			name:      "rejects stop_services referencing the job itself",
			swarmMode: false,
			project: &types.Project{
				Name: "myproject",
				Services: types.Services{
					"backup": {
						Name: "backup",
						Labels: map[string]string{
							docoCDJobLabelNames.JobEnabled:       "true",
							docoCDJobLabelNames.JobSchedule:      "0 2 * * *",
							docoCDJobLabelNames.JobExecutionMode: string(JobExecutionModeOneOff),
							docoCDJobLabelNames.JobStopServices:  "db,backup",
						},
					},
				},
			},
			wantErr: true,
		},
		{
			name:      "rejects stop_services referencing the job itself via project prefix",
			swarmMode: false,
			project: &types.Project{
				Name: "myproject",
				Services: types.Services{
					"backup": {
						Name: "backup",
						Labels: map[string]string{
							docoCDJobLabelNames.JobEnabled:       "true",
							docoCDJobLabelNames.JobSchedule:      "0 2 * * *",
							docoCDJobLabelNames.JobExecutionMode: string(JobExecutionModeOneOff),
							docoCDJobLabelNames.JobStopServices:  "myproject/backup",
						},
					},
				},
			},
			wantErr: true,
		},
		{
			name:      "allows stop_services referencing other services",
			swarmMode: false,
			project: &types.Project{
				Name: "myproject",
				Services: types.Services{
					"backup": {
						Name: "backup",
						Labels: map[string]string{
							docoCDJobLabelNames.JobEnabled:       "true",
							docoCDJobLabelNames.JobSchedule:      "0 2 * * *",
							docoCDJobLabelNames.JobExecutionMode: string(JobExecutionModeOneOff),
							docoCDJobLabelNames.JobStopServices:  "db,other-project/cache",
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name:      "allows stop_services with restart execution mode (container/standalone)",
			swarmMode: false,
			project: &types.Project{
				Name: "myproject",
				Services: types.Services{
					"backup": {
						Name:    "backup",
						Restart: "no",
						Labels: map[string]string{
							docoCDJobLabelNames.JobEnabled:      "true",
							docoCDJobLabelNames.JobSchedule:     "0 2 * * *",
							docoCDJobLabelNames.JobStopServices: "db",
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

func TestValidateScheduledJobPoliciesSwarmDeployOwner(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		swarmMode   bool
		jobEnabled  string
		taskOwner   string
		deployOwner string
		ownerSet    bool
		wantErr     bool
	}{
		{name: "invalid effective deploy owner", swarmMode: true, jobEnabled: "true", deployOwner: "bad/owner", wantErr: true},
		{name: "empty effective deploy owner", swarmMode: true, jobEnabled: "true", ownerSet: true, wantErr: true},
		{name: "whitespace effective deploy owner", swarmMode: true, jobEnabled: "true", deployOwner: " ", wantErr: true},
		{name: "valid effective deploy owner", swarmMode: true, jobEnabled: "true", deployOwner: "instance-a"},
		{name: "task owner overrides invalid deploy owner", swarmMode: true, jobEnabled: "true", taskOwner: "instance-a", deployOwner: "bad/owner"},
		{name: "invalid task owner is not hidden by deploy owner", swarmMode: true, jobEnabled: "true", taskOwner: "bad/owner", deployOwner: "instance-a", wantErr: true},
		{name: "disabled job ignores deploy owner", swarmMode: true, jobEnabled: "false", deployOwner: "bad/owner"},
		{name: "standalone ignores swarm deploy owner", jobEnabled: "true", deployOwner: "bad/owner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			labels := types.Labels{
				DocoCDJobLabels.JobEnabled:  tc.jobEnabled,
				DocoCDJobLabels.JobSchedule: "@hourly",
			}
			if tc.taskOwner != "" {
				labels[DocoCDJobLabels.JobOwner] = tc.taskOwner
			}

			deployLabels := types.Labels{}
			if tc.deployOwner != "" || tc.ownerSet {
				deployLabels[DocoCDJobLabels.JobOwner] = tc.deployOwner
			}

			project := &types.Project{Services: types.Services{
				"backup": {
					Name:   "backup",
					Labels: labels,
					Deploy: &types.DeployConfig{Labels: deployLabels},
				},
			}}

			err := validateScheduledJobPolicies(project, tc.swarmMode)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateScheduledJobPolicies() error = %v, wantErr=%t", err, tc.wantErr)
			}

			if tc.wantErr && !strings.Contains(err.Error(), DocoCDJobLabels.JobOwner) {
				t.Errorf("expected owner label in error, got %v", err)
			}

			if _, ok := labels[DocoCDJobLabels.JobOwner]; ok != (tc.taskOwner != "") {
				t.Error("validation mutated task-template labels")
			}
		})
	}
}
