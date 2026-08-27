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
}

func (e *pollRunsFailedError) Error() string {
	return fmt.Sprintf("%d/%d poll jobs failed", e.Failed, e.Total)
}

type triggerPollInput struct {
	Configs []triggerPollConfig `json:"configs" jsonschema:"poll configurations to run"`
	Wait    *bool               `json:"wait,omitempty" jsonschema:"wait for completion; defaults to true"`
}

type triggerPollConfig struct {
	Source       config.SourceType `json:"source" default:"git"`
	SourceURL    string            `json:"url"`
	Reference    string            `json:"reference,omitempty"`
	CustomTarget string            `json:"target" default:""`
	Deployments  []*deploy.Config  `json:"deployments" default:"[]"`
}

type triggerPollOutput struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"`
}

func (h *handlerData) addPollMCPTools(server *mcp.Server) {
	destructive := true
	closedWorld := false

	inputSchema, err := jsonschema.For[triggerPollInput](nil)
	if err != nil {
		panic(fmt.Sprintf("infer trigger_poll input schema: %v", err))
	}

	inputSchema.Properties["configs"].MaxItems = jsonschema.Ptr(maxTriggerPollConfigs)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "trigger_poll",
		Description: "Trigger one or more poll configurations immediately. Prefer wait=false and poll get_deployment_run - long wait=true calls are cancelled if the server shuts down (10s grace).",
		Annotations: &mcp.ToolAnnotations{
			DestructiveHint: &destructive,
			IdempotentHint:  false,
			OpenWorldHint:   &closedWorld,
		},
		InputSchema: inputSchema,
	}, instrumentMCPTool(h.log, "trigger_poll", h.triggerPollTool))
}

func (h *handlerData) triggerPollTool(ctx context.Context, _ *mcp.CallToolRequest, input triggerPollInput) (*mcp.CallToolResult, triggerPollOutput, error) {
	wait := true
	if input.Wait != nil {
		wait = *input.Wait
	}

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

	status := deploymentRunStatusAccepted
	if wait {
		status = deploymentRunStatusSucceeded
	}

	output := triggerPollOutput{JobID: jobID, Status: string(status)}
	if err != nil {
		output.Status = string(deploymentRunStatusFailed)

		return &mcp.CallToolResult{ //nolint:nilerr // Operational errors are returned as structured MCP tool errors with the deployment job ID.
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
		}, output, nil
	}

	return nil, output, nil
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

	if h.runTracker != nil {
		h.runTracker.TrackAccepted(jobID, deploymentRunTriggerPoll)

		if len(configs) > 0 {
			repository := "multiple"
			if len(configs) == 1 {
				repository = pollRepositoryName(configs[0])
			}

			h.runTracker.SetMetadata(jobID, repository, "", "")
		}
	}

	run := func(runCtx context.Context) error {
		if h.runTracker != nil {
			h.runTracker.MarkRunning(jobID)
		}

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
							Revision:                 notification.GetRevision(cfg.Reference, ""),
							JobID:                    jobID,
							DeploymentTargetObserver: h.deploymentTargetObserver(jobID),
						}
						errs <- runner(runCtx, cfg, h.appConfig, h.dataMountPoint, h.dockerCli, h.contexts, h.log.Logger, metadata, h.secretProvider, pollTriggerDefault)
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

		for runErr := range errs {
			if runErr != nil {
				failedRuns++
			}
		}

		if failedRuns > 0 {
			err := &pollRunsFailedError{Failed: failedRuns, Total: len(configs)}
			if h.runTracker != nil {
				h.runTracker.MarkFailed(jobID, err.Error())
			}

			return err
		}

		if h.runTracker != nil {
			h.runTracker.MarkSucceeded(jobID, "poll jobs complete")
		}

		return nil
	}

	if wait {
		err := h.runSynchronous(ctx, run)
		if err != nil && h.runTracker != nil {
			h.runTracker.MarkFailed(jobID, err.Error())
		}

		return jobID, err
	}

	if err := h.runBackground(ctx, func(backgroundCtx context.Context) {
		_ = run(backgroundCtx)
	}); err != nil {
		if h.runTracker != nil {
			h.runTracker.MarkFailed(jobID, err.Error())
		}

		return jobID, err
	}

	return jobID, nil
}

func pollRepositoryName(cfg poll.Config) string {
	sourceType := config.NormalizeSourceType(cfg.Source)
	if sourceType == config.SourceTypeOCI {
		return oci.RepositoryNameFromArtifact(cfg.SourceUrl)
	}

	return git.GetRepoName(cfg.SourceUrl)
}
