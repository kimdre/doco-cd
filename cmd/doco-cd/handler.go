package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"path/filepath"
	"strings"

	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/config"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/reconciliation"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/source/oci"
	"github.com/kimdre/doco-cd/internal/stages"
	"github.com/kimdre/doco-cd/internal/webhook"
)

type handleError struct {
	msg string // errMsg
	err error  // detail err, can be nil if not applicable

	httpStatusCode int // http status code use to respond to http request
}

func logEntityForSourceType(sourceType config.SourceType) string {
	if config.NormalizeSourceType(sourceType) == config.SourceTypeOCI {
		return "artifact"
	}

	return "repository"
}

func (r handleError) Error() string {
	ret := r.msg

	if r.err != nil {
		ret = fmt.Sprintf("%s: %v", r.msg, r.err)
	}

	return ret
}

func handle(ctx context.Context, jobLog *slog.Logger,
	appConfig *app.Config,
	dataMountPoint container.MountPoint,
	secretProvider *secretprovider.SecretProvider,
	dockerCli command.Cli,
	jobTrigger stages.JobTrigger,
	sourceType config.SourceType, sourceRef string, ref string, private bool,
	metadata notification.Metadata,
	customTarget string, testName string,
	pollConfig poll.Config,
	payload webhook.ParsedPayload,
) error {
	sourceType = config.NormalizeSourceType(sourceType)
	if err := config.ValidateSourceType(sourceType); err != nil {
		return handleError{
			err:            err,
			msg:            "invalid source type",
			httpStatusCode: http.StatusBadRequest,
		}
	}

	if sourceType == config.SourceTypeGit {
		git.ConfigureAuthResolver(
			appConfig.GitAuthDomains,
			appConfig.SSHPrivateKey,
			appConfig.SSHPrivateKeyPassphrase,
			appConfig.GitAccessToken,
			git.GitHubAppConfig{
				ID:             appConfig.GitHubAppID,
				PrivateKey:     appConfig.GitHubAppPrivateKey,
				InstallationID: appConfig.GitHubAppInstallationID,
			},
		)
	}

	repoName := git.GetRepoName(sourceRef)
	if sourceType == config.SourceTypeOCI {
		repoName = oci.RepositoryNameFromArtifact(sourceRef)
	}

	logField := logEntityForSourceType(sourceType)

	logValue := repoName
	if sourceType == config.SourceTypeOCI {
		logValue = strings.TrimSpace(sourceRef)
	}

	jobLog = jobLog.With(
		slog.String(logField, logValue),
	)

	if customTarget != "" {
		jobLog = jobLog.With(slog.String("target", customTarget))
	}

	if strings.Contains(repoName, "..") {
		return handleError{
			err:            fmt.Errorf("invalid repository name: %s, contains '..'", repoName),
			msg:            "invalid repository name",
			httpStatusCode: http.StatusBadRequest,
		}
	}

	if err := swarm.RefreshModeEnabled(ctx, dockerCli.Client()); err != nil {
		return handleError{
			err:            err,
			msg:            "failed to check if docker host is running in swarm mode",
			httpStatusCode: http.StatusInternalServerError,
		}
	}

	// Path inside the container
	internalRepoPath, err := filesystem.VerifyAndSanitizePath(
		filepath.Join(dataMountPoint.Destination, repoName),
		dataMountPoint.Destination,
	)
	if err != nil {
		return handleError{
			err:            err,
			msg:            "failed to verify and sanitize internal filesystem path",
			httpStatusCode: http.StatusBadRequest,
		}
	}

	// Path on the host
	externalRepoPath, err := filesystem.VerifyAndSanitizePath(
		filepath.Join(dataMountPoint.Source, repoName),
		dataMountPoint.Source,
	)
	if err != nil {
		return handleError{
			err:            err,
			msg:            "failed to verify and sanitize external filesystem path",
			httpStatusCode: http.StatusBadRequest,
		}
	}

	resolvedRevision := strings.TrimSpace(payload.Digest)

	switch sourceType {
	case config.SourceTypeGit:
		if _, err := git.CloneOrUpdateRepository(jobLog,
			sourceRef, ref, internalRepoPath, externalRepoPath,
			private, appConfig.SSHPrivateKey, appConfig.SSHPrivateKeyPassphrase, appConfig.GitAccessToken,
			appConfig.SkipTLSVerification, appConfig.HttpProxy, appConfig.GitCloneSubmodules, appConfig.GitCloneDepth,
		); err != nil {
			return handleError{
				err:            err,
				msg:            "failed to clone repository",
				httpStatusCode: http.StatusInternalServerError,
			}
		}
	case config.SourceTypeOCI:
		pullResult, err := oci.PullAndExtract(ctx,
			sourceRef, strings.TrimSpace(payload.Digest), config.OciArtifactLayoutV1,
			internalRepoPath, customTarget)
		if err != nil {
			return handleError{
				err:            err,
				msg:            "failed to pull oci artifact",
				httpStatusCode: http.StatusInternalServerError,
			}
		}

		resolvedRevision = pullResult.Digest
		payload.Source = webhook.PayloadSourceOCI
		payload.Artifact = sourceRef
		payload.Digest = pullResult.Digest

		payload.CommitSHA = pullResult.Digest
		if payload.FullName == "" {
			payload.FullName = repoName
		}

		if payload.Name == "" {
			payload.Name = path.Base(repoName)
		}

		if payload.WebURL == "" {
			payload.WebURL = sourceRef
		}
	}

	jobLog.Debug("retrieving deployment configuration")

	var deployConfigs []*deploy.Config

	gitOpts := &deploy.GitOptions{
		SSHPrivateKey:           appConfig.SSHPrivateKey,
		SSHPrivateKeyPassphrase: appConfig.SSHPrivateKeyPassphrase,
		GitAccessToken:          appConfig.GitAccessToken,
		SkipTLSVerification:     appConfig.SkipTLSVerification,
		HttpProxy:               appConfig.HttpProxy,
		GitCloneSubmodules:      appConfig.GitCloneSubmodules,
		GitCloneDepth:           appConfig.GitCloneDepth,
	}

	switch jobTrigger {
	case stages.JobTriggerWebhook:
		deployConfigs, err = deploy.GetConfigs(internalRepoPath, appConfig.DeployConfigBaseDir, customTarget, payload.Ref, gitOpts)
		if err != nil {
			return handleError{
				err:            err,
				msg:            "failed to get deploy configuration",
				httpStatusCode: http.StatusInternalServerError,
			}
		}
	case stages.JobTriggerPoll:
		deployConfigs, err = deploy.ResolveConfigs(pollConfig.Deployments, pollConfig.CustomTarget, ref, internalRepoPath, appConfig.DeployConfigBaseDir, gitOpts)
		if err != nil {
			return handleError{
				err:            err,
				msg:            "failed to get deploy configuration",
				httpStatusCode: http.StatusInternalServerError,
			}
		}
	default:
		return handleError{
			err:            fmt.Errorf("unsupported job trigger: %s", jobTrigger),
			msg:            "unsupported job trigger",
			httpStatusCode: http.StatusBadRequest,
		}
	}

	// For OCI sources, the deploy config's reference must reflect the actual artifact tag that
	// triggered this deployment (e.g. "latest"), overriding any reference baked into the config file.
	if sourceType == config.SourceTypeOCI && ref != "" {
		for _, cfg := range deployConfigs {
			cfg.Reference = ref
		}
	}

	repoData := stages.RepositoryData{
		Source:       sourceType,
		SourceUrl:    sourceRef,
		Name:         repoName,
		PathInternal: internalRepoPath,
		PathExternal: externalRepoPath,
		Revision:     resolvedRevision,
	}

	if err := reconciliation.Deploy(ctx, jobLog, appConfig,
		dataMountPoint, dockerCli, secretProvider, metadata, jobTrigger,
		repoData, deployConfigs, &payload, testName); err != nil {
		return handleError{
			err:            err,
			msg:            "deployment failed",
			httpStatusCode: http.StatusInternalServerError,
		}
	}

	return nil
}
