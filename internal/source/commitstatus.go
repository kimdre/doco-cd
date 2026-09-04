package source

import (
	"context"
	"log/slog"
	"strings"

	"github.com/kimdre/doco-cd/internal/commitstatus"
	"github.com/kimdre/doco-cd/internal/common/lifecycle"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/webhook"
)

// postEarlyFailureCommitStatus reports a Git clone or deploy-configuration
// resolution failure as a commit status, before reconciliation ever starts.
// It is a no-op when GIT_COMMIT_STATUS is disabled, the source is not Git, or
// no access token/commit SHA is available. Errors are logged as warnings so
// they never mask the original failure returned to the caller.
func (p *Preparer) postEarlyFailureCommitStatus(
	ctx context.Context, req Request, sourceType config.SourceType, revision string, payload webhook.ParsedPayload, cause error,
) {
	commitSHA := strings.TrimSpace(revision)
	if commitSHA == "" {
		commitSHA = strings.TrimSpace(payload.CommitSHAString())
	}

	commitStatusReq, ok := commitstatus.ResolveRequest(req.Logger, commitstatus.RequestParams{
		Enabled:          p.appConfig.GitCommitStatus,
		SourceIsGit:      sourceType == config.SourceTypeGit,
		SourceURL:        req.SourceRef,
		CommitSHA:        commitSHA,
		PayloadWebURL:    payload.WebURL,
		PayloadFullName:  payload.FullName,
		ProviderOverride: p.appConfig.GitScmProvider,
		APIBaseURL:       string(p.appConfig.GitScmApiUrl),
		AccessToken:      p.appConfig.GitAccessToken,
		ContextName:      commitstatus.DeployContext,
	})
	if !ok {
		return
	}

	description := commitstatus.FailureDescription(cause)

	req.Logger.Debug("posting commit status",
		slog.String("provider", string(commitStatusReq.Provider)),
		slog.String("repository", commitStatusReq.RepoFullName),
		slog.String("commit_sha", commitStatusReq.CommitSHA),
		slog.String("context", commitStatusReq.Context),
		slog.String("state", string(commitstatus.StateError)),
		slog.String("description", description),
	)

	if err := commitStatusReq.Post(ctx, commitstatus.Status{
		State:       commitstatus.StateError,
		Description: description,
	}); err != nil {
		if lifecycle.IsCanceled(err) {
			req.Logger.Debug("skipped commit status during application shutdown", slog.String("error", err.Error()))

			return
		}

		req.Logger.Warn("failed to post commit status", slog.String("error", err.Error()))
	}
}
