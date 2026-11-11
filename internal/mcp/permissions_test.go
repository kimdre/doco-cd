package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/restapi"
)

type permissionRuns struct {
	RunOperations
	compactions atomic.Int32
}

func (*permissionRuns) List(int, string, string) []controlplane.Run {
	return []controlplane.Run{}
}

func (*permissionRuns) Get(string) (controlplane.Run, bool) {
	return controlplane.Run{}, false
}

func (runs *permissionRuns) TriggerMirrorCompaction(context.Context, string, controlplane.MirrorCompactionRequest, bool) (string, error) {
	runs.compactions.Add(1)

	return "test-job", nil
}

func newPermissionTestServer(t *testing.T) (*httptest.Server, *permissionRuns) {
	t.Helper()

	runs := &permissionRuns{}
	handler := newHandler(Dependencies{
		Version: "test",
		APIKeys: restapi.Keyring{
			{Key: "reader", Role: restapi.RoleRead},
			{Key: "writer", Role: restapi.RoleWrite},
			{Key: "admin", Role: restapi.RoleFull},
		},
		MaxPayloadSize: 1024 * 1024,
		Logger:         logger.New(logger.LevelCritical),
		Runs:           runs,
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return server, runs
}

func TestMCPRoleCatalogsAndCalls(t *testing.T) {
	server, runs := newPermissionTestServer(t)

	toolRoles := map[string]restapi.Role{
		"get_health":            restapi.RoleRead,
		"list_deployment_runs":  restapi.RoleRead,
		"get_deployment_run":    restapi.RoleRead,
		"list_scheduled_jobs":   restapi.RoleRead,
		"list_sync_windows":     restapi.RoleRead,
		"list_projects":         restapi.RoleRead,
		"get_project":           restapi.RoleRead,
		"list_stacks":           restapi.RoleRead,
		"get_stack":             restapi.RoleRead,
		"control_project":       restapi.RoleWrite,
		"control_stack":         restapi.RoleWrite,
		"trigger_scheduled_job": restapi.RoleWrite,
		"trigger_poll":          restapi.RoleWrite,
		"compact_mirrors":       restapi.RoleWrite,
		"destroy_project":       restapi.RoleFull,
		"remove_stack":          restapi.RoleFull,
	}
	arguments := map[string]any{
		"control_project":       map[string]any{"project_name": "test", "action": "stop"},
		"control_stack":         map[string]any{"stack_name": "test", "action": "restart"},
		"trigger_scheduled_job": map[string]any{"job_name": "test"},
		"trigger_poll":          map[string]any{"configs": []any{}},
		"compact_mirrors":       map[string]any{},
		"destroy_project":       map[string]any{"project_name": "test", "volumes": false, "images": false},
		"remove_stack":          map[string]any{"stack_name": "test"},
	}

	for _, testCase := range []struct {
		key       string
		role      restapi.Role
		wantCount int
	}{
		{"reader", restapi.RoleRead, 9},
		{"writer", restapi.RoleWrite, 14},
		{"admin", restapi.RoleFull, 16},
	} {
		t.Run(testCase.key, func(t *testing.T) {
			session := connectMCPTestClientWithKey(t, server, testCase.key)

			result, err := session.ListTools(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}

			if len(result.Tools) != testCase.wantCount {
				t.Fatalf("got %d tools, want %d", len(result.Tools), testCase.wantCount)
			}

			seen := make(map[string]bool)

			for _, tool := range result.Tools {
				required, ok := toolRoles[tool.Name]
				if !ok || !testCase.role.Allows(required) {
					t.Fatalf("unexpected tool %q for role %q", tool.Name, testCase.role)
				}

				seen[tool.Name] = true

				if tool.Name == "control_project" && (tool.Annotations == nil || tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint) {
					t.Fatal("control_project lost its destructive annotation")
				}

				if tool.Name == "compact_mirrors" && (tool.Annotations == nil || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint) {
					t.Fatal("compact_mirrors annotation changed")
				}
			}

			for tool, required := range toolRoles {
				if testCase.role.Allows(required) {
					if !seen[tool] {
						t.Errorf("allowed tool %q is missing", tool)
					}

					continue
				}

				_, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: tool, Arguments: arguments[tool]})
				if err == nil || !strings.Contains(strings.ToLower(err.Error()), "unknown tool") {
					t.Errorf("hidden tool %q returned %v, want unknown-tool error", tool, err)
				}
			}

			readResult, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "list_deployment_runs", Arguments: map[string]any{}})
			if err != nil || readResult == nil || readResult.IsError {
				t.Fatalf("allowed read call: result = %#v, error = %v", readResult, err)
			}

			if testCase.role.Allows(restapi.RoleWrite) {
				writeResult, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "compact_mirrors", Arguments: map[string]any{}})
				if err != nil || writeResult == nil || writeResult.IsError {
					t.Fatalf("allowed write call: result = %#v, error = %v", writeResult, err)
				}
			}
		})
	}

	if got := runs.compactions.Load(); got != 2 {
		t.Fatalf("compaction calls = %d, want 2", got)
	}
}

type changingKeyTransport struct {
	key  atomic.Value
	base http.RoundTripper
}

func (transport *changingKeyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	cloned := request.Clone(request.Context())
	cloned.Header.Set(restapi.KeyHeader, transport.key.Load().(string))

	return transport.base.RoundTrip(cloned)
}

func TestMCPRoleIsSelectedForEveryRequest(t *testing.T) {
	server, _ := newPermissionTestServer(t)
	transport := &changingKeyTransport{base: server.Client().Transport}
	transport.key.Store("admin")

	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "role-test", Version: "test"}, nil)

	session, err := client.Connect(t.Context(), &sdkmcp.StreamableClientTransport{
		Endpoint:             server.URL + testMCPPath,
		HTTPClient:           &http.Client{Transport: transport},
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

	for _, testCase := range []struct {
		key       string
		wantCount int
	}{
		{"admin", 16},
		{"reader", 9},
		{"writer", 14},
		{"admin", 16},
	} {
		transport.key.Store(testCase.key)

		result, err := session.ListTools(t.Context(), nil)
		if err != nil {
			t.Fatalf("key %q: %v", testCase.key, err)
		}

		if len(result.Tools) != testCase.wantCount {
			t.Fatalf("key %q: got %d tools, want %d", testCase.key, len(result.Tools), testCase.wantCount)
		}
	}

	transport.key.Store("invalid")

	response, err := transport.RoundTrip(httptest.NewRequest(http.MethodPost, server.URL+testMCPPath, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close() //nolint:errcheck

	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("invalid key status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}

	transport.key.Store("reader")

	_, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name:      "destroy_project",
		Arguments: map[string]any{"project_name": "test"},
	})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "unknown tool") {
		t.Fatalf("restricted call error = %v, want unknown-tool error", err)
	}
}

func TestMCPRoleCatalogsAreConcurrent(t *testing.T) {
	server, _ := newPermissionTestServer(t)

	for _, testCase := range []struct {
		key       string
		wantCount int
	}{
		{"reader", 9},
		{"writer", 14},
		{"admin", 16},
	} {
		t.Run(testCase.key, func(t *testing.T) {
			t.Parallel()

			session := connectMCPTestClientWithKey(t, server, testCase.key)
			for range 10 {
				result, err := session.ListTools(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}

				if len(result.Tools) != testCase.wantCount {
					t.Fatalf("got %d tools, want %d", len(result.Tools), testCase.wantCount)
				}
			}
		})
	}
}
