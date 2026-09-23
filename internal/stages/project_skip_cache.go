package stages

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/compose-spec/compose-go/v2/types"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"go.yaml.in/yaml/v4"

	"github.com/kimdre/doco-cd/internal/common/types/clone"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/git"
)

const maxProjectSkipCacheEntries = 512

// projectSkipKey is a unique identifier for a cached project snapshot.
// It is based on the repository mirror, working directory, and deployment configuration.
type projectSkipKey struct {
	mirror, context, name, reference, directory string
}

// projectSkipSnapshot holds the state of a project at a specific revision,
// including the deployed commit, configuration hash, and expected services.
type projectSkipSnapshot struct {
	revision,
	deployedCommit,
	configHash,
	composeHash,
	projectName string
	treeHash plumbing.Hash
	services types.Services
}

// ProjectSkipCache holds proof from a full, unchanged pre-deploy check. It
// never stores a Compose project or paths into an old published artifact.
type ProjectSkipCache struct {
	mu      sync.Mutex
	entries map[projectSkipKey]projectSkipSnapshot
	order   []projectSkipKey
}

// NewProjectSkipCache creates a new instance of ProjectSkipCache with an initialized entries map.
func NewProjectSkipCache() *ProjectSkipCache {
	return &ProjectSkipCache{entries: make(map[projectSkipKey]projectSkipSnapshot)}
}

