package docker

import (
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/go-git/go-git/v5/plumbing"
	swarmTypes "github.com/moby/moby/api/types/swarm"
	dockerClient "github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/encryption"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/test"
	"github.com/kimdre/doco-cd/internal/webhook"
)

// runningStackTaskIDs returns the IDs of all running tasks of a stack.
func runningStackTaskIDs(ctx context.Context, t *testing.T, cli dockerClient.APIClient, stackName string) []string {
	t.Helper()

	tasks, err := cli.TaskList(ctx, dockerClient.TaskListOptions{
		Filters: make(dockerClient.Filters).Add("label", swarm.StackNamespaceLabel+"="+stackName),
	})
	if err != nil {
		t.Fatalf("Failed to list tasks of stack %s: %v", stackName, err)
	}

	var ids []string

	for _, task := range tasks.Items {
		if task.DesiredState == swarmTypes.TaskStateRunning {
			ids = append(ids, task.ID)
		}
	}

	slices.Sort(ids)

	return ids
}

// TestDeploySwarmStackIsIdempotent verifies that redeploying an unchanged stack does
// not recreate its tasks.
//
// Deployment metadata such as the timestamp changes on every deployment. As long as it
// is stored in the service spec, swarm leaves the running tasks alone. Storing it in
// the task template instead would replace every task of every service in the stack on
// each deployment, see https://github.com/kimdre/doco-cd/issues/1153.
func TestDeploySwarmStackIsIdempotent(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

	dockerCli, err := CreateDockerCli(false)
	if err != nil {
		t.Fatalf("Failed to create Docker CLI: %v", err)
	}

	if err = swarm.RefreshModeEnabled(t.Context(), dockerCli.Client()); err != nil {
		t.Fatalf("Failed to check if Docker daemon is in Swarm mode: %v", err)
	}

	if !swarm.GetModeEnabled() {
		t.Skip("Swarm mode is not enabled, skipping test")
	}

	stackName := test.ConvertTestName(t.Name())
	tmpDir := t.TempDir()
	ctx := t.Context()

	c, err := app.GetConfig()
	if err != nil {
		t.Fatalf("Failed to get app config: %v", err)
	}

	p := webhook.ParsedPayload{
		Ref:       git.SwarmModeBranch,
		CommitSHA: plumbing.NewHash("244b6f9a5b3dc546ab3822d9c0744846f539c6ef"),
		Name:      stackName,
		FullName:  "kimdre/doco-cd_tests",
		CloneURL:  cloneUrlTest,
		Private:   false,
	}

	repo, err := git.CloneOrUpdateRepository(slog.Default(), p.CloneURL, p.Ref, tmpDir, tmpDir,
		p.Private, c.SSHPrivateKey, c.SSHPrivateKeyPassphrase, c.GitAccessToken, c.SkipTLSVerification,
		c.HttpProxy, c.GitCloneSubmodules, 0)
	if err != nil {
		t.Fatal(err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	filePath := filepath.Join(worktree.Filesystem.Root(), "docker-compose.yml")

	project, err := LoadCompose(ctx, nil, tmpDir, tmpDir, stackName, []string{filePath}, []string{".env"}, []string{}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}

	deployConfigs, err := deploy.GetConfigs(tmpDir, c.DeployConfigBaseDir, customTarget, p.Ref, nil)
	if err != nil {
		t.Fatal(err)
	}

	deployConfigs[0].Name = stackName

	projectHash, err := ProjectHash(project)
	if err != nil {
		t.Fatalf("Failed to get project hash: %v", err)
	}

	const commit = "e8e2d31f0fa0c924400b3bac751b6c2c6930adb1"

	deployStack := func() error {
		cfg, opts, loadErr := LoadSwarmStack(dockerCli, project, deployConfigs[0], tmpDir)
		if loadErr != nil {
			return loadErr
		}

		timestamp := time.Now().UTC().Format(time.RFC3339)
		addSwarmServiceLabels(cfg, deployConfigs[0], &p, tmpDir, "dev", timestamp, commit, projectHash)
		addSwarmVolumeLabels(cfg, deployConfigs[0], &p, tmpDir)
		addSwarmConfigLabels(cfg, deployConfigs[0], &p, tmpDir, "dev", timestamp, commit)
		addSwarmSecretLabels(cfg, deployConfigs[0], &p, tmpDir, "dev", timestamp, commit)

		return retry.New(
			retry.Attempts(5),
			retry.Delay(2*time.Second),
			retry.Context(ctx),
		).Do(func() error {
			return DeploySwarmStack(ctx, dockerCli, cfg, opts)
		})
	}

	t.Cleanup(func() {
		if err = RemoveSwarmStack(context.Background(), dockerCli, stackName); err != nil {
			t.Logf("Failed to remove swarm stack: %v", err)
		}
	})

	if err = deployStack(); err != nil {
		t.Fatalf("Failed to deploy swarm stack: %v", err)
	}

	before := runningStackTaskIDs(ctx, t, dockerCli.Client(), stackName)
	if len(before) == 0 {
		t.Fatal("Expected the stack to have running tasks after the first deployment")
	}

	timestampBefore := stackDeploymentTimestamps(ctx, t, dockerCli.Client(), stackName)

	// The deployment timestamp has a resolution of one second, so make sure the
	// second deployment gets a different one.
	time.Sleep(2 * time.Second)

	if err = deployStack(); err != nil {
		t.Fatalf("Failed to redeploy swarm stack: %v", err)
	}

	after := runningStackTaskIDs(ctx, t, dockerCli.Client(), stackName)

	if !slices.Equal(before, after) {
		t.Fatalf("Redeploying an unchanged stack recreated its tasks: %v -> %v", before, after)
	}

	// The tasks must survive precisely because the metadata moved out of the task
	// template, not because it stopped being written.
	timestampAfter := stackDeploymentTimestamps(ctx, t, dockerCli.Client(), stackName)

	for service, timestamp := range timestampBefore {
		if timestampAfter[service] == timestamp {
			t.Errorf("service %q: expected the deployment timestamp to be updated, got %q", service, timestamp)
		}
	}
}

// stackDeploymentTimestamps returns the deployment timestamp of every service of a
// stack and asserts that it is stored as a service label rather than in the task
// template.
func stackDeploymentTimestamps(ctx context.Context, t *testing.T, cli dockerClient.APIClient, stackName string) map[string]string {
	t.Helper()

	services, err := swarm.GetStackServices(ctx, cli, stackName)
	if err != nil {
		t.Fatalf("Failed to get services of stack %s: %v", stackName, err)
	}

	if len(services) == 0 {
		t.Fatalf("Expected stack %s to have services", stackName)
	}

	timestamps := make(map[string]string, len(services))

	for _, service := range services {
		timestamp := service.Spec.Labels[DocoCDLabels.Deployment.Timestamp]
		if timestamp == "" {
			t.Errorf("service %q: expected a deployment timestamp in the service labels", service.Spec.Name)
		}

		if service.Spec.TaskTemplate.ContainerSpec != nil {
			if _, ok := service.Spec.TaskTemplate.ContainerSpec.Labels[DocoCDLabels.Deployment.Timestamp]; ok {
				t.Errorf("service %q: deployment timestamp must not be set in the task template", service.Spec.Name)
			}
		}

		timestamps[service.Spec.Name] = timestamp
	}

	return timestamps
}
