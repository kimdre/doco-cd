//go:build e2e

package e2e

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kimdre/doco-cd/internal/restapi"
)

type mcpKeyTransport struct {
	key string
}

func (transport mcpKeyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	cloned := request.Clone(request.Context())
	cloned.Header.Set(restapi.KeyHeader, transport.key)

	return http.DefaultTransport.RoundTrip(cloned)
}

func connectMCPWithKey(t *testing.T, h *Harness, key string) *sdkmcp.ClientSession {
	t.Helper()

	endpoint, err := h.daemon.Endpoint(h.ctx, "http")
	if err != nil {
		t.Fatalf("get daemon MCP endpoint: %v", err)
	}

	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "e2e-role-test", Version: "test"}, nil)

	session, err := client.Connect(t.Context(), &sdkmcp.StreamableClientTransport{
		Endpoint:             endpoint + "/mcp",
		HTTPClient:           &http.Client{Transport: mcpKeyTransport{key: key}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Errorf("close MCP session: %v", err)
		}
	})

	return session
}

func TestAPIKeyRoles(t *testing.T) {
	requireStandaloneSuite(t)
	t.Parallel()

	const (
		project = "e2e-api-key-roles"
		reader  = "e2e-api-reader"
		writer  = "e2e-api-writer"
		admin   = "e2e-api-admin"
	)

	h := NewHarness(t, "api-key-roles")
	h.EnableAPIKeys(admin, restapi.Keyring{
		{Key: reader, Role: restapi.RoleRead},
		{Key: writer, Role: restapi.RoleWrite},
		{Key: admin, Role: restapi.RoleFull},
	})
	h.SetEnv("MCP_ENABLED", "true")
	h.SetPollInterval(24 * time.Hour)
	h.Start()

	var containerID string

	h.WaitFor(2*time.Minute, "API role fixture starts", func() bool {
		containerID = h.ComposeContainerID(project, "app")
		return containerID != ""
	})

	projectPath := "/v1/api/project/" + project
	h.APIRequestWithKey(http.MethodGet, projectPath, "", http.StatusUnauthorized)
	h.APIRequestWithKey(http.MethodGet, projectPath, "invalid", http.StatusUnauthorized)

	for _, key := range []string{reader, writer, admin} {
		h.APIRequestWithKey(http.MethodGet, projectPath, key, http.StatusOK)
	}

	h.APIRequestWithKey(http.MethodPost, projectPath+"/stop", reader, http.StatusForbidden)
	h.APIRequestWithKey(http.MethodDelete, projectPath, reader, http.StatusForbidden)
	h.APIRequestWithKey(http.MethodDelete, projectPath, writer, http.StatusForbidden)

	if !h.ContainerRunning(containerID) {
		t.Fatal("a denied REST request changed the fixture container")
	}

	for _, testCase := range []struct {
		key       string
		wantCount int
	}{
		{reader, 9},
		{writer, 14},
		{admin, 16},
	} {
		t.Run(testCase.key, func(t *testing.T) {
			session := connectMCPWithKey(t, h, testCase.key)

			tools, err := session.ListTools(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}

			if len(tools.Tools) != testCase.wantCount {
				t.Fatalf("got %d tools, want %d", len(tools.Tools), testCase.wantCount)
			}

			if testCase.key == reader {
				_, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
					Name:      "control_project",
					Arguments: map[string]any{"project_name": project, "action": "stop"},
				})
				if err == nil || !strings.Contains(strings.ToLower(err.Error()), "unknown tool") {
					t.Fatalf("reader control call = %v, want unknown tool", err)
				}
			}

			if testCase.key == writer {
				_, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
					Name:      "destroy_project",
					Arguments: map[string]any{"project_name": project},
				})
				if err == nil || !strings.Contains(strings.ToLower(err.Error()), "unknown tool") {
					t.Fatalf("writer destroy call = %v, want unknown tool", err)
				}

				result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
					Name:      "control_project",
					Arguments: map[string]any{"project_name": project, "action": "stop", "timeout": 1},
				})
				if err != nil || result == nil || result.IsError {
					t.Fatalf("writer control call: result = %#v, error = %v", result, err)
				}
			}
		})
	}

	h.WaitFor(30*time.Second, "writer stopped the fixture through MCP", func() bool {
		return !h.ContainerRunning(containerID)
	})

	h.APIRequestWithKey(http.MethodPost, projectPath+"/start", writer, http.StatusOK)
	h.WaitFor(30*time.Second, "writer started the fixture through REST", func() bool {
		return h.ContainerRunning(containerID)
	})

	h.APIRequestWithKey(http.MethodDelete, projectPath+"?timeout=1&volumes=false&images=false", admin, http.StatusOK)
	h.WaitForContainerRemoval(project, "app", 30*time.Second)
}

