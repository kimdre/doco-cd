package docker

import (
	"maps"
	"testing"

	"github.com/docker/compose/v5/pkg/api"
)

func TestParseJobScheduleExpression(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		spec    string
		wantErr bool
	}{
		{name: "valid 5-field", spec: "*/5 * * * *", wantErr: false},
		{name: "valid predefined yearly schedule", spec: "@yearly", wantErr: false},
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

func TestParseJobScheduleLabels_RejectsOneShotExecutionMode(t *testing.T) {
	t.Parallel()

	_, _, err := ParseJobScheduleLabels(map[string]string{
		docoCDJobLabelNames.JobEnabled:       "true",
		docoCDJobLabelNames.JobSchedule:      "0 * * * *",
		docoCDJobLabelNames.JobExecutionMode: "one_shot",
	})
	if err == nil {
		t.Fatal("expected one_shot execution mode to be rejected")
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

func TestParseStopServiceRefs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		want    []StopServiceRef
		wantErr bool
	}{
		{
			name: "single same-project service",
			raw:  "db",
			want: []StopServiceRef{{Service: "db"}},
		},
		{
			name: "multiple same-project services",
			raw:  "db,cache",
			want: []StopServiceRef{{Service: "db"}, {Service: "cache"}},
		},
		{
			name: "cross-project service",
			raw:  "other-project/db",
			want: []StopServiceRef{{Project: "other-project", Service: "db"}},
		},
		{
			name: "mixed same- and cross-project",
			raw:  "db,other-project/cache",
			want: []StopServiceRef{{Service: "db"}, {Project: "other-project", Service: "cache"}},
		},
		{
			name: "whitespace around entries is trimmed",
			raw:  " db , other/cache ",
			want: []StopServiceRef{{Service: "db"}, {Project: "other", Service: "cache"}},
		},
		{
			name: "empty entries are skipped",
			raw:  "db,,cache",
			want: []StopServiceRef{{Service: "db"}, {Service: "cache"}},
		},
		{
			name:    "missing project name is rejected",
			raw:     "/db",
			wantErr: true,
		},
		{
			name:    "missing service name is rejected",
			raw:     "project/",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseStopServiceRefs(tt.raw)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseStopServiceRefs(%q) err=%v wantErr=%v", tt.raw, err, tt.wantErr)
			}

			if tt.wantErr {
				return
			}

			if len(got) != len(tt.want) {
				t.Fatalf("got %d refs, want %d: %+v", len(got), len(tt.want), got)
			}

			for i, ref := range got {
				if ref != tt.want[i] {
					t.Errorf("ref[%d]: got %+v, want %+v", i, ref, tt.want[i])
				}
			}
		})
	}
}

func TestParseJobScheduleLabels_StopServices(t *testing.T) {
	t.Parallel()

	baseLabels := func(extra map[string]string) map[string]string {
		m := map[string]string{
			docoCDJobLabelNames.JobEnabled:       "true",
			docoCDJobLabelNames.JobSchedule:      "0 2 * * *",
			docoCDJobLabelNames.JobExecutionMode: string(JobExecutionModeOneOff),
			api.ProjectLabel:                     "myproject",
			api.ServiceLabel:                     "backup",
		}
		maps.Copy(m, extra)

		return m
	}

	t.Run("valid same-project services", func(t *testing.T) {
		t.Parallel()

		cfg, _, err := ParseJobScheduleLabels(baseLabels(map[string]string{
			docoCDJobLabelNames.JobStopServices: "db,cache",
		}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(cfg.StopServices) != 2 {
			t.Fatalf("expected 2 stop_services, got %d", len(cfg.StopServices))
		}
	})

	t.Run("valid cross-project service", func(t *testing.T) {
		t.Parallel()

		cfg, _, err := ParseJobScheduleLabels(baseLabels(map[string]string{
			docoCDJobLabelNames.JobStopServices: "other-project/db",
		}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(cfg.StopServices) != 1 || cfg.StopServices[0].Project != "other-project" {
			t.Fatalf("unexpected stop_services: %+v", cfg.StopServices)
		}
	})

	t.Run("restart execution mode is allowed", func(t *testing.T) {
		t.Parallel()

		cfg, _, err := ParseJobScheduleLabels(baseLabels(map[string]string{
			docoCDJobLabelNames.JobExecutionMode: string(JobExecutionModeRestart),
			docoCDJobLabelNames.JobStopServices:  "db",
		}))
		if err != nil {
			t.Fatalf("unexpected error for stop_services with execution_mode=restart: %v", err)
		}

		if len(cfg.StopServices) != 1 {
			t.Fatalf("expected 1 stop_service, got %d", len(cfg.StopServices))
		}
	})

	t.Run("default (restart) execution mode is allowed", func(t *testing.T) {
		t.Parallel()

		labels := baseLabels(map[string]string{
			docoCDJobLabelNames.JobStopServices: "db",
		})
		delete(labels, docoCDJobLabelNames.JobExecutionMode)

		cfg, _, err := ParseJobScheduleLabels(labels)
		if err != nil {
			t.Fatalf("unexpected error for stop_services with default execution_mode: %v", err)
		}

		if len(cfg.StopServices) != 1 {
			t.Fatalf("expected 1 stop_service, got %d", len(cfg.StopServices))
		}
	})

	t.Run("empty stop_services is allowed with any execution mode", func(t *testing.T) {
		t.Parallel()

		cfg, _, err := ParseJobScheduleLabels(baseLabels(map[string]string{
			docoCDJobLabelNames.JobExecutionMode: string(JobExecutionModeRestart),
			docoCDJobLabelNames.JobStopServices:  "  ,  ",
		}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(cfg.StopServices) != 0 {
			t.Fatalf("expected no stop_services, got %+v", cfg.StopServices)
		}
	})
}

func TestValidateStopServicesSelfReference(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{name: "same-project bare self-reference is rejected", raw: "backup", wantErr: true},
		{name: "explicit self-reference via project prefix is rejected", raw: "myproject/backup", wantErr: true},
		{name: "different service in same project is allowed", raw: "db", wantErr: false},
		{name: "different project with same service name is allowed", raw: "other-project/backup", wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			refs, err := parseStopServiceRefs(tt.raw)
			if err != nil {
				t.Fatalf("unexpected parse error: %v", err)
			}

			err = ValidateStopServicesSelfReference("myproject", "backup", refs)
			if tt.wantErr && err == nil {
				t.Fatal("expected self-reference error, got nil")
			}

			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
