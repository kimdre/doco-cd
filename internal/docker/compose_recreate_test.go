package docker

import (
	"errors"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/api"

	"github.com/kimdre/doco-cd/internal/config/deploy"
)

func TestSelectRecreateServices(t *testing.T) {
	project := &types.Project{
		Name: "example",
		Services: types.Services{
			"web": {
				Name: "web",
				DependsOn: types.DependsOnConfig{
					"db": {},
				},
			},
			"db":       {Name: "db"},
			"inactive": {Name: "inactive"},
		},
	}

	t.Run("specific service includes dependencies", func(t *testing.T) {
		selected, targets, err := selectRecreateServices(project, "web", nil)
		if err != nil {
			t.Fatal(err)
		}

		if len(selected.Services) != 2 {
			t.Fatalf("selected services = %v, want web and db", selected.ServiceNames())
		}

		if len(targets) != 1 || targets[0] != "web" {
			t.Fatalf("recreate targets = %v, want [web]", targets)
		}
	})

	t.Run("whole project excludes inactive profiles", func(t *testing.T) {
		selected, targets, err := selectRecreateServices(project, "", []string{"web"})
		if err != nil {
			t.Fatal(err)
		}

		if len(selected.Services) != 2 {
			t.Fatalf("selected services = %v, want web and db", selected.ServiceNames())
		}

		if targets != nil {
			t.Fatalf("recreate targets = %v, want nil", targets)
		}
	})

	t.Run("missing service", func(t *testing.T) {
		_, _, err := selectRecreateServices(project, "missing", nil)
		if !errors.Is(err, ErrComposeServiceNotFound) {
			t.Fatalf("error = %v, want ErrComposeServiceNotFound", err)
		}
	})
}

func TestAddComposeServiceTrackingLabels(t *testing.T) {
	project := &types.Project{
		Name:         "example",
		WorkingDir:   "/srv/example",
		ComposeFiles: []string{"/srv/example/compose.yml"},
		Services: types.Services{
			"web": {
				Name:         "web",
				CustomLabels: map[string]string{"example.com/custom": "preserved"},
			},
		},
	}

	addComposeServiceTrackingLabels(project)

	labels := project.Services["web"].CustomLabels
	for key, want := range map[string]string{
		"example.com/custom": "preserved",
		api.ProjectLabel:     "example",
		api.ServiceLabel:     "web",
		api.WorkingDirLabel:  "/srv/example",
		api.ConfigFilesLabel: "/srv/example/compose.yml",
		api.VersionLabel:     api.ComposeVersion,
		api.OneoffLabel:      "False",
	} {
		if got := labels[key]; got != want {
			t.Errorf("label %q = %q, want %q", key, got, want)
		}
	}
}

func TestRestoreDeploymentConfigHash(t *testing.T) {
	t.Run("preserves existing hash", func(t *testing.T) {
		config := &deploy.Config{}
		config.Internal.Hash = "reloaded-hash"

		restoreDeploymentConfigHash(config, map[string]string{
			DocoCDLabels.Deployment.ConfigHash: "deployed-hash",
		})

		if config.Internal.Hash != "deployed-hash" {
			t.Fatalf("config hash = %q, want deployed hash", config.Internal.Hash)
		}
	})

	t.Run("keeps reloaded hash when label is absent", func(t *testing.T) {
		config := &deploy.Config{}
		config.Internal.Hash = "reloaded-hash"

		restoreDeploymentConfigHash(config, nil)

		if config.Internal.Hash != "reloaded-hash" {
			t.Fatalf("config hash = %q, want reloaded hash", config.Internal.Hash)
		}
	})
}
