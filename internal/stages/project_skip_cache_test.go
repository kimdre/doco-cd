package stages

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/git"
	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

func TestProjectSkipCacheOnlySkipsUnchangedProjectTree(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "stack"), 0o750); err != nil {
		t.Fatal(err)
	}

	composePath := filepath.Join(root, "stack", "compose.yaml")
	if err := os.WriteFile(composePath, []byte("services:\n  web:\n    image: nginx\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	unrelatedPath := filepath.Join(root, "README.md")
	if err := os.WriteFile(unrelatedPath, []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	repo, err := gogit.PlainInit(root, false)
	if err != nil {
		t.Fatal(err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := wt.Add("stack/compose.yaml"); err != nil {
		t.Fatal(err)
	}

	if _, err := wt.Add("README.md"); err != nil {
		t.Fatal(err)
	}

	first := commitN(t, wt, 1, 0)[0]

	cfg := &deploy.Config{
		Name:             "stack",
		WorkingDirectory: "stack",
		ComposeFiles:     []string{"compose.yaml"},
		AutoDiscovery:    deploy.AutoDiscoveryConfig{Enabled: true},
	}
	cfg.Internal.Hash = "effective-config"
	s := &StageManager{
		Repository: &RepositoryData{
			Source:       config.SourceTypeGit,
			MirrorDir:    root,
			Revision:     first.String(),
			PathInternal: root,
			PathExternal: root,
		},
		DeployConfig: cfg,
		AppConfig:    &app.Config{},
		Docker: &Docker{Project: &types.Project{
			ComposeFiles: []string{composePath},
			Services: types.Services{
				"web": {Name: "web", Restart: "always"},
			},
		}},
		ProjectSkips: NewProjectSkipCache(),
		GitAncestry:  NewGitAncestryCache(),
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	status := map[docker.Service]docker.ServiceStatus{"web": {Replicas: 1}}

	s.cacheUnchangedProject(log, first.String(), "hash")

	if !s.skipFromCachedProject(log, first.String(), "hash", status) {
		t.Fatal("expected cached, unchanged project to skip")
	}

	if err := os.WriteFile(unrelatedPath, []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := wt.Add("README.md"); err != nil {
		t.Fatal(err)
	}

	second := commitN(t, wt, 1, 1)[0]

	s.Repository.Revision = second.String()
	if !s.skipFromCachedProject(log, first.String(), "hash", status) {
		t.Fatal("an unrelated commit should reuse the validated project")
	}

	if s.skipFromCachedProject(log, first.String(), "hash", nil) {
		t.Fatal("missing service must still trigger the full drift check")
	}

	if s.skipFromCachedProject(log, first.String(), "hash", map[docker.Service]docker.ServiceStatus{
		"web":   {Replicas: 0},
		"extra": {Replicas: 1},
	}) {
		t.Fatal("stopped or extra services must still trigger the full drift check")
	}

	if s.skipFromCachedProject(log, first.String(), "new hash", status) {
		t.Fatal("a different deployed project must not use the cache")
	}

	s.cacheUnchangedProject(log, first.String(), "hash")

	s.Repository.Revision = first.String()
	if s.skipFromCachedProject(log, first.String(), "hash", status) {
		t.Fatal("a validated future revision must not skip an older one")
	}

	s.Repository.Revision = second.String()

	missingCommit := "ffffffffffffffffffffffffffffffffffffffff"
	s.cacheUnchangedProject(log, missingCommit, "hash")

	if s.skipFromCachedProject(log, missingCommit, "hash", status) {
		t.Fatal("an unavailable deployed commit must take the full recovery path")
	}

	s.cacheUnchangedProject(log, first.String(), "hash")

	cfg.Internal.Hash = "updated-config"

	if s.skipFromCachedProject(log, first.String(), "hash", status) {
		t.Fatal("updated config or external-secret values must invalidate the cache")
	}

	cfg.Internal.Hash = "effective-config"

	cfg.ForceImagePull = true

	if s.skipFromCachedProject(log, first.String(), "hash", status) {
		t.Fatal("forced image pulls must still check the registry")
	}

	cfg.ForceImagePull = false

	s.AppConfig.GitCloneSubmodules = true
	if !s.skipFromCachedProject(log, first.String(), "hash", status) {
		t.Fatal("default submodule cloning should permit a repository without submodules")
	}

	two := 2
	s.Docker.SwarmMode = true
	s.Docker.Project.Services["web"] = types.ServiceConfig{
		Name: "web", Scale: &two, Deploy: &types.DeployConfig{Mode: "replicated", Replicas: &two},
	}
	s.cacheUnchangedProject(log, first.String(), "hash")

	swarmStatus := map[docker.Service]docker.ServiceStatus{
		"web": {SwarmMode: swarm.DeployModeReplicated, Replicas: 2},
	}
	if !s.skipFromCachedProject(log, first.String(), "hash", swarmStatus) {
		t.Fatal("matching Swarm service status should skip")
	}

	swarmStatus["web"] = docker.ServiceStatus{SwarmMode: swarm.DeployModeReplicated, Replicas: 1}
	if s.skipFromCachedProject(log, first.String(), "hash", swarmStatus) {
		t.Fatal("Swarm scale drift must trigger full pre-deploy")
	}

	swarmStatus["web"] = docker.ServiceStatus{SwarmMode: swarm.DeployModeGlobal, Replicas: 2}
	if s.skipFromCachedProject(log, first.String(), "hash", swarmStatus) {
		t.Fatal("Swarm mode drift must trigger full pre-deploy")
	}

	if err := os.WriteFile(composePath, []byte("services:\n  web:\n    image: nginx:alpine\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := wt.Add("stack/compose.yaml"); err != nil {
		t.Fatal(err)
	}

	s.Repository.Revision = commitN(t, wt, 1, 2)[0].String()
	if s.skipFromCachedProject(log, first.String(), "hash", status) {
		t.Fatal("changed compose content must take the full pre-deploy path")
	}
}

func TestProjectSkipCacheRejectsExternalInputsAndIncludes(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	stackDir := filepath.Join(root, "stack")
	if err := os.Mkdir(stackDir, 0o750); err != nil {
		t.Fatal(err)
	}

	composePath := filepath.Join(stackDir, "compose.yaml")
	writeCompose := func(content string) {
		t.Helper()

		if err := os.WriteFile(composePath, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeCompose("services:\n  web:\n    image: nginx\n")

	cfg := &deploy.Config{WorkingDirectory: "stack", ComposeFiles: []string{"compose.yaml"}, AutoDiscovery: deploy.AutoDiscoveryConfig{Enabled: true}}
	project := &types.Project{ComposeFiles: []string{composePath}, Services: types.Services{"web": {Name: "web"}}}
	s := &StageManager{
		Repository:   &RepositoryData{Source: config.SourceTypeGit, MirrorDir: root, Revision: "abc", PathInternal: root, PathExternal: root},
		DeployConfig: cfg,
		AppConfig:    &app.Config{},
	}

	if !s.localProjectInputs(project) {
		t.Fatal("expected local compose-only project to be eligible")
	}

	project.Services["web"] = types.ServiceConfig{Name: "web", Volumes: []types.ServiceVolumeConfig{{Type: "bind", Source: filepath.Join(root, "shared")}}}
	if s.localProjectInputs(project) {
		t.Fatal("shared bind mount must require full project loading")
	}

	project.Services["web"] = types.ServiceConfig{Name: "web"}

	project.Services["web"] = types.ServiceConfig{Name: "web", Build: &types.BuildConfig{
		Context:    filepath.Join(stackDir, "build"),
		Dockerfile: "../../shared.Dockerfile",
	}}
	if s.localProjectInputs(project) {
		t.Fatal("a Dockerfile outside the stack must require full project loading")
	}

	project.Services["web"] = types.ServiceConfig{Name: "web", Build: &types.BuildConfig{
		Context: filepath.Join(stackDir, "build"),
		AdditionalContexts: types.Mapping{
			"shared": "git@github.com:owner/shared.git",
		},
	}}
	if s.localProjectInputs(project) {
		t.Fatal("a remote build context must require full project loading")
	}

	project.Services["web"] = types.ServiceConfig{Name: "web", Build: &types.BuildConfig{
		Context: filepath.Join(stackDir, "build"),
		SSH:     types.SSHConfig{{ID: "default"}},
	}}
	if s.localProjectInputs(project) {
		t.Fatal("a build using mutable SSH credentials must require full project loading")
	}

	project.Services["web"] = types.ServiceConfig{Name: "web", CredentialSpec: &types.CredentialSpecConfig{File: filepath.Join(root, "shared.json")}}
	if s.localProjectInputs(project) {
		t.Fatal("a credential spec outside the stack must require full project loading")
	}

	project.Services["web"] = types.ServiceConfig{Name: "web"}

	writeCompose("include:\n  - ../shared.yaml\nservices:\n  web:\n    image: nginx\n")

	if s.localProjectInputs(project) {
		t.Fatal("a Compose include must require full loading")
	}

	writeCompose("services:\n  web:\n    image: nginx\n")

	s.AppConfig.PassEnv = true
	if s.localProjectInputs(project) {
		t.Fatal("process environment interpolation is not covered by the Git tree")
	}

	s.AppConfig.PassEnv = false

	s.AppConfig.GitCloneSubmodules = true
	if !s.localProjectInputs(project) {
		t.Fatal("default submodule cloning should allow repos without submodules")
	}

	if err := os.WriteFile(filepath.Join(root, ".gitmodules"), []byte("[submodule \"shared\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Gitlinks are checked per project subtree when a snapshot is stored.
	if !s.localProjectInputs(project) {
		t.Fatal("a submodule elsewhere in the repository is not a project input")
	}

	s.AppConfig.GitCloneSubmodules = false
	if !s.localProjectInputs(project) {
		t.Fatal("disabled submodule cloning is not a dynamic project input")
	}

	if err := os.Symlink("compose.yaml", filepath.Join(stackDir, "linked.yaml")); err != nil {
		t.Fatal(err)
	}

	project.Services["web"] = types.ServiceConfig{Name: "web", Volumes: []types.ServiceVolumeConfig{{Type: "bind", Source: stackDir}}}
	if s.localProjectInputs(project) {
		t.Fatal("symlink targets in referenced directories may change outside the Git subtree")
	}
}

func TestProjectSkipCacheBoundedAndConcurrent(t *testing.T) {
	t.Parallel()

	cache := NewProjectSkipCache()

	for i := range maxProjectSkipCacheEntries + 1 {
		key := projectSkipKey{name: strconv.Itoa(i + 1)}
		cache.store(key, projectSkipSnapshot{composeHash: "hash"})
	}

	if _, ok := cache.load(projectSkipKey{name: "1"}); ok {
		t.Fatal("oldest cache entry should be evicted")
	}

	if got := len(cache.entries); got != maxProjectSkipCacheEntries {
		t.Fatalf("cache entries = %d, want %d", got, maxProjectSkipCacheEntries)
	}

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			key := projectSkipKey{name: strconv.Itoa(i + 1)}
			cache.store(key, projectSkipSnapshot{composeHash: "updated"})
			cache.load(key)
		})
	}

	wg.Wait()
}

func BenchmarkUnchangedAutoDiscoveredStack(b *testing.B) {
	root := b.TempDir()

	stackDir := filepath.Join(root, "stack")
	if err := os.Mkdir(stackDir, 0o750); err != nil {
		b.Fatal(err)
	}

	composePath := filepath.Join(stackDir, "compose.yaml")
	readmePath := filepath.Join(root, "README.md")

	if err := os.WriteFile(composePath, []byte("services:\n  web:\n    image: nginx\n"), 0o600); err != nil {
		b.Fatal(err)
	}

	if err := os.WriteFile(readmePath, []byte("one\n"), 0o600); err != nil {
		b.Fatal(err)
	}

	repo, err := gogit.PlainInit(root, false)
	if err != nil {
		b.Fatal(err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		b.Fatal(err)
	}

	for _, file := range []string{"stack/compose.yaml", "README.md"} {
		if _, err := wt.Add(file); err != nil {
			b.Fatal(err)
		}
	}

	commit := func() string {
		b.Helper()

		hash, err := wt.Commit("update", &gogit.CommitOptions{
			Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()},
		})
		if err != nil {
			b.Fatal(err)
		}

		return hash.String()
	}
	deployed := commit()

	if err := os.WriteFile(readmePath, []byte("two\n"), 0o600); err != nil {
		b.Fatal(err)
	}

	if _, err := wt.Add("README.md"); err != nil {
		b.Fatal(err)
	}

	latest := commit()

	cfg := &deploy.Config{
		Name:             "stack",
		WorkingDirectory: "stack",
		ComposeFiles:     []string{"compose.yaml"},
		AutoDiscovery:    deploy.AutoDiscoveryConfig{Enabled: true},
	}
	cfg.Internal.Hash = "effective-config"
	s := &StageManager{
		Repository: &RepositoryData{
			Source: config.SourceTypeGit, MirrorDir: root, Revision: deployed,
			PathInternal: root, PathExternal: root,
		},
		DeployConfig: cfg,
		AppConfig:    &app.Config{GitCloneSubmodules: true},
		Docker: &Docker{Project: &types.Project{
			ComposeFiles: []string{composePath},
			Services:     types.Services{"web": {Name: "web"}},
		}},
		ProjectSkips: NewProjectSkipCache(),
		GitAncestry:  NewGitAncestryCache(),
		GitChanges:   NewGitChangeCache(),
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s.cacheUnchangedProject(log, deployed, "hash")
	s.Repository.Revision = latest
	status := map[docker.Service]docker.ServiceStatus{"web": {Replicas: 1}}

	b.Run("warm_preflight", func(b *testing.B) {
		for b.Loop() {
			if !s.skipFromCachedProject(log, deployed, "hash", status) {
				b.Fatal("expected an unchanged stack to skip")
			}
		}
	})
	b.Run("compose_load_and_hash", func(b *testing.B) {
		for b.Loop() {
			if _, err := s.loadComposeProjectHash(context.Background()); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("full_unchanged_check", func(b *testing.B) {
		for b.Loop() {
			if _, err := s.loadComposeProjectHash(context.Background()); err != nil {
				b.Fatal(err)
			}

			var files []git.ChangedFile

			err := s.withMirrorRead(func(repo *gogit.Repository) error {
				oldHash, newHash := plumbing.NewHash(deployed), plumbing.NewHash(latest)
				if isStaleDeployment(repo, root, newHash, oldHash, s.GitAncestry, log) {
					b.Fatal("unexpected stale revision")
				}

				var err error

				files, err = s.GitChanges.changedFiles(root, oldHash, newHash, func() ([]git.ChangedFile, error) {
					return git.GetChangedFilesBetweenCommits(repo, oldHash, newHash)
				})

				return err
			})
			if err != nil {
				b.Fatal(err)
			}

			changed := docker.GetPathsFromGitChangedFiles(files, root)

			changes, ignored, err := docker.ProjectFilesHaveChanges(root, changed, s.Docker.Project)
			if err != nil || len(changes) != 0 || !ignored.IsEmpty() {
				b.Fatalf("unexpected change mapping: %v, %v, %v", changes, ignored, err)
			}
		}
	})
}

func TestShouldTryCachedProjectSkip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                                             string
		autoDiscovery, retry, migration, labelDriftFound bool
		want                                             bool
	}{
		{name: "unchanged auto-discovered stack", autoDiscovery: true, want: true},
		{name: "not auto-discovered"},
		{name: "retry after failure", autoDiscovery: true, retry: true},
		{name: "deployment mode migration", autoDiscovery: true, migration: true},
		{name: "auto-discovery label drift", autoDiscovery: true, labelDriftFound: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := shouldTryCachedProjectSkip(tt.autoDiscovery, tt.retry, tt.migration, tt.labelDriftFound); got != tt.want {
				t.Fatalf("shouldTryCachedProjectSkip() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestProjectSkipCacheSchedulerHoldsAndPKIRoles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "stack"), 0o750); err != nil {
		t.Fatal(err)
	}

	composePath := filepath.Join(root, "stack", "compose.yaml")
	if err := os.WriteFile(composePath, []byte("services:\n  web:\n    image: nginx\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	repo, err := gogit.PlainInit(root, false)
	if err != nil {
		t.Fatal(err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := wt.Add("stack/compose.yaml"); err != nil {
		t.Fatal(err)
	}

	commit := commitN(t, wt, 1, 0)[0].String()

	cfg := &deploy.Config{
		Name:             "stack",
		WorkingDirectory: "stack",
		ComposeFiles:     []string{"compose.yaml"},
		AutoDiscovery:    deploy.AutoDiscoveryConfig{Enabled: true},
		ExternalSecrets: map[string]secrettypes.ExternalSecretRef{
			"CERT": {LegacyRef: "pki-role:web"},
		},
	}
	cfg.Internal.Environment = map[string]string{
		"CERT":                                "certificate-1",
		"CERT" + secrettypes.PKIRoleKeySuffix: "key-1",
		"STATIC":                              "one",
	}
	holds := &fakeSchedulerHolds{held: map[string]bool{"/stack/web": true}}
	s := &StageManager{
		Repository: &RepositoryData{
			Source:       config.SourceTypeGit,
			MirrorDir:    root,
			Revision:     commit,
			PathInternal: root,
			PathExternal: root,
		},
		DeployConfig: cfg,
		AppConfig:    &app.Config{},
		Docker: &Docker{Project: &types.Project{
			Name:         "stack",
			ComposeFiles: []string{composePath},
			Services:     types.Services{"web": {Name: "web", Restart: "always"}},
		}},
		ProjectSkips:   NewProjectSkipCache(),
		GitAncestry:    NewGitAncestryCache(),
		SchedulerHolds: holds,
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	s.cacheUnchangedProject(log, commit, "hash")

	// The warm path runs before the Compose project is loaded.
	s.Docker.Project = nil
	stopped := map[docker.Service]docker.ServiceStatus{"web": {Replicas: 0}}

	if !s.skipFromCachedProject(log, commit, "hash", stopped) {
		t.Fatal("a service held stopped by the job scheduler must not force a full pre-deploy")
	}

	delete(holds.held, "/stack/web")

	if s.skipFromCachedProject(log, commit, "hash", stopped) {
		t.Fatal("a stopped service without a scheduler hold must trigger the full drift check")
	}

	running := map[docker.Service]docker.ServiceStatus{"web": {Replicas: 1}}
	cfg.Internal.Environment["CERT"] = "certificate-2"
	cfg.Internal.Environment["CERT"+secrettypes.PKIRoleKeySuffix] = "key-2"

	if !s.skipFromCachedProject(log, commit, "hash", running) {
		t.Fatal("a freshly issued pki-role certificate must not invalidate the cached project")
	}

	cfg.Internal.Environment["STATIC"] = "two"

	if s.skipFromCachedProject(log, commit, "hash", running) {
		t.Fatal("a changed non-pki secret value must invalidate the cached project")
	}
}

func TestProjectSkipCacheSubmodulesOnlyBlockTheirOwnProject(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	for _, dir := range []string{"plain", "linked"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o750); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(filepath.Join(root, dir, "compose.yaml"), []byte("services:\n  web:\n    image: nginx\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.WriteFile(filepath.Join(root, ".gitmodules"),
		[]byte("[submodule \"module\"]\n\tpath = linked/module\n\turl = https://example.com/module.git\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	repo, err := gogit.PlainInit(root, false)
	if err != nil {
		t.Fatal(err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	for _, file := range []string{".gitmodules", "plain/compose.yaml", "linked/compose.yaml"} {
		if _, err := wt.Add(file); err != nil {
			t.Fatal(err)
		}
	}

	idx, err := repo.Storer.Index()
	if err != nil {
		t.Fatal(err)
	}

	idx.Entries = append(idx.Entries, &index.Entry{
		Name: "linked/module",
		Hash: plumbing.NewHash("1111111111111111111111111111111111111111"),
		Mode: filemode.Submodule,
	})
	if err := repo.Storer.SetIndex(idx); err != nil {
		t.Fatal(err)
	}

	revision := commitN(t, wt, 1, 0)[0].String()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	status := map[docker.Service]docker.ServiceStatus{"web": {Replicas: 1}}

	newStage := func(dir string, submodules bool) *StageManager {
		cfg := &deploy.Config{
			Name:             dir,
			WorkingDirectory: dir,
			ComposeFiles:     []string{"compose.yaml"},
			AutoDiscovery:    deploy.AutoDiscoveryConfig{Enabled: true},
		}
		cfg.Internal.Hash = "effective-config"

		return &StageManager{
			Repository: &RepositoryData{
				Source: config.SourceTypeGit, MirrorDir: root, Revision: revision,
				PathInternal: root, PathExternal: root,
			},
			DeployConfig: cfg,
			AppConfig:    &app.Config{GitCloneSubmodules: submodules},
			Docker: &Docker{Project: &types.Project{
				ComposeFiles: []string{filepath.Join(root, dir, "compose.yaml")},
				Services:     types.Services{"web": {Name: "web"}},
			}},
			ProjectSkips: NewProjectSkipCache(),
			GitAncestry:  NewGitAncestryCache(),
		}
	}

	tests := []struct {
		dir        string
		submodules bool
		want       bool
	}{
		{dir: "plain", submodules: true, want: true},
		{dir: "linked", submodules: true, want: false},
		{dir: "linked", submodules: false, want: true},
	}

	for _, tt := range tests {
		s := newStage(tt.dir, tt.submodules)
		s.cacheUnchangedProject(log, revision, "hash")

		if got := s.skipFromCachedProject(log, revision, "hash", status); got != tt.want {
			t.Fatalf("%s with submodules=%t: skip = %t, want %t", tt.dir, tt.submodules, got, tt.want)
		}
	}
}
