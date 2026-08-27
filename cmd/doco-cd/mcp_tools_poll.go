package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kimdre/doco-cd/internal/common/id"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/source/oci"
)

var (
	errNoPollConfiguration       = errors.New("no poll configuration provided in request body")
	errTooManyPollConfigurations = errors.New("too many poll configurations: maximum is 32")
	errPollRunPanicked           = errors.New("poll run panicked")
)

const (
	maxTriggerPollConfigs           = 32
	defaultConcurrentPollExecutions = 4
)

type pollConfigValidationError struct {
	Index int
	Err   error
}

func (e *pollConfigValidationError) Error() string {
	return fmt.Sprintf("invalid poll configuration at index %d: %v", e.Index, e.Err)
}

func (e *pollConfigValidationError) Unwrap() error {
	return e.Err
}

type pollRunsFailedError struct {
	Failed int
	Total  int
	Cause  error
}

func (e *pollRunsFailedError) Error() string {
	return fmt.Sprintf("%d/%d poll jobs failed", e.Failed, e.Total)
}

func (e *pollRunsFailedError) Unwrap() error {
	return e.Cause
}

type triggerPollInput struct {
	Configs []triggerPollConfig `json:"configs" jsonschema:"poll configurations to run"`
	Wait    *bool               `json:"wait,omitempty" jsonschema:"wait for completion; defaults to true"`
}

type triggerPollConfig struct {
	Source       config.SourceType `json:"source,omitempty"`
	SourceURL    string            `json:"url"`
	Reference    string            `json:"reference,omitempty"`
	CustomTarget string            `json:"target,omitempty"`
	Deployments  []*deploy.Config  `json:"deployments,omitempty"`
}

type triggerPollOutput struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"`
}

func (h *handlerData) addPollMCPTools(server *mcp.Server) {
	inputSchema := mustToolInputSchema[triggerPollInput]("trigger_poll")
	inputSchema.Properties["configs"].MaxItems = jsonschema.Ptr(maxTriggerPollConfigs)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "trigger_poll",
		Description: "Trigger one or more poll configurations immediately. Prefer wait=false and poll get_deployment_run - long wait=true calls are cancelled if the server shuts down (10s grace).",
		Annotations: destructiveMCPAnnotations(false),
		InputSchema: inputSchema,
	}, instrumentMCPTool(h.log, "trigger_poll", h.triggerPollTool))
}

func (h *handlerData) triggerPollTool(ctx context.Context, _ *mcp.CallToolRequest, input triggerPollInput) (*mcp.CallToolResult, triggerPollOutput, error) {
	wait := valueOr(input.Wait, true)

	configs := make([]poll.Config, len(input.Configs))
	for i, cfg := range input.Configs {
		configs[i] = poll.Config{
			Source:       cfg.Source,
			SourceUrl:    cfg.SourceURL,
			Reference:    cfg.Reference,
			CustomTarget: cfg.CustomTarget,
			Deployments:  cfg.Deployments,
		}
	}

	jobID, err := h.runPollConfigs(ctx, configs, wait, h.log.Logger)
	result, status := triggerRunToolResult(wait, err)

	return result, triggerPollOutput{JobID: jobID, Status: status}, nil
}

func (h *handlerData) runPollConfigs(ctx context.Context, configs []poll.Config, wait bool, jobLog *slog.Logger) (string, error) {
	jobID := id.New()
	if len(configs) == 0 {
		return jobID, errNoPollConfiguration
	}

	if len(configs) > maxTriggerPollConfigs {
		return jobID, errTooManyPollConfigurations
	}

	if jobLog == nil {
		jobLog = h.log.Logger
	}

	jobLog = jobLog.With(slog.String("job_id", jobID))

	for i := range configs {
		configs[i].RunOnce = true
		configs[i].Interval = 0

		if err := configs[i].Validate(); err != nil {
			return jobID, &pollConfigValidationError{Index: i, Err: err}
		}
	}

	h.runTracker.TrackAccepted(jobID, deploymentRunTriggerPoll)

	repository := "multiple"
	target := ""

	if len(configs) == 1 {
		repository = pollRepositoryName(configs[0])
		target = configs[0].CustomTarget
	}

	h.runTracker.SetMetadata(jobID, repository, target, "")

	run := func(runCtx context.Context) error {
		h.runTracker.MarkRunning(jobID)

		runner := h.runPoll
		if runner == nil {
			runner = RunPoll
		}

		workerCount := defaultConcurrentPollExecutions
		if h.appConfig != nil && h.appConfig.MaxConcurrentDeployments > 0 {
			workerCount = int(min(h.appConfig.MaxConcurrentDeployments, uint(len(configs))))
		}

		workerCount = min(workerCount, len(configs))

		jobs := make(chan poll.Config)
		errs := make(chan error, len(configs))

		var wg sync.WaitGroup

		for range workerCount {
			wg.Go(func() {
				for cfg := range jobs {
					func() {
						defer func() {
							if recovered := recover(); recovered != nil {
								logRecoveredPanic(jobLog, "poll run", recovered)

								errs <- errPollRunPanicked
							}
						}()

						metadata := notification.Metadata{
							Repository:               pollRepositoryName(cfg),
							Target:                   cfg.CustomTarget,
							Revision:                 notification.GetRevision(cfg.Reference, ""),
							JobID:                    jobID,
							DeploymentTargetObserver: h.deploymentTargetObserver(jobID),
						}
						errs <- runner(runCtx, cfg, h.appConfig, h.dataMountPoint, h.dockerCli, h.contexts, jobLog, metadata, h.secretProvider, pollTriggerDefault)
					}()
				}
			})
		}

		for _, cfg := range configs {
			jobs <- cfg
		}

		close(jobs)

		wg.Wait()
		close(errs)

		failedRuns := 0

		var lifecycleErr error

		for runErr := range errs {
			if runErr != nil {
				failedRuns++

				if lifecycleErr == nil && isLifecycleCancellation(runErr) {
					lifecycleErr = runErr
				}
			}
		}

		if failedRuns > 0 {
			err := &pollRunsFailedError{Failed: failedRuns, Total: len(configs), Cause: lifecycleErr}
			h.runTracker.MarkFailed(jobID, err.Error())

			return err
		}

		h.runTracker.MarkSucceeded(jobID, "poll jobs complete")

		return nil
	}

	var err error
	if wait {
		err = h.runSynchronous(ctx, run)
	} else {
		err = h.runBackground(ctx, func(backgroundCtx context.Context) {
			// The run outcome is already recorded in runTracker by run itself.
			_ = run(backgroundCtx)
		})
	}

	if errors.Is(err, errBackgroundWorkClosed) {
		h.runTracker.MarkFailed(jobID, err.Error())
	}

	return jobID, err
}

func pollRepositoryName(cfg poll.Config) string {
	sourceType := config.NormalizeSourceType(cfg.Source)
	if sourceType == config.SourceTypeOCI {
		return oci.RepositoryNameFromArtifact(cfg.SourceUrl)
	}

	return git.GetRepoName(cfg.SourceUrl)
}