// load retrieves a cached project snapshot for the given key.
// It returns the snapshot and a boolean indicating whether the snapshot was found.
func (c *ProjectSkipCache) load(key projectSkipKey) (projectSkipSnapshot, bool) {
	if c == nil {
		return projectSkipSnapshot{}, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	snapshot, ok := c.entries[key]

	return snapshot, ok
}

// store saves a cached project snapshot for the given key.
func (c *ProjectSkipCache) store(key projectSkipKey, snapshot projectSkipSnapshot) {
	if c == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.entries[key]; !exists {
		if len(c.entries) >= maxProjectSkipCacheEntries {
			delete(c.entries, c.order[0])
			c.order = c.order[1:]
		}

		c.order = append(c.order, key)
	}

	c.entries[key] = snapshot
}

// projectSkipKey generates a unique key for the current deployment configuration
// and repository state.
func (s *StageManager) projectSkipKey() projectSkipKey {
	return projectSkipKey{
		mirror:    s.Repository.MirrorDir,
		context:   s.DeployConfig.Context,
		name:      s.DeployConfig.Name,
		reference: s.DeployConfig.Reference,
		directory: path.Clean(s.DeployConfig.WorkingDirectory),
	}
}

// localProjectInputs proves that the Git subtree contains every input that
// can affect the loaded project. Anything remote, shared outside this stack,
// mutable through the process environment, or ambiguous uses the full path.
func (s *StageManager) localProjectInputs(project *types.Project) bool {
	if project == nil || !s.DeployConfig.AutoDiscovery.Enabled ||
		s.Repository.Source != config.SourceTypeGit ||
		s.Repository.MirrorDir == "" || s.Repository.Revision == "" ||
		s.DeployConfig.RepositoryUrl != "" ||
		!s.staticProjectEnvironment() {
		return false
	}

	internalDir, err := getAbsWorkingDir(s.Repository.PathInternal, s.DeployConfig.WorkingDirectory)
	if err != nil {
		return false
	}

	externalDir, err := getAbsWorkingDir(s.Repository.PathExternal, s.DeployConfig.WorkingDirectory)
	if err != nil {
		return false
	}

	hasSymlink := func(p string) bool {
		rel, err := filepath.Rel(externalDir, p)
		if err != nil {
			return true
		}

		current := internalDir
		for _, part := range append([]string{"."}, strings.Split(rel, string(filepath.Separator))...) {
			current = filepath.Join(current, part)

			info, err := os.Lstat(current)
			if os.IsNotExist(err) {
				return false
			}

			if err != nil || info.Mode()&fs.ModeSymlink != 0 {
				return true
			}
		}

		return false
	}

	var directories []string

	inside := func(p string) bool {
		if p == "" || strings.Contains(p, ":") {
			return false
		}

		if !filepath.IsAbs(p) {
			p = filepath.Join(externalDir, p)
		}

		rel, err := filepath.Rel(externalDir, p)

		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) &&
			!hasSymlink(p)
	}

	for _, file := range append(append([]string{}, s.DeployConfig.ComposeFiles...), project.ComposeFiles...) {
		if !inside(file) {
			return false
		}
	}

	for _, file := range append(append([]string{}, s.DeployConfig.EnvFiles...), s.DeployConfig.ExternalSecretsFiles...) {
		if !inside(file) {
			return false
		}
	}

	for _, cfg := range project.Configs {
		if cfg.File != "" && !inside(cfg.File) {
			return false
		}
	}

	for _, secret := range project.Secrets {
		if secret.File != "" && !inside(secret.File) {
			return false
		}
	}

	for _, svc := range project.AllServices() {
		if svc.Extends != nil {
			return false
		}

		if svc.Dockerfile != "" && !inside(svc.Dockerfile) {
			return false
		}

		if svc.CredentialSpec != nil && svc.CredentialSpec.File != "" && !inside(svc.CredentialSpec.File) {
			return false
		}

		for _, f := range svc.EnvFiles {
			if !inside(f.Path) {
				return false
			}
		}

		for _, f := range svc.LabelFiles {
			if !inside(f) {
				return false
			}
		}

		for _, volume := range svc.Volumes {
			if volume.Type == "bind" && !inside(volume.Source) {
				return false
			}

			if volume.Type == "bind" {
				directories = append(directories, volume.Source)
			}
		}

		if svc.Build != nil {
			if !inside(svc.Build.Context) {
				return false
			}

			directories = append(directories, svc.Build.Context)
			if svc.Build.Dockerfile != "" {
				dockerfile := svc.Build.Dockerfile
				if !filepath.IsAbs(dockerfile) {
					dockerfile = filepath.Join(svc.Build.Context, dockerfile)
				}

				if !inside(dockerfile) {
					return false
				}
			}

			if len(svc.Build.SSH) > 0 {
				// Agent sockets and key material are not revision-bound.
				return false
			}

			for _, context := range svc.Build.AdditionalContexts {
				if !inside(context) {
					return false
				}

				directories = append(directories, context)
			}

			for _, secret := range svc.Build.Secrets {
				if secret.Source != "" && !inside(secret.Source) {
					return false
				}
			}
		}
	}

	// Includes can be resolved away in types.Project, so inspect source YAML
	// rather than assuming project.ComposeFiles lists every input.
	for _, file := range project.ComposeFiles {
		if !filepath.IsAbs(file) {
			file = filepath.Join(externalDir, file)
		}

		rel, err := filepath.Rel(externalDir, file)
		if err != nil {
			return false
		}

		contents, err := os.ReadFile(filepath.Join(internalDir, rel)) // #nosec G304
		if err != nil {
			return false
		}

		var node yaml.Node
		if err := yaml.Unmarshal(contents, &node); err != nil || hasComposeIncludes(node) {
			return false
		}
	}

	// Referenced directories may contain symlinks to mutable files outside
	// the repository even when their Git tree is unchanged.
	for _, dir := range directories {
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(externalDir, dir)
		}

		rel, err := filepath.Rel(externalDir, dir)
		if err != nil {
			return false
		}

		symlinkFound := false

		err = filepath.WalkDir(filepath.Join(internalDir, rel), func(_ string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}

			if entry.Type()&fs.ModeSymlink != 0 {
				symlinkFound = true
				return fs.SkipAll
			}

			return nil
		})
		if err != nil || symlinkFound {
			return false
		}
	}

	return true
}

