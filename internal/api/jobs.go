package api

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/kimdre/doco-cd/internal/common/id"
	"github.com/kimdre/doco-cd/internal/controlplane"

	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/restapi"
	"github.com/kimdre/doco-cd/internal/scheduler"
)

// GetScheduledJobsHandler lists scheduled jobs for an optional context and stack.
func (h *Handler) GetScheduledJobsHandler(w http.ResponseWriter, r *http.Request) {
	jobID := id.New()
	jobLog := h.log.With(slog.String("job_id", jobID), slog.String("ip", h.requestIP(r)))

	jobLog.Debug("received api request")

	if !requireMethod(w, jobLog, r, http.MethodGet) {
		return
	}

	if !restapi.ValidateApiKey(r, h.appConfig.ApiSecret) {
		jobLog.Error(restapi.ErrInvalidApiKey.Error())
		restapi.JSONError(w, restapi.ErrInvalidApiKey.Error(), "", jobID, http.StatusUnauthorized)

		return
	}

	_, contextName, ok := h.dockerCliForRequest(w, r, jobLog, jobID)
	if !ok {
		return
	}

	jobLog = jobLog.With(slog.String("context", contextName))
	stackName := r.URL.Query().Get("stack")

	jobs, err := h.controlPlaneRuns.ListScheduledJobs(r.Context(), contextName, stackName)
	if err != nil {
		errMsg := "failed to list scheduled jobs"
		jobLog.With(logger.ErrAttr(err)).Error(errMsg)
		restapi.JSONError(w, errMsg, err.Error(), jobID, http.StatusInternalServerError)

		return
	}

	restapi.JSONResponse(w, jobs, jobID, http.StatusOK)
}

// TriggerScheduledJobHandler starts a scheduled job synchronously or asynchronously.
func (h *Handler) TriggerScheduledJobHandler(w http.ResponseWriter, r *http.Request) {
	jobID := id.New()
	jobLog := h.log.With(slog.String("job_id", jobID), slog.String("ip", h.requestIP(r)))

	jobLog.Debug("received api request")

	if !requireMethod(w, jobLog, r, http.MethodPost) {
		return
	}

	if !restapi.ValidateApiKey(r, h.appConfig.ApiSecret) {
		jobLog.Error(restapi.ErrInvalidApiKey.Error())
		restapi.JSONError(w, restapi.ErrInvalidApiKey.Error(), "", jobID, http.StatusUnauthorized)

		return
	}

	jobName := r.PathValue("jobName")
	if jobName == "" {
		err := errors.New("missing job name")
		jobLog.Error(err.Error())
		restapi.JSONError(w, err, "", jobID, http.StatusBadRequest)

		return
	}

	_, contextName, ok := h.dockerCliForRequest(w, r, jobLog, jobID)
	if !ok {
		return
	}

	jobLog = jobLog.With(slog.String("context", contextName))
	stackName := r.URL.Query().Get("stack")

	wait, ok := getBoolQueryParam(r, w, jobLog, jobID, "wait", true)
	if !ok {
		return
	}

	_, err := h.controlPlaneRuns.TriggerScheduledJob(r.Context(), jobID, contextName, jobName, stackName, wait)
	if err != nil {
		switch {
		case controlplane.IsLifecycleCancellation(err):
			restapi.JSONError(w, err.Error(), "", jobID, http.StatusServiceUnavailable)
		case errors.Is(err, scheduler.ErrScheduledJobNotFound):
			restapi.JSONError(w, err.Error(), "", jobID, http.StatusNotFound)
		case errors.Is(err, scheduler.ErrScheduledJobDisabled), errors.Is(err, scheduler.ErrScheduledJobAmbiguous):
			restapi.JSONError(w, err.Error(), "", jobID, http.StatusConflict)
		default:
			restapi.JSONError(w, "failed to trigger scheduled job run", err.Error(), jobID, http.StatusInternalServerError)
		}

		return
	}

	if !wait {
		restapi.JSONResponse(w, "scheduled job trigger accepted", jobID, http.StatusAccepted)

		return
	}

	restapi.JSONResponse(w, "scheduled job triggered", jobID, http.StatusOK)
}
