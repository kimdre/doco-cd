package source

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/logger"
	sourcecache "github.com/kimdre/doco-cd/internal/source/cache"
	"github.com/kimdre/doco-cd/internal/source/oci"
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

	_, err := p.resolveDeployConfigs(t.Context(), Request{JobTrigger: "unsupported"}, t.TempDir(), "", "", "")
	if !errors.Is(err, ErrUnsupportedJobTrigger) {
		t.Fatalf("expected ErrUnsupportedJobTrigger, got %v", err)
	}
}

// initLocalGitRepo initializes a bare-worktree local git repository at a temp path with HEAD symbolically pointed
// at "main", and returns the repo, its worktree, and the path.
// Shared by createLocalGitFixture and createLocalGitFixtureTwoRevisions.
func initLocalGitRepo(t *testing.T) (repo *gogit.Repository, wt *gogit.Worktree, srcPath string) {
	t.Helper()

	srcPath = t.TempDir()

	repo, err := gogit.PlainInit(srcPath, false)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}

	if err := repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatalf("set HEAD: %v", err)
	}

	wt, err = repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	return repo, wt, srcPath
}

// writeAndAddFiles writes files under srcPath and stages them in wt.
func writeAndAddFiles(t *testing.T, wt *gogit.Worktree, srcPath string, files map[string]string) {
	t.Helper()

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
}

var testGitSignature = &object.Signature{Name: "test", Email: "test@example.com"}

