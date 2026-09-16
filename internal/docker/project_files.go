package docker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/kimdre/doco-cd/internal/common/types/set"
	"github.com/kimdre/doco-cd/internal/filesystem"
)

// projectFile is a single path of the file set a compose project resolves to.
// Paths are absolute and on the docker host, like the paths in types.Project.
type projectFile struct {
	Path string
	// IsDir marks a directory whose whole content belongs to the project,
	// e.g. a bind mounted directory or a build context.
	IsDir bool
}

// projectFileSet collects projectFiles without duplicates.
type projectFileSet struct {
	index map[string]int
	files []projectFile
}

func newProjectFileSet() *projectFileSet {
	return &projectFileSet{index: make(map[string]int)}
}

// absProjectPath resolves path against the working directory of the project if it is relative.
func absProjectPath(p *types.Project, path string) string {
	if !filepath.IsAbs(path) {
		path = filepath.Join(p.WorkingDir, path)
	}

	return filepath.Clean(path)
}

// add resolves path against the working directory of the project if it is relative
// and adds it to the set.
func (s *projectFileSet) add(p *types.Project, path string, isDir bool) {
	if path == "" {
		return
	}

	path = absProjectPath(p, path)

	if i, ok := s.index[path]; ok {
		// A path used as file and as directory is a directory.
		s.files[i].IsDir = s.files[i].IsDir || isDir

		return
	}

	s.index[path] = len(s.files)
	s.files = append(s.files, projectFile{Path: path, IsDir: isDir})
}

// list returns the collected files sorted by path.
func (s *projectFileSet) list() []projectFile {
	slices.SortFunc(s.files, func(a, b projectFile) int {
		return strings.Compare(a.Path, b.Path)
	})

	return s.files
}

// serviceFiles resolves the files the services of a compose project read at deploy time:
// configs, secrets, bind mount sources, env files, dockerfiles and build secrets.
// Bind mount sources are the only entries that can be a directory.
//
// DecryptProjectFiles and resolvedProjectFiles both build on this, so the file set they
// work on cannot drift apart.
func serviceFiles(p *types.Project) ([]projectFile, error) {
	files := newProjectFileSet()

	for _, s := range p.Services {
		for _, cfg := range s.Configs {
			if cfg.Source != "" {
				if cfgConfig, ok := p.Configs[cfg.Source]; ok && cfgConfig.File != "" {
					files.add(p, cfgConfig.File, false)
				}
			}
		}

		for _, secret := range s.Secrets {
			if secret.Source != "" {
				if secretConfig, ok := p.Secrets[secret.Source]; ok && secretConfig.File != "" {
					files.add(p, secretConfig.File, false)
				}
			}
		}

		for _, v := range s.Volumes {
			if v.Type != "bind" || v.Source == "" {
				continue
			}

			source := absProjectPath(p, v.Source)

			info, err := os.Stat(source)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}

				return nil, fmt.Errorf("failed to stat bind mount source '%s': %w", v.Source, err)
			}

			files.add(p, source, info.IsDir())
		}

		for _, envFile := range s.EnvFiles {
			files.add(p, envFile.Path, false)
		}

		if s.Build == nil {
			continue
		}

		if s.Build.Dockerfile != "" {
			if filepath.IsAbs(s.Build.Dockerfile) {
				files.add(p, s.Build.Dockerfile, false)
			} else {
				files.add(p, filepath.Join(s.Build.Context, s.Build.Dockerfile), false)
			}
		}

		for _, secret := range s.Build.Secrets {
			if secret.Source == "" {
				continue
			}

			if filepath.IsAbs(secret.Source) {
				files.add(p, secret.Source, false)
			} else {
				files.add(p, filepath.Join(s.Build.Context, secret.Source), false)
			}
		}
	}

	return files.list(), nil
}

