package docker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"sync"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"

	"github.com/kimdre/doco-cd/internal/encryption"
	"github.com/kimdre/doco-cd/internal/filesystem"
)

// withMutationLock runs fn while holding l, when l is non-nil.
func withMutationLock(l sync.Locker, fn func() error) error {
	if l != nil {
		l.Lock()
		defer l.Unlock()
	}

	return fn()
}

// LoadCompose parses and loads Compose files as specified by the Docker Compose specification.
// dockerCli is required to load OCI artifact includes. opts bundles the Docker-owned settings
// (env passthrough, Git/OCI remote include configuration) needed beyond the compose project
// parameters themselves; callers resolve it explicitly (see NewComposeLoadOptions) instead of
// LoadCompose reading the application configuration itself.
func LoadCompose(ctx context.Context, dockerCli command.Cli, repoPath, workingDir, projectName string, composeFiles,
	envFiles, profiles []string, environment map[string]string, opts ComposeLoadOptions,
) (*types.Project, error) {
	var (
		absComposeFiles []string
		err             error
		decryptedFiles  []string
	)

	// Resolve compose file paths to absolute paths relative to workingDir.
	// This is necessary because the compose-go library's LoadConfigFiles internally
	// uses filepath.Abs which resolves relative paths against os.Getwd(), not against
	// the specified working directory. Without this, concurrent deployments with
	// different working directories would fail since they share the same process
	// working directory.

	// If the user changed the default compose files, we throw an error of the custom compose file is not found
	throwError := !reflect.DeepEqual(composeFiles, cli.DefaultFileNames)

	for _, f := range composeFiles {
		if !filepath.IsAbs(f) {
			f = filepath.Join(workingDir, f)
		}

		// Check if file exists
		if _, err = os.Stat(f); err != nil {
			if throwError {
				return nil, fmt.Errorf("could not find compose file: %w", err)
			}

			continue
		}

		absComposeFiles = append(absComposeFiles, f)
	}

	// if envFiles only contains ".env", we check if the file exists in the working directory
	if len(envFiles) == 1 && envFiles[0] == ".env" {
		if _, err := os.Stat(path.Join(workingDir, ".env")); errors.Is(err, os.ErrNotExist) {
			envFiles = []string{}
		}
	}

	absEnvFiles := make([]string, 0, len(envFiles))
	for _, f := range envFiles {
		if filepath.IsAbs(f) {
			absEnvFiles = append(absEnvFiles, f)
		} else {
			absEnvFiles = append(absEnvFiles, filepath.Join(workingDir, f))
		}
	}

	decryptFiles := slices.Concat(absComposeFiles, absEnvFiles)

	err = withMutationLock(opts.MutationLock, func() error {
		for _, file := range decryptFiles {
			decrypted, err := encryption.DecryptFileInPlace(file)
			if err != nil {
				return fmt.Errorf("failed to decrypt file %s: %w", file, err)
			}

			if decrypted {
				decryptedFiles = append(decryptedFiles, file)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	projectOptions := []cli.ProjectOptionsFn{
		cli.WithName(projectName),
		cli.WithWorkingDirectory(workingDir),
		cli.WithInterpolation(true),
		cli.WithResolvedPaths(true),
		cli.WithEnvFiles(absEnvFiles...), // env files for variable interpolation
		cli.WithProfiles(profiles),
	}

	// Remote include support (Git repositories and OCI artifacts).
	for _, remoteLoader := range newRemoteResourceLoaders(opts, dockerCli, repoPath) {
		projectOptions = append(projectOptions, cli.WithResourceLoader(remoteLoader))
	}

	options, err := cli.NewProjectOptions(
		absComposeFiles,
		projectOptions...,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create project options: %w", err)
	}

	if len(composeFiles) == 0 {
		err = cli.WithDefaultConfigPath(options)
		if err != nil {
			return nil, fmt.Errorf("failed to use default compose file: %w", err)
		}
	}

	if opts.PassEnv {
		err = cli.WithOsEnv(options)
		if err != nil {
			return nil, fmt.Errorf("failed to get OS environment variables for interpolation: %w", err)
		}
	}

	// Inject external secrets into the environment for variable interpolation
	maps.Copy(options.Environment, environment)

	err = cli.WithDotEnv(options)
	if err != nil {
		return nil, fmt.Errorf("failed to get .env file for interpolation: %w", err)
	}

	var project *types.Project

	err = withMutationLock(opts.MutationLock, func() error {
		// Keep discovery, decryption, and the conditional reload atomic. Another
		// caller may otherwise decrypt a file after this project parsed it but
		// before this call decides whether a reload is necessary.
		project, err = options.LoadProject(ctx)
		if err != nil {
			return fmt.Errorf("failed to load compose project: %w", err)
		}

		files, decryptErr := DecryptProjectFiles(repoPath, project)
		if decryptErr != nil {
			return fmt.Errorf("failed to decrypt project files: %w", decryptErr)
		}

		decryptedFiles = append(decryptedFiles, files...)
		if len(files) == 0 {
			return nil
		}

		project, err = options.LoadProject(ctx)
		if err != nil {
			return fmt.Errorf("failed to reload compose project after decryption: %w", err)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	if len(decryptedFiles) > 0 {
		slog.Debug("decrypted SOPS-encrypted files", slog.String("stack", project.Name), slog.Any("files", decryptedFiles))
	}

	project, err = project.WithServicesEnvironmentResolved(false)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve services environment: %w", err)
	}

	return project, nil
}

// CheckDefaultComposeFiles checks if the default compose files are used and returns them if true.
func CheckDefaultComposeFiles(composeFiles []string, workingDir string) ([]string, error) {
	if reflect.DeepEqual(composeFiles, cli.DefaultFileNames) {
		var (
			err             error
			tmpComposeFiles []string
		)

		// Check if the default compose files exist

		for _, f := range composeFiles {
			if _, err = os.Stat(path.Join(workingDir, f)); errors.Is(err, os.ErrNotExist) {
				continue
			}

			tmpComposeFiles = append(tmpComposeFiles, f)
		}

		if len(tmpComposeFiles) == 0 {
			errMsg := "no compose files found"
			return nil, fmt.Errorf("%s: %w", errMsg, err)
		}

		return tmpComposeFiles, nil
	}

	return composeFiles, nil
}

// DecryptProjectFiles decrypts all files used in the compose project that are encrypted using doco-cd's encryption mechanism.
// This includes configs, secrets, bind mounts, env files and build contexts.
// Since absolute file paths in types.Project are paths on the docker host, repoPath also needs to be the external path to the repository.
// We use the symlink inside the container to follow the external path to the correct internal path.
func DecryptProjectFiles(repoPath string, p *types.Project) ([]string, error) {
	var decryptedFiles []string

	projectFiles, err := serviceFiles(p)
	if err != nil {
		return decryptedFiles, err
	}

	for _, f := range projectFiles {
		if f.IsDir {
			files, err := encryption.DecryptFilesInDirectory(repoPath, f.Path)
			if err != nil {
				if errors.Is(err, filesystem.ErrPathTraversal) {
					continue
				}

				return decryptedFiles, fmt.Errorf("failed to decrypt files in bind mount directory '%s': %w", f.Path, err)
			}

			decryptedFiles = append(decryptedFiles, files...)

			continue
		}

		decrypted, err := encryption.DecryptFileInPlace(f.Path)
		if err != nil {
			return decryptedFiles, fmt.Errorf("failed to decrypt project file '%s': %w", f.Path, err)
		}

		if decrypted {
			decryptedFiles = append(decryptedFiles, f.Path)
		}
	}

	return decryptedFiles, nil
}
