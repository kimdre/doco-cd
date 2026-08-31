package scheduler

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/go-co-op/gocron/v2"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/secretprovider"
)

var (
	ErrScheduledJobNotFound  = errors.New("scheduled job not found")
	ErrScheduledJobDisabled  = errors.New("scheduled job is disabled")
	ErrScheduledJobAmbiguous = errors.New("multiple scheduled jobs matched, narrow your selection")
)

type scheduledJobMode string

const (
	scheduledJobModeContainer scheduledJobMode = "container"
	scheduledJobModeSwarm     scheduledJobMode = "swarm"
)

type scheduledJob struct {
	key             string
	name            string
	id              string
	mode            scheduledJobMode
	labels          map[string]string
	containerState  string // Docker container state (container mode only), e.g. "running", "exited"
	containerStatus string // Docker container status string (container mode only), e.g. "Exited (0) 2 hours ago"
	running         bool   // An execution is currently active (e.g. a running one_off ephemeral container)
	// context is the normalized Docker context name the job was discovered on
	// (empty string means the default context). See docker.NormalizeContextName.
	context string
}

type scheduledJobState struct {
	fingerprint string
	schedule    gocron.Cron
	lastRun     time.Time
	nextRun     time.Time
	deployment  string
	cfg         docker.JobScheduleConfig
}

type scheduler struct {
	dockerCli command.Cli
	// contextName is the normalized Docker context this worker operates on
	// (empty string means the default context). See docker.NormalizeContextName.
	contextName string
	// mode is the runtime this worker manages jobs for. A Swarm manager hosts
	// both Compose projects and Swarm stacks, so it runs one worker per mode
	// instead of deriving behavior from the process-global
	// swarm.GetModeEnabled().
	mode           scheduledJobMode
	secretProvider secretprovider.SecretProvider
	log            *slog.Logger
	wg             *sync.WaitGroup
	startedAt      time.Time

	states map[string]scheduledJobState

	runningMu sync.Mutex
	running   map[string]bool

	// stopHolds reference-counts services currently held stopped by
	// stopServicesForJob/startServicesForJob. It is keyed by (mode, resolved
	// project/stack, service) so that if two concurrent scheduled runs both
	// declare the same target in stop_services, the target is only actually
	// stopped by the first holder and only actually restarted once the last
	// holder releases it — this prevents one run from prematurely restarting
	// a service another concurrent run still needs stopped. For swarm mode,
	// the held state also records the original replica count so it can be
	// restored when the last holder releases it.
	stopHoldsMu sync.Mutex
	stopHolds   map[stopHoldKey]*stopHoldState
}

// stopHoldKey identifies a service that may be concurrently held stopped by
// more than one scheduled job run. context is the normalized Docker context
// name the hold applies to, so that two workers operating on different
// contexts never share a hold for a same-named project/service.
type stopHoldKey struct {
	context string
	mode    scheduledJobMode
	project string
	service string
}

// stopHoldState tracks how many concurrent job runs currently hold a service
// stopped, and (for swarm mode) the replica count it should be restored to.
type stopHoldState struct {
	refCount int
	replicas uint64
}

// JobInfo describes one scheduler-managed target and its runtime scheduling status.
type JobInfo struct {
	Name           string                  `json:"name"`
	Context        string                  `json:"context"`
	Enabled        bool                    `json:"enabled"`
	Stack          string                  `json:"stack,omitempty"`
	Mode           string                  `json:"mode"`
	Schedule       string                  `json:"schedule,omitempty"`
	ExecutionMode  docker.JobExecutionMode `json:"execution_mode,omitempty"`
	SkipRunning    bool                    `json:"skip_running"`
	NotifyOn       docker.JobNotifyOn      `json:"notify_on,omitempty"`
	Replicas       uint64                  `json:"replicas,omitempty"`
	StopServices   []string                `json:"stop_services,omitempty"`
	Status         string                  `json:"status,omitempty"`
	LastRunAt      *time.Time              `json:"last_run_at,omitempty"`
	NextRunAt      *time.Time              `json:"next_run_at,omitempty"`
	LabelNextRunAt *time.Time              `json:"label_next_run_at,omitempty"`
	Repository     string                  `json:"repository,omitempty"`
	ScheduleError  string                  `json:"schedule_error,omitempty"`
	Valid          bool                    `json:"valid"`
}

// newSchedulerForMode builds a scheduler worker bound to a single Docker
// context and runtime mode. log and wg may be nil for short-lived, one-shot
// workers (e.g. a single ListJobs/TriggerNow call) that never call run().
func newSchedulerForMode(cc docker.ContextClient, mode scheduledJobMode, log *slog.Logger, wg *sync.WaitGroup, secretProvider secretprovider.SecretProvider) *scheduler {
	if log == nil {
		log = slog.Default()
	}

	contextName := docker.NormalizeContextName(cc.Name)

	return &scheduler{
		dockerCli:      cc.Cli,
		contextName:    contextName,
		mode:           mode,
		secretProvider: secretProvider,
		log:            log.With(slog.String("component", "scheduler"), slog.String("context", docker.DisplayContextName(contextName))),
		wg:             wg,
		startedAt:      schedulerNow(),
		states:         map[string]scheduledJobState{},
		running:        map[string]bool{},
		stopHolds:      map[stopHoldKey]*stopHoldState{},
	}
}