// resolvedProjectFiles returns the full file set of a compose project: the compose files,
// everything serviceFiles resolves and the build contexts. A change has to touch one of
// these paths to affect the stack.
//
// Files reached through `include:` or `extends: file:` are missing: compose-go resolves
// both away and types.Project does not name them any more.
func resolvedProjectFiles(p *types.Project) ([]projectFile, error) {
	deployFiles, err := serviceFiles(p)
	if err != nil {
		return nil, err
	}

	files := newProjectFileSet()

	for _, f := range deployFiles {
		files.add(p, f.Path, f.IsDir)
	}

	for _, composeFile := range p.ComposeFiles {
		files.add(p, composeFile, false)
	}

	for _, s := range p.Services {
		if s.Build == nil {
			continue
		}

		files.add(p, s.Build.Context, true)

		for _, ctx := range s.Build.AdditionalContexts {
			if ctx == "" {
				continue
			}

			// A context that does not exist yet is treated as a directory, so files that
			// appear under it later still match. Additional contexts also take non-path
			// values like docker-image://alpine, which never match a path in the
			// repository either way.
			files.add(p, ctx, !filesystem.IsFile(absProjectPath(p, ctx)))
		}
	}

	return files.list(), nil
}

// repoRelativePath converts an absolute path on the docker host to the repository-relative,
// slash-separated form go-git reports in a commit log. ok is false when the path is not
// inside the repository, which is how a host path or a remote include is dropped.
func repoRelativePath(repoPath, absPath string) (rel string, ok bool) {
	if !filesystem.InBasePath(repoPath, absPath) {
		return "", false
	}

	rel, err := filepath.Rel(filepath.Clean(repoPath), absPath)
	if err != nil {
		return "", false
	}

	return filepath.ToSlash(rel), true
}

// projectRepoPaths converts the file set of a compose project to paths relative to the
// root of the repository, the form go-git reports in a commit log. Paths outside the
// repository, e.g. a host path or a remote include, are dropped.
//
// repoPath is the path to the repository on the docker host, like the absolute paths in
// types.Project. coversRepoRoot reports that the stack owns the whole repository, which
// makes every path of it relevant.
func projectRepoPaths(repoPath string, p *types.Project) (files, dirs []string, coversRepoRoot bool, err error) {
	projectFiles, err := resolvedProjectFiles(p)
	if err != nil {
		return nil, nil, false, err
	}

	for _, f := range projectFiles {
		rel, ok := repoRelativePath(repoPath, f.Path)
		if !ok {
			continue
		}

		if !f.IsDir {
			files = append(files, rel)

			continue
		}

		if rel == "." {
			return nil, nil, true, nil
		}

		dirs = append(dirs, rel)
	}

	return files, dirs, false, nil
}

// ProjectPathFilter builds a filter for git.LogOptions.PathFilter that matches the file
// set of a compose project, so a commit log can be reduced to the commits that touch this
// stack. go-git reports paths relative to the root of the repository, so the file set is
// converted to that form. Files match exactly, directories match everything below them.
//
// repoPath is the path to the repository on the docker host, like the absolute paths in
// types.Project.
//
// The returned filter is nil when the project covers the repository root or resolves to no
// path inside the repository. Both match every path, so filtering would only cost tree
// diffs, and callers read a nil filter as "do not filter".
func ProjectPathFilter(repoPath string, p *types.Project, extraPaths ...string) (func(string) bool, error) {
	if p == nil {
		return nil, nil
	}

	filePaths, dirPaths, coversRepoRoot, err := projectRepoPaths(repoPath, p)
	if err != nil {
		return nil, err
	}

	// Files that belong to the stack without being part of the compose project. The
	// deployment configuration is the motivating case: it is what declares the stack and
	// carries its image tags, so a commit that changes only it is the reason this deploy
	// happened — but it is not a compose file, so the project never names it and its
	// commit was dropped from the changelog.
	// Paths outside the repository are ignored, the same as project files.
	for _, extra := range extraPaths {
		rel, ok := repoRelativePath(repoPath, extra)
		if !ok {
			continue
		}

		filePaths = append(filePaths, rel)
	}

	if coversRepoRoot || (len(filePaths) == 0 && len(dirPaths) == 0) {
		return nil, nil
	}

	files := set.New(filePaths...)

	prefixes := make([]string, 0, len(dirPaths))
	for _, d := range dirPaths {
		prefixes = append(prefixes, d+"/")
	}

	return func(path string) bool {
		if files.Contains(path) {
			return true
		}

		for _, prefix := range prefixes {
			if strings.HasPrefix(path, prefix) {
				return true
			}
		}

		return false
	}, nil
}
