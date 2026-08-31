package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/docker/cli/cli/command"

	"github.com/kimdre/doco-cd/internal/common/id"

	"github.com/kimdre/doco-cd/internal/logger"
	restAPI "github.com/kimdre/doco-cd/internal/restapi"
	"github.com/kimdre/doco-cd/internal/scheduler"
	"github.com/kimdre/doco-cd/internal/secretprovider"
)

// GetScheduledJobsHandler handles API requests to list scheduler-managed jobs.
func (h *handlerData) GetScheduledJobsHandler(w http.ResponseWriter, r *http.Request) {
	jobID := id.New()
	jobLog := h.log.With(slog.String("job_id", jobID), slog.String("ip", h.requestIP(r)))

	jobLog.Debug("received api request")

	if !requireMethod(w, jobLog, r, http.MethodGet) {
		return
	}

	if !restAPI.ValidateApiKey(r, h.appConfig.ApiSecret) {
		jobLog.Error(restAPI.ErrInvalidApiKey.Error())
		JSONError(w, restAPI.ErrInvalidApiKey.Error(), "", jobID, http.StatusUnauthorized)

		return
	}

	dockerCli, contextName, ok := h.dockerCliForRequest(w, r, jobLog, jobID)
	if !ok {
		return
	}

	jobLog = jobLog.With(slog.String("context", contextName))
	stackName := r.URL.Query().Get("stack")

	var (
		jobs []scheduler.JobInfo
		err  error
	)
	if h.scheduler != nil {
		jobs, err = h.scheduler.ListJobs(r.Context(), contextName, stackName)
	} else {
		jobs, err = scheduler.ListJobs(r.Context(), dockerCli, stackName)
	}

	if err != nil {
		errMsg := "failed to list scheduled jobs"
		jobLog.With(logger.ErrAttr(err)).Error(errMsg)
		JSONError(w, errMsg, err.Error(), jobID, http.StatusInternalServerError)

		return
	}

	JSONResponse(w, jobs, jobID, http.StatusOK)
}

// TriggerScheduledJobHandler handles API requests to run one configured scheduled job immediately.
func (h *handlerData) TriggerScheduledJobHandler(w http.ResponseWriter, r *http.Request) {
	jobID := id.New()
	jobLog := h.log.With(slog.String("job_id", jobID), slog.String("ip", h.requestIP(r)))

	jobLog.Debug("received api request")

	if !requireMethod(w, jobLog, r, http.MethodPost) {
		return
	}

	if !restAPI.ValidateApiKey(r, h.appConfig.ApiSecret) {
		jobLog.Error(restAPI.ErrInvalidApiKey.Error())
		JSONError(w, restAPI.ErrInvalidApiKey.Error(), "", jobID, http.StatusUnauthorized)

		return
	}

	jobName := r.PathValue("jobName")
	if jobName == "" {
		err := errors.New("missing job name")
		jobLog.Error(err.Error())
		JSONError(w, err, "", jobID, http.StatusBadRequest)

		return
	}

	dockerCli, contextName, ok := h.dockerCliForRequest(w, r, jobLog, jobID)
	if !ok {
		return
	}

	jobLog = jobLog.With(slog.String("context", contextName))
	stackName := r.URL.Query().Get("stack")

	wait, ok := getBoolQueryParam(r, w, jobLog, jobID, "wait", true)
	if !ok {
		return
	}

	_, err := h.triggerScheduledJobRun(r.Context(), jobID, dockerCli, contextName, jobName, stackName, wait)
	if err != nil {
		switch {
		case isLifecycleCancellation(err):
			JSONError(w, err.Error(), "", jobID, http.StatusServiceUnavailable)
		case errors.Is(err, scheduler.ErrScheduledJobNotFound):
			JSONError(w, err.Error(), "", jobID, http.StatusNotFound)
		case errors.Is(err, scheduler.ErrScheduledJobDisabled), errors.Is(err, scheduler.ErrScheduledJobAmbiguous):
			JSONError(w, err.Error(), "", jobID, http.StatusConflict)
		default:
			JSONError(w, "failed to trigger scheduled job run", err.Error(), jobID, http.StatusInternalServerError)
		}

		return
	}

	if !wait {
		JSONResponse(w, "scheduled job trigger accepted", jobID, http.StatusAccepted)

		return
	}

	JSONResponse(w, "scheduled job triggered", jobID, http.StatusOK)
}

type scheduledJobTrigger func(context.Context, command.Cli, *slog.Logger, string, string, *secretprovider.SecretProvider) (string, error)

var errScheduledJobRunPanicked = errors.New("scheduled job run panicked")

func (h *handlerData) triggerScheduledJobRun(ctx context.Context, jobID string, dockerCli command.Cli, contextName, jobName, stackName string, wait bool) (string, error) {
	if jobID == "" {
		jobID = id.New()
	}

	jobLog := h.log.With(slog.String("job_id", jobID), slog.String("context", contextName))
	h.runTracker.TrackAccepted(jobID, deploymentRunTriggerScheduledJob)
	h.runTracker.SetMetadata(jobID, "scheduled:"+jobName, stackName, "")
	h.runTracker.AddDeployment(jobID, stackName, contextName)

	run := func(ctx context.Context) (err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				logRecoveredPanic(jobLog, "scheduled job run", recovered)
				h.runTracker.MarkFailed(jobID, errScheduledJobRunPanicked.Error())

				err = errScheduledJobRunPanicked
			}
		}()

		h.runTracker.MarkRunning(jobID)

		jobLog.Info("scheduled job run triggered", slog.String("job", jobName), slog.String("stack", stackName))

		var scheduledRunID string

		switch {
		case h.triggerScheduledJob != nil:
			scheduledRunID, err = h.triggerScheduledJob(ctx, dockerCli, h.log.Logger, jobName, stackName, h.secretProvider)
		case h.scheduler != nil:
			scheduledRunID, err = h.scheduler.TriggerNow(ctx, contextName, jobName, stackName, h.secretProvider)
		default:
			scheduledRunID, err = scheduler.TriggerNow(ctx, dockerCli, h.log.Logger, jobName, stackName, h.secretProvider)
		}

		runLog := jobLog
		if scheduledRunID != "" {
			runLog = runLog.With(slog.String("scheduled_run_id", scheduledRunID))
		}

		if err != nil {
			runLog.With(logger.ErrAttr(err)).Error("failed to trigger scheduled job run", slog.String("job", jobName), slog.String("stack", stackName))
			h.runTracker.MarkFailed(jobID, err.Error())

			return err
		}

		h.runTracker.MarkSucceeded(jobID, "scheduled job trigger completed")

		return nil
	}

	var err error
	if wait {
		err = h.runSynchronous(ctx, run)
	} else {
		err = h.runBackground(ctx, func(ctx context.Context) {
			// The run outcome is already recorded in runTracker by run itself.
			_ = run(ctx)
		})
	}

	if errors.Is(err, errBackgroundWorkClosed) {
		h.runTracker.MarkFailed(jobID, err.Error())
	}

	return jobID, err
}
