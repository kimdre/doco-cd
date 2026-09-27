package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/kimdre/doco-cd/internal/commitstatus"
	"github.com/kimdre/doco-cd/internal/common/lifecycle"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/selfupdate"
)

func selfUpdateMetadata(record selfupdate.Record) notification.Metadata {
	return notification.Metadata{
		Repository: record.Source.FullName,
		Stack:      record.Stack,
		Context:    record.Context,
		Target:     record.Source.ConfigTarget,
		Revision:   record.Source.CommitSHA,
		JobID:      record.Source.JobID,
	}
}

// selfUpdateReporter reports a resolved handover on behalf of the deployment
// that started it: the notification and the final state of the commit status
// that deployment left pending. A nil reporter reports nothing.
type selfUpdateReporter struct {
	notifier  *notification.Notifier
	appConfig *app.Config
}

func newSelfUpdateReporter(appConfig *app.Config, notifier *notification.Notifier) *selfUpdateReporter {
	return &selfUpdateReporter{notifier: notifier, appConfig: appConfig}
}

func (r *selfUpdateReporter) reportSuccess(ctx context.Context, log *logger.Logger, record selfupdate.Record) {
	if r == nil {
		return
	}

	if r.notifier != nil {
		err := r.notifier.Send(
			notification.Success,
			"Deployment completed",
			fmt.Sprintf("Successfully deployed stack %s (self-update, %s)", record.Stack, record.Strategy),
			selfUpdateMetadata(record),
		)
		if err != nil {
			log.Warn("self-update: failed to send the success notification", logger.ErrAttr(err))
		}
	}

	var startedAt time.Time
	if record.Source.CommitStatus != nil {
		startedAt = record.Source.CommitStatus.StartedAt
	}

	r.postCommitStatus(ctx, log, record, commitstatus.StateSuccess, commitstatus.SuccessDescription(startedAt, time.Now()))
}

func (r *selfUpdateReporter) reportFailure(ctx context.Context, log *logger.Logger, record selfupdate.Record) {
	if r == nil {
		return
	}

	reason := record.Error
	if reason == "" {
		reason = "the new version did not become healthy"
	}

	if r.notifier != nil {
		err := r.notifier.Send(
			notification.Failure,
			"Deployment failed",
			fmt.Sprintf("Self-update of stack %s was rolled back: %s", record.Stack, reason),
			selfUpdateMetadata(record),
		)
		if err != nil {
			log.Warn("self-update: failed to send the failure notification", logger.ErrAttr(err))
		}
	}

	r.postCommitStatus(ctx, log, record, commitstatus.StateFailure, commitstatus.FailureDescription(errors.New(reason)))
}

// postCommitStatus resolves the commit status the predecessor left pending. The
// token is resolved from this process's configuration, never from the journal.
func (r *selfUpdateReporter) postCommitStatus(
	ctx context.Context, log *logger.Logger, record selfupdate.Record, state commitstatus.State, description string,
) {
	info := record.Source.CommitStatus
	if r.appConfig == nil || info == nil {
		return
	}

	req, ok := commitstatus.ResolveRequest(log.Logger, commitstatus.RequestParams{
		Enabled: r.appConfig.GitCommitStatus,
		// Only Git deployments record a commit status target.
		SourceIsGit:      true,
		SourceURL:        info.SourceURL,
		CommitSHA:        info.CommitSHA,
		PayloadWebURL:    info.RepoURL,
		PayloadFullName:  info.FullName,
		ProviderOverride: r.appConfig.GitScmProvider,
		APIBaseURL:       string(r.appConfig.GitScmApiUrl),
		AccessToken:      r.appConfig.GitAccessToken,
		ContextName:      info.Context,
	})
	if !ok {
		return
	}

	log.Debug("self-update: posting commit status",
		slog.String("id", record.ID),
		slog.String("provider", string(req.Provider)),
		slog.String("repository", req.RepoFullName),
		slog.String("commit_sha", req.CommitSHA),
		slog.String("context", req.Context),
		slog.String("state", string(state)),
		slog.String("description", description),
	)

	if err := req.Post(ctx, commitstatus.Status{State: state, Description: description}); err != nil {
		if lifecycle.IsCanceled(err) {
			log.Debug("self-update: skipped commit status during application shutdown", logger.ErrAttr(err))

			return
		}

		log.Warn("self-update: failed to post the commit status", logger.ErrAttr(err))
	}
}
