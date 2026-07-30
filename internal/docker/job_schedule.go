package docker

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/docker/compose/v5/pkg/api"
	"github.com/go-co-op/gocron/v2"
)

type JobExecutionMode string

const (
	JobExecutionModeRestart JobExecutionMode = "restart"
	JobExecutionModeOneOff  JobExecutionMode = "one_off"

	// JobExecutionModeOneShotDeprecated is the deprecated alias for JobExecutionModeOneOff.
	//
	// Deprecated: still accepted for backward compatibility but will log a warning
	// TODO: Remove in a future release.
	JobExecutionModeOneShotDeprecated JobExecutionMode = "one_shot"
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
	SkipRunning   bool
	ExecutionMode JobExecutionMode
	NotifyOn      JobNotifyOn
	SwarmReplicas uint64
	StopServices  []StopServiceRef
}

// StopServiceRef identifies a compose service (or swarm service) to be temporarily
// stopped before a scheduled job runs and restarted after it completes.
//
// In standalone (compose) mode:
//   - Service is the compose service name as declared in the compose file (the map
//     key under `services:`). It is always the service name, never the container_name.
//   - Project identifies the compose project. When empty, the job's own project is used.
//
// In swarm mode:
//   - Service is the short service name as declared in the compose file.
//   - Project is the stack name. When empty, the job's own stack is used.
//   - The full swarm service name is resolved as "<project>_<service>".
type StopServiceRef struct {
	Project string // empty = same project/stack as the job
	Service string
}

func (c JobScheduleConfig) ShouldNotifySuccess() bool {
	return c.NotifyOn == JobNotifyAll || c.NotifyOn == JobNotifySuccess
}

func (c JobScheduleConfig) ShouldNotifyFailure() bool {
	return c.NotifyOn == JobNotifyAll || c.NotifyOn == JobNotifyFailure
}

func NewJobScheduleParser() gocron.Cron {
	// 5-field cron format with descriptors and @every durations. Seconds are intentionally unsupported.
	return gocron.NewDefaultCron(false)
}

func ParseJobScheduleExpression(spec string) (gocron.Cron, error) {
	schedule := NewJobScheduleParser()
	if err := schedule.IsValid(strings.TrimSpace(spec), time.Local, time.Now()); err != nil {
		return nil, fmt.Errorf("invalid job schedule %q: %w", spec, err)
	}

	return schedule, nil
}

func ParseJobScheduleLabels(labels map[string]string, log ...*slog.Logger) (JobScheduleConfig, bool, error) {
	var logger *slog.Logger
	if len(log) > 0 && log[0] != nil {
		logger = log[0]
	} else {
		logger = slog.Default()
	}

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
		case JobExecutionModeRestart, JobExecutionModeOneOff:
			cfg.ExecutionMode = mode
		case JobExecutionModeOneShotDeprecated:
			logger.Warn(
				fmt.Sprintf("label %s: value %q is deprecated, use %q instead", docoCDJobLabelNames.JobExecutionMode, JobExecutionModeOneShotDeprecated, JobExecutionModeOneOff),
				slog.String("label", docoCDJobLabelNames.JobExecutionMode),
			)
			cfg.ExecutionMode = JobExecutionModeOneOff
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

	if stopRaw, ok := labels[docoCDJobLabelNames.JobStopServices]; ok {
		refs, parseErr := parseStopServiceRefs(stopRaw)
		if parseErr != nil {
			return cfg, false, fmt.Errorf("invalid %s label value %q: %w", docoCDJobLabelNames.JobStopServices, stopRaw, parseErr)
		}

		// Validate that the job does not list itself.
		jobProject := strings.TrimSpace(labels[api.ProjectLabel])
		jobService := strings.TrimSpace(labels[api.ServiceLabel])

		for _, ref := range refs {
			resolvedProject := ref.Project
			if resolvedProject == "" {
				resolvedProject = jobProject
			}

			if resolvedProject == jobProject && ref.Service == jobService {
				return cfg, false, fmt.Errorf("%s: a job cannot stop itself (%s/%s)", docoCDJobLabelNames.JobStopServices, resolvedProject, ref.Service)
			}
		}

		cfg.StopServices = refs
	}

	return cfg, true, nil
}

// parseStopServiceRefs parses a comma-separated list of "project/service" or "service"
// entries into StopServiceRef values. Empty entries are silently skipped.
func parseStopServiceRefs(raw string) ([]StopServiceRef, error) {
	var refs []StopServiceRef

	for entry := range strings.SplitSeq(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		project, service, hasDelimiter := strings.Cut(entry, "/")
		project = strings.TrimSpace(project)
		service = strings.TrimSpace(service)

		if service == "" {
			if hasDelimiter {
				return nil, fmt.Errorf("entry %q has an empty service name", entry)
			}

			// No "/" found: the whole entry is the service name, same project.
			refs = append(refs, StopServiceRef{Service: project})

			continue
		}

		if project == "" {
			return nil, fmt.Errorf("entry %q has an empty project name", entry)
		}

		refs = append(refs, StopServiceRef{Project: project, Service: service})
	}

	return refs, nil
}
