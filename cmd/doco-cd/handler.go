package main

import (
	"context"
	"errors"
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
	"github.com/kimdre/doco-cd/internal/common/validation"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/reconciliation"
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

func (r handleError) Unwrap() error {
	return r.err
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
		commitSHA = strings.TrimSpace(payload.CommitSHAString())
	}

	if commitSHA == "" {
		jobLog.Debug("skipping commit status: no commit SHA available")

		return
	}

	resolved := git.ResolveAuthConfig(sourceRef, "", "", "")

	token, err := git.ResolveHTTPToken(sourceRef, resolved)
	if err != nil {
		jobLog.Warn("failed to resolve commit status token", slog.String("error", err.Error()))
	}

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
		contextName = commitstatus.DeployContext
	}

	jobLog.Debug("posting commit status",
		slog.String("provider", string(provider)),
		slog.String("repository", repoFullName),
		slog.String("commit_sha", commitSHA),
		slog.String("context", contextName),
		slog.String("state", string(commitstatus.StateError)),
		slog.String("description", description),
	)

	err = commitstatus.Post(ctx, provider, string(appConfig.GitScmApiUrl), repoURL, repoFullName, commitSHA, token, commitstatus.Status{
		State:       commitstatus.StateError,
		Description: description,
		Context:     contextName,
	})
	if err != nil {
		jobLog.Warn("failed to post commit status", slog.String("error", err.Error()))
	}
}

// handleRequest bundles handle's per-call, per-deployment-request input: the source location and
// its trigger/reference/visibility, notification metadata, an optional custom deploy target,
// an optional test identity, poll configuration (used only for poll-triggered requests), and the
// parsed webhook payload (zero value for non-webhook triggers).
type handleRequest struct {
	JobTrigger   stages.JobTrigger `validate:"required,oneof=webhook poll"`
	SourceType   config.SourceType `validate:"required"`
	SourceRef    string            `validate:"required"`
	Ref          string
	Private      bool
	Metadata     notification.Metadata
	CustomTarget string
	TestName     string
	PollConfig   poll.Config
	Payload      webhook.ParsedPayload
}

