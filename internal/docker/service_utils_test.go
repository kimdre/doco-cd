package docker

import (
	"context"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/compose-spec/compose-go/v2/types"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/test"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func Test_getLatestServiceState(t *testing.T) {
	t.Parallel()

	cache := &sync.Map{}
	cache.Store(getDeployStatusCacheKey("github.com/owner/repo", "cache"), deployStatus{
		ComposeHash: "cache_compose_hash",
		CommitSHA:   "cache_commit_sha",
	})

	tests := []struct {
		name          string
		serviceStatus map[Service]ServiceStatus
		repoName      string
		deployName    string
		cache         *sync.Map
		want          LatestServiceStatus
	}{
		{
			name:          "empty serviceLabels",
			serviceStatus: map[Service]ServiceStatus{},
			repoName:      "repo",
			deployName:    "deploy",
			want: LatestServiceStatus{
				DeployedStatus: map[Service]ServiceStatus{},
			},
		},
		{
			name: "single service with no timestamp",
			serviceStatus: map[Service]ServiceStatus{
				"svc1": {
					Labels: Labels{
						DocoCDLabels.Repository.Name:        "repo",
						DocoCDLabels.Deployment.CommitSHA:   "commit_sha",
						DocoCDLabels.Deployment.ComposeHash: "compose_hash",
					},
					Replicas: 1,
				},
			},
			repoName: "repo",
			want: LatestServiceStatus{
				deploymentCommitSHA:   "commit_sha",
				deploymentComposeHash: "compose_hash",
				DeployedStatus: map[Service]ServiceStatus{
					"svc1": {
						Labels: Labels{
							DocoCDLabels.Repository.Name:        "repo",
							DocoCDLabels.Deployment.CommitSHA:   "commit_sha",
							DocoCDLabels.Deployment.ComposeHash: "compose_hash",
						},
						Replicas: 1,
					},
				},
			},
		},
		{
			name: "single service but repo this is full clone URL",
			serviceStatus: map[Service]ServiceStatus{
				"svc1": {
					Labels: Labels{
						DocoCDLabels.Repository.Name:        "owner/repo",
						DocoCDLabels.Deployment.CommitSHA:   "commit_sha",
						DocoCDLabels.Deployment.ComposeHash: "compose_hash",
					},
					Replicas: 1,
				},
			},
			repoName: "https://github.com/owner/repo.git",
			want: LatestServiceStatus{
				deploymentCommitSHA:   "commit_sha",
				deploymentComposeHash: "compose_hash",
				DeployedStatus: map[Service]ServiceStatus{
					"svc1": {
						Labels: Labels{
							DocoCDLabels.Repository.Name:        "owner/repo",
							DocoCDLabels.Deployment.CommitSHA:   "commit_sha",
							DocoCDLabels.Deployment.ComposeHash: "compose_hash",
						},
						Replicas: 1,
					},
				},
			},
		},
		{
			name: "cache hit, single service with no timestamp",
			serviceStatus: map[Service]ServiceStatus{
				"svc1": {
					Labels: Labels{
						DocoCDLabels.Repository.Name:        "owner/repo",
						DocoCDLabels.Deployment.CommitSHA:   "commit_sha",
						DocoCDLabels.Deployment.ComposeHash: "compose_hash",
					},
					Replicas: 1,
				},
			},
			repoName:   "https://github.com/owner/repo.git",
			deployName: "cache",
			cache:      cache,
			want: LatestServiceStatus{
				deploymentCommitSHA:   "cache_commit_sha",
				deploymentComposeHash: "cache_compose_hash",
				DeployedStatus: map[Service]ServiceStatus{
					"svc1": {
						Labels: Labels{
							DocoCDLabels.Repository.Name:        "owner/repo",
							DocoCDLabels.Deployment.CommitSHA:   "commit_sha",
							DocoCDLabels.Deployment.ComposeHash: "compose_hash",
						},
						Replicas: 1,
					},
				},
			},
		},
		{
			name: "single service with timestamp",
			serviceStatus: map[Service]ServiceStatus{
				"svc1": {
					Labels: Labels{
						DocoCDLabels.Repository.Name:        "repo",
						DocoCDLabels.Deployment.Timestamp:   "2006-01-02T15:04:05Z07:00",
						DocoCDLabels.Deployment.CommitSHA:   "commit_sha",
						DocoCDLabels.Deployment.ComposeHash: "compose_hash",
					},
					Replicas: 1,
				},
			},
			repoName: "repo",
			want: LatestServiceStatus{
				deploymentCommitSHA:   "commit_sha",
				deploymentComposeHash: "compose_hash",
				DeployedStatus: map[Service]ServiceStatus{
					"svc1": {
						Labels: Labels{
							DocoCDLabels.Repository.Name:        "repo",
							DocoCDLabels.Deployment.Timestamp:   "2006-01-02T15:04:05Z07:00",
							DocoCDLabels.Deployment.CommitSHA:   "commit_sha",
							DocoCDLabels.Deployment.ComposeHash: "compose_hash",
						},
						Replicas: 1,
					},
				},
			},
		},
		{
			name: "two service with timestamp but repo not match",
			serviceStatus: map[Service]ServiceStatus{
				"svc1": {
					Labels: Labels{
						DocoCDLabels.Repository.Name:      "repo-2",
						DocoCDLabels.Deployment.Timestamp: "2006-01-02T15:04:05Z07:00",
					},
					Replicas: 1,
				},
				"svc2": {
					Labels: Labels{
						DocoCDLabels.Repository.Name:      "repo-2",
						DocoCDLabels.Deployment.Timestamp: "2016-01-02T15:04:05Z07:00",
					},
					Replicas: 1,
				},
			},
			repoName: "repo",
			want: LatestServiceStatus{
				deploymentCommitSHA:   "",
				deploymentComposeHash: "",
				DeployedStatus:        map[Service]ServiceStatus{},
			},
		},
		{
			name: "two service with timestamp but repo mixed",
			serviceStatus: map[Service]ServiceStatus{
				"svc1": {
					Labels: Labels{
						DocoCDLabels.Repository.Name:        "repo",
						DocoCDLabels.Deployment.Timestamp:   "2006-01-02T15:04:05Z07:00",
						DocoCDLabels.Deployment.CommitSHA:   "commit_sha1",
						DocoCDLabels.Deployment.ComposeHash: "compose_hash1",
					},
					Replicas: 1,
				},
				"svc2": {
					Labels: Labels{
						DocoCDLabels.Repository.Name:        "repo-2",
						DocoCDLabels.Deployment.Timestamp:   "2016-01-02T15:04:05Z07:00",
						DocoCDLabels.Deployment.CommitSHA:   "commit_sha2",
						DocoCDLabels.Deployment.ComposeHash: "compose_hash2",
					},
					Replicas: 1,
				},
			},
			repoName: "repo",
			want: LatestServiceStatus{
				deploymentCommitSHA:   "commit_sha1",
				deploymentComposeHash: "compose_hash1",
				DeployedStatus: map[Service]ServiceStatus{
					"svc1": {
						Labels: Labels{
							DocoCDLabels.Repository.Name:        "repo",
							DocoCDLabels.Deployment.Timestamp:   "2006-01-02T15:04:05Z07:00",
							DocoCDLabels.Deployment.CommitSHA:   "commit_sha1",
							DocoCDLabels.Deployment.ComposeHash: "compose_hash1",
						},
						Replicas: 1,
					},
				},
			},
		},
		{
			name: "two service with timestamp but repo mismatch",
			serviceStatus: map[Service]ServiceStatus{
				"svc1": {
					Labels: Labels{
						DocoCDLabels.Deployment.Timestamp:   "2006-01-02T15:04:05Z07:00",
						DocoCDLabels.Deployment.CommitSHA:   "commit_sha1",
						DocoCDLabels.Deployment.ComposeHash: "compose_hash1",
					},
					Replicas: 1,
				},
				"svc2": {
					Labels: Labels{
						DocoCDLabels.Deployment.Timestamp:   "2016-01-02T15:04:05Z07:00",
						DocoCDLabels.Deployment.CommitSHA:   "commit_sha2",
						DocoCDLabels.Deployment.ComposeHash: "compose_hash2",
					},
					Replicas: 1,
				},
			},
			repoName: "repo",
			want: LatestServiceStatus{
				DeployedStatus: map[Service]ServiceStatus{},
			},
		},
		{
			name: "two service with timestamp but empty repo",
			serviceStatus: map[Service]ServiceStatus{
				"svc1": {
					Labels: Labels{
						DocoCDLabels.Repository.Name:        "",
						DocoCDLabels.Deployment.Timestamp:   "2006-01-02T15:04:05Z07:00",
						DocoCDLabels.Deployment.CommitSHA:   "commit_sha1",
						DocoCDLabels.Deployment.ComposeHash: "compose_hash1",
					},
					Replicas: 1,
				},
				"svc2": {
					Labels: Labels{
						DocoCDLabels.Repository.Name:        "",
						DocoCDLabels.Deployment.Timestamp:   "2016-01-02T15:04:05Z07:00",
						DocoCDLabels.Deployment.CommitSHA:   "commit_sha2",
						DocoCDLabels.Deployment.ComposeHash: "compose_hash2",
					},
					Replicas: 2,
				},
			},
			repoName: "",
			want: LatestServiceStatus{
				deploymentCommitSHA:   "commit_sha2",
				deploymentComposeHash: "compose_hash2",
				DeployedStatus: map[Service]ServiceStatus{
					"svc2": {
						Labels: Labels{
							DocoCDLabels.Repository.Name:        "",
							DocoCDLabels.Deployment.Timestamp:   "2016-01-02T15:04:05Z07:00",
							DocoCDLabels.Deployment.CommitSHA:   "commit_sha2",
							DocoCDLabels.Deployment.ComposeHash: "compose_hash2",
						},
						Replicas: 2,
					},
					"svc1": {
						Labels: Labels{
							DocoCDLabels.Repository.Name:        "",
							DocoCDLabels.Deployment.Timestamp:   "2006-01-02T15:04:05Z07:00",
							DocoCDLabels.Deployment.CommitSHA:   "commit_sha1",
							DocoCDLabels.Deployment.ComposeHash: "compose_hash1",
						},
						Replicas: 1,
					},
				},
			},
		},
		{
			name: "two service with timestamp",
			serviceStatus: map[Service]ServiceStatus{
				"svc1": {
					Labels: Labels{
						DocoCDLabels.Repository.Name:      "repo",
						DocoCDLabels.Deployment.Timestamp: "2006-01-02T15:04:05Z07:00",
					},
					Replicas: 1,
				},
				"svc2": {
					Labels: Labels{
						DocoCDLabels.Repository.Name:        "repo",
						DocoCDLabels.Deployment.Timestamp:   "2016-01-02T15:04:05Z07:00",
						DocoCDLabels.Deployment.CommitSHA:   "commit_sha2",
						DocoCDLabels.Deployment.ComposeHash: "compose_hash2",
					},
					Replicas: 1,
				},
			},
			repoName: "repo",
			want: LatestServiceStatus{
				deploymentCommitSHA:   "commit_sha2",
				deploymentComposeHash: "compose_hash2",
				DeployedStatus: map[Service]ServiceStatus{
					"svc1": {
						Labels: Labels{
							DocoCDLabels.Repository.Name:      "repo",
							DocoCDLabels.Deployment.Timestamp: "2006-01-02T15:04:05Z07:00",
						},
						Replicas: 1,
					},
					"svc2": {
						Labels: Labels{
							DocoCDLabels.Repository.Name:        "repo",
							DocoCDLabels.Deployment.Timestamp:   "2016-01-02T15:04:05Z07:00",
							DocoCDLabels.Deployment.CommitSHA:   "commit_sha2",
							DocoCDLabels.Deployment.ComposeHash: "compose_hash2",
						},
						Replicas: 1,
					},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache := &sync.Map{}
			if tt.cache != nil {
				cache = tt.cache
			}

			got := getLatestServiceStatus(cache, tt.serviceStatus, tt.repoName, tt.deployName)

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("GetLatestServiceState() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetLatestServiceState(t *testing.T) {
	dockerCli, err := CreateDockerCli(false)
	if err != nil {
		t.Fatalf("Failed to create Docker CLI: %v", err)
	}

	if err := swarm.RefreshModeEnabled(t.Context(), dockerCli.Client()); err != nil {
		t.Fatalf("Failed to check if Docker daemon is in Swarm mode: %v", err)
	}

	if swarm.GetModeEnabled() {
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

	latest, err := GetLatestDeployStatus(ctx, stack.Client, repoName, stackName)
	if err != nil {
		t.Fatal(err)
	}

	for svc, want := range map[string]struct {
		Replicas uint64
	}{
		"test2": {Replicas: 2},
		"test":  {Replicas: 2},
		"nginx": {Replicas: 1},
	} {
		got := latest.DeployedStatus[Service(svc)]
		if got.Replicas != want.Replicas {
			t.Errorf("expected running tasks for service %s to be %d, got %d", svc, want.Replicas, got.Replicas)
		}

		if got.SwarmMode != "" {
			t.Errorf("expected swarm mode for service %s to be empty, got %s", svc, got.SwarmMode)
		}
	}
}

func TestGetLatestServiceSwarm(t *testing.T) {
	t.Parallel()

	dockerCli, err := CreateDockerCli(false)
	if err != nil {
		t.Fatalf("Failed to create Docker CLI: %v", err)
	}

	if err := swarm.RefreshModeEnabled(t.Context(), dockerCli.Client()); err != nil {
		t.Fatalf("Failed to check if Docker daemon is in Swarm mode: %v", err)
	}

	if !swarm.GetModeEnabled() {
		t.Skip("Swarm mode is not enabled, skipping test")
	}

	ctx := t.Context()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "test.compose.yaml")

	cfg := `
services:
  replicas:
    image: nginx:latest
    deploy:
      replicas: 2
    environment:
      TZ: Europe/Berlin
  global:
    image: nginx:latest
    deploy:
      mode: global
    environment:
      TZ: Europe/Berlin
  replicated-job:
    image: nginx:latest
    command: 'sh -c "exit 0"'
    deploy:
      replicas: 2
      mode: replicated-job
    environment:
      TZ: Europe/Berlin
  global-job:
    image: nginx:latest
    command: 'sh -c "exit 0"'
    deploy:
      mode: global-job
    environment:
      TZ: Europe/Berlin

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

	swarmStack, opts, err := LoadSwarmStack(dockerCli, project, deployCfg, tmpDir)
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

	dockerClient := dockerCli.Client()

	latest, err := GetLatestDeployStatus(ctx, dockerClient, repoName, stackName)
	if err != nil {
		t.Fatal(err)
	}

	for svc, want := range map[string]struct {
		Replicas uint64
		Mode     swarm.DeployMode
	}{
		"replicas": {
			Replicas: 2,
			Mode:     swarm.DeployModeReplicated,
		},
		"global": {
			Replicas: 0,
			Mode:     swarm.DeployModeGlobal,
		},
		"replicated-job": {
			Replicas: 2,
			Mode:     swarm.DeployModeReplicatedJob,
		},
		"global-job": {
			Replicas: 0,
			Mode:     swarm.DeployModeGlobalJob,
		},
	} {
		got := latest.DeployedStatus[Service(svc)]
		if got.Replicas != want.Replicas {
			t.Errorf("expected running tasks for service %s to be %d, got %d", svc, want.Replicas, got.Replicas)
		}

		if got.SwarmMode != want.Mode {
			t.Errorf("expected swarm mode for service %s to be %s, got %s", svc, want.Mode, got.SwarmMode)
		}
	}

	t.Cleanup(func() {
		ctx := context.Background()

		err = PruneStackConfigs(ctx, dockerClient, stackName)
		if err != nil {
			t.Fatalf("Failed to prune stack configs: %v", err)
		} else {
			t.Logf("Stack configs pruned successfully")
		}

		err = PruneStackSecrets(ctx, dockerClient, stackName)
		if err != nil {
			t.Fatalf("Failed to prune stack secrets: %v", err)
		} else {
			t.Logf("Stack secrets pruned successfully")
		}

		err = RemoveSwarmStack(ctx, dockerCli, stackName)
		if err != nil {
			t.Fatalf("Failed to remove swarm stack: %v", err)
		} else {
			t.Logf("Swarm stack removed successfully")
		}
	})
}

func TestCheckServiceMismatch(t *testing.T) {
	swarmServices := types.Services{
		"replicated": {
			Name:   "replicated",
			Scale:  new(2),
			Deploy: &types.DeployConfig{Mode: string(swarm.DeployModeReplicated)},
		},
		"replicated-job": {
			Name:   "replicated-job",
			Scale:  new(2),
			Deploy: &types.DeployConfig{Mode: string(swarm.DeployModeReplicatedJob)},
		},
		"global": {
			Name:   "global",
			Deploy: &types.DeployConfig{Mode: string(swarm.DeployModeGlobal)},
		},
		"global-job": {
			Name:   "global-job",
			Deploy: &types.DeployConfig{Mode: string(swarm.DeployModeGlobalJob)},
		},
	}

	tests := []struct {
		name            string
		deployed        map[Service]ServiceStatus
		swarmModeEnable bool
		services        types.Services
		want            []ServiceMismatch
	}{
		{
			name: "swarmMode=false, no mismatch",
			deployed: map[Service]ServiceStatus{
				"foo": {Replicas: 1},
			},
			swarmModeEnable: false,
			services: types.Services{
				"foo": {},
			},
			want: nil,
		},
		{
			name: "swarmMode=false, unnecessary service",
			deployed: map[Service]ServiceStatus{
				"foo": {Replicas: 1},
				"bar": {Replicas: 1},
			},
			swarmModeEnable: false,
			services: types.Services{
				"foo": {},
			},
			want: []ServiceMismatch{
				{
					ServiceName: "bar",
					Reasons: []ServiceMismatchReason{
						{
							Reason: ServiceMismatchReasonUnnecessary,
						},
					},
				},
			},
		},
		{
			name: "swarmMode=false, ignore deploy mode",
			deployed: map[Service]ServiceStatus{
				"foo": {Replicas: 1},
			},
			swarmModeEnable: false,
			services: types.Services{
				"foo": {
					Deploy: &types.DeployConfig{
						Mode: "global",
					},
				},
			},
			want: nil,
		},
		{
			name: "swarmMode=false, mismatch replicas for restart always",
			deployed: map[Service]ServiceStatus{
				"foo": {Replicas: 1},
			},
			swarmModeEnable: false,
			services: types.Services{
				"foo": {
					Restart: "always",
					Scale:   new(2),
				},
			},
			want: []ServiceMismatch{
				{
					ServiceName: "foo",
					Reasons: []ServiceMismatchReason{
						{
							Reason: ServiceMismatchReasonReplicas,
							Want:   2,
							Got:    uint64(1),
						},
					},
				},
			},
		},
		{
			name:            "swarmMode=false, no deployed for restart always",
			deployed:        map[Service]ServiceStatus{},
			swarmModeEnable: false,
			services: types.Services{
				"foo": {
					Restart: "always",
					Scale:   new(2),
				},
			},
			want: []ServiceMismatch{
				{
					ServiceName: "foo",
					Reasons: []ServiceMismatchReason{
						{
							Reason: ServiceMismatchReasonNotDeployed,
						},
					},
				},
			},
		},
		{
			name:            "swarmMode=false, no restart policy may remain stopped",
			deployed:        map[Service]ServiceStatus{},
			swarmModeEnable: false,
			services: types.Services{
				"job": {},
			},
			want: nil,
		},
		{
			name:            "swarmMode=false, restart on-failure may remain stopped",
			deployed:        map[Service]ServiceStatus{},
			swarmModeEnable: false,
			services: types.Services{
				"job": {
					Restart: "on-failure",
				},
			},
			want: nil,
		},
		{
			name:            "swarmMode=false, restart no may remain stopped",
			deployed:        map[Service]ServiceStatus{},
			swarmModeEnable: false,
			services: types.Services{
				"job": {
					Restart: "no",
				},
			},
			want: nil,
		},
		{
			name: "swarmMode=false, restart always should stay running",
			deployed: map[Service]ServiceStatus{
				"web": {Replicas: 0},
			},
			swarmModeEnable: false,
			services: types.Services{
				"web": {
					Restart: "always",
				},
			},
			want: []ServiceMismatch{
				{
					ServiceName: "web",
					Reasons: []ServiceMismatchReason{
						{
							Reason: ServiceMismatchReasonReplicas,
							Want:   1,
							Got:    uint64(0),
						},
					},
				},
			},
		},
		{
			name: "swarmMode=false, restart unless-stopped should stay running",
			deployed: map[Service]ServiceStatus{
				"web": {Replicas: 0},
			},
			swarmModeEnable: false,
			services: types.Services{
				"web": {
					Restart: "unless-stopped",
				},
			},
			want: []ServiceMismatch{
				{
					ServiceName: "web",
					Reasons: []ServiceMismatchReason{
						{
							Reason: ServiceMismatchReasonReplicas,
							Want:   1,
							Got:    uint64(0),
						},
					},
				},
			},
		},
		{
			name: "swarmMode=true, no missing",
			deployed: map[Service]ServiceStatus{
				"foo":         {Replicas: 1, SwarmMode: swarm.DeployModeReplicated},
				"replicated":  {Replicas: 1, SwarmMode: swarm.DeployModeReplicated},
				"replicated2": {Replicas: 1, SwarmMode: swarm.DeployModeReplicated},
			},
			swarmModeEnable: true,
			services: types.Services{
				"foo": {
					Deploy: &types.DeployConfig{Mode: string(swarm.DeployModeReplicated), Replicas: new(1)},
				},
				"replicated": {
					Name: "replicated",
				},
				"replicated2": {
					Name:   "replicated2",
					Deploy: &types.DeployConfig{Replicas: new(1)},
				},
			},
			want: nil,
		},
		{
			name: "swarmMode=true, unnecessary service",
			deployed: map[Service]ServiceStatus{
				"foo":        {Replicas: 1, SwarmMode: swarm.DeployModeReplicated},
				"replicated": {Replicas: 1, SwarmMode: swarm.DeployModeReplicated},
			},
			swarmModeEnable: true,
			services: types.Services{
				"foo": {
					Deploy: &types.DeployConfig{Mode: string(swarm.DeployModeReplicated), Replicas: new(1)},
				},
			},
			want: []ServiceMismatch{
				{
					ServiceName: "replicated",
					Reasons: []ServiceMismatchReason{
						{
							Reason: ServiceMismatchReasonUnnecessary,
						},
					},
				},
			},
		},
		{
			name: "swarmMode=true, no missing",
			deployed: map[Service]ServiceStatus{
				"replicated":     {Replicas: 2, SwarmMode: swarm.DeployModeReplicated},
				"replicated-job": {Replicas: 2, SwarmMode: swarm.DeployModeReplicatedJob},
				// global ignore replicas
				"global":     {Replicas: 2, SwarmMode: swarm.DeployModeGlobal},
				"global-job": {Replicas: 2, SwarmMode: swarm.DeployModeGlobalJob},
			},
			swarmModeEnable: true,
			services:        swarmServices,
			want:            nil,
		},
		{
			name: "swarmMode=true, mismatch",
			deployed: map[Service]ServiceStatus{
				"replicated":     {Replicas: 2, SwarmMode: swarm.DeployModeReplicatedJob},
				"replicated-job": {Replicas: 1, SwarmMode: swarm.DeployModeReplicatedJob},
				"global-job":     {Replicas: 2, SwarmMode: swarm.DeployModeGlobalJob},
			},
			swarmModeEnable: true,
			services:        swarmServices,
			want: []ServiceMismatch{
				{
					ServiceName: "replicated",
					Reasons: []ServiceMismatchReason{
						{
							Reason: ServiceMismatchReasonSwarmMode,
							Want:   swarm.DeployModeReplicated,
							Got:    swarm.DeployModeReplicatedJob,
						},
					},
				},
				{
					ServiceName: "replicated-job",
					Reasons: []ServiceMismatchReason{
						{
							Reason: ServiceMismatchReasonReplicas,
							Want:   2,
							Got:    uint64(1),
						},
					},
				},
				{
					ServiceName: "global",
					Reasons: []ServiceMismatchReason{
						{
							Reason: ServiceMismatchReasonNotDeployed,
						},
					},
				},
			},
		},
	}
	cmpFunc := func(a, b ServiceMismatch) int {
		return strings.Compare(a.ServiceName, b.ServiceName)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CheckServiceMismatch(tt.swarmModeEnable, tt.deployed, tt.services)
			slices.SortFunc(got, cmpFunc)
			slices.SortFunc(tt.want, cmpFunc)

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("CheckServiceMismatch() = %#v, want %#v", got, tt.want)
			}
		})
	}
}
