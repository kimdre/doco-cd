package stages

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/git"
)

func shouldSkipDeployment(composeChanged bool,
	autoDiscoveryLabelChanged bool,
	changedServices []docker.Change,
	ignoredInfo docker.IgnoredInfo,
	imagesChanged bool,
	mismatchServices []docker.ServiceMismatch,
) bool {
	return !composeChanged &&
		!autoDiscoveryLabelChanged &&
		len(changedServices) == 0 &&
		!ignoredInfo.IsNeedSignal() &&
		!imagesChanged &&
		len(mismatchServices) == 0
}

func autoDiscoveryConfigLabelDriftServices(deployedStatus map[docker.Service]docker.ServiceStatus, expected string) ([]string, string) {
	expected = strings.TrimSpace(expected)

	if len(deployedStatus) == 0 {
		return nil, ""
	}

	affected := make([]string, 0, len(deployedStatus))
	expectedCfg := docker.ParseAutoDiscoveryConfig(expected)

	var firstObserved string

	for serviceName, status := range deployedStatus {
		actual, ok := status.Labels[docker.DocoCDLabels.Deployment.AutoDiscoveryConfig]

		actual = strings.TrimSpace(actual)
		if ok && actual == expected {
			continue
		}

		// This label only steers the cleanup of obsolete auto-discovered stacks, and that
		// cleanup reads containers labeled as auto-discovered. With auto-discovery off in
		// both configs, including the legacy enabled label when the config label is absent,
		// the config label is inert, so a changed default is no reason to recreate the stack.
		deployedCfg := docker.ParseAutoDiscoveryConfig(actual)
		legacyAutoDiscoveryEnabled, _ := strconv.ParseBool(status.Labels[docker.DocoCDLabels.Deployment.AutoDiscovery])

		if !expectedCfg.Enabled && !deployedCfg.Enabled && (ok || !legacyAutoDiscoveryEnabled) {
			continue
		}

		affected = append(affected, string(serviceName))

		if firstObserved == "" {
			firstObserved = actual
		}
	}

	if len(affected) == 0 {
		return nil, expected
	}

	slices.Sort(affected)

	return affected, firstObserved
}

func shouldSkipOCIDeployment(forceRecreate bool, deployedDigest, resolvedDigest string) bool {
	if forceRecreate {
		return false
	}

	deployedDigest = strings.TrimSpace(deployedDigest)
	resolvedDigest = strings.TrimSpace(resolvedDigest)

	if deployedDigest == "" || resolvedDigest == "" {
		return false
	}

	return deployedDigest == resolvedDigest
}

// shouldRecoverFromMissingDeployedCommit checks if the error indicates that the deployed commit is no longer reachable
// in the Git history. This can happen if the commit has been removed or rewritten (e.g., due to a force push).
// In such cases, we may want to recover by treating it as a full-change deployment.
func shouldRecoverFromMissingDeployedCommit(err error) bool {
	return git.IsRefUnreachableError(err)
}

