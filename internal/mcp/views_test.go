package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	composeapi "github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
	dockerswarmtypes "github.com/moby/moby/api/types/swarm"
)

func TestMCPReadOutputSanitizesDockerObjects(t *testing.T) {
	t.Parallel()

	const secret = "sentinel-secret"

	projects := summarizeProjects([]composeapi.Stack{{
		ID:          "project-id",
		Name:        "project",
		Status:      composeapi.RUNNING,
		ConfigFiles: "/home/" + secret + "/compose.yaml",
		Reason:      secret,
	}})
	containers := summarizeContainers([]composeapi.ContainerSummary{{
		ID:         "container-id",
		Name:       "container",
		Image:      "example:latest",
		Command:    secret,
		Service:    "web",
		State:      container.StateRunning,
		Status:     "Up",
		Health:     container.Healthy,
		Labels:     map[string]string{"secret": secret},
		Mounts:     []string{secret},
		Publishers: composeapi.PortPublishers{{TargetPort: 80, PublishedPort: 8080, Protocol: "tcp"}},
	}})
	replicas := uint64(2)
	services := summarizeServices([]dockerswarmtypes.Service{{
		ID: "service-id",
		Spec: dockerswarmtypes.ServiceSpec{
			Annotations: dockerswarmtypes.Annotations{
				Name:   "stack_web",
				Labels: map[string]string{"secret": secret},
			},
			TaskTemplate: dockerswarmtypes.TaskSpec{ContainerSpec: &dockerswarmtypes.ContainerSpec{
				Image:   "example:latest",
				Env:     []string{"SECRET=" + secret},
				Command: []string{secret},
			}},
			Mode: dockerswarmtypes.ServiceMode{Replicated: &dockerswarmtypes.ReplicatedService{Replicas: &replicas}},
		},
		Endpoint: dockerswarmtypes.Endpoint{Ports: []dockerswarmtypes.PortConfig{{
			TargetPort: 80, PublishedPort: 8080,
		}}},
		ServiceStatus: &dockerswarmtypes.ServiceStatus{DesiredTasks: 2, RunningTasks: 1},
	}})

	encoded, err := json.Marshal(struct {
		Projects   []mcpProjectSummary   `json:"projects"`
		Containers []mcpContainerSummary `json:"containers"`
		Services   []mcpServiceSummary   `json:"services"`
	}{
		Projects:   projects,
		Containers: containers,
		Services:   services,
	})
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(encoded), secret) {
		t.Fatalf("sanitized MCP output leaks secret: %s", encoded)
	}

	if projects[0].Name != "project" || containers[0].Service != "web" || services[0].RunningTasks != 1 {
		t.Fatalf("sanitized MCP output lost safe summary fields: %s", encoded)
	}
}
