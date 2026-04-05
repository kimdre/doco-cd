package docker

import (
	"log"
	"path/filepath"
	"testing"
	"time"

	"github.com/avast/retry-go/v5"
	composetypes "github.com/docker/cli/cli/compose/types"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/encryption"
	"github.com/kimdre/doco-cd/internal/test"

	"github.com/kimdre/doco-cd/internal/docker/swarm"

	"github.com/kimdre/doco-cd/internal/git"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func TestDeploySwarmStack(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

	dockerCli, err := CreateDockerCli(false, false)
	if err != nil {
		t.Fatalf("Failed to create Docker CLI: %v", err)
	}

	swarm.ModeEnabled, err = swarm.CheckDaemonIsSwarmManager(t.Context(), dockerCli)
	if err != nil {
		log.Fatalf("Failed to check if Docker daemon is in Swarm mode: %v", err)
	}

	if !swarm.ModeEnabled {
		t.Skip("Swarm mode is not enabled, skipping test")
	}

	stackName := test.ConvertTestName(t.Name())

	tmpDir := t.TempDir()

	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatalf("Failed to get app config: %v", err)
	}

	p := webhook.ParsedPayload{
		Ref:       git.SwarmModeBranch,
		CommitSHA: "244b6f9a5b3dc546ab3822d9c0744846f539c6ef",
		Name:      stackName,
		FullName:  "kimdre/doco-cd_tests",
		CloneURL:  cloneUrlTest,
		Private:   true,
	}

	auth, err := git.GetAuthMethod(p.CloneURL, c.SSHPrivateKey, c.SSHPrivateKeyPassphrase, c.GitAccessToken)
	if err != nil {
		t.Fatalf("Failed to get auth method: %v", err)
	}

	if auth != nil {
		t.Logf("Using auth method: %s", auth.Name())
	} else {
		t.Log("No auth method configured, using anonymous access")
	}

	repo, err := git.CloneRepository(tmpDir, p.CloneURL, git.SwarmModeBranch, c.SkipTLSVerification, c.HttpProxy, auth, c.GitCloneSubmodules)
	if err != nil {
		t.Fatalf("Failed to clone repository: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	repoPath := worktree.Filesystem.Root()
	filePath := filepath.Join(repoPath, "docker-compose.yml")

	project, err := LoadCompose(t.Context(), tmpDir, tmpDir, stackName, []string{filePath}, []string{".env"}, []string{}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}

	deployConfigs, err := config.GetDeployConfigs(tmpDir, c.DeployConfigBaseDir, stackName, customTarget, p.Ref)
	if err != nil {
		t.Fatal(err)
	}

	ctx := t.Context()

	cfg, opts, err := LoadSwarmStack(&dockerCli, project, deployConfigs[0], tmpDir)
	if err != nil {
		t.Fatalf("Failed to load swarm stack: %v", err)
	}

	commit := "e8e2d31f0fa0c924400b3bac751b6c2c6930adb1"

	projectHash, err := ProjectHash(project)
	if err != nil {
		t.Fatalf("failed to get project hash: %v", err)
	}

	err = retry.New(
		// retry.RetryIf(func(err error) bool {
		//	if err == nil {
		//		return false
		//	}
		//	errStr := err.Error()
		//	return strings.Contains(errStr, "network") && strings.Contains(errStr, "not found")
		// }),
		retry.Attempts(5),
		retry.Delay(2*time.Second),
		retry.Context(ctx),
	).Do(
		func() error {
			timestamp := time.Now().UTC().Format(time.RFC3339)
			addSwarmServiceLabels(cfg, deployConfigs[0], &p, tmpDir, "dev", timestamp, commit, projectHash)
			addSwarmVolumeLabels(cfg, deployConfigs[0], &p, tmpDir, "dev", timestamp, commit)
			addSwarmConfigLabels(cfg, deployConfigs[0], &p, tmpDir, "dev", timestamp, commit)
			addSwarmSecretLabels(cfg, deployConfigs[0], &p, tmpDir, "dev", timestamp, commit)

			return DeploySwarmStack(ctx, dockerCli, cfg, opts)
		},
	)
	if err != nil {
		t.Fatalf("Failed to deploy swarm stack: %v", err)
	}

	t.Logf("Swarm stack deployed successfully")

	dockerClient, _ := client.New(
		client.FromEnv,
	)

	t.Cleanup(func() {
		err = dockerClient.Close()
		if err != nil {
			t.Logf("Failed to close Docker client: %v", err)
		}
	})

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

	err = RemoveSwarmStack(t.Context(), dockerCli, deployConfigs[0].Name)
	if err != nil {
		t.Fatalf("Failed to remove swarm stack: %v", err)
	} else {
		t.Logf("Swarm stack removed successfully")
	}
}

func TestAddSwarmServiceLabels(t *testing.T) {
	stack := &composetypes.Config{
		Services: []composetypes.ServiceConfig{
			{Name: "web", Labels: map[string]string{"user.label": "keep"}},
			{Name: "db"},
		},
	}

	deployConfig := &config.DeployConfig{
		Name:      "my-stack",
		Reference: "refs/heads/main",
	}

	payload := &webhook.ParsedPayload{
		CommitSHA: "abc123",
		FullName:  "user/repo",
		WebURL:    "https://github.com/user/repo",
	}

	addSwarmServiceLabels(stack, deployConfig, payload, "/work", "1.0.0", "2025-01-01T00:00:00Z", "def456", "hash123")

	for _, s := range stack.Services {
		// Volatile labels should be in Deploy.Labels only
		if _, ok := s.Labels[DocoCDLabels.Deployment.Timestamp]; ok {
			t.Errorf("service %s: timestamp should not be in container labels", s.Name)
		}

		if _, ok := s.Labels[DocoCDLabels.Deployment.Trigger]; ok {
			t.Errorf("service %s: trigger should not be in container labels", s.Name)
		}

		if s.Deploy.Labels[DocoCDLabels.Deployment.Timestamp] != "2025-01-01T00:00:00Z" {
			t.Errorf("service %s: expected timestamp in deploy labels", s.Name)
		}

		if s.Deploy.Labels[DocoCDLabels.Deployment.Trigger] != "abc123" {
			t.Errorf("service %s: expected trigger in deploy labels", s.Name)
		}

		// Stable labels should be in container labels
		if s.Labels[DocoCDLabels.Deployment.Name] != "my-stack" {
			t.Errorf("service %s: expected name in container labels", s.Name)
		}

		if s.Labels[DocoCDLabels.Deployment.CommitSHA] != "def456" {
			t.Errorf("service %s: expected commit SHA in container labels", s.Name)
		}
	}

	// Existing user labels should be preserved
	if stack.Services[0].Labels["user.label"] != "keep" {
		t.Error("existing user label was overwritten")
	}
}

func TestSwarmServiceLabelsStability(t *testing.T) {
	stack := &composetypes.Config{
		Services: []composetypes.ServiceConfig{
			{Name: "web"},
		},
	}

	deployConfig := &config.DeployConfig{
		Name:      "my-stack",
		Reference: "refs/heads/main",
	}

	payload := &webhook.ParsedPayload{
		CommitSHA: "abc123",
		FullName:  "user/repo",
	}

	addSwarmServiceLabels(stack, deployConfig, payload, "/work", "1.0.0", "2025-01-01T00:00:00Z", "commit1", "hash1")

	// Snapshot container labels after first call
	firstLabels := make(map[string]string)
	for k, v := range stack.Services[0].Labels {
		firstLabels[k] = v
	}

	// Call again with different timestamp but same commit
	addSwarmServiceLabels(stack, deployConfig, payload, "/work", "1.0.0", "2025-06-15T12:00:00Z", "commit1", "hash1")

	// Container labels should be identical (timestamp is not in container labels)
	for k, v := range stack.Services[0].Labels {
		if firstLabels[k] != v {
			t.Errorf("container label %s changed: %q -> %q", k, firstLabels[k], v)
		}
	}

	// Deploy labels should reflect the new timestamp
	if stack.Services[0].Deploy.Labels[DocoCDLabels.Deployment.Timestamp] != "2025-06-15T12:00:00Z" {
		t.Error("deploy timestamp was not updated")
	}
}
