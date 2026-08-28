package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
)

func TestProjectResponsesUseSnakeCaseJSON(t *testing.T) {
	projects := []api.Stack{{
		ID:          "project-id",
		Name:        "project-name",
		Status:      "running",
		ConfigFiles: "compose.yaml",
		Reason:      "healthy",
	}}

	got, err := json.Marshal(projectResponses(projects))
	if err != nil {
		t.Fatal(err)
	}

	want := `[{"id":"project-id","name":"project-name","status":"running","config_files":"compose.yaml","reason":"healthy"}]`
	if string(got) != want {
		t.Fatalf("JSON = %s, want %s", got, want)
	}

	if strings.Contains(string(got), `"ID"`) {
		t.Fatalf("JSON contains PascalCase field: %s", got)
	}
}

func TestContainerResponsesUseSnakeCaseJSON(t *testing.T) {
	containers := []api.ContainerSummary{{
		ID:       "container-id",
		Name:     "web-1",
		Names:    []string{"web-1"},
		Image:    "nginx:latest",
		Command:  "nginx -g daemon off;",
		Project:  "project-name",
		Service:  "web",
		Created:  123,
		State:    container.ContainerState("running"),
		Status:   "Up 1 minute",
		Health:   container.HealthStatus("healthy"),
		ExitCode: 0,
		Publishers: api.PortPublishers{{
			URL:           "0.0.0.0",
			TargetPort:    80,
			PublishedPort: 8080,
			Protocol:      "tcp",
		}},
		Labels:       map[string]string{"com.example": "value"},
		SizeRw:       1,
		SizeRootFs:   2,
		Mounts:       []string{"/host:/container"},
		Networks:     []string{"default"},
		LocalVolumes: 3,
	}}

	got, err := json.Marshal(containerResponses(containers))
	if err != nil {
		t.Fatal(err)
	}

	want := `[{"id":"container-id","name":"web-1","names":["web-1"],"image":"nginx:latest","command":"nginx -g daemon off;","project":"project-name","service":"web","created":123,"state":"running","status":"Up 1 minute","health":"healthy","exit_code":0,"publishers":[{"url":"0.0.0.0","target_port":80,"published_port":8080,"protocol":"tcp"}],"labels":{"com.example":"value"},"size_rw":1,"size_root_fs":2,"mounts":["/host:/container"],"networks":["default"],"local_volumes":3}]`
	if string(got) != want {
		t.Fatalf("JSON = %s, want %s", got, want)
	}

	for _, field := range []string{`"ID"`, `"TargetPort"`, `"SizeRootFs"`} {
		if strings.Contains(string(got), field) {
			t.Fatalf("JSON contains PascalCase field %s: %s", field, got)
		}
	}
}
