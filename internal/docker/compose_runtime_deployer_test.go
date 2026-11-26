package docker

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/common/types/duration"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/test"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func TestDeployStackComposeContentDigest(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	filePath := filepath.Join(root, "compose.yaml")
	createComposeFile(t, filePath, `services:
  app: &service
    image: alpine:3.21
    command: ["sleep", "infinity"]
    stop_grace_period: 1s
    secrets: [password]
  ignored:
    <<: *service
    labels:
      cd.doco.deployment.recreate.ignore: "{secrets: [password]}"
  other:
    <<: *service
    secrets: []
secrets:
  password:
    environment: DB_PASSWORD
`)

	dockerCli, err := test.NewDockerCli()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := dockerCli.Client().Close(); err != nil {
			t.Error(err)
		}
	})

	service, err := compose.NewComposeService(dockerCli)
	if err != nil {
		t.Fatal(err)
	}

	stack := &test.ComposeStack{
		Name:      test.ConvertTestName(t.Name()),
		Service:   service,
		DockerCli: dockerCli,
		Client:    dockerCli.Client(),
	}
	t.Cleanup(func() {
		if err := service.Down(context.WithoutCancel(ctx), stack.Name, api.DownOptions{RemoveOrphans: true}); err != nil {
			t.Error(err)
		}
	})

	config := &deploy.Config{
		Name:             stack.Name,
		WorkingDirectory: ".",
		ComposeFiles:     []string{filePath},
		Timeout:          duration.Duration(time.Minute),
	}
	run := func(password string) {
		t.Helper()

		config.Internal.Environment = map[string]string{"DB_PASSWORD": password}
		if err := DeployStack(ctx, DeployRequest{
			JobLog:           slog.Default(),
			ExternalRepoPath: root,
			DockerCLI:        dockerCli,
			Payload:          &webhook.ParsedPayload{FullName: "example/content-digest"},
			DeployConfig:     config,
			AppVersion:       "test",
		}); err != nil {
			t.Fatal(err)
		}
	}

	run("initial")

	services := []string{"app", "ignored", "other"}
	before := make(map[string]string, len(services))

	for _, name := range services {
		before[name] = stack.ServiceContainerID(ctx, t, name)
	}

	run("initial")

	for _, name := range services {
		if got := stack.ServiceContainerID(ctx, t, name); got != before[name] {
			t.Errorf("service %s was recreated without a value change", name)
		}
	}

	run("rotated")

	for _, name := range services {
		recreated := stack.ServiceContainerID(ctx, t, name) != before[name]
		if recreated != (name == "app") {
			t.Errorf("service %s: recreated = %v, want %v", name, recreated, name == "app")
		}
	}

	for name, want := range map[string]string{"app": "rotated", "ignored": "initial"} {
		code, _ := stack.Exec(ctx, t, name, []string{"sh", "-c", `test "$(cat /run/secrets/password)" = "$1"`, "sh", want})
		if code != 0 {
			t.Errorf("service %s has the wrong secret value", name)
		}
	}
}

