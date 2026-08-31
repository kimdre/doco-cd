package api

import (
	"net/http"

	"github.com/kimdre/doco-cd/internal/common/id"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/restapi"
)

// HealthCheckHandler handles health check requests.
func (h *Handler) HealthCheckHandler(w http.ResponseWriter, _ *http.Request) {
	var (
		err     error
		errType error
	)

	jobID := id.New()

	err, errType = docker.VerifyDockerAPIAccess() //nolint:contextcheck // REST health checks must not propagate caller cancellation to notifications.
	if err != nil {
		h.healthFailureReporter(w, h.log.With(logger.ErrAttr(err)), jobID, errType, err)

		return
	}

	restapi.JSONResponse(w, "healthy", jobID, http.StatusOK)
}
