package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/poll"

	"github.com/kimdre/doco-cd/internal/lock"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/stages"

	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/prometheus"
	"github.com/kimdre/doco-cd/internal/utils/id"
	"github.com/kimdre/doco-cd/internal/webhook"
)

var ErrInvalidHTTPMethod = errors.New("invalid http method")

func repositoryNameFromWebhookPayload(payload webhook.ParsedPayload) string {
	if payload.FullName != "" {
		return payload.FullName
	}

	if payload.CloneURL != "" {
		return git.GetRepoName(payload.CloneURL)
	}

	if payload.Artifact != "" {
		return payload.Artifact
	}

	return "unknown"
}

type handlerData struct {
	appConfig      *app.Config          // Application configuration
	appVersion     string               // Application version
	dataMountPoint container.MountPoint // Mount point for the data directory
	dockerCli      command.Cli          // Docker CLI client
	log            *logger.Logger       // Logger for logging messages
	runPoll        pollRunner
	secretProvider *secretprovider.SecretProvider
	testName       string // Overwrites the deployConfig.Name to make test deployments unique and prevent conflicts between tests when running in parallel. Not used in production.
}

// onError handles errors by logging them, sending a JSON error response, and sending a notification.
func onError(w http.ResponseWriter, log *slog.Logger, errMsg string, details any, statusCode int, metadata notification.Metadata) {
	prometheus.WebhookErrorsTotal.WithLabelValues(metadata.Repository).Inc()
	log.Error(errMsg)
	JSONError(w,
		errMsg,
		details,
		metadata.JobID,
		statusCode)

	if _, ok := details.(error); ok {
		details = fmt.Sprintf("%v", details)
	}

	if details != "" {
		errMsg = fmt.Sprintf("%s\n%s", errMsg, details)
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				logRecoveredPanic(log, "webhook error notification", r)
			}
		}()

		err := notification.Send(notification.Failure, "Deployment Failed", errMsg, metadata)
		if err != nil {
			log.Error("failed to send notification", logger.ErrAttr(err))
		}
	}()
}

// HandleEvent executes the deployment process for a given webhook event.
func HandleEvent(ctx context.Context, jobLog *slog.Logger, w http.ResponseWriter, appConfig *app.Config,
	dataMountPoint container.MountPoint, payload webhook.ParsedPayload, customTarget string, metadata notification.Metadata,
	dockerCli command.Cli, secretProvider *secretprovider.SecretProvider,
	testName string,
) {
	startTime := time.Now()
	repoName := repositoryNameFromWebhookPayload(payload)

	if payload.Source != webhook.PayloadSourceOCI && payload.Ref == "" {
		msg := "no reference provided in webhook payload, skipping event"
		jobLog.Warn(msg)
		JSONError(w, msg, msg, metadata.JobID, http.StatusBadRequest)

		return
	}

	sourceType := config.SourceTypeGit

	sourceRef := payload.CloneURL
	if payload.Source == webhook.PayloadSourceOCI {
		sourceType = config.SourceTypeOCI
		sourceRef = payload.Artifact
	}

	entity := logEntityForSourceType(sourceType)

	logValue := repoName
	if sourceType == config.SourceTypeOCI {
		logValue = sourceRef
	}

	jobLog = jobLog.With(slog.String(entity, logValue))

	if customTarget != "" {
		jobLog = jobLog.With(slog.String("target", customTarget))
	}

	jobLog.Info("received new "+entity+" job",
		slog.Group("trigger",
			slog.String("commit", payload.CommitSHA), slog.String("ref", payload.Ref),
			slog.String("event", string(stages.JobTriggerWebhook))))

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

	// Only attempt SSH clone when URL-specific credentials include an SSH private key.
	resolvedSSH := git.ResolveAuthConfig(payload.SSHUrl, appConfig.SSHPrivateKey, appConfig.SSHPrivateKeyPassphrase, appConfig.GitAccessToken)
	if sourceType == config.SourceTypeGit && payload.SSHUrl != "" && resolvedSSH.SSHPrivateKey != "" {
		sshAuth, authErr := git.GetAuthMethod(payload.SSHUrl, appConfig.SSHPrivateKey, appConfig.SSHPrivateKeyPassphrase, appConfig.GitAccessToken)
		if authErr != nil {
			onError(w, jobLog.With(logger.ErrAttr(authErr)), "failed to resolve SSH auth method", authErr.Error(), http.StatusInternalServerError, metadata)

			return
		}

		if sshAuth != nil {
			sourceRef = payload.SSHUrl
		}
	}

	deployErr := handle(ctx, jobLog,
		appConfig, dataMountPoint, secretProvider, dockerCli,
		stages.JobTriggerWebhook, sourceType, sourceRef, payload.Ref, payload.Private,
		metadata, customTarget, testName, poll.Config{}, payload,
	)
	if deployErr != nil {
		// In synchronous mode we should return an error to the caller
		// For async mode, w is noopResponseWriter and JSONError is a no-op
		if hr, ok := deployErr.(handleError); ok {
			onError(w, jobLog.With(logger.ErrAttr(hr.err)), hr.msg, hr.err.Error(), hr.httpStatusCode, metadata)
		} else {
			onError(w, jobLog.With(logger.ErrAttr(deployErr)), "deployment failed", deployErr.Error(), http.StatusInternalServerError, metadata)
		}

		return
	}

	msg := "job completed successfully"
	elapsedTime := time.Since(startTime)
	jobLog.Info(msg, slog.String("elapsed_time", elapsedTime.Truncate(time.Millisecond).String()))
	JSONResponse(w, msg, metadata.JobID, http.StatusCreated)

	prometheus.WebhookRequestsTotal.WithLabelValues(repoName).Inc()
	prometheus.WebhookDuration.WithLabelValues(repoName).Observe(elapsedTime.Seconds())
}

