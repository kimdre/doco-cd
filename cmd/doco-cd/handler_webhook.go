package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/common/id"

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
	"github.com/kimdre/doco-cd/internal/webhook"
)

var ErrInvalidHTTPMethod = errors.New("invalid http method")

// normalizeSourceURLRewriteKey normalizes the source URL rewrite key
// by trimming whitespace and converting it to lowercase.
func normalizeSourceURLRewriteKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// resolveWebhookGitCloneURL checks if there is a configured clone URL override for the given webhook payload and
// returns the resolved clone URL along with a boolean indicating whether an override was applied.
func resolveWebhookGitCloneURL(payload webhook.ParsedPayload, appConfig *app.Config) (string, bool) {
	defaultURL := strings.TrimSpace(payload.CloneURL)
	if appConfig == nil || len(appConfig.SourceURLRewrites) == 0 {
		return defaultURL, false
	}

	return rewriteSourceURL(defaultURL, appConfig.SourceURLRewrites)
}

// rewriteSourceURL applies the configured source URL rewrites to the given source URL and
// returns the rewritten URL along with a boolean indicating whether a rewrite was applied.
func rewriteSourceURL(sourceURL string, rewrites map[string]string) (string, bool) {
	sourceURL = strings.TrimSpace(sourceURL)
	if sourceURL == "" || len(rewrites) == 0 {
		return sourceURL, false
	}

	keys := make([]string, 0, len(rewrites))
	for key := range rewrites {
		normalizedKey := normalizeSourceURLRewriteKey(key)
		if normalizedKey == "" {
			continue
		}

		keys = append(keys, normalizedKey)
	}

	sort.Slice(keys, func(i, j int) bool {
		return len(keys[i]) > len(keys[j])
	})

	for _, key := range keys {
		target := strings.TrimSpace(rewrites[key])
		if target == "" {
			continue
		}

		if strings.HasPrefix(sourceURL, key) {
			return target + sourceURL[len(key):], true
		}

		if rewrittenURL, ok := rewriteGitURLHost(sourceURL, key, target); ok {
			return rewrittenURL, true
		}
	}

	return sourceURL, false
}

// rewriteGitURLHost rewrites the host in the given Git URL if it matches the specified matchHost,
// replacing it with the target host.
func rewriteGitURLHost(sourceURL, matchHost, target string) (string, bool) {
	if rewrittenURL, ok := rewriteHostInStandardURL(sourceURL, matchHost, target); ok {
		return rewrittenURL, true
	}

	return rewriteHostInSCPURL(sourceURL, matchHost, target)
}

// rewriteHostInStandardURL rewrites the host in a standard URL (e.g., "https://example.com/repo.git")
// if it matches the specified matchHost,.
func rewriteHostInStandardURL(sourceURL, matchHost, target string) (string, bool) {
	u, err := url.Parse(sourceURL)
	if err != nil || u.Host == "" {
		return "", false
	}

	if !hostsMatch(u.Hostname(), u.Host, matchHost) {
		return "", false
	}

	if replacementURL, repErr := url.Parse(target); repErr == nil && replacementURL.Host != "" {
		if replacementURL.Scheme != "" {
			u.Scheme = replacementURL.Scheme
		}

		if replacementURL.User != nil {
			u.User = replacementURL.User
		}

		u.Host = replacementURL.Host

		return u.String(), true
	}

	u.Host = target

	return u.String(), true
}

// rewriteHostInSCPURL rewrites the host in an SCP-style Git URL (e.g., "git@github.com:user/repo.git")
// if it matches the specified matchHost, replacing it with the target host.
func rewriteHostInSCPURL(sourceURL, matchHost, target string) (string, bool) {
	if strings.Contains(sourceURL, "://") {
		return "", false
	}

	at := strings.Index(sourceURL, "@")

	colon := strings.Index(sourceURL, ":")
	if at <= 0 || colon <= at+1 {
		return "", false
	}

	user := sourceURL[:at]
	host := sourceURL[at+1 : colon]

	repoPath := sourceURL[colon+1:]
	if repoPath == "" || !hostsMatch(host, host, matchHost) {
		return "", false
	}

	targetHost := target
	targetUser := user

	if replacementURL, err := url.Parse(target); err == nil && replacementURL.Host != "" {
		targetHost = replacementURL.Host
		if replacementURL.User != nil {
			targetUser = replacementURL.User.Username()
		}
	} else if strings.Contains(target, "@") {
		parts := strings.SplitN(target, "@", 2)
		if strings.TrimSpace(parts[0]) != "" {
			targetUser = strings.TrimSpace(parts[0])
		}

		targetHost = strings.TrimSpace(parts[1])
	}

	if targetHost == "" {
		return "", false
	}

	return fmt.Sprintf("%s@%s:%s", targetUser, targetHost, repoPath), true
}