func handle(ctx context.Context, jobLog *slog.Logger,
	reconciliationManager *reconciliation.Manager,
	appConfig *app.Config,
	dataMountPoint container.MountPoint,
	dockerCli command.Cli,
	req handleRequest,
) error {
	if err := validation.Validate(req); err != nil {
		return handleError{
			err:            err,
			msg:            "invalid deployment request",
			httpStatusCode: http.StatusInternalServerError,
		}
	}

	sourceType := config.NormalizeSourceType(req.SourceType)
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
			appConfig.GitAccessTokenUser,
			git.GitHubAppConfig{
				ID:             appConfig.GitHubAppID,
				PrivateKey:     appConfig.GitHubAppPrivateKey,
				InstallationID: appConfig.GitHubAppInstallationID,
			},
		)
	}

	repoName := git.GetRepoName(req.SourceRef)
	if sourceType == config.SourceTypeOCI {
		repoName = oci.RepositoryNameFromArtifact(req.SourceRef)
	}

	logField := logEntityForSourceType(sourceType)

	logValue := repoName
	if sourceType == config.SourceTypeOCI {
		logValue = strings.TrimSpace(req.SourceRef)
	}

	jobLog = jobLog.With(
		slog.String(logField, logValue),
	)

	if req.CustomTarget != "" {
		jobLog = jobLog.With(slog.String("target", req.CustomTarget))
		req.Metadata.Target = strings.TrimSpace(req.CustomTarget)
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

	resolvedRevision := strings.TrimSpace(req.Payload.Digest)
	ociTrusted := sourceType != config.SourceTypeOCI

	switch sourceType {
	case config.SourceTypeGit:
		// Skip the network fetch when the payload carries the exact commit SHA and
		// the local repo HEAD already matches it (e.g. webhook re-deliveries).
		if sha := strings.TrimSpace(req.Payload.CommitSHAString()); sha != "" {
			if matches, _ := git.HeadMatchesCommit(internalRepoPath, sha); matches {
				jobLog.Debug("skipping fetch, repository already at requested commit", slog.String("commit", sha))

				if repo, openErr := git.OpenRepository(internalRepoPath); openErr == nil {
					if latestCommit, latestErr := git.GetLatestCommit(repo, req.Ref); latestErr == nil {
						resolvedRevision = strings.TrimSpace(latestCommit)
					}
				}

				break
			}
		}

		repo, err := git.CloneOrUpdateRepository(jobLog,
			req.SourceRef, req.Ref, internalRepoPath, externalRepoPath,
			req.Private, appConfig.SSHPrivateKey, appConfig.SSHPrivateKeyPassphrase, appConfig.GitAccessToken,
			appConfig.SkipTLSVerification, appConfig.HttpProxy, appConfig.GitCloneSubmodules, appConfig.GitCloneDepth,
		)
		if err != nil {
			postEarlyCommitStatus(ctx, jobLog, appConfig, sourceType, req.SourceRef, resolvedRevision, req.Payload, commitstatus.DeployContext, earlyFailureCommitStatusDescription(err))

			return handleError{
				err:            err,
				msg:            "failed to clone repository",
				httpStatusCode: http.StatusInternalServerError,
			}
		}

		latestCommit, err := git.GetLatestCommit(repo, req.Ref)
		if err == nil {
			resolvedRevision = strings.TrimSpace(latestCommit)
		}
	case config.SourceTypeOCI:
		resolvedDigest, err := oci.ResolveDigest(ctx, req.SourceRef, strings.TrimSpace(req.Payload.Digest))
		if err != nil {
			return handleError{
				err:            err,
				msg:            "failed to resolve oci artifact digest",
				httpStatusCode: http.StatusInternalServerError,
			}
		}

		if err := oci.VerifyWithCosign(ctx, req.SourceRef, resolvedDigest, appConfig.OciTrustPolicy, config.OciTrustPolicyOverride{}, appConfig.OciVerifyMaxWorkers); err != nil {
			return handleError{
				err:            err,
				msg:            "failed OCI signature verification",
				httpStatusCode: http.StatusInternalServerError,
			}
		}

		pullResult, err := oci.PullAndExtract(ctx,
			req.SourceRef, resolvedDigest, config.OciArtifactLayoutV1,
			internalRepoPath, req.CustomTarget)
		if err != nil {
			return handleError{
				err:            err,
				msg:            "failed to pull oci artifact",
				httpStatusCode: http.StatusInternalServerError,
			}
		}

		resolvedRevision = pullResult.Digest
		ociTrusted = true
		req.Payload.Source = webhook.PayloadSourceOCI
		req.Payload.Artifact = req.SourceRef
		req.Payload.Digest = pullResult.Digest

		req.Payload.Trigger = pullResult.Digest
		if req.Payload.FullName == "" {
			req.Payload.FullName = repoName
		}

		if req.Payload.Name == "" {
			req.Payload.Name = path.Base(repoName)
		}

		if req.Payload.WebURL == "" {
			req.Payload.WebURL = req.SourceRef
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

	switch req.JobTrigger {
	case stages.JobTriggerWebhook:
		deployConfigs, err = deploy.GetConfigs(internalRepoPath, appConfig.DeployConfigBaseDir, req.CustomTarget, req.Payload.Ref, gitOpts)
		if err != nil {
			postEarlyCommitStatus(ctx, jobLog, appConfig, sourceType, req.SourceRef, resolvedRevision, req.Payload, commitstatus.DeployContext, earlyFailureCommitStatusDescription(err))

			return handleError{
				err:            err,
				msg:            "failed to get deploy configuration",
				httpStatusCode: http.StatusInternalServerError,
			}
		}
	case stages.JobTriggerPoll:
		deployConfigs, err = deploy.ResolveConfigs(req.PollConfig.Deployments, req.PollConfig.CustomTarget, req.Ref, internalRepoPath, appConfig.DeployConfigBaseDir, gitOpts)
		if err != nil {
			postEarlyCommitStatus(ctx, jobLog, appConfig, sourceType, req.SourceRef, resolvedRevision, req.Payload, commitstatus.DeployContext, earlyFailureCommitStatusDescription(err))

			return handleError{
				err:            err,
				msg:            "failed to get deploy configuration",
				httpStatusCode: http.StatusInternalServerError,
			}
		}
	default:
		return handleError{
			err:            fmt.Errorf("unsupported job trigger: %s", req.JobTrigger),
			msg:            "unsupported job trigger",
			httpStatusCode: http.StatusBadRequest,
		}
	}

	// For OCI sources, the deploy config's reference must reflect the actual artifact tag that
	// triggered this deployment (e.g. "latest"), overriding any reference baked into the config file.
	if sourceType == config.SourceTypeOCI && req.Ref != "" {
		for _, cfg := range deployConfigs {
			cfg.Reference = req.Ref
		}
	}

	for _, cfg := range deployConfigs {
		cfg.Internal.ConfigTarget = strings.TrimSpace(req.CustomTarget)
		if req.Metadata.DeploymentTargetObserver != nil {
			req.Metadata.DeploymentTargetObserver(cfg.Name, cfg.Context)
		}
	}

	repoData := stages.RepositoryData{
		Source:       sourceType,
		SourceUrl:    req.SourceRef,
		Name:         repoName,
		PathInternal: internalRepoPath,
		PathExternal: externalRepoPath,
		Revision:     resolvedRevision,
		OCITrusted:   ociTrusted,
	}

	if reconciliationManager == nil {
		return handleError{
			err:            errors.New("reconciliation manager is required"),
			msg:            "deployment failed",
			httpStatusCode: http.StatusInternalServerError,
		}
	}

	if err := reconciliationManager.Deploy(ctx, reconciliation.DeployRequest{
		Logger:        jobLog,
		Metadata:      req.Metadata,
		JobTrigger:    req.JobTrigger,
		Repository:    repoData,
		DeployConfigs: deployConfigs,
		Payload:       &req.Payload,
		TestName:      req.TestName,
	}); err != nil {
		if errors.Is(err, stages.ErrWebhookFilterMismatch) {
			return err
		}

		return handleError{
			err:            err,
			msg:            "deployment failed",
			httpStatusCode: http.StatusInternalServerError,
		}
	}

	return nil
}
