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

	"github.com/kimdre/doco-cd/internal/commitstatus"
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

const maxCommitStatusDescriptionLength = 140

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

func earlyFailureCommitStatusDescription(err error) string {
	if err == nil {
		return "Failed"
	}

	description := strings.Join(strings.Fields(err.Error()), " ")
	if len([]rune(description)) <= maxCommitStatusDescriptionLength {
		return description
	}

	truncated := []rune(description)

	return string(truncated[:maxCommitStatusDescriptionLength-3]) + "..."
}

func postEarlyCommitStatus(ctx context.Context, jobLog *slog.Logger, appConfig *app.Config,
	sourceType config.SourceType, sourceRef, commitSHA string, payload webhook.ParsedPayload, contextName, description string,
) {
	if !appConfig.GitCommitStatus || config.NormalizeSourceType(sourceType) != config.SourceTypeGit {
		return
	}

	commitSHA = strings.TrimSpace(commitSHA)
	if commitSHA == "" {
		commitSHA = strings.TrimSpace(payload.CommitSHA)
	}

	if commitSHA == "" {
		jobLog.Debug("skipping commit status: no commit SHA available")

		return
	}

	resolved := git.ResolveAuthConfig(sourceRef, "", "", "")

	token := resolved.GitAccessToken
	if token == "" {
		token = appConfig.GitAccessToken
	}

	if token == "" {
		jobLog.Debug("skipping commit status: no access token configured")

		return
	}

	repoURL := strings.TrimSpace(payload.WebURL)
	if repoURL == "" {
		repoURL = sourceRef
	}

	repoFullName := strings.TrimSpace(payload.FullName)
	if repoFullName == "" {
		repoFullName = git.GetFullName(repoURL)
	}

	provider, _ := commitstatus.ParseProvider(appConfig.GitScmProvider)

	contextName = strings.TrimSpace(contextName)
	if contextName == "" {
		contextName = commitstatus.BaseContext
	}

	jobLog.Debug("posting commit status",
		slog.String("provider", string(provider)),
		slog.String("repository", repoFullName),
		slog.String("commit_sha", commitSHA),
		slog.String("context", contextName),
		slog.String("state", string(commitstatus.StateError)),
	)

	err := commitstatus.Post(ctx, provider, repoURL, repoFullName, commitSHA, token, commitstatus.Status{
		State:       commitstatus.StateError,
		Description: description,
		Context:     contextName,
	})
	if err != nil {
		jobLog.Warn("failed to post commit status", slog.String("error", err.Error()))
	}
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
	ociTrusted := sourceType != config.SourceTypeOCI

	switch sourceType {
	case config.SourceTypeGit:
		repo, err := git.CloneOrUpdateRepository(jobLog,
			sourceRef, ref, internalRepoPath, externalRepoPath,
			private, appConfig.SSHPrivateKey, appConfig.SSHPrivateKeyPassphrase, appConfig.GitAccessToken,
			appConfig.SkipTLSVerification, appConfig.HttpProxy, appConfig.GitCloneSubmodules, appConfig.GitCloneDepth,
		)
		if err != nil {
			postEarlyCommitStatus(ctx, jobLog, appConfig, sourceType, sourceRef, resolvedRevision, payload, commitstatus.BaseContext, earlyFailureCommitStatusDescription(err))

			return handleError{
				err:            err,
				msg:            "failed to clone repository",
				httpStatusCode: http.StatusInternalServerError,
			}
		}

		latestCommit, err := git.GetLatestCommit(repo, ref)
		if err == nil {
			resolvedRevision = strings.TrimSpace(latestCommit)
		}
	case config.SourceTypeOCI:
		resolvedDigest, err := oci.ResolveDigest(ctx, sourceRef, strings.TrimSpace(payload.Digest))
		if err != nil {
			return handleError{
				err:            err,
				msg:            "failed to resolve oci artifact digest",
				httpStatusCode: http.StatusInternalServerError,
			}
		}

		if err := oci.VerifyWithCosign(ctx, sourceRef, resolvedDigest, appConfig.OciTrustPolicy, config.OciTrustPolicyOverride{}, appConfig.OciVerifyMaxWorkers); err != nil {
			return handleError{
				err:            err,
				msg:            "failed OCI signature verification",
				httpStatusCode: http.StatusInternalServerError,
			}
		}

		pullResult, err := oci.PullAndExtract(ctx,
			sourceRef, resolvedDigest, config.OciArtifactLayoutV1,
			internalRepoPath, customTarget)
		if err != nil {
			return handleError{
				err:            err,
				msg:            "failed to pull oci artifact",
				httpStatusCode: http.StatusInternalServerError,
			}
		}

		resolvedRevision = pullResult.Digest
		ociTrusted = true
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
			postEarlyCommitStatus(ctx, jobLog, appConfig, sourceType, sourceRef, resolvedRevision, payload, commitstatus.BaseContext, earlyFailureCommitStatusDescription(err))

			return handleError{
				err:            err,
				msg:            "failed to get deploy configuration",
				httpStatusCode: http.StatusInternalServerError,
			}
		}
	case stages.JobTriggerPoll:
		deployConfigs, err = deploy.ResolveConfigs(pollConfig.Deployments, pollConfig.CustomTarget, ref, internalRepoPath, appConfig.DeployConfigBaseDir, gitOpts)
		if err != nil {
			postEarlyCommitStatus(ctx, jobLog, appConfig, sourceType, sourceRef, resolvedRevision, payload, commitstatus.BaseContext, earlyFailureCommitStatusDescription(err))

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

	for _, cfg := range deployConfigs {
		cfg.Internal.ConfigTarget = strings.TrimSpace(customTarget)
	}

	repoData := stages.RepositoryData{
		Source:       sourceType,
		SourceUrl:    sourceRef,
		Name:         repoName,
		PathInternal: internalRepoPath,
		PathExternal: externalRepoPath,
		Revision:     resolvedRevision,
		OCITrusted:   ociTrusted,
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