// createLocalGitFixture initializes a local git repository at a temp path with
// the given files committed to "main", and returns its path and HEAD commit hash.
func createLocalGitFixture(t *testing.T, files map[string]string) (string, string) {
	t.Helper()

	_, wt, srcPath := initLocalGitRepo(t)

	writeAndAddFiles(t, wt, srcPath, files)

	commit, err := wt.Commit("initial commit", &gogit.CommitOptions{Author: testGitSignature})
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

// createLocalGitFixtureTwoRevisions is like createLocalGitFixture, but
// commits filesA, tags that commit "v1", then commits filesB on top of it as
// the new "main" HEAD. It returns the repo path plus both commit hashes, so
// callers can exercise resolving two distinct, independently valid revisions
// of the same repository (e.g. concurrently).
func createLocalGitFixtureTwoRevisions(t *testing.T, filesA, filesB map[string]string) (srcPath string, commitA, commitB string) {
	t.Helper()

	repo, wt, srcPath := initLocalGitRepo(t)

	writeAndAddFiles(t, wt, srcPath, filesA)

	firstCommit, err := wt.Commit("first commit", &gogit.CommitOptions{Author: testGitSignature})
	if err != nil {
		t.Fatalf("commit A: %v", err)
	}

	if _, err := repo.CreateTag("v1", firstCommit, nil); err != nil {
		t.Fatalf("tag v1: %v", err)
	}

	writeAndAddFiles(t, wt, srcPath, filesB)

	secondCommit, err := wt.Commit("second commit", &gogit.CommitOptions{Author: testGitSignature})
	if err != nil {
		t.Fatalf("commit B: %v", err)
	}

	return srcPath, firstCommit.String(), secondCommit.String()
}

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

func TestPrepare_WebhookInlineDeploymentsOverrideRepositoryConfig(t *testing.T) {
	t.Parallel()

	srcPath, _ := createLocalGitFixture(t, map[string]string{
		".doco-cd.yaml": `
name: repository-config
reference: main
`,
		"compose.yaml": "services: {}\n",
	})

	inline := &deploy.Config{
		Name:             "inline-config",
		Reference:        git.MainBranch,
		WorkingDirectory: ".",
		ComposeFiles:     []string{"compose.yaml"},
	}
	p := newTestPreparer(t, &app.Config{})

	result, err := p.Prepare(t.Context(), Request{
		Logger:         logger.New(logger.LevelCritical).Logger,
		JobTrigger:     stages.JobTriggerWebhook,
		SourceType:     config.SourceTypeGit,
		SourceRef:      "file://" + srcPath,
		Ref:            git.MainBranch,
		Deployments:    []*deploy.Config{inline},
		Payload:        webhook.ParsedPayload{Ref: git.MainBranch},
		DataMountPoint: testMountPoint(t),
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	if len(result.DeployConfigs) != 1 || result.DeployConfigs[0].Name != "inline-config" {
		t.Fatalf("expected inline config to override repository config, got %+v", result.DeployConfigs)
	}

	if result.DeployConfigs[0] == inline {
		t.Fatal("expected inline deployment to be copied before resolution")
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

func TestPrepare_HoldsSharedGCLockUntilResultRelease(t *testing.T) {
	t.Parallel()

	srcPath, _ := createLocalGitFixture(t, map[string]string{
		".doco-cd.yaml":     testDeployConfigYAML,
		"test.compose.yaml": "services: {}\n",
	})

	mountPoint := testMountPoint(t)

	result, err := newTestPreparer(t, &app.Config{}).Prepare(t.Context(), Request{
		Logger:         logger.New(logger.LevelCritical).Logger,
		JobTrigger:     stages.JobTriggerWebhook,
		SourceType:     config.SourceTypeGit,
		SourceRef:      "file://" + srcPath,
		Ref:            git.MainBranch,
		Payload:        webhook.ParsedPayload{Ref: git.MainBranch},
		DataMountPoint: mountPoint,
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	defer result.Release()

	exclusiveAcquired := make(chan func(), 1)

	go func() {
		repoDir := filepath.Join(mountPoint.Destination, result.RepoName)

		unlock, lockErr := sourcecache.AcquireExclusiveGCPathLock(repoDir)
		if lockErr == nil {
			exclusiveAcquired <- unlock
		}
	}()

	select {
	case unlock := <-exclusiveAcquired:
		unlock()
		t.Fatal("exclusive GC lock acquired before Result.Release")
	case <-time.After(100 * time.Millisecond):
	}

	result.Release()

	select {
	case unlock := <-exclusiveAcquired:
		unlock()
	case <-time.After(2 * time.Second):
		t.Fatal("exclusive GC lock remained blocked after Result.Release")
	}
}

func TestApplyOCIReferencePreservesDeploymentGitReference(t *testing.T) {
	t.Parallel()

	ociConfig := deploy.New("oci-app", "configured-tag")
	gitConfig := deploy.New("git-app", "main")
	gitConfig.RepositoryUrl = "https://github.com/owner/app.git"

	applyOCIReference([]*deploy.Config{ociConfig, gitConfig}, "latest")

	if ociConfig.Reference != "latest" {
		t.Fatalf("OCI reference = %q, want trigger ref", ociConfig.Reference)
	}

	if gitConfig.Reference != "main" {
		t.Fatalf("Git reference = %q, want configured Git ref", gitConfig.Reference)
	}
}

func TestApplyOCIReferenceUsesArtifactTagWhenWebhookRefIsEmpty(t *testing.T) {
	t.Parallel()

	deployConfig := deploy.New("oci-app", deploy.DefaultReference)

	applyOCIReference([]*deploy.Config{deployConfig}, oci.TagFromArtifact("ghcr.io/owner/app:stable"))

	if deployConfig.Reference != "stable" {
		t.Fatalf("OCI reference = %q, want artifact tag", deployConfig.Reference)
	}
}

// TestPrepare_ConcurrentDifferentRevisions_NonInterfering proves that two
// concurrent Prepare calls for the same repository, resolving two distinct
// revisions ("v1" and "main"), do not serialize against each other and each
// produce their own correct, independent artifact - the end-to-end
// (Preparer.Prepare, not just the underlying GitStore) analogue of
// TestGitStore_PublishConcurrent_DifferentRevisions.
func TestPrepare_ConcurrentDifferentRevisions_NonInterfering(t *testing.T) {
	t.Parallel()

	srcPath, commitA, commitB := createLocalGitFixtureTwoRevisions(t,
		map[string]string{
			".doco-cd.yaml":     testDeployConfigYAML,
			"test.compose.yaml": "services: {}\n",
		},
		map[string]string{
			"test.compose.yaml": "services: {}\n# second revision\n",
		},
	)

	p := newTestPreparer(t, &app.Config{})
	mountPoint := testMountPoint(t)

	type outcome struct {
		result Result
		err    error
	}

	run := func(ref string) outcome {
		result, err := p.Prepare(t.Context(), Request{
			Logger:         logger.New(logger.LevelCritical).Logger,
			JobTrigger:     stages.JobTriggerWebhook,
			SourceType:     config.SourceTypeGit,
			SourceRef:      "file://" + srcPath,
			Ref:            ref,
			Payload:        webhook.ParsedPayload{Ref: ref},
			DataMountPoint: mountPoint,
		})

		return outcome{result: result, err: err}
	}

	var (
		wg                   sync.WaitGroup
		outcomeV1, outcomeB2 outcome
	)

	wg.Add(2)

	go func() {
		defer wg.Done()

		outcomeV1 = run("refs/tags/v1")
	}()

	go func() {
		defer wg.Done()

		outcomeB2 = run(git.MainBranch)
	}()

	wg.Wait()

	if outcomeV1.err != nil {
		t.Fatalf("Prepare(v1) error = %v", outcomeV1.err)
	}

	if outcomeB2.err != nil {
		t.Fatalf("Prepare(main) error = %v", outcomeB2.err)
	}

	if outcomeV1.result.Revision != commitA {
		t.Fatalf("Prepare(v1) revision = %q, want %q", outcomeV1.result.Revision, commitA)
	}

	if outcomeB2.result.Revision != commitB {
		t.Fatalf("Prepare(main) revision = %q, want %q", outcomeB2.result.Revision, commitB)
	}

	if outcomeV1.result.PathInternal == outcomeB2.result.PathInternal {
		t.Fatalf("expected distinct artifact paths for distinct revisions, both got %q", outcomeV1.result.PathInternal)
	}

	if _, err := os.Stat(outcomeV1.result.PathInternal); err != nil {
		t.Fatalf("v1 artifact path missing: %v", err)
	}

	if _, err := os.Stat(outcomeB2.result.PathInternal); err != nil {
		t.Fatalf("main artifact path missing: %v", err)
	}
}

// TestPrepare_ConcurrentSameRevision_Reused proves that two concurrent
// Prepare calls for the same repository and the same revision both succeed
// and agree on the resolved revision and artifact path, without racing or
// erroring - the end-to-end analogue of
// TestGitStore_PublishConcurrent_SameRevision.
func TestPrepare_ConcurrentSameRevision_Reused(t *testing.T) {
	t.Parallel()

	srcPath, commitHash := createLocalGitFixture(t, map[string]string{
		".doco-cd.yaml":     testDeployConfigYAML,
		"test.compose.yaml": "services: {}\n",
	})

	p := newTestPreparer(t, &app.Config{})
	mountPoint := testMountPoint(t)

	const concurrency = 4

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		results  []Result
		firstErr error
	)

	wg.Add(concurrency)

	for range concurrency {
		go func() {
			defer wg.Done()

			result, err := p.Prepare(t.Context(), Request{
				Logger:         logger.New(logger.LevelCritical).Logger,
				JobTrigger:     stages.JobTriggerWebhook,
				SourceType:     config.SourceTypeGit,
				SourceRef:      "file://" + srcPath,
				Ref:            git.MainBranch,
				Payload:        webhook.ParsedPayload{Ref: git.MainBranch},
				DataMountPoint: mountPoint,
			})

			mu.Lock()
			defer mu.Unlock()

			if err != nil && firstErr == nil {
				firstErr = err
			}

			results = append(results, result)
		}()
	}

	wg.Wait()

	if firstErr != nil {
		t.Fatalf("Prepare() error = %v", firstErr)
	}

	for i, result := range results {
		if result.Revision != commitHash {
			t.Fatalf("result[%d].Revision = %q, want %q", i, result.Revision, commitHash)
		}

		if result.PathInternal != results[0].PathInternal {
			t.Fatalf("result[%d].PathInternal = %q, want %q (same as result[0])", i, result.PathInternal, results[0].PathInternal)
		}
	}
}
