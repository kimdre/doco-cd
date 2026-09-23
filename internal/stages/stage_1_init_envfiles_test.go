package stages

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/deploy"
)

// initEnvFileTestRepo creates a local (non-bare) git repository at path whose initial commit on
// "main" contains files, keyed by repository-relative path. Parent directories are created as
// needed so stacks can be committed under a nested working directory.
func initEnvFileTestRepo(t *testing.T, path string, files map[string]string) string {
	t.Helper()

	repo, err := gogit.PlainInit(path, false)
	if err != nil {
		t.Fatalf("failed to init local test repo: %v", err)
	}

	if err := repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatalf("failed to set HEAD to main: %v", err)
	}

	for name, content := range files {
		absPath := filepath.Join(path, name)

		if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
			t.Fatalf("failed to create directory for %s: %v", name, err)
		}

		if err := os.WriteFile(absPath, []byte(content), 0o600); err != nil {
			t.Fatalf("failed to write %s: %v", name, err)
		}
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	if err := wt.AddWithOptions(&gogit.AddOptions{All: true}); err != nil {
		t.Fatalf("failed to stage test files: %v", err)
	}

	if _, err := wt.Commit("initial commit", &gogit.CommitOptions{
		Author: &object.Signature{Name: "envfile-test", Email: "envfile-test@example.com", When: time.Now()},
	}); err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	return "file://" + path
}

// TestRunInitStageLoadsEnvAndSecretsWithoutRepositoryUrl is the regression test for deploy configs
// that live in the same repository that triggered the deployment (RepositoryUrl is empty). Loading
// used to be gated behind RepositoryUrl != "", so dotenv and external secrets files were silently
// skipped for by far the most common setup - most visibly when they sit in a working directory
// rather than the repository root.
func TestRunInitStageLoadsEnvAndSecretsWithoutRepositoryUrl(t *testing.T) {
	t.Parallel()

	dataMount := t.TempDir()
	originPath := filepath.Join(t.TempDir(), "origin")
	// #nosec G101 -- test fixture file contents, not real credentials.
	cloneURL := initEnvFileTestRepo(t, originPath, map[string]string{
		"stack/.env":                 "FROM_ENV_FILE=loaded\nSHARED=from-env-file\n",
		"stack/secrets.doco-cd.yaml": "DB_PASSWORD: vault-ref\n",
		"stack/compose.yaml":         "services:\n  app:\n    image: nginx\n",
		"root-only/.env":             "MUST_NOT_LOAD=wrong-directory\n",
	})

	repoName := "envfiles/repo"
	baseDir := filepath.Join(dataMount, repoName)

	artifactPath, revision, mirrorDir := resolveViaGitStore(t, cloneURL, "main", baseDir)

	repository := &RepositoryData{
		Source: config.SourceTypeGit,
		// Unreachable so the test fails loudly if the fast path is ever skipped, keeping this
		// test about file loading rather than about resolving the repository again.
		SourceUrl:         "file:///no/such/repo/that/has/gone/away.git",
		Name:              repoName,
		PathInternal:      artifactPath,
		PathExternal:      artifactPath,
		MirrorDir:         mirrorDir,
		Revision:          revision,
		ResolvedReference: "refs/heads/main",
	}

	deployConfig := deploy.New("app", "main")
	deployConfig.WorkingDirectory = "stack"
	deployConfig.EnvFiles = []string{".env"}
	deployConfig.ExternalSecretsFiles = []string{"secrets.doco-cd.yaml"}

	sm := newFastPathStageManager(t, repository, deployConfig, dataMount)

	if err := sm.RunInitStage(context.Background(), sm.Log); err != nil {
		t.Fatalf("RunInitStage() error = %v", err)
	}

	if got, want := sm.DeployConfig.Internal.Environment["FROM_ENV_FILE"], "loaded"; got != want {
		t.Errorf("Internal.Environment[FROM_ENV_FILE] = %q, want %q", got, want)
	}

	if got, want := sm.DeployConfig.Internal.Environment["SHARED"], "from-env-file"; got != want {
		t.Errorf("Internal.Environment[SHARED] = %q, want %q", got, want)
	}

	if _, loaded := sm.DeployConfig.Internal.Environment["MUST_NOT_LOAD"]; loaded {
		t.Error("Internal.Environment contains MUST_NOT_LOAD, want env files resolved against the working directory only")
	}

	if got, want := sm.DeployConfig.ExternalSecrets["DB_PASSWORD"].LegacyRef, "vault-ref"; got != want {
		t.Errorf("ExternalSecrets[DB_PASSWORD].LegacyRef = %q, want %q", got, want)
	}

	// Both loaders hand their remaining "remote:"-prefixed entries back for a second pass and
	// clear everything they consumed. Nothing is left here, so later stages must not be asked to
	// load these files a second time.
	if len(sm.DeployConfig.EnvFiles) != 0 {
		t.Errorf("EnvFiles = %v, want empty after the files were consumed", sm.DeployConfig.EnvFiles)
	}

	if len(sm.DeployConfig.ExternalSecretsFiles) != 0 {
		t.Errorf("ExternalSecretsFiles = %v, want empty after the files were consumed", sm.DeployConfig.ExternalSecretsFiles)
	}
}

