package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/docker/cli/cli/command"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/restapi"
)

const dockerContextHeader = "X-Doco-CD-Context"

func (h *Handler) requestIP(r *http.Request) string {
	if h == nil || h.appConfig == nil {
		return r.RemoteAddr
	}

	return restapi.ResolveRequestIP(
		r.RemoteAddr,
		strings.TrimSpace(h.appConfig.TrustedProxyHeader),
		r.Header,
		h.appConfig.TrustedProxyNetworks,
	)
}

func (h *Handler) dockerCliForRequest(
	w http.ResponseWriter,
	r *http.Request,
	jobLog *slog.Logger,
	jobID string,
) (command.Cli, string, bool) {
	values, present := r.URL.Query()["context"]
	if len(values) > 1 {
		restapi.JSONError(w, "invalid parameter: context", "'context' parameter must be specified at most once", jobID, http.StatusBadRequest)
		return nil, "", false
	}

	contextName := ""
	if present && len(values) == 1 {
		contextName = docker.NormalizeContextName(values[0])
	}

	displayName := docker.DisplayContextName(contextName)
	w.Header().Set(dockerContextHeader, displayName)

	if h.contexts == nil {
		if contextName != "" {
			restapi.JSONError(w, "unknown docker context: "+displayName, "", jobID, http.StatusBadRequest)
			return nil, "", false
		}

		return h.dockerCli, displayName, true
	}

	contextClient, err := h.contexts.Get(r.Context(), contextName)
	if err != nil {
		status := http.StatusInternalServerError
		errMsg := "failed to resolve docker context: " + displayName

		if errors.Is(err, docker.ErrDockerContextNotFound) {
			status = http.StatusBadRequest
			errMsg = "unknown docker context: " + displayName
		}

		jobLog.Error(errMsg, logger.ErrAttr(err))
		restapi.JSONError(w, errMsg, err.Error(), jobID, status)

		return nil, "", false
	}

	return contextClient.Cli, contextClient.DisplayName(), true
}

func getIntQueryParam(r *http.Request, w http.ResponseWriter, log *slog.Logger, jobID, key string, defaultValue int) (int, bool) {
	queryParam := r.URL.Query().Get(key)
	if queryParam == "" {
		return defaultValue, true
	}

	value, err := strconv.Atoi(queryParam)
	if err != nil {
		err = errors.New("invalid parameter: " + key)
		errMsg := "'" + key + "' parameter must be an integer"
		log.With(logger.ErrAttr(err)).Error(errMsg)
		restapi.JSONError(w, err, errMsg, jobID, http.StatusBadRequest)

		return 0, false
	}

	return value, true
}

func getBoolQueryParam(r *http.Request, w http.ResponseWriter, log *slog.Logger, jobID, key string, defaultValue bool) (bool, bool) {
	queryParam := r.URL.Query().Get(key)
	if queryParam == "" {
		return defaultValue, true
	}

	value, err := strconv.ParseBool(queryParam)
	if err != nil {
		err = errors.New("invalid parameter: " + key)
		errMsg := "'" + key + "' parameter must be true or false"
		log.With(logger.ErrAttr(err)).Error(errMsg)
		restapi.JSONError(w, err, errMsg, jobID, http.StatusBadRequest)

		return false, false
	}

	return value, true
}

func requireMethod(w http.ResponseWriter, log *slog.Logger, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}

	err := restapi.ErrInvalidHTTPMethod
	log.Error(err.Error())
	restapi.JSONError(w, err.Error(), "requires method: "+method, "", http.StatusMethodNotAllowed)

	return false
}