// WebhookHandler handles incoming webhook requests.
func (h *handlerData) WebhookHandler(w http.ResponseWriter, r *http.Request) {
	ctx := context.WithoutCancel(r.Context())

	customTarget := r.PathValue("customTarget")

	// Add a job id to the context to track deployments in the logs
	jobID := id.GenID()

	jobLog := h.log.With(slog.String("job_id", jobID))

	jobLog.Debug("received webhook event")

	// If wait=false (default), return immediately and run the deployment in the background.
	// If wait=true, run the deployment synchronously and return when it's completed.
	wait := false
	if v := r.URL.Query().Get("wait"); v != "" {
		// Only treat explicit "true" as synchronous. Everything else (including invalid) is async.
		wait = strings.EqualFold(v, "true") || v == "1"
	}

	metadata := notification.Metadata{
		JobID:      jobID,
		Repository: "unknown", // Will be updated later if we can parse the payload
		Stack:      "",
		Revision:   "",
	}

	// Limit the request body size
	r.Body = http.MaxBytesReader(w, r.Body, h.appConfig.MaxPayloadSize)

	provider, payload, err := webhook.Parse(r, h.appConfig.WebhookSecret)
	if err != nil {
		var (
			statusCode int
			errMsg     string
		)

		switch {
		case errors.Is(err, webhook.ErrHMACVerificationFailed):
			errMsg = webhook.ErrIncorrectSecretKey.Error()
			statusCode = http.StatusUnauthorized
		case errors.Is(err, webhook.ErrGitlabTokenVerificationFailed):
			errMsg = webhook.ErrGitlabTokenVerificationFailed.Error()
			statusCode = http.StatusUnauthorized
		case errors.Is(err, webhook.ErrMissingSecurityHeader):
			errMsg = webhook.ErrMissingSecurityHeader.Error()
			statusCode = http.StatusBadRequest
		case errors.Is(err, webhook.ErrParsingPayload):
			errMsg = webhook.ErrParsingPayload.Error()
			statusCode = http.StatusInternalServerError
		case errors.Is(err, webhook.ErrInvalidHTTPMethod):
			errMsg = webhook.ErrInvalidHTTPMethod.Error()
			statusCode = http.StatusMethodNotAllowed
		default:
			errMsg = webhook.ErrParsingPayload.Error()
			statusCode = http.StatusInternalServerError
		}

		if repositoryName := repositoryNameFromWebhookPayload(payload); repositoryName != "unknown" {
			metadata.Repository = repositoryName
			metadata.Revision = notification.GetRevision(payload.Ref, payload.CommitSHA)
		}

		onError(w, jobLog.With(slog.String("ip", r.RemoteAddr), logger.ErrAttr(err)), errMsg, err.Error(), statusCode, metadata)

		return
	}

	if deletionEvent, eErr := webhook.IsBranchOrTagDeletionEvent(r, payload, provider); eErr == nil && deletionEvent {
		errMsg := "branch or tag deletion event received, skipping webhook event"
		jobLog.Info(errMsg)
		JSONResponse(w, errMsg, jobID, http.StatusAccepted)

		return
	} else if eErr != nil {
		errMsg := "failed to check if event is branch or tag deletion"
		jobLog.Error(errMsg, logger.ErrAttr(eErr))
		JSONError(w, errMsg, eErr.Error(), jobID, http.StatusInternalServerError)

		return
	}

	if metadata.Repository == "" || metadata.Repository == "unknown" {
		metadata.Repository = repositoryNameFromWebhookPayload(payload)
		metadata.Revision = notification.GetRevision(payload.Ref, payload.CommitSHA)
	}

	lockEntity := "repository"
	lockLogValue := metadata.Repository

	if payload.Source == webhook.PayloadSourceOCI {
		lockEntity = "artifact"
		lockLogValue = payload.Artifact
	}

	// Prevent concurrent deployments for the same repository using a lock
	repoLock := lock.GetRepoLock(metadata.Repository)

	handleFn := func(w http.ResponseWriter) {
		defer func() {
			if r := recover(); r != nil {
				logRecoveredPanic(jobLog, "webhook deployment", r)
			}
		}()

		locked := make(chan struct{})

		go func() {
			repoLock.Lock()
			close(locked)
		}()

		select {
		case <-locked:
			// Acquired immediately
		case <-time.After(10 * time.Millisecond):
			jobLog.Info("waiting for webhook "+lockEntity+" lock", slog.String(lockEntity, lockLogValue))
			<-locked
		}

		defer repoLock.Unlock()

		HandleEvent(ctx, jobLog, w, h.appConfig, h.dataMountPoint, payload, customTarget, metadata, h.dockerCli, h.secretProvider, h.testName)
	}

	if wait {
		handleFn(w)
	} else {
		// Async mode: respond immediately and run the deployment in the background.
		JSONResponse(w, "job accepted", jobID, http.StatusAccepted)

		go handleFn(noopResponseWriter{})
	}
}

// noopResponseWriter is used when we run HandleEvent asynchronously.
// It prevents writes to the original HTTP connection after we've already responded.
type noopResponseWriter struct{}

func (noopResponseWriter) Header() http.Header       { return http.Header{} }
func (noopResponseWriter) Write([]byte) (int, error) { return 0, nil }
func (noopResponseWriter) WriteHeader(_ int)         {}
