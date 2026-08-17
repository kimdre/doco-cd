package docker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/containerd/errdefs"
	composetypes "github.com/docker/cli/cli/compose/types"
	"github.com/go-git/go-git/v5/plumbing"
	swarmTypes "github.com/moby/moby/api/types/swarm"
	mobyclient "github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/encryption"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/test"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func TestDeploySwarmStack(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

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

	stackName := test.ConvertTestName(t.Name())

	tmpDir := t.TempDir()

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

	repoPath := worktree.Filesystem.Root()
	filePath := filepath.Join(repoPath, "docker-compose.yml")

	project, err := LoadCompose(t.Context(), nil, tmpDir, tmpDir, stackName, []string{filePath}, []string{".env"}, []string{}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}

	deployConfigs, err := deploy.GetConfigs(tmpDir, c.DeployConfigBaseDir, customTarget, p.Ref, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx := t.Context()

	cfg, opts, err := LoadSwarmStack(dockerCli, project, deployConfigs[0], tmpDir)
	if err != nil {
		t.Fatalf("Failed to load swarm stack: %v", err)
	}

	commit := "e8e2d31f0fa0c924400b3bac751b6c2c6930adb1"

	projectHash, err := ProjectHash(project)
	if err != nil {
		t.Fatalf("failed to get project hash: %v", err)
	}

	err = retry.New(
		retry.Attempts(5),
		retry.Delay(2*time.Second),
		retry.Context(ctx),
	).Do(
		func() error {
			timestamp := time.Now().UTC().Format(time.RFC3339)
			addSwarmServiceLabels(cfg, project, deployConfigs[0], &p, tmpDir, "dev", timestamp, commit, projectHash)
			addSwarmVolumeLabels(cfg, deployConfigs[0], &p, tmpDir)
			addSwarmConfigLabels(cfg, deployConfigs[0], &p, tmpDir, "dev", timestamp, commit)
			addSwarmSecretLabels(cfg, deployConfigs[0], &p, tmpDir, "dev", timestamp, commit)

			return DeploySwarmStack(ctx, dockerCli, cfg, opts)
		},
	)
	if err != nil {
		t.Fatalf("Failed to deploy swarm stack: %v", err)
	}

	t.Logf("Swarm stack deployed successfully")

	dockerClient := dockerCli.Client()

	t.Cleanup(func() {
		err = dockerClient.Close()
		if err != nil {
			t.Logf("Failed to close Docker client: %v", err)
		}
	})

	err = PruneStackConfigs(t.Context(), dockerClient, stackName, 0)
	if err != nil {
		t.Fatalf("Failed to prune stack configs: %v", err)
	} else {
		t.Logf("Stack configs pruned successfully")
	}

	err = PruneStackSecrets(t.Context(), dockerClient, stackName, 0)
	if err != nil {
		t.Fatalf("Failed to prune stack secrets: %v", err)
	} else {
		t.Logf("Stack secrets pruned successfully")
	}

	err = RemoveSwarmStack(t.Context(), dockerCli, deployConfigs[0].Name)
	if err != nil {
		t.Fatalf("Failed to remove swarm stack: %v", err)
	}

	t.Logf("Swarm stack removed successfully")
}

