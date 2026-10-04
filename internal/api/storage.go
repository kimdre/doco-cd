package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/kimdre/doco-cd/internal/common/id"
	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/restapi"
)

// CompactMirrorsHandler starts a compaction of the Git mirrors in the data
// directory. With wait it responds with the finished run.
func (h *Handler) CompactMirrorsHandler(w http.ResponseWriter, r *http.Request) {
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

	wait, ok := getBoolQueryParam(r, w, jobLog, jobID, "wait", false)
	if !ok {
		return
	}

	query := r.URL.Query()
	req := controlplane.MirrorCompactionRequest{
		Repository: query.Get("repository"),
		Mode:       git.MirrorCompactionMode(query.Get("mode")),
	}

	if value := query.Get("max_size"); value != "" {
		maxSize, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			errMsg := "'max_size' parameter must be an integer"
			jobLog.Error(errMsg, logger.ErrAttr(err))
			restapi.JSONError(w, "invalid parameter: max_size", errMsg, jobID, http.StatusBadRequest)

			return
		}

		req.MaxSizeBytes = &maxSize
	}

	jobID, err := h.controlPlaneRuns.TriggerMirrorCompaction(r.Context(), jobID, req, wait)
	if err != nil {
		switch {
		case errors.Is(err, controlplane.ErrInvalidMirrorCompactionRequest):
			restapi.JSONError(w, err.Error(), "", jobID, http.StatusBadRequest)
		case errors.Is(err, controlplane.ErrNoMirrors):
			restapi.JSONError(w, err.Error(), "", jobID, http.StatusNotFound)
		case isMirrorCompactionActive(err):
			restapi.JSONError(w, err.Error(), "", jobID, http.StatusConflict)
		case controlplane.IsLifecycleCancellation(err):
			restapi.JSONError(w, err.Error(), "", jobID, http.StatusServiceUnavailable)
		default:
			// A finished run carries the summary of what was compacted.
			detail := ""
			if run, ok := h.controlPlaneRuns.Get(jobID); ok {
				detail = run.Message
			}

			restapi.JSONError(w, err.Error(), detail, jobID, http.StatusInternalServerError)
		}

		return
	}

	if !wait {
		restapi.JSONResponse(w, "mirror compaction started", jobID, http.StatusAccepted)

		return
	}

	// The run was just finished, so the tracker still holds it.
	run, _ := h.controlPlaneRuns.Get(jobID)
	restapi.JSONResponse(w, run, jobID, http.StatusOK)
}

func isMirrorCompactionActive(err error) bool {
	_, ok := errors.AsType[*controlplane.MirrorCompactionActiveError](err)

	return ok
}
