package source

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/stages"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func newTestPreparer(t *testing.T, appConfig *app.Config) *Preparer {
	t.Helper()

	if appConfig == nil {
		appConfig = &app.Config{}
	}

	preparer, err := NewPreparer(Dependencies{AppConfig: appConfig})
	if err != nil {
		t.Fatalf("NewPreparer() error = %v", err)
	}

	return preparer
}

func testMountPoint(t *testing.T) container.MountPoint {
	t.Helper()

	dir := t.TempDir()

	return container.MountPoint{
		Type:        "bind",
		Source:      dir,
		Destination: dir,
		Mode:        "rw",
	}
}

func TestEntityLabel(t *testing.T) {
	t.Parallel()

	if got := EntityLabel(config.SourceTypeGit); got != "repository" {
		t.Fatalf("EntityLabel(git) = %q, want %q", got, "repository")
	}

	if got := EntityLabel(config.SourceTypeOCI); got != "artifact" {
		t.Fatalf("EntityLabel(oci) = %q, want %q", got, "artifact")
	}

	if got := EntityLabel(""); got != "repository" {
		t.Fatalf("EntityLabel(\"\") = %q, want %q", got, "repository")
	}
}

func TestPrepare_InvalidRequest(t *testing.T) {
	t.Parallel()

	p := newTestPreparer(t, nil)

	_, err := p.Prepare(t.Context(), Request{})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest, got %v", err)
	}
}

func TestPrepare_InvalidSourceType(t *testing.T) {
	t.Parallel()

	p := newTestPreparer(t, nil)

	_, err := p.Prepare(t.Context(), Request{
		Logger:         logger.New(logger.LevelCritical).Logger,
		JobTrigger:     stages.JobTriggerWebhook,
		SourceType:     "invalid",
		SourceRef:      "https://example.com/owner/repo.git",
		DataMountPoint: testMountPoint(t),
	})
	if !errors.Is(err, ErrInvalidSourceType) {
		t.Fatalf("expected ErrInvalidSourceType, got %v", err)
	}
}

func TestPrepare_InvalidRepositoryName(t *testing.T) {
	t.Parallel()

	p := newTestPreparer(t, nil)

	_, err := p.Prepare(t.Context(), Request{
		Logger:         logger.New(logger.LevelCritical).Logger,
		JobTrigger:     stages.JobTriggerWebhook,
		SourceRef:      "https://example.com/../evil",
		DataMountPoint: testMountPoint(t),
	})
	if !errors.Is(err, ErrInvalidRepositoryName) {
		t.Fatalf("expected ErrInvalidRepositoryName, got %v", err)
	}
}

func TestResolveDeployConfigs_UnsupportedJobTrigger(t *testing.T) {
	t.Parallel()

	p := newTestPreparer(t, nil)

	_, err := p.resolveDeployConfigs(Request{JobTrigger: "unsupported"}, t.TempDir(), "")
	if !errors.Is(err, ErrUnsupportedJobTrigger) {
		t.Fatalf("expected ErrUnsupportedJobTrigger, got %v", err)
	}
}

// createLocalGitFixture initializes a local git repository at a temp path with
// the given files committed to "main", and returns its path and HEAD commit hash.
func createLocalGitFixture(t *testing.T, files map[string]string) (string, string) {
	t.Helper()

	srcPath := t.TempDir()

	repo, err := gogit.PlainInit(srcPath, false)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}

	if err := repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatalf("set HEAD: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	for name, content := range files {
		fullPath := filepath.Join(srcPath, name)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o750); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}

		if err := os.WriteFile(fullPath, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}

		if _, err := wt.Add(name); err != nil {
			t.Fatalf("add %s: %v", name, err)
		}
	}

	sig := &object.Signature{Name: "test", Email: "test@example.com"}

	commit, err := wt.Commit("initial commit", &gogit.CommitOptions{Author: sig})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	return srcPath, commit.String()
}

const testDeployConfigYAML = `
name: test-deploy
reference: main
working_dir: .
compose_files:
  - test.compose.yaml
`

