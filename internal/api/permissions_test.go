package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/restapi"
)

func testAPIKeys(key string) restapi.Keyring {
	return restapi.Keyring{{Key: key, Role: restapi.RoleFull}}
}

func TestRESTOperationPermissions(t *testing.T) {
	t.Parallel()

	keys := restapi.Keyring{
		{Key: "reader", Role: restapi.RoleRead},
		{Key: "writer", Role: restapi.RoleWrite},
		{Key: "admin", Role: restapi.RoleFull},
	}
	h := &Handler{
		appConfig: &app.Config{APIKeys: keys, MaxPayloadSize: 1024},
		log:       logger.New(logger.LevelCritical),
	}

	operations := []struct {
		name     string
		method   string
		target   string
		required restapi.Role
		handler  http.HandlerFunc
	}{
		{"list runs", http.MethodGet, "/runs?limit=0", restapi.RoleRead, h.GetDeploymentRunsHandler},
		{"get run", http.MethodGet, "/run/", restapi.RoleRead, h.GetDeploymentRunHandler},
		{"list jobs", http.MethodGet, "/jobs?context=unknown", restapi.RoleRead, h.GetScheduledJobsHandler},
		{"list projects", http.MethodGet, "/projects?context=unknown", restapi.RoleRead, h.GetProjectsApiHandler},
		{"get project", http.MethodGet, "/project/", restapi.RoleRead, h.ProjectApiHandler},
		{"list stacks", http.MethodGet, "/stacks?context=unknown", restapi.RoleRead, h.GetStacksApiHandler},
		{"get stack", http.MethodGet, "/stack/", restapi.RoleRead, h.StackApiHandler},
		{"list sync windows", http.MethodGet, "/sync-windows?origin=invalid", restapi.RoleRead, h.GetSyncWindowsHandler},
		{"trigger job", http.MethodPost, "/job/run", restapi.RoleWrite, h.TriggerScheduledJobHandler},
		{"control project", http.MethodPost, "/project/start", restapi.RoleWrite, h.ProjectActionApiHandler},
		{"control stack", http.MethodPost, "/stack/restart", restapi.RoleWrite, h.StackActionApiHandler},
		{"trigger poll", http.MethodPost, "/poll/run", restapi.RoleWrite, h.TriggerPollHandler},
		{"compact mirrors", http.MethodPost, "/storage/compact?max_size=invalid", restapi.RoleWrite, h.CompactMirrorsHandler},
		{"delete project", http.MethodDelete, "/project/", restapi.RoleFull, h.ProjectApiHandler},
		{"delete stack", http.MethodDelete, "/stack/", restapi.RoleFull, h.StackApiHandler},
	}

	for _, operation := range operations {
		for _, credential := range []struct {
			name string
			key  string
			role restapi.Role
		}{
			{"missing", "", ""},
			{"invalid", "invalid", ""},
			{"read", "reader", restapi.RoleRead},
			{"write", "writer", restapi.RoleWrite},
			{"full", "admin", restapi.RoleFull},
		} {
			t.Run(operation.name+"/"+credential.name, func(t *testing.T) {
				t.Parallel()

				request := httptest.NewRequest(operation.method, operation.target, strings.NewReader("{"))
				request.Header.Set(restapi.KeyHeader, credential.key)

				response := httptest.NewRecorder()
				operation.handler(response, request)

				want := http.StatusUnauthorized
				if credential.role != "" {
					want = http.StatusForbidden
					if credential.role.Allows(operation.required) {
						want = http.StatusBadRequest
					}
				}

				if response.Code != want {
					t.Fatalf("status = %d, want %d; body = %s", response.Code, want, response.Body.String())
				}
			})
		}
	}
}

func TestRESTWriteAccessRunsMaintenance(t *testing.T) {
	t.Parallel()

	calls := 0
	runs := &fakeCompactionRuns{
		trigger: func(context.Context, string, controlplane.MirrorCompactionRequest, bool) (string, error) {
			calls++

			return "test-job", nil
		},
	}
	h := &Handler{
		appConfig: &app.Config{APIKeys: restapi.Keyring{
			{Key: "reader", Role: restapi.RoleRead},
			{Key: "writer", Role: restapi.RoleWrite},
			{Key: "admin", Role: restapi.RoleFull},
		}},
		log:              logger.New(logger.LevelCritical),
		controlPlaneRuns: runs,
	}

	for _, testCase := range []struct {
		key       string
		wantCode  int
		wantCalls int
	}{
		{"reader", http.StatusForbidden, 0},
		{"writer", http.StatusAccepted, 1},
		{"admin", http.StatusAccepted, 2},
	} {
		request := httptest.NewRequest(http.MethodPost, "/v1/api/storage/compact", nil)
		request.Header.Set(restapi.KeyHeader, testCase.key)

		response := httptest.NewRecorder()
		h.CompactMirrorsHandler(response, request)

		if response.Code != testCase.wantCode || calls != testCase.wantCalls {
			t.Fatalf("key %q: status = %d, calls = %d; want %d, %d", testCase.key, response.Code, calls, testCase.wantCode, testCase.wantCalls)
		}
	}
}