func TestDeployStackComposeForceRecreate(t *testing.T) {
	tests := []struct {
		name                   string
		forceRecreate          bool
		forcedByReconciliation bool
		removeApp              bool
		changes                []Change
		wantRecreated          map[string]bool
	}{
		{
			name:          "unchanged services are preserved by default",
			wantRecreated: map[string]bool{"app": false, "other": false},
		},
		{
			name:          "explicit force recreates unchanged services",
			forceRecreate: true,
			wantRecreated: map[string]bool{"app": true, "other": true},
		},
		{
			name:          "file changes recreate only affected services by default",
			changes:       []Change{{Type: "configs", Services: []string{"app"}}},
			wantRecreated: map[string]bool{"app": true, "other": false},
		},
		{
			name:          "explicit force overrides service scoped changes",
			forceRecreate: true,
			changes:       []Change{{Type: "configs", Services: []string{"app"}}},
			wantRecreated: map[string]bool{"app": true, "other": true},
		},
		{
			name:                   "reconciliation force preserves unchanged services",
			forceRecreate:          true,
			forcedByReconciliation: true,
			wantRecreated:          map[string]bool{"app": false, "other": false},
		},
		{
			name:                   "reconciliation force recreates only affected services",
			forceRecreate:          true,
			forcedByReconciliation: true,
			changes:                []Change{{Type: "configs", Services: []string{"app"}}},
			wantRecreated:          map[string]bool{"app": true, "other": false},
		},
		{
			name:                   "reconciliation restores missing services without recreating siblings",
			forceRecreate:          true,
			forcedByReconciliation: true,
			removeApp:              true,
			wantRecreated:          map[string]bool{"app": true, "other": false},
		},
		{
			name:                   "reconciliation preserves whole project recreation for failed deploy retries",
			forceRecreate:          true,
			forcedByReconciliation: true,
			changes:                []Change{{Type: ChangeTypeFailedDeployRetry}},
			wantRecreated:          map[string]bool{"app": true, "other": true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			root := t.TempDir()
			filePath := filepath.Join(root, "compose.yaml")
			createComposeFile(t, filePath, `services:
  app:
    image: alpine:3.21
    pull_policy: missing
    command: ["sleep", "infinity"]
    stop_grace_period: 1s
  other:
    image: alpine:3.21
    pull_policy: missing
    command: ["sleep", "infinity"]
    stop_grace_period: 1s
`)
			stack := test.ComposeUp(ctx, t, test.WithFile(filePath), test.WithCustomLabel(map[string]string{
				DocoCDLabels.Metadata.Version: "legacy",
				DocoCDLabels.Source.URL:       "",
			}))

			before, err := composeServiceContainers(ctx, stack.Client, stack.Name)
			if err != nil {
				t.Fatal(err)
			}

			if tt.removeApp {
				if len(before["app"]) != 1 {
					t.Fatal("expected one app container before removal")
				}

				if _, err := stack.Client.ContainerRemove(ctx, before["app"][0].ID, client.ContainerRemoveOptions{Force: true}); err != nil {
					t.Fatal(err)
				}
			}

			deployConfig := &deploy.Config{
				Name:             stack.Name,
				WorkingDirectory: ".",
				ComposeFiles:     []string{filePath},
				ForceRecreate:    tt.forceRecreate,
				Timeout:          duration.Duration(time.Minute),
			}
			deployConfig.Internal.ForceRecreateByReconciliation = tt.forcedByReconciliation

			err = DeployStack(ctx, DeployRequest{
				JobLog:           slog.Default(),
				ExternalRepoPath: root,
				DockerCLI:        stack.DockerCli,
				Payload:          &webhook.ParsedPayload{FullName: "example/app"},
				SourceURL:        "https://github.com/example/app.git",
				DeployConfig:     deployConfig,
				DetectedChanges:  tt.changes,
				AppVersion:       "test",
			})
			if err != nil {
				t.Fatal(err)
			}

			after, err := composeServiceContainers(ctx, stack.Client, stack.Name)
			if err != nil {
				t.Fatal(err)
			}

			for service, wantRecreated := range tt.wantRecreated {
				if len(before[service]) != 1 || len(after[service]) != 1 {
					t.Fatalf("service %s: expected one container before and after deployment", service)
				}

				if recreated := before[service][0].ID != after[service][0].ID; recreated != wantRecreated {
					t.Errorf("service %s: recreated = %v, want %v", service, recreated, wantRecreated)
				}

				wantLabels := before[service][0].Labels
				if wantRecreated {
					wantLabels = map[string]string{
						DocoCDLabels.Metadata.Version: "test",
						DocoCDLabels.Source.URL:       "https://github.com/example/app.git",
					}
				}

				for _, label := range []string{DocoCDLabels.Metadata.Version, DocoCDLabels.Source.URL} {
					if got := after[service][0].Labels[label]; got != wantLabels[label] {
						t.Errorf("service %s: label %s = %q, want %q", service, label, got, wantLabels[label])
					}
				}
			}
		})
	}
}