// hostsMatch checks if the given hostname or host with port matches the specified rule,
// considering normalization and potential port differences.
func hostsMatch(hostname, hostWithPort, rule string) bool {
	rule = normalizeSourceURLRewriteKey(rule)
	if rule == "" || strings.ContainsAny(rule, "/@") {
		return false
	}

	normalizedHostname := normalizeSourceURLRewriteKey(hostname)
	normalizedHostWithPort := normalizeSourceURLRewriteKey(hostWithPort)

	if strings.Contains(rule, ":") {
		return normalizedHostWithPort == rule
	}

	return normalizedHostname == rule
}

// shouldUsePayloadSSHURL determines whether to use the SSH URL from the webhook payload for cloning,
// based on whether a clone URL override was applied and the presence of an SSH private key in the resolved auth config.
func shouldUsePayloadSSHURL(overrideApplied bool, payloadSSHURL string, resolved git.ResolvedAuthConfig) bool {
	if overrideApplied {
		return false
	}

	return strings.TrimSpace(payloadSSHURL) != "" && resolved.SSHPrivateKey != ""
}

// repositoryNameFromWebhookPayload extracts the repository name from the webhook payload,
// prioritizing the full name, then the clone URL, and finally the artifact. If none are available, it returns "unknown".
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
	runTracker     *deploymentRunTracker
	runPoll        pollRunner
	secretProvider *secretprovider.SecretProvider
	testName       string // Overwrites the deployConfig.Name to make test deployments unique and prevent conflicts between tests when running in parallel. Not used in production.
}

