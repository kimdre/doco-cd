package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/docker/cli/cli/command"
	composeapi "github.com/docker/compose/v5/pkg/api"

	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/docker"
	dockerswarm "github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/test"
)

const projectToolsComposeContent = `services:
  nginx:
    image: nginx:latest
    volumes:
      - data:/data
volumes:
  data:
`

const composeContent = `services:
  nginx:
    image: nginx:latest
    ports:
      - "80"
`

func TestMCPProjectToolValidation(t *testing.T) {
	h := &Handler{log: logger.New(logger.LevelCritical)}
	server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)
	session := connectMCPTestClient(t, server)

	for _, testCase := range []struct {
		name      string
		tool      string
		arguments map[string]any
		contains  string
	}{
		{name: "control missing project", tool: "control_project", arguments: map[string]any{"action": "start"}, contains: "project_name"},
		{name: "control blank project", tool: "control_project", arguments: map[string]any{"project_name": "  ", "action": "start"}, contains: "missing project name"},
		{name: "control missing action", tool: "control_project", arguments: map[string]any{"project_name": "project"}, contains: "action"},
		{name: "control invalid action", tool: "control_project", arguments: map[string]any{"project_name": "project", "action": "invalid"}, contains: "enum"},
		{name: "control invalid timeout", tool: "control_project", arguments: map[string]any{"project_name": "project", "action": "start", "timeout": "invalid"}, contains: "timeout"},
		{name: "control zero timeout", tool: "control_project", arguments: map[string]any{"project_name": "project", "action": "start", "timeout": 0}, contains: "minimum"},
		{name: "control overflowing timeout", tool: "control_project", arguments: map[string]any{"project_name": "project", "action": "start", "timeout": controlplane.MaxProjectActionTimeout + 1}, contains: "maximum"},
		{name: "control nil docker", tool: "control_project", arguments: map[string]any{"project_name": "project", "action": "start"}, contains: "docker cli is required"},
		{name: "destroy missing project", tool: "destroy_project", arguments: map[string]any{}, contains: "project_name"},
		{name: "destroy blank project", tool: "destroy_project", arguments: map[string]any{"project_name": "  "}, contains: "missing project name"},
		{name: "destroy invalid timeout", tool: "destroy_project", arguments: map[string]any{"project_name": "project", "timeout": "invalid"}, contains: "timeout"},
		{name: "destroy zero timeout", tool: "destroy_project", arguments: map[string]any{"project_name": "project", "timeout": 0}, contains: "minimum"},
		{name: "destroy overflowing timeout", tool: "destroy_project", arguments: map[string]any{"project_name": "project", "timeout": controlplane.MaxProjectActionTimeout + 1}, contains: "maximum"},
		{name: "destroy nil docker", tool: "destroy_project", arguments: map[string]any{"project_name": "project"}, contains: "docker cli is required"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			assertMCPToolError(t, session, testCase.tool, testCase.arguments, testCase.contains)
		})
	}
}

func TestMCPProjectTools(t *testing.T) {
	skipWithoutLiveDocker(t)

	dockerCli, err := docker.CreateDockerCli(false)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = dockerCli.Client().Close() })

	if dockerswarm.GetModeEnabled() {
		t.Skip("compose project tools require standalone mode")
	}

	projectName := test.ConvertTestName(t.Name())
	test.ComposeUp(t.Context(), t, test.WithYAML(projectToolsComposeContent), test.WithName(projectName))

	h := &Handler{dockerCli: dockerCli, log: logger.New(logger.LevelCritical)}
	server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)
	session := connectMCPTestClient(t, server)

	for _, action := range []string{"stop", "start", "restart"} {
		result := callMCPTool(t, session, "control_project", map[string]any{
			"project_name": projectName,
			"action":       action,
			"timeout":      30,
		})

		var output controlProjectOutput
		decodeMCPStructuredContent(t, result, &output)

		if output.ProjectName != projectName || output.Action != action || output.Status != "completed" {
			t.Fatalf("unexpected %s output: %#v", action, output)
		}

		wantState := "running"
		if action == "stop" {
			wantState = "exited"
		}

		waitForProjectState(t, dockerCli, projectName, wantState)
	}

	assertMCPToolError(t, session, "control_project", map[string]any{
		"project_name": "missing",
		"action":       "stop",
	}, "project not found")

	result := callMCPTool(t, session, "destroy_project", map[string]any{
		"project_name": projectName,
		"timeout":      30,
		"volumes":      true,
		"images":       false,
	})

	var output destroyProjectOutput
	decodeMCPStructuredContent(t, result, &output)

	if output.ProjectName != projectName || output.Status != "destroyed" || !output.Volumes || output.Images {
		t.Fatalf("unexpected destroy output: %#v", output)
	}

	containers, err := docker.GetProjectContainers(t.Context(), dockerCli, projectName)
	if err != nil {
		t.Fatal(err)
	}

	if len(containers) != 0 {
		t.Fatalf("project still has containers after destroy: %#v", containers)
	}

	volumes, err := docker.GetLabeledVolumes(t.Context(), dockerCli.Client(), composeapi.ProjectLabel, projectName)
	if err != nil {
		t.Fatal(err)
	}

	if len(volumes) != 0 {
		t.Fatalf("project still has volumes after destroy: %#v", volumes)
	}
}

