package docker

import (
	"log"
	"path/filepath"
	"testing"

	"github.com/docker/docker/client"

	"github.com/kimdre/doco-cd/internal/docker/swarm"

	"github.com/kimdre/doco-cd/internal/git"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func TestDeploySwarmStack(t *testing.T) {
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

	tmpDir := t.TempDir()

	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatalf("Failed to get app config: %v", err)
	}

	p := webhook.ParsedPayload{
		Ref:       git.SwarmModeBranch,
		CommitSHA: "244b6f9a5b3dc546ab3822d9c0744846f539c6ef",
		Name:      "test",
		FullName:  "kimdre/doco-cd_tests",
		CloneURL:  git.GetAuthUrl(cloneUrlTest, c.AuthType, c.GitAccessToken),
		Private:   true,
	}

	t.Chdir(tmpDir)

	repo, err := git.CloneRepository(tmpDir, p.CloneURL, git.SwarmModeBranch, c.SkipTLSVerification, c.HttpProxy)
	if err != nil {
		t.Fatalf("Failed to clone repository: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	repoPath := worktree.Filesystem.Root()
	filePath := filepath.Join(repoPath, "docker-compose.yml")

	project, err := LoadCompose(t.Context(), tmpDir, projectName, []string{filePath}, []string{}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}

	deployConfigs, err := config.GetDeployConfigs(tmpDir, projectName, customTarget, p.Ref)
	if err != nil {
		t.Fatal(err)
	}

	err = DeploySwarmStack(t.Context(), dockerCli, project, deployConfigs[0], p, tmpDir, "e8e2d31f0fa0c924400b3bac751b6c2c6930adb1", "dev", "", map[string]string{})
	if err != nil {
		t.Fatalf("Failed to deploy swarm stack: %v", err)
	} else {
		t.Logf("Swarm stack deployed successfully")
	}

	dockerClient, _ := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)

	t.Cleanup(func() {
		err = dockerClient.Close()
		if err != nil {
			t.Logf("Failed to close Docker client: %v", err)
		}
	})

	err = PruneStackConfigs(t.Context(), dockerClient, projectName)
	if err != nil {
		t.Fatalf("Failed to prune stack configs: %v", err)
	} else {
		t.Logf("Stack configs pruned successfully")
	}

	err = PruneStackSecrets(t.Context(), dockerClient, projectName)
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