// staticProjectEnvironment returns true if the project environment is static and
// does not depend on external mutable inputs. This is the case when the project
// is not configured to pass through the process environment and does not use
// Git submodules, or if it does use submodules but there is no .gitmodules file
// in the repository.
func (s *StageManager) staticProjectEnvironment() bool {
	if s.AppConfig == nil || s.AppConfig.PassEnv {
		return false
	}

	if !s.AppConfig.GitCloneSubmodules {
		return true
	}

	// With no submodule manifest, enabling the default clone option cannot
	// materialize inputs outside this revision's Git tree.
	_, err := os.Lstat(filepath.Join(s.Repository.PathInternal, ".gitmodules"))

	return errors.Is(err, fs.ErrNotExist)
}

// hasComposeIncludes checks if a YAML node contains any
// Compose "include" or "extends" directives.
func hasComposeIncludes(node yaml.Node) bool {
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == "include" || node.Content[i].Value == "extends" {
				return true
			}
		}
	}

	for _, child := range node.Content {
		if hasComposeIncludes(*child) {
			return true
		}
	}

	return false
}

// projectExpectedServices returns a map of service names to their expected configurations
// based on the provided project. It includes the service name, restart policy,
// scale, and deploy configuration (mode and replicas).
func projectExpectedServices(project *types.Project) types.Services {
	services := make(types.Services, len(project.Services))
	for name, service := range project.Services {
		expected := types.ServiceConfig{
			Name:    service.Name,
			Restart: service.Restart,
		}
		if service.Scale != nil {
			scale := *service.Scale
			expected.Scale = &scale
		}

		if service.Deploy != nil {
			expected.Deploy = &types.DeployConfig{Mode: service.Deploy.Mode}
			if service.Deploy.Replicas != nil {
				replicas := *service.Deploy.Replicas
				expected.Deploy.Replicas = &replicas
			}
		}

		services[name] = expected
	}

	return services
}

// cacheUnchangedProject stores a snapshot of the current project state in the cache
// if the project inputs are local and the configuration hash can be computed.
// It includes the deployed commit, configuration hash, Compose project hash,
// and expected services.
func (s *StageManager) cacheUnchangedProject(stageLog *slog.Logger, deployedCommit, projectHash string) {
	if s.ProjectSkips == nil || !s.localProjectInputs(s.Docker.Project) {
		return
	}

	configHash, err := s.projectSkipConfigHash()
	if err != nil {
		stageLog.Debug("could not hash project config; using full pre-deploy next run", slog.String("reason", err.Error()))
		return
	}

	var treeHash plumbing.Hash

	err = s.withMirrorRead(func(repo *gogit.Repository) error {
		var err error

		treeHash, err = projectTreeHash(repo, plumbing.NewHash(s.Repository.Revision), s.DeployConfig.WorkingDirectory)

		return err
	})
	if err != nil {
		stageLog.Debug("could not cache project Git tree; using full pre-deploy next run", slog.String("reason", err.Error()))
		return
	}

	s.ProjectSkips.store(s.projectSkipKey(), projectSkipSnapshot{
		revision:       s.Repository.Revision,
		deployedCommit: deployedCommit,
		configHash:     configHash,
		composeHash:    projectHash,
		projectName:    s.Docker.Project.Name,
		treeHash:       treeHash,
		services:       projectExpectedServices(s.Docker.Project),
	})
}

// projectSkipConfigHash is the effective config hash with pki-role values
// replaced by their references. Those roles issue a fresh certificate on every
// resolution, and the Compose project hash already normalizes them the same
// way, so the raw config hash would never match a cached snapshot.
func (s *StageManager) projectSkipConfigHash() (string, error) {
	norm := pkiRoleNormMap(s.DeployConfig.ExternalSecrets, s.DeployConfig.Internal.Environment)
	if len(norm) == 0 {
		return s.DeployConfig.Internal.Hash, nil
	}

	normalized := clone.New(s.DeployConfig)
	for key, value := range normalized.Internal.Environment {
		if ref, ok := norm[value]; ok {
			normalized.Internal.Environment[key] = ref
		}
	}

	return normalized.Hash()
}