func TestMCPDockerReadTools(t *testing.T) {
	skipWithoutLiveDocker(t)

	dockerCli, err := docker.CreateDockerCli(false)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = dockerCli.Client().Close() })

	if dockerswarm.GetModeEnabled() {
		t.Skip("compose project tools require standalone mode")
	}

	projectName := test.ConvertTestName(t.Name())
	test.ComposeUp(t.Context(), t, test.WithYAML(composeContent), test.WithName(projectName))

	h := &Handler{dockerCli: dockerCli, log: logger.New(logger.LevelCritical)}
	server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)
	session := connectMCPTestClient(t, server)

	t.Run("list projects", func(t *testing.T) {
		result := callMCPTool(t, session, "list_projects", map[string]any{"all": true})

		var output listProjectsOutput
		decodeMCPStructuredContent(t, result, &output)

		if !containsProject(output.Projects, projectName) {
			t.Fatalf("expected project %q in %#v", projectName, output.Projects)
		}
	})

	t.Run("get project containers", func(t *testing.T) {
		result := callMCPTool(t, session, "get_project", map[string]any{"project_name": projectName})

		var output getProjectOutput
		decodeMCPStructuredContent(t, result, &output)

		if len(output.Containers) == 0 {
			t.Fatalf("expected containers for project %q", projectName)
		}
	})

	assertMCPToolError(t, session, "get_project", map[string]any{"project_name": "missing"}, "project not found")
}

func TestMCPGetProjectRequiresName(t *testing.T) {
	h := &Handler{log: logger.New(logger.LevelCritical)}
	server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)
	session := connectMCPTestClient(t, server)

	assertMCPToolError(t, session, "get_project", map[string]any{}, "project_name")
	assertMCPToolError(t, session, "get_project", map[string]any{"project_name": "  "}, "missing project name")
}

func TestMCPDockerReadToolsRequireDockerCLI(t *testing.T) {
	h := &Handler{log: logger.New(logger.LevelCritical)}
	server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)
	session := connectMCPTestClient(t, server)

	assertMCPToolError(t, session, "list_scheduled_jobs", map[string]any{}, "docker cli is required")
	assertMCPToolError(t, session, "list_projects", map[string]any{}, "docker cli is required")
	assertMCPToolError(t, session, "get_project", map[string]any{"project_name": "project"}, "docker cli is required")
}

func containsProject(projects []mcpProjectSummary, name string) bool {
	for _, project := range projects {
		if project.Name == name {
			return true
		}
	}

	return false
}

func waitForProjectState(t *testing.T, dockerCli command.Cli, projectName, want string) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		containers, err := docker.GetProjectContainers(t.Context(), dockerCli, projectName)
		if err == nil && len(containers) > 0 && strings.EqualFold(string(containers[0].State), want) {
			return
		}

		time.Sleep(100 * time.Millisecond)
	}

	containers, err := docker.GetProjectContainers(t.Context(), dockerCli, projectName)
	if err != nil {
		t.Fatal(err)
	}

	t.Fatalf("project %q state = %#v, want %q", projectName, containers, want)
}
