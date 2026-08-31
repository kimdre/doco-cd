package main

import (
	"net/http"

	"github.com/kimdre/doco-cd/internal/common/id"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
)

// HealthCheckHandler handles health check requests.
func (h *handlerData) HealthCheckHandler(w http.ResponseWriter, _ *http.Request) {
	var (
		err     error
		errType error
	)

	jobID := id.New()

	metadata := notification.Metadata{
		JobID:      jobID,
		Repository: "healthcheck",
		Stack:      "",
		Revision:   "",
	}

	err, errType = docker.VerifyDockerAPIAccess() //nolint:contextcheck // REST health checks must not propagate caller cancellation to notifications.
	if err != nil {
		onError(w, h.log.With(logger.ErrAttr(err)), errType.Error(), err.Error(), http.StatusServiceUnavailable, metadata, err)

		return
	}

	JSONResponse(w, "healthy", jobID, http.StatusOK)
}