func TestRESTRemovalRejectsRestrictedKeysBeforeDocker(t *testing.T) {
	t.Parallel()

	h := &Handler{
		appConfig: &app.Config{APIKeys: restapi.Keyring{
			{Key: "reader", Role: restapi.RoleRead},
			{Key: "writer", Role: restapi.RoleWrite},
		}},
		log: logger.New(logger.LevelCritical),
	}

	for _, endpoint := range []struct {
		name      string
		pathValue string
		handler   http.HandlerFunc
	}{
		{"project", "projectName", h.ProjectApiHandler},
		{"stack", "stackName", h.StackApiHandler},
	} {
		for _, key := range []string{"reader", "writer"} {
			t.Run(endpoint.name+"/"+key, func(t *testing.T) {
				t.Parallel()

				request := httptest.NewRequest(http.MethodDelete, "/", nil)
				request.SetPathValue(endpoint.pathValue, "test-workload")
				request.Header.Set(restapi.KeyHeader, key)

				response := httptest.NewRecorder()
				endpoint.handler(response, request)

				if response.Code != http.StatusForbidden {
					t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
				}
			})
		}
	}
}

func TestRESTUnsupportedMethodsPreserveAuthentication(t *testing.T) {
	t.Parallel()

	h := &Handler{
		appConfig: &app.Config{APIKeys: restapi.Keyring{{Key: "reader", Role: restapi.RoleRead}}},
		log:       logger.New(logger.LevelCritical),
	}

	for _, endpoint := range []struct {
		name      string
		pathValue string
		handler   http.HandlerFunc
	}{
		{"project", "projectName", h.ProjectApiHandler},
		{"stack", "stackName", h.StackApiHandler},
	} {
		for _, testCase := range []struct {
			name       string
			key        string
			target     string
			hasName    bool
			wantStatus int
		}{
			{"missing key", "", "/", true, http.StatusUnauthorized},
			{"invalid key", "invalid", "/", true, http.StatusUnauthorized},
			{"missing name", "reader", "/", false, http.StatusBadRequest},
			{"unknown context", "reader", "/?context=unknown", true, http.StatusBadRequest},
			{"valid request", "reader", "/", true, http.StatusMethodNotAllowed},
		} {
			t.Run(endpoint.name+"/"+testCase.name, func(t *testing.T) {
				t.Parallel()

				request := httptest.NewRequest(http.MethodPatch, testCase.target, nil)
				if testCase.hasName {
					request.SetPathValue(endpoint.pathValue, "test-workload")
				}

				request.Header.Set(restapi.KeyHeader, testCase.key)

				response := httptest.NewRecorder()
				endpoint.handler(response, request)

				if response.Code != testCase.wantStatus {
					t.Fatalf("status = %d, want %d", response.Code, testCase.wantStatus)
				}
			})
		}
	}
}

func TestRESTUnknownOperationFailsClosed(t *testing.T) {
	t.Parallel()

	h := &Handler{
		appConfig: &app.Config{APIKeys: testAPIKeys("admin")},
		log:       logger.New(logger.LevelCritical),
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set(restapi.KeyHeader, "admin")

	response := httptest.NewRecorder()

	if h.authorize(response, request, h.log.Logger, "", "unclassified") {
		t.Fatal("unclassified operation was authorized")
	}

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusInternalServerError)
	}
}

func TestRESTOpenAPIPermissions(t *testing.T) {
	t.Parallel()

	want := map[apiOperation]restapi.Role{
		opListDeploymentRuns:   restapi.RoleRead,
		opGetDeploymentRun:     restapi.RoleRead,
		opListScheduledJobs:    restapi.RoleRead,
		opTriggerScheduledJob:  restapi.RoleWrite,
		opListComposeProjects:  restapi.RoleRead,
		opGetComposeProject:    restapi.RoleRead,
		opDeleteComposeProject: restapi.RoleFull,
		opRunComposeProject:    restapi.RoleWrite,
		opListSwarmStacks:      restapi.RoleRead,
		opGetSwarmStack:        restapi.RoleRead,
		opDeleteSwarmStack:     restapi.RoleFull,
		opRunSwarmStack:        restapi.RoleWrite,
		opTriggerPoll:          restapi.RoleWrite,
		opCompactMirrors:       restapi.RoleWrite,
		opListSyncWindows:      restapi.RoleRead,
	}

	h := &Handler{appConfig: &app.Config{}, log: logger.New(logger.LevelCritical)}

	routes, err := createRouteCatalog(h, Mounts{}, newSchemaBuilder())
	if err != nil {
		t.Fatal(err)
	}

	seen := make(map[apiOperation]bool)

	for _, route := range routes {
		if route.Root != APIPath {
			continue
		}

		for _, metadata := range route.Operations {
			id := apiOperation(metadata.Operation.OperationID)

			role, ok := want[id]
			if !ok {
				t.Errorf("unexpected REST operation %q", id)
				continue
			}

			seen[id] = true

			if roleForOperation(id) != role {
				t.Errorf("%s role = %q, want %q", id, roleForOperation(id), role)
			}

			description := "Required API key role: `" + string(role) + "`."
			if !strings.Contains(metadata.Operation.Description, description) {
				t.Errorf("%s is missing role description %q", id, description)
			}

			if role != restapi.RoleRead && metadata.Operation.Responses.Value("403") == nil {
				t.Errorf("%s is missing a 403 response", id)
			}
		}
	}

	if len(seen) != len(want) {
		t.Fatalf("documented %d REST operations, want %d", len(seen), len(want))
	}
}