// projectTreeHash returns the Git tree hash for the specified commit and directory.
func projectTreeHash(repo *gogit.Repository, hash plumbing.Hash, dir string) (plumbing.Hash, error) {
	commit, err := repo.CommitObject(hash)
	if err != nil {
		return plumbing.ZeroHash, err
	}

	tree, err := commit.Tree()
	if err != nil {
		return plumbing.ZeroHash, err
	}

	if dir != "." {
		tree, err = tree.Tree(dir)
		if err != nil {
			return plumbing.ZeroHash, err
		}
	}

	return tree.Hash, nil
}

// skipFromCachedProject determines whether the current project can be skipped
// based on a cached snapshot of its previous state.
func (s *StageManager) skipFromCachedProject(
	stageLog *slog.Logger, deployedCommit, deployedComposeHash string,
	deployedStatus map[docker.Service]docker.ServiceStatus,
) bool {
	if s.ProjectSkips == nil || !s.DeployConfig.AutoDiscovery.Enabled ||
		s.DeployConfig.ForceImagePull || s.DeployConfig.ForceRecreate ||
		s.Repository.Source != config.SourceTypeGit ||
		!s.staticProjectEnvironment() {
		return false
	}

	configHash, err := s.projectSkipConfigHash()
	if err != nil {
		return false
	}

	snapshot, ok := s.ProjectSkips.load(s.projectSkipKey())
	if !ok || snapshot.configHash != configHash ||
		snapshot.deployedCommit != deployedCommit ||
		snapshot.composeHash == "" || snapshot.composeHash != deployedComposeHash ||
		snapshot.treeHash.IsZero() ||
		s.Repository.Revision == "" {
		return false
	}

	latest := plumbing.NewHash(s.Repository.Revision)
	validated := plumbing.NewHash(snapshot.revision)

	deployed := plumbing.NewHash(snapshot.deployedCommit)
	if latest.IsZero() || validated.IsZero() || deployed.IsZero() {
		return false
	}

	unchanged := false

	err = s.withMirrorRead(func(repo *gogit.Repository) error {
		if _, err := repo.CommitObject(deployed); err != nil {
			return fmt.Errorf("deployed commit %s is unavailable: %w", deployed, err)
		}

		if deployed != latest && isStaleDeployment(repo, s.Repository.MirrorDir, latest, deployed, s.GitAncestry, stageLog) {
			return nil
		}
		// Do not use a cached answer from a newer or diverged revision.
		ancestor, err := s.GitAncestry.isAncestor(s.Repository.MirrorDir, validated, latest, func() (bool, error) {
			return git.IsAncestorCommit(repo, validated, latest)
		})
		if err != nil || !ancestor {
			return err
		}

		var treeHash plumbing.Hash

		treeHash, err = projectTreeHash(repo, latest, s.DeployConfig.WorkingDirectory)
		unchanged = err == nil && treeHash == snapshot.treeHash

		return err
	})
	if err != nil {
		stageLog.Debug("cached project comparison unavailable; using full pre-deploy", slog.String("reason", err.Error()))
		return false
	}

	if !unchanged {
		return false
	}

	mismatches := docker.CheckServiceMismatch(s.Docker.SwarmMode, deployedStatus, snapshot.services)
	if len(s.dropSchedulerHeldProjectMismatches(mismatches, snapshot.projectName, stageLog)) != 0 {
		return false
	}

	stageLog.Debug("unchanged Git subtree and deployed project, skipping full pre-deploy",
		slog.String("directory", s.DeployConfig.WorkingDirectory),
		slog.String("validated_revision", snapshot.revision),
	)

	return true
}