func TestSwarmConfigAndSecretRotationRetention(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

	dockerCli, err := CreateDockerCli(false)
	if err != nil {
		t.Fatalf("failed to create Docker CLI: %v", err)
	}

	if err := swarm.RefreshModeEnabled(t.Context(), dockerCli.Client()); err != nil {
		t.Fatalf("failed to check if Docker daemon is in Swarm mode: %v", err)
	}

	if !swarm.GetModeEnabled() {
		t.Skip("Swarm mode is not enabled, skipping test")
	}

	testCases := []struct {
		name             string
		keepOldRevisions int
	}{
		{name: "disable-pruning", keepOldRevisions: -1},
		{name: "keep-no-old-revisions", keepOldRevisions: 0},
		{name: "keep-one-old-revision", keepOldRevisions: 1},
		{name: "keep-two-old-revisions", keepOldRevisions: 2},
		{name: "keep-six-old-revisions", keepOldRevisions: 6},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			apiClient := dockerCli.Client()
			stackName := fmt.Sprintf("swarm-retention-%d-%d", tc.keepOldRevisions, time.Now().UnixNano())
			resourcePrefix := fmt.Sprintf("ret-%d-%x", tc.keepOldRevisions, time.Now().UnixNano())

			var latestConfigName, latestSecretName string

			const revisions = 3

			for revision := 1; revision <= revisions; revision++ {
				configName := fmt.Sprintf("%s-cfg_%08x", resourcePrefix, revision)
				secretName := fmt.Sprintf("%s-sec_%08x", resourcePrefix, revision)

				createConfigResp, err := apiClient.ConfigCreate(ctx, mobyclient.ConfigCreateOptions{
					Spec: swarmTypes.ConfigSpec{
						Annotations: swarmTypes.Annotations{
							Name: configName,
							Labels: map[string]string{
								swarm.StackNamespaceLabel: stackName,
							},
						},
						Data: fmt.Appendf(nil, "config revision %d", revision),
					},
				})
				if err != nil {
					t.Fatalf("failed to create config revision %d: %v", revision, err)
				}

				configID := createConfigResp.ID
				configNameForCleanup := configName

				t.Cleanup(func() {
					cleanupCtx := context.Background()

					_, err := apiClient.ConfigRemove(cleanupCtx, configID, mobyclient.ConfigRemoveOptions{})
					if err != nil && !errdefs.IsNotFound(err) {
						t.Logf("cleanup: failed to remove config %q (%s): %v", configNameForCleanup, configID, err)
					}
				})

				createSecretResp, err := apiClient.SecretCreate(ctx, mobyclient.SecretCreateOptions{
					Spec: swarmTypes.SecretSpec{
						Annotations: swarmTypes.Annotations{
							Name: secretName,
							Labels: map[string]string{
								swarm.StackNamespaceLabel: stackName,
							},
						},
						Data: fmt.Appendf(nil, "secret revision %d", revision),
					},
				})
				if err != nil {
					t.Fatalf("failed to create secret revision %d: %v", revision, err)
				}

				secretID := createSecretResp.ID
				secretNameForCleanup := secretName

				t.Cleanup(func() {
					cleanupCtx := context.Background()

					_, err := apiClient.SecretRemove(cleanupCtx, secretID, mobyclient.SecretRemoveOptions{})
					if err != nil && !errdefs.IsNotFound(err) {
						t.Logf("cleanup: failed to remove secret %q (%s): %v", secretNameForCleanup, secretID, err)
					}
				})

				latestConfigName = configName
				latestSecretName = secretName
			}

			if err := PruneStackConfigs(ctx, apiClient, stackName, tc.keepOldRevisions); err != nil {
				t.Fatalf("failed to prune stack configs with keepOldRevisions=%d: %v", tc.keepOldRevisions, err)
			}

			if err := PruneStackSecrets(ctx, apiClient, stackName, tc.keepOldRevisions); err != nil {
				t.Fatalf("failed to prune stack secrets with keepOldRevisions=%d: %v", tc.keepOldRevisions, err)
			}

			configs, err := GetLabeledConfigs(ctx, apiClient, swarm.StackNamespaceLabel, stackName)
			if err != nil {
				t.Fatalf("failed to list stack configs: %v", err)
			}

			secrets, err := GetLabeledSecrets(ctx, apiClient, swarm.StackNamespaceLabel, stackName)
			if err != nil {
				t.Fatalf("failed to list stack secrets: %v", err)
			}

			expectedRevisions := revisions
			if tc.keepOldRevisions >= 0 {
				if keep := tc.keepOldRevisions + 1; keep < expectedRevisions {
					expectedRevisions = keep
				}
			}

			if len(configs) != expectedRevisions {
				t.Fatalf("expected %d config revisions after prune, got %d", expectedRevisions, len(configs))
			}

			if len(secrets) != expectedRevisions {
				t.Fatalf("expected %d secret revisions after prune, got %d", expectedRevisions, len(secrets))
			}

			if !hasConfigName(configs, latestConfigName) {
				t.Fatalf("expected latest config %q to be retained after prune", latestConfigName)
			}

			if !hasSecretName(secrets, latestSecretName) {
				t.Fatalf("expected latest secret %q to be retained after prune", latestSecretName)
			}
		})
	}
}

func hasConfigName(configs []swarmTypes.Config, name string) bool {
	for _, c := range configs {
		if c.Spec.Name == name {
			return true
		}
	}

	return false
}

