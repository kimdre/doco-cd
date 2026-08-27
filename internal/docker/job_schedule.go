package docker

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-co-op/gocron/v2"
)

type JobExecutionMode string

const (
	JobExecutionModeRestart JobExecutionMode = "restart"
	JobExecutionModeOneOff  JobExecutionMode = "one_off"
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

		cfg.StopServices = refs
	}

	return cfg, true, nil
}

// ValidateStopServicesSelfReference returns an error if refs contains an entry
// that resolves to the job's own project/stack and service name, which would
// cause the scheduler to stop the job's own service right before running it.
//
// This cannot be checked inside ParseJobScheduleLabels because it only has
// access to the raw label map: standalone/compose containers always carry
// com.docker.compose.project/com.docker.compose.service labels, but Swarm
// services deployed by doco-cd do not carry those labels on the task spec, so
// the job's own project/service identity must be resolved by the caller
// (which knows how to derive it for both compose and Swarm jobs) and passed
// in explicitly.
func ValidateStopServicesSelfReference(project, service string, refs []StopServiceRef) error {
	for _, ref := range refs {
		resolvedProject := ref.Project
		if resolvedProject == "" {
			resolvedProject = project
		}

		if resolvedProject == project && ref.Service == service {
			return fmt.Errorf("%s: a job cannot stop itself (%s/%s)", docoCDJobLabelNames.JobStopServices, resolvedProject, ref.Service)
		}
	}

	return nil
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
