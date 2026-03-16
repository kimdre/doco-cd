package docker

import (
	"log"
	"path/filepath"
	"testing"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/moby/moby/client"

	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"

	"github.com/kimdre/doco-cd/internal/test"

	"github.com/kimdre/doco-cd/internal/docker/swarm"

	"github.com/kimdre/doco-cd/internal/git"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func TestDeploySwarmStack(t *testing.T) {
	t.Parallel()

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

	resolvedSecrets := secrettypes.ResolvedSecrets{}

	project, err := LoadCompose(t.Context(), tmpDir, stackName, []string{filePath}, []string{".env"}, []string{}, resolvedSecrets)
	if err != nil {
		t.Fatal(err)
	}

	deployConfigs, err := config.GetDeployConfigs(tmpDir, c.DeployConfigBaseDir, stackName, customTarget, p.Ref)
	if err != nil {
		t.Fatal(err)
	}

	ctx := t.Context()

	cfg, opts, err := LoadSwarmStack(&dockerCli, project, deployConfigs[0], resolvedSecrets, tmpDir)
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