func (s *StageManager) RunPreDeployStage(ctx context.Context, stageLog *slog.Logger) error {
	s.Stages.PreDeploy.StartedAt = time.Now()

	defer func() {
		s.Stages.PreDeploy.FinishedAt = time.Now()
	}()

	// Check for external secret changes and current deployed commit
	var (
		imagesChanged bool // Flag to indicate if images have changed
	)

	// Compare external secrets if a secret provider is configured
	if s.SecretProvider != nil && *s.SecretProvider != nil && len(s.DeployConfig.ExternalSecrets) > 0 {
		stageLog.Debug("resolving external secrets", slog.Any("external_secrets", s.DeployConfig.ExternalSecrets))

		encodedSecrets, err := secrettypes.EncodeExternalSecretRefs(s.DeployConfig.ExternalSecrets)
		if err != nil {
			return fmt.Errorf("failed to encode external secret references: %w", err)
		}

		// Resolve external secrets
		resolvedSecrets, err := (*s.SecretProvider).ResolveSecretReferences(ctx, encodedSecrets)
		if err != nil {
			return fmt.Errorf("failed to resolve external secrets: %w", err)
		}

		if s.DeployConfig.Internal.Environment == nil {
			s.DeployConfig.Internal.Environment = make(map[string]string)
		}

		maps.Copy(s.DeployConfig.Internal.Environment, resolvedSecrets)
	}

	var err error

	s.DeployConfig.Internal.Hash, err = s.DeployConfig.Hash()
	if err != nil {
		return fmt.Errorf("failed to hash deploy configuration: %w", err)
	}

	deployedState, err := docker.GetLatestDeployStatus(ctx, s.Docker.Cmd.Client(), s.Docker.SwarmMode, s.Repository.Name, s.DeployConfig.Name)
	if err != nil {
		return fmt.Errorf("failed to get latest state from deployed services: %w", err)
	}

	expectedAutoDiscoveryLabel := docker.MarshalAutoDiscoveryConfig(s.DeployConfig.AutoDiscovery)
	autoDiscoveryDriftServices, deployedAutoDiscoveryLabel := autoDiscoveryConfigLabelDriftServices(
		deployedState.DeployedStatus,
		expectedAutoDiscoveryLabel,
	)

	autoDiscoveryConfigChanged := len(autoDiscoveryDriftServices) > 0
	if autoDiscoveryConfigChanged {
		stageLog.Debug("auto-discovery config label changed, proceeding with deployment",
			slog.Any("affected_services", autoDiscoveryDriftServices),
			slog.String("deployed_auto_discovery_config", deployedAutoDiscoveryLabel),
			slog.String("expected_auto_discovery_config", expectedAutoDiscoveryLabel),
		)
	}

	if s.Repository.Source == config.SourceTypeOCI {
		deployedDigest := deployedState.GetDeploymentCommitSHA()
		resolvedDigest := s.Repository.Revision

		if shouldSkipOCIDeployment(s.DeployConfig.ForceRecreate, deployedDigest, resolvedDigest) && !autoDiscoveryConfigChanged {
			stageLog.Debug("OCI artifact digest unchanged, skipping deployment",
				slog.String("deployed_digest", strings.TrimSpace(deployedDigest)),
				slog.String("resolved_digest", strings.TrimSpace(resolvedDigest)),
			)

			return ErrSkipDeployment
		}

		if strings.TrimSpace(deployedDigest) == "" {
			stageLog.Debug("no previous OCI deployment digest found, proceeding with deployment",
				slog.String("resolved_digest", strings.TrimSpace(resolvedDigest)),
			)
		} else {
			stageLog.Debug("OCI artifact digest changed, proceeding with deployment",
				slog.String("deployed_digest", strings.TrimSpace(deployedDigest)),
				slog.String("resolved_digest", strings.TrimSpace(resolvedDigest)),
			)
		}

		if autoDiscoveryConfigChanged {
			s.DeployState.changedServices = []docker.Change{{
				Type:     "auto_discovery_config_label",
				Services: autoDiscoveryDriftServices,
			}}
		}

		return nil
	}

	if deployedCommit := deployedState.GetDeploymentCommitSHA(); deployedCommit != "" {
		s.DeployState.DeployedCommit = deployedCommit

		latestCommit, err := git.GetLatestCommit(s.Repository.Git, s.DeployConfig.Reference)
		if err != nil {
			return fmt.Errorf("failed to get latest commit: %w", err)
		}

		stageLog.Debug("comparing commits",
			slog.String("deployed_commit", deployedCommit),
			slog.String("latest_commit", latestCommit))

		// Validate and sanitize the working directory
		if strings.Contains(s.DeployConfig.WorkingDirectory, "..") {
			return errors.New("invalid working directory: must not contain '..' to prevent directory traversal")
		}

		extAbsWorkingDir, err := getAbsWorkingDir(s.Repository.PathExternal, s.DeployConfig.WorkingDirectory)
		if err != nil {
			return fmt.Errorf("failed to get absolute path of external working directory: %w", err)
		}

		intAbsWorkingDir, err := getAbsWorkingDir(s.Repository.PathInternal, s.DeployConfig.WorkingDirectory)
		if err != nil {
			return fmt.Errorf("failed to get absolute path of internal working directory: %w", err)
		}

		s.DeployConfig.ComposeFiles, err = docker.CheckDefaultComposeFiles(s.DeployConfig.ComposeFiles, intAbsWorkingDir)
		if err != nil {
			return fmt.Errorf("failed to check for default compose files: %w", err)
		}

		s.Docker.Project, err = docker.LoadCompose(
			ctx, s.Docker.Cmd, s.Repository.PathExternal, extAbsWorkingDir, s.DeployConfig.Name,
			s.DeployConfig.ComposeFiles, s.DeployConfig.EnvFiles,
			s.DeployConfig.Profiles, s.DeployConfig.Internal.Environment)
		if err != nil {
			return fmt.Errorf("failed to load compose project: %w", err)
		}

		if s.DeployConfig.ForceRecreate {
			stageLog.Debug("force recreate enabled, skipping pre-deploy image pull check")
		} else if s.DeployConfig.ForceImagePull {
			stageLog.Debug("force image pull enabled, checking deployed image digests against registry")

			imagesChanged, err = docker.HaveDeployedServiceImageDigestsChanged(ctx, s.Docker.Cmd, s.Docker.SwarmMode, s.Docker.Project, stageLog)
			if err != nil {
				return fmt.Errorf("failed to compare deployed service image digests: %w", err)
			}

			if imagesChanged {
				stageLog.Debug("deployed image digests differ from registry, proceeding with deployment")
			} else {
				stageLog.Debug("deployed image digests match registry")
			}
		}

		// pki-role external secrets issue a fresh certificate on every resolution.
		// Substitute the resolved cert/key PEM values with a stable per-ref placeholder
		// before hashing so the hash only changes when the ref changes, not when the
		// cert is re-issued (which would otherwise trigger a spurious redeploy every cycle).
		hashProject := docker.WithNormalizedEnvValues(s.Docker.Project, pkiRoleNormMap(s.DeployConfig.ExternalSecrets, s.DeployConfig.Internal.Environment))

		newHash, err := docker.ProjectHash(hashProject)
		if err != nil {
			return fmt.Errorf("failed to get project hash: %w", err)
		}

		curProjectHash := deployedState.GetDeploymentComposeHash()

		composeChanged := newHash != curProjectHash
		if composeChanged {
			stageLog.Debug("compose project has changed, proceeding with deployment", slog.String("new_hash", newHash), slog.String("old_hash", curProjectHash))
		}

		// Check for file changes
		deployedHash := plumbing.NewHash(deployedCommit)
		latestHash := plumbing.NewHash(latestCommit)
		gitChangedFiles := make([]git.ChangedFile, 0)

		if _, err := s.Repository.Git.CommitObject(deployedHash); err != nil {
			if shouldRecoverFromMissingDeployedCommit(err) {
				stageLog.Warn("previous deployed commit is no longer reachable; continuing with full-change deployment comparison",
					slog.String("deployed_commit", deployedCommit),
					slog.String("latest_commit", latestCommit),
					slog.String("reason", err.Error()),
				)

				composeChanged = true
			} else {
				return fmt.Errorf("failed to resolve deployed commit %s: %w", deployedCommit, err)
			}
		} else {
			gitChangedFiles, err = git.GetChangedFilesBetweenCommits(s.Repository.Git, deployedHash, latestHash)
			if err != nil {
				return fmt.Errorf("failed to get changed files between commits: %w", err)
			}
		}

		changedFiles := docker.GetPathsFromGitChangedFiles(gitChangedFiles, s.Repository.PathExternal)

		changedServices, ignoredInfo, err := docker.ProjectFilesHaveChanges(s.Repository.PathExternal, changedFiles, s.Docker.Project)
		if err != nil {
			return fmt.Errorf("failed to check for changed project files: %s", err)
		}

		if autoDiscoveryConfigChanged {
			changedServices = append(changedServices, docker.Change{
				Type:     "auto_discovery_config_label",
				Services: autoDiscoveryDriftServices,
			})
		}

		mismatchServices := docker.CheckServiceMismatch(s.Docker.SwarmMode, deployedState.DeployedStatus, s.Docker.Project.Services)

		if s.DeployConfig.ForceRecreate {
			stageLog.Debug("force recreate enabled, proceeding with deployment",
				slog.String("directory", s.DeployConfig.WorkingDirectory),
			)
		} else if shouldSkipDeployment(composeChanged, autoDiscoveryConfigChanged, changedServices, ignoredInfo, imagesChanged, mismatchServices) {
			stageLog.Debug("no changes detected, skipping deployment",
				slog.String("directory", s.DeployConfig.WorkingDirectory),
			)

			return ErrSkipDeployment
		}

		s.DeployState.changedServices = changedServices
		s.DeployState.ignoredInfo = ignoredInfo

		stageLog.Debug("changes detected, proceeding with deployment",
			slog.String("directory", s.DeployConfig.WorkingDirectory),
			slog.Bool("force_recreate", s.DeployConfig.ForceRecreate),
			slog.Group("has_changes",
				slog.Bool("compose_config", composeChanged),
				slog.Bool("auto_discovery_label", autoDiscoveryConfigChanged),
				slog.Any("files", changedServices),
				slog.Any("ignored_info", ignoredInfo),
				slog.Bool("images", imagesChanged),
				slog.Any("mismatch_services", mismatchServices),
			),
		)
	}

	return nil
}

