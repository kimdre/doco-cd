package api

import (
	"log/slog"
	"net/http"

	"github.com/kimdre/doco-cd/internal/restapi"
)

type apiOperation string

const (
	opListDeploymentRuns   apiOperation = "listDeploymentRuns"
	opGetDeploymentRun     apiOperation = "getDeploymentRun"
	opListScheduledJobs    apiOperation = "listScheduledJobs"
	opTriggerScheduledJob  apiOperation = "triggerScheduledJob"
	opListComposeProjects  apiOperation = "listComposeProjects"
	opGetComposeProject    apiOperation = "getComposeProject"
	opDeleteComposeProject apiOperation = "deleteComposeProject"
	opRunComposeProject    apiOperation = "runComposeProjectAction"
	opListSwarmStacks      apiOperation = "listSwarmStacks"
	opGetSwarmStack        apiOperation = "getSwarmStack"
	opDeleteSwarmStack     apiOperation = "deleteSwarmStack"
	opRunSwarmStack        apiOperation = "runSwarmStackAction"
	opTriggerPoll          apiOperation = "triggerPoll"
	opCompactMirrors       apiOperation = "compactMirrors"
	opListSyncWindows      apiOperation = "listSyncWindows"
)

func permissionForOperation(operation apiOperation) restapi.Permission {
	switch operation {
	case opListDeploymentRuns, opGetDeploymentRun:
		return restapi.PermissionRunsRead
	case opListScheduledJobs:
		return restapi.PermissionJobsRead
	case opListSyncWindows:
		return restapi.PermissionSyncWindowsRead
	case opListComposeProjects, opGetComposeProject:
		return restapi.PermissionProjectsInspect
	case opListSwarmStacks, opGetSwarmStack:
		return restapi.PermissionStacksInspect
	case opTriggerScheduledJob:
		return restapi.PermissionJobsTrigger
	case opRunComposeProject:
		return restapi.PermissionProjectsControl
	case opRunSwarmStack:
		return restapi.PermissionStacksControl
	case opTriggerPoll:
		return restapi.PermissionPollTrigger
	case opCompactMirrors:
		return restapi.PermissionMirrorsCompact
	case opDeleteComposeProject:
		return restapi.PermissionProjectsDelete
	case opDeleteSwarmStack:
		return restapi.PermissionStacksDelete
	default:
		return ""
	}
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, log *slog.Logger, jobID string, operation apiOperation) bool {
	required := permissionForOperation(operation)
	if required == "" {
		log.Error("unclassified API operation", slog.String("operation", string(operation)))
		restapi.JSONError(w, "unclassified API operation", "", jobID, http.StatusInternalServerError)

		return false
	}

	role, ok := h.authenticate(w, r, log, jobID)
	if !ok {
		return false
	}

	if !role.Allows(required) {
		const message = "insufficient API key permissions"

		log.Warn(message, slog.String("role", string(role)), slog.String("required_permission", string(required)))
		restapi.JSONError(w, message, "", jobID, http.StatusForbidden)

		return false
	}

	return true
}

func (h *Handler) authenticate(w http.ResponseWriter, r *http.Request, log *slog.Logger, jobID string) (restapi.Role, bool) {
	role, ok := h.appConfig.APIKeys.Authenticate(r)
	if !ok {
		log.Error(restapi.ErrInvalidApiKey.Error())
		restapi.JSONError(w, restapi.ErrInvalidApiKey.Error(), "", jobID, http.StatusUnauthorized)

		return "", false
	}

	return role, true
}