func hasSecretName(secrets []swarmTypes.Secret, name string) bool {
	for _, s := range secrets {
		if s.Spec.Name == name {
			return true
		}
	}

	return false
}

func TestSetConfigHashPrefixes_NameTooLong(t *testing.T) {
	longName := strings.Repeat("a", swarmBaseNameMaxLen+1)

	stack := &composetypes.Config{
		Configs: map[string]composetypes.ConfigObjConfig{
			"cfg": {
				Name: longName,
				File: filepath.Join(t.TempDir(), "cfg"),
			},
		},
	}

	if err := os.WriteFile(stack.Configs["cfg"].File, []byte("x"), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}

	err := SetConfigHashPrefixes(stack, "ns")
	if err == nil {
		t.Fatal("expected error for too-long config name, got nil")
	}

	if !strings.Contains(err.Error(), "too long") {
		t.Fatalf("expected length error, got %v", err)
	}
}

func TestSetSecretHashPrefixes_NameTooLong(t *testing.T) {
	longName := strings.Repeat("s", swarmBaseNameMaxLen+1)

	stack := &composetypes.Config{
		Secrets: map[string]composetypes.SecretConfig{
			"sec": {
				Name: longName,
				File: filepath.Join(t.TempDir(), "sec"),
			},
		},
	}

	if err := os.WriteFile(stack.Secrets["sec"].File, []byte("x"), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}

	err := SetSecretHashPrefixes(stack, "ns")
	if err == nil {
		t.Fatal("expected error for too-long secret name, got nil")
	}

	if !strings.Contains(err.Error(), "too long") {
		t.Fatalf("expected length error, got %v", err)
	}
}

func TestValidateSwarmBaseNameLength_Boundaries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		resourceType string
		value        string
		expectErr    bool
	}{
		{
			name:         "empty name rejected",
			resourceType: "config",
			value:        "",
			expectErr:    true,
		},
		{
			name:         "max-length allowed",
			resourceType: "secret",
			value:        strings.Repeat("a", swarmBaseNameMaxLen),
			expectErr:    false,
		},
		{
			name:         "over-limit rejected",
			resourceType: "config",
			value:        strings.Repeat("b", swarmBaseNameMaxLen+1),
			expectErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateSwarmBaseNameLength(tt.value, tt.resourceType)
			if tt.expectErr && err == nil {
				t.Fatal("expected error, got nil")
			}

			if !tt.expectErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}

func TestSetConfigHashPrefixes_NameAtLimit(t *testing.T) {
	base := strings.Repeat("c", swarmBaseNameMaxLen)

	stack := &composetypes.Config{
		Configs: map[string]composetypes.ConfigObjConfig{
			"cfg": {
				Name: base,
				File: filepath.Join(t.TempDir(), "cfg"),
			},
		},
	}

	if err := os.WriteFile(stack.Configs["cfg"].File, []byte("x"), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}

	if err := SetConfigHashPrefixes(stack, "ns"); err != nil {
		t.Fatalf("expected name at limit to pass, got %v", err)
	}

	got := stack.Configs["cfg"].Name
	if len(got) != swarmResourceNameMaxLen {
		t.Fatalf("expected final config name length %d, got %d (%q)", swarmResourceNameMaxLen, len(got), got)
	}

	if !strings.HasPrefix(got, base+"_") {
		t.Fatalf("expected final config name to keep base+hash suffix, got %q", got)
	}
}

func TestSetSecretHashPrefixes_NameAtLimit(t *testing.T) {
	base := strings.Repeat("s", swarmBaseNameMaxLen)

	stack := &composetypes.Config{
		Secrets: map[string]composetypes.SecretConfig{
			"sec": {
				Name: base,
				File: filepath.Join(t.TempDir(), "sec"),
			},
		},
	}

	if err := os.WriteFile(stack.Secrets["sec"].File, []byte("x"), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}

	if err := SetSecretHashPrefixes(stack, "ns"); err != nil {
		t.Fatalf("expected name at limit to pass, got %v", err)
	}

	got := stack.Secrets["sec"].Name
	if len(got) != swarmResourceNameMaxLen {
		t.Fatalf("expected final secret name length %d, got %d (%q)", swarmResourceNameMaxLen, len(got), got)
	}

	if !strings.HasPrefix(got, base+"_") {
		t.Fatalf("expected final secret name to keep base+hash suffix, got %q", got)
	}
}