// TestRunInitStageInlineEnvironmentOverridesEnvFile verifies that now that env files always load,
// an explicitly configured `environment` entry still wins over the same key coming from a dotenv
// file, rather than the newly loaded file silently taking precedence.
func TestRunInitStageInlineEnvironmentOverridesEnvFile(t *testing.T) {
	t.Parallel()

	dataMount := t.TempDir()
	originPath := filepath.Join(t.TempDir(), "origin")
	cloneURL := initEnvFileTestRepo(t, originPath, map[string]string{
		".env": "SHARED=from-env-file\nONLY_IN_FILE=kept\n",
	})

	repoName := "inlineenv/repo"
	baseDir := filepath.Join(dataMount, repoName)

	artifactPath, revision, mirrorDir := resolveViaGitStore(t, cloneURL, "main", baseDir)

	repository := &RepositoryData{
		Source:            config.SourceTypeGit,
		SourceUrl:         "file:///no/such/repo/that/has/gone/away.git",
		Name:              repoName,
		PathInternal:      artifactPath,
		PathExternal:      artifactPath,
		MirrorDir:         mirrorDir,
		Revision:          revision,
		ResolvedReference: "refs/heads/main",
	}

	deployConfig := deploy.New("app", "main")
	deployConfig.EnvFiles = []string{".env"}
	deployConfig.Environment = map[string]string{"SHARED": "from-deploy-config"}

	sm := newFastPathStageManager(t, repository, deployConfig, dataMount)

	if err := sm.RunInitStage(context.Background(), sm.Log); err != nil {
		t.Fatalf("RunInitStage() error = %v", err)
	}

	if got, want := sm.DeployConfig.Internal.Environment["SHARED"], "from-deploy-config"; got != want {
		t.Errorf("Internal.Environment[SHARED] = %q, want inline environment to win with %q", got, want)
	}

	if got, want := sm.DeployConfig.Internal.Environment["ONLY_IN_FILE"], "kept"; got != want {
		t.Errorf("Internal.Environment[ONLY_IN_FILE] = %q, want %q", got, want)
	}
}

// TestRunInitStageLoadsLocalAndRemoteFilesWithRepositoryUrl guards the two-phase load that a
// deploy config pointing at a separate deployment repository relies on: unprefixed entries resolve
// against the config source, "remote:"-prefixed ones against the deployment repository's working
// directory. Collapsing the old if/else must not have merged those passes into one.
func TestRunInitStageLoadsLocalAndRemoteFilesWithRepositoryUrl(t *testing.T) {
	t.Parallel()

	dataMount := t.TempDir()

	configSourcePath := filepath.Join(t.TempDir(), "config-source")
	// #nosec G101 -- test fixture file contents, not real credentials.
	configSourceURL := initEnvFileTestRepo(t, configSourcePath, map[string]string{
		".env":                 "CONFIG_SOURCE_VAR=from-config-source\n",
		"secrets.doco-cd.yaml": "CONFIG_SOURCE_SECRET: config-source-ref\n",
	})

	deployRepoPath := filepath.Join(t.TempDir(), "deploy-repo")
	// #nosec G101 -- test fixture file contents, not real credentials.
	deployRepoURL := initEnvFileTestRepo(t, deployRepoPath, map[string]string{
		"stack/.env":                 "DEPLOY_REPO_VAR=from-deploy-repo\n",
		"stack/secrets.doco-cd.yaml": "DEPLOY_REPO_SECRET: deploy-repo-ref\n",
	})

	repoName := "configsource/repo"
	baseDir := filepath.Join(dataMount, repoName)

	artifactPath, revision, mirrorDir := resolveViaGitStore(t, configSourceURL, "main", baseDir)

	repository := &RepositoryData{
		Source:            config.SourceTypeGit,
		SourceUrl:         configSourceURL,
		Name:              repoName,
		PathInternal:      artifactPath,
		PathExternal:      artifactPath,
		MirrorDir:         mirrorDir,
		Revision:          revision,
		ResolvedReference: "refs/heads/main",
	}

	deployConfig := deploy.New("app", "main")
	deployConfig.RepositoryUrl = config.GitUrl(deployRepoURL)
	deployConfig.WorkingDirectory = "stack"
	deployConfig.EnvFiles = []string{".env", "remote:.env"}
	deployConfig.ExternalSecretsFiles = []string{"secrets.doco-cd.yaml", "remote:secrets.doco-cd.yaml"}

	sm := newFastPathStageManager(t, repository, deployConfig, dataMount)

	if err := sm.RunInitStage(context.Background(), sm.Log); err != nil {
		t.Fatalf("RunInitStage() error = %v", err)
	}

	if got, want := sm.DeployConfig.Internal.Environment["CONFIG_SOURCE_VAR"], "from-config-source"; got != want {
		t.Errorf("Internal.Environment[CONFIG_SOURCE_VAR] = %q, want %q", got, want)
	}

	if got, want := sm.DeployConfig.Internal.Environment["DEPLOY_REPO_VAR"], "from-deploy-repo"; got != want {
		t.Errorf("Internal.Environment[DEPLOY_REPO_VAR] = %q, want %q", got, want)
	}

	if got, want := sm.DeployConfig.ExternalSecrets["CONFIG_SOURCE_SECRET"].LegacyRef, "config-source-ref"; got != want {
		t.Errorf("ExternalSecrets[CONFIG_SOURCE_SECRET].LegacyRef = %q, want %q", got, want)
	}

	if got, want := sm.DeployConfig.ExternalSecrets["DEPLOY_REPO_SECRET"].LegacyRef, "deploy-repo-ref"; got != want {
		t.Errorf("ExternalSecrets[DEPLOY_REPO_SECRET].LegacyRef = %q, want %q", got, want)
	}
}
