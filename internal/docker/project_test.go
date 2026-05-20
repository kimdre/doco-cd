package docker

import (
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
)

func TestProjectHash_ChangesWhenJobLabelsChange(t *testing.T) {
	t.Parallel()

	base := &types.Project{
		Services: types.Services{
			"worker": {
				Name: "worker",
				Labels: types.Labels{
					DocoCDJobLabels.JobEnabled:  "true",
					DocoCDJobLabels.JobSchedule: "@every 30m",
				},
			},
		},
	}

	changed := &types.Project{
		Services: types.Services{
			"worker": {
				Name: "worker",
				Labels: types.Labels{
					DocoCDJobLabels.JobEnabled:  "true",
					DocoCDJobLabels.JobSchedule: "@every 30s",
				},
			},
		},
	}

	baseHash, err := ProjectHash(base)
	if err != nil {
		t.Fatalf("ProjectHash(base) error: %v", err)
	}

	changedHash, err := ProjectHash(changed)
	if err != nil {
		t.Fatalf("ProjectHash(changed) error: %v", err)
	}

	if baseHash == changedHash {
		t.Fatalf("expected hash change for job schedule label update, got %q", baseHash)
	}
}

func TestProjectHash_ChangesWhenRecreateLabelsChange(t *testing.T) {
	t.Parallel()

	base := &types.Project{
		Services: types.Services{
			"api": {
				Name: "api",
				Labels: types.Labels{
					DocoCDLabels.Deployment.RecreateIgnore: "{configs: [nginx]}",
				},
			},
		},
	}

	changed := &types.Project{
		Services: types.Services{
			"api": {
				Name: "api",
				Labels: types.Labels{
					DocoCDLabels.Deployment.RecreateIgnore: "{configs: [app]}",
				},
			},
		},
	}

	baseHash, err := ProjectHash(base)
	if err != nil {
		t.Fatalf("ProjectHash(base) error: %v", err)
	}

	changedHash, err := ProjectHash(changed)
	if err != nil {
		t.Fatalf("ProjectHash(changed) error: %v", err)
	}

	if baseHash == changedHash {
		t.Fatalf("expected hash change for recreate label update, got %q", baseHash)
	}
}

func TestProjectHash_IgnoresDocoMetadataLabels(t *testing.T) {
	t.Parallel()

	base := &types.Project{
		Services: types.Services{
			"api": {
				Name: "api",
				Labels: types.Labels{
					DocoCDJobLabels.JobEnabled: "true",
				},
			},
		},
	}

	withMetadataChange := &types.Project{
		Services: types.Services{
			"api": {
				Name: "api",
				Labels: types.Labels{
					DocoCDJobLabels.JobEnabled:    "true",
					DocoCDLabels.Metadata.Version: "v-next",
				},
			},
		},
	}

	baseHash, err := ProjectHash(base)
	if err != nil {
		t.Fatalf("ProjectHash(base) error: %v", err)
	}

	withMetadataChangeHash, err := ProjectHash(withMetadataChange)
	if err != nil {
		t.Fatalf("ProjectHash(withMetadataChange) error: %v", err)
	}

	if baseHash != withMetadataChangeHash {
		t.Fatalf("expected metadata label changes to be ignored, got %q and %q", baseHash, withMetadataChangeHash)
	}
}

func TestProjectHash_IgnoresComposeGeneratedLabels(t *testing.T) {
	t.Parallel()

	base := &types.Project{
		Services: types.Services{
			"api": {
				Name: "api",
				Labels: types.Labels{
					DocoCDJobLabels.JobEnabled: "true",
				},
			},
		},
	}

	withComposeRuntimeLabel := &types.Project{
		Services: types.Services{
			"api": {
				Name: "api",
				Labels: types.Labels{
					DocoCDJobLabels.JobEnabled:   "true",
					"com.docker.compose.project": "another",
				},
			},
		},
	}

	baseHash, err := ProjectHash(base)
	if err != nil {
		t.Fatalf("ProjectHash(base) error: %v", err)
	}

	withComposeRuntimeLabelHash, err := ProjectHash(withComposeRuntimeLabel)
	if err != nil {
		t.Fatalf("ProjectHash(withComposeRuntimeLabel) error: %v", err)
	}

	if baseHash != withComposeRuntimeLabelHash {
		t.Fatalf("expected compose-generated label to be ignored, got %q and %q", baseHash, withComposeRuntimeLabelHash)
	}
}
