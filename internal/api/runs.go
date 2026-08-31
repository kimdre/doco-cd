package api

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/kimdre/doco-cd/internal/common/id"
	"github.com/kimdre/doco-cd/internal/controlplane"

	"github.com/kimdre/doco-cd/internal/restapi"
)

// GetDeploymentRunsHandler lists tracked deployment runs using optional filters.
func (h *Handler) GetDeploymentRunsHandler(w http.ResponseWriter, r *http.Request) {
	jobID := id.New()
	jobLog := h.log.With(slog.String("job_id", jobID), slog.String("ip", h.requestIP(r)))

	if !requireMethod(w, jobLog, r, http.MethodGet) {
		return
	}

	if !restapi.ValidateApiKey(r, h.appConfig.ApiSecret) {
		jobLog.Error(restapi.ErrInvalidApiKey.Error())
		restapi.JSONError(w, restapi.ErrInvalidApiKey.Error(), "", jobID, http.StatusUnauthorized)

		return
	}

	limit := 50

	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			restapi.JSONError(w, "invalid parameter: limit", "'limit' parameter must be a positive integer", jobID, http.StatusBadRequest)
			return
		}

		limit = min(n, 200)
	}

	status, err := controlplane.NormalizeRunStatus(r.URL.Query().Get("status"))
	if err != nil {
		restapi.JSONError(w, err.Error(), "valid status values: accepted, running, succeeded, failed, skipped", jobID, http.StatusBadRequest)
		return
	}

	trigger, err := controlplane.NormalizeRunTrigger(r.URL.Query().Get("trigger"))
	if err != nil {
		restapi.JSONError(w, err.Error(), "valid trigger values: webhook, poll, scheduled_job", jobID, http.StatusBadRequest)
		return
	}

	runs := h.controlPlaneRuns.List(limit, trigger, status)
	restapi.JSONResponse(w, runs, jobID, http.StatusOK)
}

// GetDeploymentRunHandler returns one tracked deployment run by job ID.
func (h *Handler) GetDeploymentRunHandler(w http.ResponseWriter, r *http.Request) {
	jobID := id.New()
	jobLog := h.log.With(slog.String("job_id", jobID), slog.String("ip", h.requestIP(r)))

	if !requireMethod(w, jobLog, r, http.MethodGet) {
		return
	}

	if !restapi.ValidateApiKey(r, h.appConfig.ApiSecret) {
		jobLog.Error(restapi.ErrInvalidApiKey.Error())
		restapi.JSONError(w, restapi.ErrInvalidApiKey.Error(), "", jobID, http.StatusUnauthorized)

		return
	}

	requestedJobID := strings.TrimSpace(r.PathValue("jobID"))
	if requestedJobID == "" {
		restapi.JSONError(w, "missing job id", "", jobID, http.StatusBadRequest)
		return
	}

	run, ok := h.controlPlaneRuns.Get(requestedJobID)
	if !ok {
		restapi.JSONError(w, "run not found: "+requestedJobID, "", jobID, http.StatusNotFound)
		return
	}

	restapi.JSONResponse(w, run, jobID, http.StatusOK)
}
