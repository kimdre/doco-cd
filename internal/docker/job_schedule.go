package docker

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/robfig/cron/v3"
)

type JobExecutionMode string

const (
	JobExecutionModeRestart JobExecutionMode = "restart"
	JobExecutionModeOneShot JobExecutionMode = "one_shot"
)

type JobNotifyOn string

const (
	JobNotifyNone    JobNotifyOn = "none"
	JobNotifySuccess JobNotifyOn = "success"
	JobNotifyFailure JobNotifyOn = "failure"
	JobNotifyAll     JobNotifyOn = "all"
)

type JobScheduleConfig struct {
	Enabled       bool
	Schedule      string
	RunOnDeploy   bool
	SkipRunning   bool
	ExecutionMode JobExecutionMode
	NotifyOn      JobNotifyOn
	SwarmReplicas uint64
}

func (c JobScheduleConfig) ShouldNotifySuccess() bool {
	return c.NotifyOn == JobNotifyAll || c.NotifyOn == JobNotifySuccess
}

func (c JobScheduleConfig) ShouldNotifyFailure() bool {
	return c.NotifyOn == JobNotifyAll || c.NotifyOn == JobNotifyFailure
}

func NewJobScheduleParser() cron.Parser {
	// 5-field cron format with descriptors and @every durations. Seconds are intentionally unsupported.
	return cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
}

func ParseJobScheduleExpression(spec string) (cron.Schedule, error) {
	schedule, err := NewJobScheduleParser().Parse(strings.TrimSpace(spec))
	if err != nil {
		return nil, fmt.Errorf("invalid job schedule %q: %w", spec, err)
	}

	return schedule, nil
}

func ParseJobScheduleLabels(labels map[string]string) (JobScheduleConfig, bool, error) {
	cfg := JobScheduleConfig{
		ExecutionMode: JobExecutionModeRestart,
		NotifyOn:      JobNotifyAll,
		SwarmReplicas: 1,
	}

	enabledRaw, exists := labels[docoCDJobLabelNames.JobEnabled]
	if !exists {
		return cfg, false, nil
	}

	enabled, err := strconv.ParseBool(strings.TrimSpace(enabledRaw))
	if err != nil {
		return cfg, false, fmt.Errorf("invalid %s label value %q", docoCDJobLabelNames.JobEnabled, enabledRaw)
	}

	if !enabled {
		return cfg, false, nil
	}

	cfg.Enabled = true

	schedule := strings.TrimSpace(labels[docoCDJobLabelNames.JobSchedule])
	if schedule == "" {
		return cfg, false, fmt.Errorf("%s label is required when %s=true", docoCDJobLabelNames.JobSchedule, docoCDJobLabelNames.JobEnabled)
	}

	if _, err = ParseJobScheduleExpression(schedule); err != nil {
		return cfg, false, err
	}

	cfg.Schedule = schedule

	if runOnDeployRaw, ok := labels[docoCDJobLabelNames.JobRunOnDeploy]; ok {
		runOnDeploy, parseErr := strconv.ParseBool(strings.TrimSpace(runOnDeployRaw))
		if parseErr != nil {
			return cfg, false, fmt.Errorf("invalid %s label value %q", docoCDJobLabelNames.JobRunOnDeploy, runOnDeployRaw)
		}

		cfg.RunOnDeploy = runOnDeploy
	}

	if skipRaw, ok := labels[docoCDJobLabelNames.JobSkipRunning]; ok {
		skip, parseErr := strconv.ParseBool(strings.TrimSpace(skipRaw))
		if parseErr != nil {
			return cfg, false, fmt.Errorf("invalid %s label value %q", docoCDJobLabelNames.JobSkipRunning, skipRaw)
		}

		cfg.SkipRunning = skip
	}

	if modeRaw, ok := labels[docoCDJobLabelNames.JobExecutionMode]; ok {
		mode := JobExecutionMode(strings.TrimSpace(modeRaw))
		switch mode {
		case JobExecutionModeRestart, JobExecutionModeOneShot:
			cfg.ExecutionMode = mode
		default:
			return cfg, false, fmt.Errorf("invalid %s label value %q", docoCDJobLabelNames.JobExecutionMode, modeRaw)
		}
	}

	if notifyRaw, ok := labels[docoCDJobLabelNames.JobNotifyOn]; ok {
		notifyOn := JobNotifyOn(strings.TrimSpace(notifyRaw))
		switch notifyOn {
		case JobNotifyNone, JobNotifySuccess, JobNotifyFailure, JobNotifyAll:
			cfg.NotifyOn = notifyOn
		default:
			return cfg, false, fmt.Errorf("invalid %s label value %q", docoCDJobLabelNames.JobNotifyOn, notifyRaw)
		}
	}

	if replicasRaw, ok := labels[docoCDJobLabelNames.JobSwarmReplicas]; ok {
		replicas, parseErr := strconv.ParseUint(strings.TrimSpace(replicasRaw), 10, 64)
		if parseErr != nil {
			return cfg, false, fmt.Errorf("invalid %s label value %q", docoCDJobLabelNames.JobSwarmReplicas, replicasRaw)
		}

		if replicas == 0 {
			return cfg, false, fmt.Errorf("%s must be greater than zero", docoCDJobLabelNames.JobSwarmReplicas)
		}

		cfg.SwarmReplicas = replicas
	}

	return cfg, true, nil
}
