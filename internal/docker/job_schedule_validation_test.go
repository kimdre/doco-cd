package docker

import (
	"errors"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
)

func TestValidateScheduledJobPolicies_PreStart(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name      string
		mode      JobExecutionMode
		enabled   bool
		swarmMode bool
		wantErr   bool
	}{
		{name: "standalone one-off", mode: JobExecutionModeOneOff, enabled: true, wantErr: true},
		{name: "standalone restart", mode: JobExecutionModeRestart, enabled: true},
		{name: "ordinary service", mode: JobExecutionModeOneOff},
		{name: "swarm unchanged", mode: JobExecutionModeOneOff, enabled: true, swarmMode: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			labels := map[string]string{}
			if tt.enabled {
				labels[DocoCDJobLabels.JobEnabled] = "true"
				labels[DocoCDJobLabels.JobSchedule] = "@every 24h"
				labels[DocoCDJobLabels.JobExecutionMode] = string(tt.mode)
			}

			service := types.ServiceConfig{
				Name:     "backup",
				Labels:   labels,
				PreStart: []types.PreStartHook{{}},
			}
			project := &types.Project{Services: types.Services{"backup": service}}

			err := validateScheduledJobPolicies(project, tt.swarmMode)
			if errors.Is(err, ErrComposeOneOffPreStartUnsupported) != tt.wantErr || (!tt.wantErr && err != nil) {
				t.Fatalf("validation error = %v, want unsupported pre_start = %v", err, tt.wantErr)
			}

			withoutHook := service
			withoutHook.PreStart = nil

			project.Services["backup"] = withoutHook
			if err := validateScheduledJobPolicies(project, tt.swarmMode); err != nil {
				t.Fatalf("service without pre_start rejected: %v", err)
			}
		})
	}
}

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