// onError handles errors by logging them, sending a JSON error response, and sending a notification.
// cause is the error behind the response: when a failure notification was already
// sent for it deeper down, the HTTP response and the log stay the same and only
// the second notification is dropped.
func onError(w http.ResponseWriter, log *slog.Logger, errMsg string, details any, statusCode int, metadata notification.Metadata, cause error) {
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

	if notification.WasNotified(cause) {
		return
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
	testName string, runTracker *deploymentRunTracker,
) {
	startTime := time.Now()

	repoName := repositoryNameFromWebhookPayload(payload)
	if runTracker != nil {
		runTracker.SetMetadata(metadata.JobID, repoName, customTarget, notification.GetRevision(payload.Ref, payload.RevisionString()))
		runTracker.MarkRunning(metadata.JobID)
	}

	if payload.Source != webhook.PayloadSourceOCI && payload.Ref == "" {
		msg := "no reference provided in webhook payload, skipping event"
		jobLog.Warn(msg)
		JSONError(w, msg, msg, metadata.JobID, http.StatusBadRequest)

		if runTracker != nil {
			runTracker.MarkSkipped(metadata.JobID, msg)
		}

		return
	}

	sourceType := config.SourceTypeGit

	var sourceRef string

	cloneURLOverrideApplied := false

	if payload.Source == webhook.PayloadSourceOCI {
		sourceType = config.SourceTypeOCI
		sourceRef = payload.Artifact
	} else {
		sourceRef, cloneURLOverrideApplied = resolveWebhookGitCloneURL(payload, appConfig)
		if cloneURLOverrideApplied {
			jobLog.Debug("using configured webhook clone URL override", slog.String("clone_url", sourceRef))
		}
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
			slog.String("commit", payload.RevisionString()), slog.String("ref", payload.Ref),
			slog.String("event", string(stages.JobTriggerWebhook))))

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

	// Only attempt SSH clone when URL-specific credentials include an SSH private key.
	// If a clone URL override is configured, do not switch back to the payload SSH URL.
	resolvedSSH := git.ResolveAuthConfig(payload.SSHUrl, appConfig.SSHPrivateKey, appConfig.SSHPrivateKeyPassphrase, appConfig.GitAccessToken)
	if sourceType == config.SourceTypeGit && shouldUsePayloadSSHURL(cloneURLOverrideApplied, payload.SSHUrl, resolvedSSH) {
		sshAuth, authErr := git.GetAuthMethod(payload.SSHUrl, appConfig.SSHPrivateKey, appConfig.SSHPrivateKeyPassphrase, appConfig.GitAccessToken)
		if authErr != nil {
			onError(w, jobLog.With(logger.ErrAttr(authErr)), "failed to resolve SSH auth method", authErr.Error(), http.StatusInternalServerError, metadata, authErr)

			if runTracker != nil {
				runTracker.MarkFailed(metadata.JobID, "failed to resolve SSH auth method: "+authErr.Error())
			}

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
			onError(w, jobLog.With(logger.ErrAttr(hr.err)), hr.msg, hr.err.Error(), hr.httpStatusCode, metadata, hr.err)

			if runTracker != nil {
				runTracker.MarkFailed(metadata.JobID, hr.Error())
			}
		} else {
			onError(w, jobLog.With(logger.ErrAttr(deployErr)), "deployment failed", deployErr.Error(), http.StatusInternalServerError, metadata, deployErr)

			if runTracker != nil {
				runTracker.MarkFailed(metadata.JobID, deployErr.Error())
			}
		}

		return
	}

	msg := "job completed successfully"
	elapsedTime := time.Since(startTime)
	jobLog.Info(msg, slog.String("elapsed_time", elapsedTime.Truncate(time.Millisecond).String()))
	JSONResponse(w, msg, metadata.JobID, http.StatusCreated)

	if runTracker != nil {
		runTracker.MarkSucceeded(metadata.JobID, msg)
	}

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
		Target:     strings.TrimSpace(customTarget),
		Revision:   "",
	}
	if h.runTracker != nil {
		h.runTracker.TrackAccepted(jobID, deploymentRunTriggerWebhook)
		h.runTracker.SetMetadata(jobID, metadata.Repository, customTarget, metadata.Revision)

		if wait {
			h.runTracker.MarkRunning(jobID)
		}
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

			metadata.Revision = notification.GetRevision(payload.Ref, payload.RevisionString())
			if h.runTracker != nil {
				h.runTracker.SetMetadata(jobID, metadata.Repository, customTarget, metadata.Revision)
			}
		}

		onError(w, jobLog.With(slog.String("ip", h.requestIP(r)), logger.ErrAttr(err)), errMsg, err.Error(), statusCode, metadata, err)

		if h.runTracker != nil {
			h.runTracker.MarkFailed(jobID, errMsg+": "+err.Error())
		}

		return
	}

	if deletionEvent, eErr := webhook.IsBranchOrTagDeletionEvent(r, payload, provider); eErr == nil && deletionEvent {
		errMsg := "branch or tag deletion event received, skipping webhook event"
		jobLog.Info(errMsg)
		JSONResponse(w, errMsg, jobID, http.StatusAccepted)

		if h.runTracker != nil {
			h.runTracker.SetMetadata(jobID, repositoryNameFromWebhookPayload(payload), customTarget, notification.GetRevision(payload.Ref, payload.RevisionString()))
			h.runTracker.MarkSkipped(jobID, errMsg)
		}

		return
	} else if eErr != nil {
		errMsg := "failed to check if event is branch or tag deletion"
		jobLog.Error(errMsg, logger.ErrAttr(eErr))
		JSONError(w, errMsg, eErr.Error(), jobID, http.StatusInternalServerError)

		if h.runTracker != nil {
			h.runTracker.MarkFailed(jobID, errMsg+": "+eErr.Error())
		}

		return
	}

	if metadata.Repository == "" || metadata.Repository == "unknown" {
		metadata.Repository = repositoryNameFromWebhookPayload(payload)
		metadata.Revision = notification.GetRevision(payload.Ref, payload.RevisionString())
	}

	if h.runTracker != nil {
		h.runTracker.SetMetadata(jobID, metadata.Repository, customTarget, metadata.Revision)
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

				if h.runTracker != nil {
					h.runTracker.MarkFailed(jobID, "webhook deployment panicked")
				}
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

		HandleEvent(ctx, jobLog, w, h.appConfig, h.dataMountPoint, payload, customTarget, metadata, h.dockerCli, h.secretProvider, h.testName, h.runTracker)
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
