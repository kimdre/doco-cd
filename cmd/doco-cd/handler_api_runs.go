package main

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/kimdre/doco-cd/internal/common/id"

	restAPI "github.com/kimdre/doco-cd/internal/restapi"
)

// GetDeploymentRunsHandler returns recent deployment runs tracked by doco-cd.
func (h *handlerData) GetDeploymentRunsHandler(w http.ResponseWriter, r *http.Request) {
	jobID := id.New()
	jobLog := h.log.With(slog.String("job_id", jobID), slog.String("ip", h.requestIP(r)))

	if !requireMethod(w, jobLog, r, http.MethodGet) {
		return
	}

	if !restAPI.ValidateApiKey(r, h.appConfig.ApiSecret) {
		jobLog.Error(restAPI.ErrInvalidApiKey.Error())
		JSONError(w, restAPI.ErrInvalidApiKey.Error(), "", jobID, http.StatusUnauthorized)

		return
	}

	limit := 50

	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			JSONError(w, "invalid parameter: limit", "'limit' parameter must be a positive integer", jobID, http.StatusBadRequest)
			return
		}

		limit = min(n, 200)
	}

	status, err := normalizeDeploymentRunStatus(r.URL.Query().Get("status"))
	if err != nil {
		JSONError(w, err.Error(), "valid status values: accepted, running, succeeded, failed, skipped", jobID, http.StatusBadRequest)
		return
	}

	trigger, err := normalizeDeploymentRunTrigger(r.URL.Query().Get("trigger"))
	if err != nil {
		JSONError(w, err.Error(), "valid trigger values: webhook, poll, scheduled_job", jobID, http.StatusBadRequest)
		return
	}

	runs := h.runTracker.List(limit, trigger, status)
	JSONResponse(w, runs, jobID, http.StatusOK)
}

// GetDeploymentRunHandler returns details for one deployment run identified by jobID.
func (h *handlerData) GetDeploymentRunHandler(w http.ResponseWriter, r *http.Request) {
	jobID := id.New()
	jobLog := h.log.With(slog.String("job_id", jobID), slog.String("ip", h.requestIP(r)))

	if !requireMethod(w, jobLog, r, http.MethodGet) {
		return
	}

	if !restAPI.ValidateApiKey(r, h.appConfig.ApiSecret) {
		jobLog.Error(restAPI.ErrInvalidApiKey.Error())
		JSONError(w, restAPI.ErrInvalidApiKey.Error(), "", jobID, http.StatusUnauthorized)

		return
	}

	requestedJobID := strings.TrimSpace(r.PathValue("jobID"))
	if requestedJobID == "" {
		JSONError(w, "missing job id", "", jobID, http.StatusBadRequest)
		return
	}

	run, ok := h.runTracker.Get(requestedJobID)
	if !ok {
		JSONError(w, "run not found: "+requestedJobID, "", jobID, http.StatusNotFound)
		return
	}

	JSONResponse(w, run, jobID, http.StatusOK)
}