func TestPrepare_GitSuccess(t *testing.T) {
	t.Parallel()

	srcPath, commitHash := createLocalGitFixture(t, map[string]string{
		".doco-cd.yaml":      testDeployConfigYAML,
		".doco-cd.prod.yaml": testDeployConfigYAML,
		"test.compose.yaml":  "services: {}\n",
	})

	p := newTestPreparer(t, &app.Config{})

	result, err := p.Prepare(t.Context(), Request{
		Logger:         logger.New(logger.LevelCritical).Logger,
		JobTrigger:     stages.JobTriggerWebhook,
		SourceType:     config.SourceTypeGit,
		SourceRef:      "file://" + srcPath,
		Ref:            git.MainBranch,
		CustomTarget:   "prod",
		Payload:        webhook.ParsedPayload{Ref: git.MainBranch},
		DataMountPoint: testMountPoint(t),
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	if result.SourceType != config.SourceTypeGit {
		t.Fatalf("SourceType = %q, want %q", result.SourceType, config.SourceTypeGit)
	}

	if result.Revision != commitHash {
		t.Fatalf("Revision = %q, want %q", result.Revision, commitHash)
	}

	if !result.OCITrusted {
		t.Fatal("expected OCITrusted to be true for Git sources")
	}

	if len(result.DeployConfigs) != 1 {
		t.Fatalf("expected 1 deploy config, got %d", len(result.DeployConfigs))
	}

	if got := result.DeployConfigs[0].Internal.ConfigTarget; got != "prod" {
		t.Fatalf("expected custom target to propagate to deploy config, got %q", got)
	}

	if result.PathInternal == "" || result.PathExternal == "" {
		t.Fatal("expected internal/external paths to be set")
	}
}

func TestPrepare_GitCloneFailure_PostsEarlyCommitStatus(t *testing.T) {
	t.Parallel()

	srcPath, _ := createLocalGitFixture(t, map[string]string{
		".doco-cd.yaml":     testDeployConfigYAML,
		"test.compose.yaml": "services: {}\n",
	})

	var received map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatalf("decode request body: %v", err)
		}

		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	p := newTestPreparer(t, &app.Config{
		GitCommitStatus: true,
		GitAccessToken:  "token",
		GitScmProvider:  "gitea",
	})

	_, err := p.Prepare(t.Context(), Request{
		Logger:         logger.New(logger.LevelCritical).Logger,
		JobTrigger:     stages.JobTriggerWebhook,
		SourceType:     config.SourceTypeGit,
		SourceRef:      "file://" + srcPath,
		Ref:            "refs/heads/does-not-exist",
		Payload:        webhook.ParsedPayload{Ref: git.MainBranch, CommitSHA: plumbing.NewHash("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"), WebURL: srv.URL + "/owner/repo", FullName: "owner/repo"},
		DataMountPoint: testMountPoint(t),
	})
	if !errors.Is(err, ErrGitClone) {
		t.Fatalf("expected ErrGitClone, got %v", err)
	}

	if received["state"] != "error" {
		t.Fatalf("expected an early 'error' commit status to be posted, got %v", received)
	}
}

func TestPrepare_DeployConfigFailure_PostsEarlyCommitStatus(t *testing.T) {
	t.Parallel()

	// No .doco-cd.yaml committed, so deploy config resolution must fail.
	srcPath, _ := createLocalGitFixture(t, map[string]string{
		"readme.md": "no deploy config here\n",
	})

	var received map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatalf("decode request body: %v", err)
		}

		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	p := newTestPreparer(t, &app.Config{
		GitCommitStatus: true,
		GitAccessToken:  "token",
		GitScmProvider:  "gitea",
	})

	_, err := p.Prepare(t.Context(), Request{
		Logger:         logger.New(logger.LevelCritical).Logger,
		JobTrigger:     stages.JobTriggerWebhook,
		SourceType:     config.SourceTypeGit,
		SourceRef:      "file://" + srcPath,
		Ref:            git.MainBranch,
		Payload:        webhook.ParsedPayload{Ref: git.MainBranch, CommitSHA: plumbing.NewHash("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"), WebURL: srv.URL + "/owner/repo", FullName: "owner/repo"},
		DataMountPoint: testMountPoint(t),
	})
	if !errors.Is(err, ErrDeployConfig) {
		t.Fatalf("expected ErrDeployConfig, got %v", err)
	}

	if received["state"] != "error" {
		t.Fatalf("expected an early 'error' commit status to be posted, got %v", received)
	}
}
