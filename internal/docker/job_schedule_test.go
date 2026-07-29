package docker

import "testing"

func TestParseJobScheduleExpression(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		spec    string
		wantErr bool
	}{
		{name: "valid 5-field", spec: "*/5 * * * *", wantErr: false},
		{name: "valid predefined schedule", spec: "@daily", wantErr: false},
		{name: "valid interval schedule", spec: "@every 1h30m", wantErr: false},
		{name: "invalid seconds field", spec: "*/5 * * * * *", wantErr: true},
		{name: "invalid expression", spec: "every minute", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := ParseJobScheduleExpression(tt.spec)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseJobScheduleExpression() err=%v wantErr=%v", err, tt.wantErr)
			}
		})
	}
}

func TestParseJobScheduleExpression_AllowedSpecialCharacters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		spec string
	}{
		{name: "minutes wildcard", spec: "* * * * *"},
		{name: "minutes step", spec: "*/5 * * * *"},
		{name: "minutes list", spec: "0,15,30,45 * * * *"},
		{name: "minutes range", spec: "10-20 * * * *"},
		{name: "day of month question mark", spec: "0 3 ? * MON-FRI"},
		{name: "month names", spec: "0 0 1 JAN,APR-DEC *"},
		{name: "day of week names", spec: "0 12 * * SUN,TUE-THU"},
		{name: "day of week question mark", spec: "0 12 1 * ?"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := ParseJobScheduleExpression(tt.spec); err != nil {
				t.Fatalf("ParseJobScheduleExpression(%q) failed: %v", tt.spec, err)
			}
		})
	}
}

func TestParseJobScheduleLabels(t *testing.T) {
	t.Parallel()

	labels := map[string]string{
		docoCDJobLabelNames.JobEnabled:       "true",
		docoCDJobLabelNames.JobSchedule:      "*/10 * * * *",
		docoCDJobLabelNames.JobSkipRunning:   "true",
		docoCDJobLabelNames.JobExecutionMode: string(JobExecutionModeOneOff),
		docoCDJobLabelNames.JobNotifyOn:      string(JobNotifyFailure),
		docoCDJobLabelNames.JobSwarmReplicas: "3",
	}

	cfg, enabled, err := ParseJobScheduleLabels(labels)
	if err != nil {
		t.Fatalf("ParseJobScheduleLabels() failed: %v", err)
	}

	if !enabled {
		t.Fatalf("expected enabled=true")
	}

	if cfg.ExecutionMode != JobExecutionModeOneOff {
		t.Fatalf("unexpected execution mode: %s", cfg.ExecutionMode)
	}

	if cfg.NotifyOn != JobNotifyFailure {
		t.Fatalf("unexpected notify_on: %s", cfg.NotifyOn)
	}

	if !cfg.SkipRunning {
		t.Fatalf("expected skip_running=true")
	}

	if cfg.SwarmReplicas != 3 {
		t.Fatalf("unexpected swarm replicas: %d", cfg.SwarmReplicas)
	}
}

func TestParseJobScheduleLabels_OneShotDeprecatedAlias(t *testing.T) {
	t.Parallel()

	cfg, enabled, err := ParseJobScheduleLabels(map[string]string{
		docoCDJobLabelNames.JobEnabled:       "true",
		docoCDJobLabelNames.JobSchedule:      "0 * * * *",
		docoCDJobLabelNames.JobExecutionMode: string(JobExecutionModeOneShotDeprecated),
	})
	if err != nil {
		t.Fatalf("ParseJobScheduleLabels() failed with deprecated one_shot alias: %v", err)
	}

	if !enabled {
		t.Fatalf("expected enabled=true")
	}

	if cfg.ExecutionMode != JobExecutionModeOneOff {
		t.Fatalf("expected execution mode to be normalized to %q, got %q", JobExecutionModeOneOff, cfg.ExecutionMode)
	}
}

func TestParseJobScheduleLabels_Defaults(t *testing.T) {
	t.Parallel()

	cfg, enabled, err := ParseJobScheduleLabels(map[string]string{
		docoCDJobLabelNames.JobEnabled:  "true",
		docoCDJobLabelNames.JobSchedule: "0 * * * *",
	})
	if err != nil {
		t.Fatalf("ParseJobScheduleLabels() failed: %v", err)
	}

	if !enabled {
		t.Fatalf("expected enabled=true")
	}

	if cfg.ExecutionMode != JobExecutionModeRestart {
		t.Fatalf("unexpected default execution mode: %s", cfg.ExecutionMode)
	}

	if cfg.NotifyOn != JobNotifyAll {
		t.Fatalf("unexpected default notify_on: %s", cfg.NotifyOn)
	}

	if cfg.SkipRunning {
		t.Fatalf("expected default skip_running=false")
	}

	if cfg.SwarmReplicas != 1 {
		t.Fatalf("expected default swarm replicas=1, got %d", cfg.SwarmReplicas)
	}
}
