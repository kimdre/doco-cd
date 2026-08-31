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

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"

	"github.com/kimdre/doco-cd/internal/common/types/slice"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/encryption"
	"github.com/kimdre/doco-cd/internal/filesystem"
)

// LoadCompose parses and loads Compose files as specified by the Docker Compose specification.
// dockerCli is required to load OCI artifact includes.
func LoadCompose(ctx context.Context, dockerCli command.Cli, repoPath, workingDir, projectName string, composeFiles,
	envFiles, profiles []string, environment map[string]string,
) (*types.Project, error) {
	// Resolve compose file paths to absolute paths relative to workingDir.
	// This is necessary because the compose-go library's LoadConfigFiles internally
	// uses filepath.Abs which resolves relative paths against os.Getwd(), not against
	// the specified working directory. Without this, concurrent deployments with
	// different working directories would fail since they share the same process
	// working directory.
	c, err := app.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get app config: %w", err)
	}

	var absComposeFiles []string

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

	var decryptedFiles []string

	decryptFiles := slices.Concat(absComposeFiles, absEnvFiles)
	for _, file := range decryptFiles {
		decrypted, err := encryption.DecryptFileInPlace(file)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt file %s: %w", file, err)
		}

		if decrypted {
			decryptedFiles = append(decryptedFiles, file)
		}
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
	for _, remoteLoader := range newRemoteResourceLoaders(c, dockerCli, repoPath) {
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

	if c.PassEnv {
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

	// Preload project for decrypting project-related files
	project, err := options.LoadProject(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load compose project: %w", err)
	}

	// Decrypt any project-related files
	files, err := DecryptProjectFiles(repoPath, project)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt project files: %w", err)
	}

	decryptedFiles = append(decryptedFiles, files...)
	if len(decryptedFiles) > 0 {
		slog.Debug("decrypted SOPS-encrypted files", slog.String("stack", project.Name), slog.Any("files", decryptedFiles))
	}

	// Reload project after decryption to ensure all decrypted values are properly loaded into the project.
	project, err = options.LoadProject(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load compose project: %w", err)
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
	var (
		projectFiles   []string
		decryptedFiles []string
	)

	for _, s := range p.Services {
		for _, cfg := range s.Configs {
			if cfg.Source != "" {
				if cfgConfig, ok := p.Configs[cfg.Source]; ok && cfgConfig.File != "" {
					projectFiles = append(projectFiles, cfgConfig.File)
				}
			}
		}

		for _, secret := range s.Secrets {
			if secret.Source != "" {
				if secretConfig, ok := p.Secrets[secret.Source]; ok && secretConfig.File != "" {
					projectFiles = append(projectFiles, secretConfig.File)
				}
			}
		}

		for _, v := range s.Volumes {
			if v.Type == "bind" && v.Source != "" {
				info, err := os.Stat(v.Source)
				if err != nil {
					if errors.Is(err, os.ErrNotExist) {
						continue
					}

					return decryptedFiles, fmt.Errorf("failed to stat bind mount source '%s': %w", v.Source, err)
				}

				if info.IsDir() {
					decryptedFiles, err = encryption.DecryptFilesInDirectory(repoPath, v.Source)
					if err != nil {
						if errors.Is(err, filesystem.ErrPathTraversal) {
							continue
						}

						return decryptedFiles, fmt.Errorf("failed to decrypt files in bind mount directory '%s': %w", v.Source, err)
					}

					continue
				}

				projectFiles = append(projectFiles, v.Source)
			}
		}

		for _, envFile := range s.EnvFiles {
			if envFile.Path != "" {
				projectFiles = append(projectFiles, envFile.Path)
			}
		}

		if s.Build != nil {
			if s.Build.Dockerfile != "" {
				if filepath.IsAbs(s.Build.Dockerfile) {
					projectFiles = append(projectFiles, s.Build.Dockerfile)
				} else {
					projectFiles = append(projectFiles, filepath.Join(s.Build.Context, s.Build.Dockerfile))
				}
			}

			for _, secret := range s.Build.Secrets {
				if secret.Source != "" {
					if filepath.IsAbs(secret.Source) {
						projectFiles = append(projectFiles, secret.Source)
					} else {
						projectFiles = append(projectFiles, filepath.Join(s.Build.Context, secret.Source))
					}
				}
			}
		}
	}

	for _, f := range slice.Unique(projectFiles) {
		if !filepath.IsAbs(f) {
			f = filepath.Join(p.WorkingDir, f)
		}

		decrypted, err := encryption.DecryptFileInPlace(f)
		if err != nil {
			return decryptedFiles, fmt.Errorf("failed to decrypt project file '%s': %w", f, err)
		}

		if decrypted {
			decryptedFiles = append(decryptedFiles, f)
		}
	}

	return decryptedFiles, nil
}
