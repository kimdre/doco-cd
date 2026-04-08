package docker

import (
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/test"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func Test_getLatestServiceState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		serviceLabels map[Service]Labels
		repoName      string
		want          LatestServiceState
	}{
		{
			name:          "empty serviceLabels",
			serviceLabels: map[Service]Labels{},
			repoName:      "repo",
			want:          LatestServiceState{},
		},
		{
			name: "signal service with no timestamp",
			serviceLabels: map[Service]Labels{
				"svc1": {
					DocoCDLabels.Repository.Name: "repo",
				},
			},
			repoName: "repo",
			want: LatestServiceState{
				Labels: Labels{
					DocoCDLabels.Repository.Name: "repo",
				},
				DeployedServicesName: []string{"svc1"},
			},
		},
		{
			name: "signal service with timestamp",
			serviceLabels: map[Service]Labels{
				"svc1": {
					DocoCDLabels.Repository.Name:      "repo",
					DocoCDLabels.Deployment.Timestamp: "2006-01-02T15:04:05Z07:00",
				},
			},
			repoName: "repo",
			want: LatestServiceState{
				Labels: Labels{
					DocoCDLabels.Repository.Name:      "repo",
					DocoCDLabels.Deployment.Timestamp: "2006-01-02T15:04:05Z07:00",
				},
				DeployedServicesName: []string{"svc1"},
			},
		},
		{
			name: "two service with timestamp but repo not match",
			serviceLabels: map[Service]Labels{
				"svc1": {
					DocoCDLabels.Repository.Name:      "repo-2",
					DocoCDLabels.Deployment.Timestamp: "2006-01-02T15:04:05Z07:00",
				},
				"svc2": {
					DocoCDLabels.Repository.Name:      "repo-2",
					DocoCDLabels.Deployment.Timestamp: "2016-01-02T15:04:05Z07:00",
				},
			},
			repoName: "repo",
			want:     LatestServiceState{},
		},
		{
			name: "two service with timestamp but repo mixed",
			serviceLabels: map[Service]Labels{
				"svc1": {
					DocoCDLabels.Repository.Name:      "repo",
					DocoCDLabels.Deployment.Timestamp: "2006-01-02T15:04:05Z07:00",
				},
				"svc2": {
					DocoCDLabels.Repository.Name:      "repo-2",
					DocoCDLabels.Deployment.Timestamp: "2016-01-02T15:04:05Z07:00",
				},
			},
			repoName: "repo",
			want: LatestServiceState{
				Labels: Labels{
					DocoCDLabels.Repository.Name:      "repo",
					DocoCDLabels.Deployment.Timestamp: "2006-01-02T15:04:05Z07:00",
				},
				DeployedServicesName: []string{"svc1"},
			},
		},
		{
			name: "two service with timestamp but no repo",
			serviceLabels: map[Service]Labels{
				"svc1": {
					DocoCDLabels.Deployment.Timestamp: "2006-01-02T15:04:05Z07:00",
				},
				"svc2": {
					DocoCDLabels.Deployment.Timestamp: "2016-01-02T15:04:05Z07:00",
				},
			},
			repoName: "repo",
			want:     LatestServiceState{},
		},
		{
			name: "two service with timestamp but empty repo",
			serviceLabels: map[Service]Labels{
				"svc1": {
					DocoCDLabels.Repository.Name:      "",
					DocoCDLabels.Deployment.Timestamp: "2006-01-02T15:04:05Z07:00",
				},
				"svc2": {
					DocoCDLabels.Repository.Name:      "",
					DocoCDLabels.Deployment.Timestamp: "2016-01-02T15:04:05Z07:00",
				},
			},
			repoName: "",
			want: LatestServiceState{
				Labels: Labels{
					DocoCDLabels.Repository.Name:      "",
					DocoCDLabels.Deployment.Timestamp: "2016-01-02T15:04:05Z07:00",
				},
				DeployedServicesName: []string{"svc1", "svc2"},
			},
		},
		{
			name: "two service with timestamp",
			serviceLabels: map[Service]Labels{
				"svc1": {
					DocoCDLabels.Repository.Name:      "repo",
					DocoCDLabels.Deployment.Timestamp: "2006-01-02T15:04:05Z07:00",
				},
				"svc2": {
					DocoCDLabels.Repository.Name:      "repo",
					DocoCDLabels.Deployment.Timestamp: "2016-01-02T15:04:05Z07:00",
				},
			},
			repoName: "repo",
			want: LatestServiceState{
				Labels: Labels{
					DocoCDLabels.Repository.Name:      "repo",
					DocoCDLabels.Deployment.Timestamp: "2016-01-02T15:04:05Z07:00",
				},
				DeployedServicesName: []string{"svc1", "svc2"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getLatestServiceState(tt.serviceLabels, tt.repoName)
			slices.Sort(tt.want.DeployedServicesName)
			slices.Sort(got.DeployedServicesName)

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("GetLatestServiceState() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetLatestServiceState(t *testing.T) {
	dockerCli, err := CreateDockerCli(false, false)
	if err != nil {
		t.Fatalf("Failed to create Docker CLI: %v", err)
	}

	swarm.ModeEnabled, err = swarm.CheckDaemonIsSwarmManager(t.Context(), dockerCli)
	if err != nil {
		t.Fatalf("Failed to check if Docker daemon is in Swarm mode: %v", err)
	}

	if swarm.ModeEnabled {
		t.Skip("Swarm mode is enabled, skipping test")
	}
	// t.Parallel()
	// cannot run in parallel, because container_name is set and need unique.
	ctx := t.Context()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "test.compose.yaml")

	cfg := `
services:
  test:
    image: nginx:latest
    scale: 2
    environment:
      TZ: Europe/Berlin
    ports:
      - "80"
    volumes:
      - ./html:/usr/share/nginx/html
  test2:
    image: nginx:latest
    deploy:
      replicas: 2
    environment:
      TZ: Europe/Berlin
    ports:
      - "80"
    volumes:
      - ./html:/usr/share/nginx/html
  nginx:
    image: nginx:latest
    container_name: nginx
    environment:
      TZ: Europe/Berlin
    volumes:
      - ./html:/usr/share/nginx/html
`
	createComposeFile(t, filePath, cfg)

	stackName := test.ConvertTestName(t.Name())

	_, err = LoadCompose(ctx, tmpDir, tmpDir, stackName, []string{filePath}, []string{".env"}, []string{}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}

	repoName := "repoName"

	stack := test.ComposeUp(ctx, t,
		test.WithYAML(cfg),
		test.WithCustomLabel(map[string]string{
			DocoCDLabels.Repository.Name: repoName,
		}),
	)

	latest, err := GetLatestServiceState(ctx, stack.Client, repoName, stackName)
	if err != nil {
		t.Fatal(err)
	}

	// auto generated name and container_name, both of them is possible in compose file, so we check both of them here.
	want := []string{
		"testgetlatestservicestate-test-1", "testgetlatestservicestate-test-2",
		"testgetlatestservicestate-test2-1", "testgetlatestservicestate-test2-2",
		"nginx",
	}
	slices.Sort(want)
	slices.Sort(latest.DeployedServicesName)

	if !reflect.DeepEqual(latest.DeployedServicesName, want) {
		t.Errorf("expected deployed service name to be %v, got %v", want, latest.DeployedServicesName)
	}
}

func TestGetLatestServiceSwarm(t *testing.T) {
	t.Skip("debug swarm test stuck in CI")
	t.Parallel()

	dockerCli, err := CreateDockerCli(false, false)
	if err != nil {
		t.Fatalf("Failed to create Docker CLI: %v", err)
	}

	swarm.ModeEnabled, err = swarm.CheckDaemonIsSwarmManager(t.Context(), dockerCli)
	if err != nil {
		t.Fatalf("Failed to check if Docker daemon is in Swarm mode: %v", err)
	}

	if !swarm.ModeEnabled {
		t.Skip("Swarm mode is not enabled, skipping test")
	}

	ctx := t.Context()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "test.compose.yaml")

	cfg := `
services:
  test2:
    image: nginx:latest
    deploy:
      replicas: 2
    environment:
      TZ: Europe/Berlin
    ports:
      - "80"
    volumes:
      - ./html:/usr/share/nginx/html
  nginx:
    image: nginx:latest
    environment:
      TZ: Europe/Berlin
    volumes:
      - ./html:/usr/share/nginx/html
`
	createComposeFile(t, filePath, cfg)

	stackName := test.ConvertTestName(t.Name())

	project, err := LoadCompose(t.Context(), tmpDir, tmpDir, stackName, []string{filePath}, []string{".env"}, []string{}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}

	projectHash, err := ProjectHash(project)
	if err != nil {
		t.Fatalf("failed to get project hash: %v", err)
	}

	repoName := "repoName"

	deployCfg := &config.DeployConfig{
		Name:          stackName,
		RemoveOrphans: true,
	}

	swarmStack, opts, err := LoadSwarmStack(&dockerCli, project, deployCfg, tmpDir)
	if err != nil {
		t.Fatalf("Failed to load swarm stack: %v", err)
	}

	p := webhook.ParsedPayload{
		Ref:       git.SwarmModeBranch,
		CommitSHA: "244b6f9a5b3dc546ab3822d9c0744846f539c6ef",
		Name:      stackName,
		FullName:  repoName,
		CloneURL:  cloneUrlTest,
		Private:   true,
	}

	err = retry.New(
		retry.Attempts(5),
		retry.Delay(2*time.Second),
		retry.Context(ctx),
	).Do(
		func() error {
			timestamp := time.Now().UTC().Format(time.RFC3339)
			addSwarmServiceLabels(swarmStack, deployCfg, &p, tmpDir, "dev", timestamp, p.CommitSHA, projectHash)
			addSwarmVolumeLabels(swarmStack, deployCfg, &p, tmpDir, "dev", timestamp, p.CommitSHA)
			addSwarmConfigLabels(swarmStack, deployCfg, &p, tmpDir, "dev", timestamp, p.CommitSHA)
			addSwarmSecretLabels(swarmStack, deployCfg, &p, tmpDir, "dev", timestamp, p.CommitSHA)

			return DeploySwarmStack(ctx, dockerCli, swarmStack, opts)
		},
	)
	if err != nil {
		t.Fatalf("Failed to deploy swarm stack: %v", err)
	}

	t.Logf("Swarm stack deployed successfully")

	dockerClient, _ := client.New(
		client.FromEnv,
	)

	latest, err := GetLatestServiceState(ctx, dockerClient, repoName, stackName)
	if err != nil {
		t.Fatal(err)
	}

	// auto generated name and container_name, both of them is possible in compose file, so we check both of them here.
	want := []string{
		"testgetlatestserviceswarm_nginx",
		"testgetlatestserviceswarm_test2",
	}
	slices.Sort(want)
	slices.Sort(latest.DeployedServicesName)

	if !reflect.DeepEqual(latest.DeployedServicesName, want) {
		t.Errorf("expected deployed service name to be %v, got %v", want, latest.DeployedServicesName)
	}

	err = PruneStackConfigs(t.Context(), dockerClient, stackName)
	if err != nil {
		t.Fatalf("Failed to prune stack configs: %v", err)
	} else {
		t.Logf("Stack configs pruned successfully")
	}

	err = PruneStackSecrets(t.Context(), dockerClient, stackName)
	if err != nil {
		t.Fatalf("Failed to prune stack secrets: %v", err)
	} else {
		t.Logf("Stack secrets pruned successfully")
	}

	err = RemoveSwarmStack(t.Context(), dockerCli, stackName)
	if err != nil {
		t.Fatalf("Failed to remove swarm stack: %v", err)
	} else {
		t.Logf("Swarm stack removed successfully")
	}
}

func Test_getComposeServiceMissing(t *testing.T) {
	svcs := types.Services{
		"svc1": {
			Name: "svc1",
		},
		"svc-with-container-name": {
			Name:          "svc-with-container-name",
			ContainerName: "container_name",
		},
		"svc-with-scale": {
			Name:  "svc-with-scale",
			Scale: new(2),
		},
	}

	tests := []struct {
		name string // description of this test case
		// Named input parameters for target function.
		deployed    []string
		projectName string
		services    types.Services
		want        []string
	}{
		{
			name:        "no deployed services",
			deployed:    []string{},
			projectName: "project",
			services:    svcs,
			want: []string{
				"project-svc1-1", "container_name",
				"project-svc-with-scale-1", "project-svc-with-scale-2",
			},
		},
		{
			name:        "all deployed",
			deployed:    []string{"project-svc1-1", "container_name", "project-svc-with-scale-1", "project-svc-with-scale-2"},
			projectName: "project",
			services:    svcs,
			want:        []string{},
		},
		{
			name:        "missing deployed",
			deployed:    []string{"project-svc-with-scale-1"},
			projectName: "project",
			services:    svcs,
			want:        []string{"project-svc1-1", "container_name", "project-svc-with-scale-2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getComposeServiceMissing(tt.deployed, tt.projectName, tt.services)
			slices.Sort(got)
			slices.Sort(tt.want)

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("getComposeServiceMissing() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_getSwarmServiceMissing(t *testing.T) {
	svcs := types.Services{
		"svc1": {
			Name: "svc1",
		},
		"svc-with-scale": {
			Name:  "svc-with-scale",
			Scale: new(2),
		},
	}

	tests := []struct {
		name string // description of this test case
		// Named input parameters for target function.
		deployed    []string
		projectName string
		services    types.Services
		want        []string
	}{
		{
			name:        "no deployed services",
			deployed:    []string{},
			projectName: "project",
			services:    svcs,
			want: []string{
				"project_svc1",
				"project_svc-with-scale",
			},
		},
		{
			name:        "all deployed",
			deployed:    []string{"project_svc1", "project_svc-with-scale"},
			projectName: "project",
			services:    svcs,
			want:        []string{},
		},
		{
			name:        "missing deployed",
			deployed:    []string{"project_svc-with-scale"},
			projectName: "project",
			services:    svcs,
			want:        []string{"project_svc1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getSwarmServiceMissing(tt.deployed, tt.projectName, tt.services)
			slices.Sort(got)
			slices.Sort(tt.want)

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("getSwarmServiceMissing() = %v, want %v", got, tt.want)
			}
		})
	}
}
