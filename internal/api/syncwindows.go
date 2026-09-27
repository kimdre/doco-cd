package api

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/kimdre/doco-cd/internal/common/id"
	"github.com/kimdre/doco-cd/internal/restapi"
	"github.com/kimdre/doco-cd/internal/syncwindow"
)

// GetSyncWindowsHandler reports the configured sync windows and, if the
// request names a target, whether a deployment of it is allowed now.
func (h *Handler) GetSyncWindowsHandler(w http.ResponseWriter, r *http.Request) {
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

	query := r.URL.Query()

	origin := syncwindow.Origin(strings.ToLower(strings.TrimSpace(query.Get("origin"))))
	switch origin {
	case "":
		origin = syncwindow.OriginAutomatic
	case syncwindow.OriginAutomatic, syncwindow.OriginManual:
	default:
		restapi.JSONError(w, "invalid origin", "origin must be automatic or manual", jobID, http.StatusBadRequest)

		return
	}

	target := syncWindowQueryTarget(query.Get("repository"), query.Get("deployment"), query.Get("context"))

	restapi.JSONResponse(w, h.appConfig.SyncWindows.Report(time.Now(), target, origin), jobID, http.StatusOK)
}

// syncWindowQueryTarget returns the target named by a request, or nil if the
// request names none.
func syncWindowQueryTarget(repository, deployment, contextName string) *syncwindow.Target {
	target := syncwindow.Target{
		Repository: strings.TrimSpace(repository),
		Deployment: strings.TrimSpace(deployment),
		Context:    strings.TrimSpace(contextName),
	}

	if target == (syncwindow.Target{}) {
		return nil
	}

	return &target
}