// getAbsWorkingDir returns the absolute path of the working directory based on the repository path and the working directory specified in the deployment configuration.
func getAbsWorkingDir(repoPath, workingDir string) (string, error) {
	absPath, err := filepath.Abs(filepath.Join(repoPath, workingDir))
	if err != nil || !strings.HasPrefix(absPath, repoPath) {
		return absPath, errors.New("invalid working directory: resolved path is outside the allowed base directory: " + absPath)
	}

	return absPath, nil
}

// pkiRoleNormMap builds a map from resolved pki-role cert and private-key PEM
// values to stable placeholder strings (the ref string itself), for use with
// docker.WithNormalizedEnvValues during project hash computation.
//
// Certs issued by OpenBao pki-role refs are always freshly generated on every
// ResolveSecretReferences call, so their PEM bytes differ each time even when no
// configuration has changed. Using the ref string as a stable placeholder prevents
// the project hash from changing on every poll cycle.
func pkiRoleNormMap(externalSecrets map[string]secrettypes.ExternalSecretRef, env map[string]string) map[string]string {
	norm := make(map[string]string)

	for envVar, ref := range externalSecrets {
		if !strings.HasPrefix(ref.LegacyRef, "pki-role:") {
			continue
		}

		if v, ok := env[envVar]; ok && v != "" {
			norm[v] = ref.LegacyRef
		}

		// The matching private-key value is stored under <NAME>_KEY.
		keyName := envVar + "_KEY"
		if v, ok := env[keyName]; ok && v != "" {
			norm[v] = ref.LegacyRef + "_KEY"
		}
	}

	return norm
}