func TestAPIKeyRolesSwarm(t *testing.T) {
	t.Parallel()

	const (
		stack  = "e2e-api-key-roles-swarm"
		reader = "e2e-swarm-reader"
		writer = "e2e-swarm-writer"
		admin  = "e2e-swarm-admin"
	)

	h := NewHarness(t, "api-key-roles-swarm")
	h.EnableRemoteSwarmContext()
	h.EnableAPIKeys(admin, restapi.Keyring{
		{Key: reader, Role: restapi.RoleRead},
		{Key: writer, Role: restapi.RoleWrite},
		{Key: admin, Role: restapi.RoleFull},
	})
	h.SetEnv("MCP_ENABLED", "true")
	h.SetPollInterval(24 * time.Hour)
	h.Start()

	waitReplicas := func(want uint64) {
		t.Helper()

		h.WaitFor(3*time.Minute, "Swarm service has the expected replica count", func() bool {
			result, err := h.remoteDocker.ServiceInspect(h.ctx, stack+"_app", client.ServiceInspectOptions{})
			if err != nil || result.Service.Spec.Mode.Replicated == nil {
				return false
			}

			replicas := result.Service.Spec.Mode.Replicated.Replicas

			return replicas != nil && *replicas == want
		})
	}
	waitReplicas(1)

	stackPath := "/v1/api/stack/" + stack
	h.APIRequestWithKey(http.MethodGet, stackPath+"?context=remote", "", http.StatusUnauthorized)
	h.APIRequestWithKey(http.MethodGet, stackPath+"?context=remote", "invalid", http.StatusUnauthorized)

	for _, key := range []string{reader, writer, admin} {
		h.APIRequestWithKey(http.MethodGet, stackPath+"?context=remote", key, http.StatusOK)
	}

	h.APIRequestWithKey(http.MethodPost, stackPath+"/scale?context=remote&replicas=0&wait=false", reader, http.StatusForbidden)
	h.APIRequestWithKey(http.MethodDelete, stackPath+"?context=remote", reader, http.StatusForbidden)
	h.APIRequestWithKey(http.MethodDelete, stackPath+"?context=remote", writer, http.StatusForbidden)
	waitReplicas(1)

	for _, testCase := range []struct {
		key  string
		tool string
	}{
		{reader, "control_stack"},
		{reader, "remove_stack"},
		{writer, "remove_stack"},
	} {
		t.Run(testCase.key+"/"+testCase.tool, func(t *testing.T) {
			session := connectMCPWithKey(t, h, testCase.key)

			_, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
				Name:      testCase.tool,
				Arguments: map[string]any{"stack_name": stack, "context": "remote"},
			})
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "unknown tool") {
				t.Fatalf("restricted %s call = %v, want unknown tool", testCase.tool, err)
			}
		})
	}

	waitReplicas(1)

	session := connectMCPWithKey(t, h, writer)

	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name: "control_stack",
		Arguments: map[string]any{
			"stack_name": stack, "context": "remote", "action": "scale", "replicas": 0, "wait": false,
		},
	})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("writer scale call: result = %#v, error = %v", result, err)
	}

	waitReplicas(0)

	h.APIRequestWithKey(http.MethodPost, stackPath+"/scale?context=remote&replicas=1&wait=false", writer, http.StatusOK)
	waitReplicas(1)

	h.APIRequestWithKey(http.MethodDelete, stackPath+"?context=remote", admin, http.StatusOK)
	h.WaitFor(30*time.Second, "Swarm service is removed", func() bool {
		_, err := h.remoteDocker.ServiceInspect(h.ctx, stack+"_app", client.ServiceInspectOptions{})

		return errdefs.IsNotFound(err)
	})
}
