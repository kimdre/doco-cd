package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/kimdre/doco-cd/internal/common/id"
	"github.com/kimdre/doco-cd/internal/config/poll"

	"github.com/kimdre/doco-cd/internal/logger"
	restAPI "github.com/kimdre/doco-cd/internal/restapi"
)

// TriggerPollHandler handles API requests to trigger a poll of the configured repositories.
// This can be used to manually trigger a poll outside the planned intervals,
// for example after a failed deployment or to check for new commits after a network outage.
func (h *handlerData) TriggerPollHandler(w http.ResponseWriter, r *http.Request) {
	// Add a job id to the context to track deployments in the logs
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
	defer func() {
		_ = r.Body.Close()
	}()

	wait, ok := getBoolQueryParam(r, w, jobLog, jobID, "wait", true)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, h.appConfig.MaxPayloadSize)

	decoder := json.NewDecoder(r.Body)

	var pollConfigs []poll.Config
	if err := decoder.Decode(&pollConfigs); err != nil {
		h.pollDecodeError(w, jobID, err)

		return
	}

	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("request body must contain a single JSON value")
		}

		h.pollDecodeError(w, jobID, err)

		return
	}

	jobID, err := h.controlPlaneRuns.TriggerPoll(r.Context(), pollConfigs, wait, jobLog)
	if err != nil {
		if isLifecycleCancellation(err) {
			JSONError(w, err.Error(), "", jobID, http.StatusServiceUnavailable)

			return
		}

		if _, ok := errors.AsType[*pollRunsFailedError](err); ok && wait {
			JSONError(w, err.Error(), "", jobID, http.StatusInternalServerError)

			return
		}

		errMsg := err.Error()
		detail := ""

		if validationErr, ok := errors.AsType[*pollConfigValidationError](err); ok {
			errMsg = "invalid poll configuration at index " + strconv.Itoa(validationErr.Index)
			detail = validationErr.Err.Error()
		}

		jobLog.Error(errMsg, logger.ErrAttr(err))
		JSONError(w, errMsg, detail, jobID, http.StatusBadRequest)

		return
	}

	if wait {
		JSONResponse(w, "poll jobs complete", jobID, http.StatusOK)

		return
	}

	JSONResponse(w, "poll jobs started", jobID, http.StatusAccepted)
}

func (h *handlerData) pollDecodeError(w http.ResponseWriter, jobID string, err error) {
	errMsg := "failed to decode json in body"
	h.log.Error(errMsg, logger.ErrAttr(err))

	status := http.StatusBadRequest
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		status = http.StatusRequestEntityTooLarge
	}

	JSONError(w, errMsg, err.Error(), jobID, status)
}
