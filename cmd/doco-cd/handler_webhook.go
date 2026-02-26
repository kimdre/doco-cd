package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/google/uuid"

	"github.com/kimdre/doco-cd/internal/test"

	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/stages"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/prometheus"
	"github.com/kimdre/doco-cd/internal/webhook"
)

var ErrInvalidHTTPMethod = errors.New("invalid http method")

type handlerData struct {
	appConfig      *config.AppConfig    // Application configuration
	appVersion     string               // Application version
	dataMountPoint container.MountPoint // Mount point for the data directory
	dockerCli      command.Cli          // Docker CLI client
	dockerClient   *client.Client       // Docker client
	log            *logger.Logger       // Logger for logging messages
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
		err := notification.Send(notification.Failure, "Deployment Failed", errMsg, metadata)
		if err != nil {
			log.Error("failed to send notification", logger.ErrAttr(err))
		}
	}()
}

// HandleEvent executes the deployment process for a given webhook event.
func HandleEvent(ctx context.Context, jobLog *slog.Logger, w http.ResponseWriter, appConfig *config.AppConfig,
	dataMountPoint container.MountPoint, payload webhook.ParsedPayload, customTarget, jobID string,
	dockerCli command.Cli, dockerClient *client.Client, secretProvider *secretprovider.SecretProvider,
	testName string,
) {
	var err error

	startTime := time.Now()
	repoName := git.GetRepoName(payload.CloneURL)

	jobLog = jobLog.With(slog.String("repository", repoName))

	if customTarget != "" {
		jobLog = jobLog.With(slog.String("custom_target", customTarget))
	}

	jobLog.Info("received new job",
		slog.Group("trigger",
			slog.String("commit", payload.CommitSHA), slog.String("ref", payload.Ref),
			slog.String("event", string(stages.JobTriggerWebhook))))

	metadata := notification.Metadata{
		JobID:      jobID,
		Repository: repoName,
		Stack:      "",
		Revision:   notification.GetRevision(payload.Ref, payload.CommitSHA),
	}

	if payload.Ref == "" {
		msg := "no reference provided in webhook payload, skipping event"
		jobLog.Warn(msg)
		JSONError(w, msg, msg, jobID, http.StatusBadRequest)

		return
	}

	if appConfig.DockerSwarmFeatures {
		// Check if docker host is running in swarm mode
		swarm.ModeEnabled, err = swarm.CheckDaemonIsSwarmManager(ctx, dockerCli)
		if err != nil {
			onError(w, jobLog.With(logger.ErrAttr(err)), "failed to check if docker host is running in swarm mode", err.Error(), http.StatusInternalServerError, metadata)

			return
		}
	} else {
		swarm.ModeEnabled = false
	}

	cloneUrl := payload.CloneURL
	if appConfig.SSHPrivateKey != "" {
		cloneUrl = payload.SSHUrl
	}

	// Clone the repository
	jobLog.Debug(
		"get repository",
		slog.String("url", cloneUrl))

	auth, err := git.GetAuthMethod(cloneUrl, appConfig.SSHPrivateKey, appConfig.SSHPrivateKeyPassphrase, appConfig.GitAccessToken)
	if err != nil {
		onError(w, jobLog.With(logger.ErrAttr(err)), "failed to set up authentication", err.Error(), http.StatusInternalServerError, metadata)
		return
	}

	if auth == nil && payload.Private {
		onError(w, jobLog, "missing access token for private repository", "", http.StatusInternalServerError, metadata)
		return
	}

	// Validate payload.FullName to prevent directory traversal
	if strings.Contains(payload.FullName, "..") {
		onError(w, jobLog.With(slog.String("repository", payload.FullName)), "invalid repository name", "", http.StatusBadRequest, metadata)

		return
	}

	internalRepoPath, err := filesystem.VerifyAndSanitizePath(filepath.Join(dataMountPoint.Destination, repoName), dataMountPoint.Destination) // Path inside the container
	if err != nil {
		onError(w, jobLog.With(logger.ErrAttr(err)), "failed to verify and sanitize internal filesystem path", err.Error(), http.StatusBadRequest, metadata)

		return
	}

	externalRepoPath, err := filesystem.VerifyAndSanitizePath(filepath.Join(dataMountPoint.Source, repoName), dataMountPoint.Source) // Path on the host
	if err != nil {
		onError(w, jobLog.With(logger.ErrAttr(err)), "failed to verify and sanitize external filesystem path", err.Error(), http.StatusBadRequest, metadata)

		return
	}

	// Try to clone the repository
	_, err = git.CloneRepository(internalRepoPath, cloneUrl, payload.Ref, appConfig.SkipTLSVerification, appConfig.HttpProxy, auth, appConfig.GitCloneSubmodules)
	if err != nil {
		// If the repository already exists, check it out to the specified commit SHA
		if errors.Is(err, git.ErrRepositoryAlreadyExists) {
			jobLog.Debug("repository already exists, checking out reference "+payload.Ref, slog.String("host_path", externalRepoPath))

			_, err = git.UpdateRepository(internalRepoPath, cloneUrl, payload.Ref, appConfig.SkipTLSVerification, appConfig.HttpProxy, auth, appConfig.GitCloneSubmodules)
			if err != nil {
				onError(w, jobLog.With(logger.ErrAttr(err)), "failed to checkout repository", err.Error(), http.StatusInternalServerError, metadata)

				return
			}
		} else {
			onError(w, jobLog.With(logger.ErrAttr(err)), "failed to clone repository", err.Error(), http.StatusInternalServerError, metadata)

			return
		}
	} else {
		jobLog.Debug("repository cloned", slog.String("path", externalRepoPath))
	}

	jobLog.Debug("retrieving deployment configuration")

	// Get the deployment configs from the repository
	deployConfigs, err := config.GetDeployConfigs(internalRepoPath, appConfig.DeployConfigBaseDir, payload.Name, customTarget, payload.Ref)
	if err != nil {
		onError(w, jobLog.With(logger.ErrAttr(err)), "failed to get deploy configuration", err.Error(), http.StatusInternalServerError, metadata)

		return
	}

	err = cleanupObsoleteAutoDiscoveredContainers(ctx, jobLog, dockerClient, dockerCli, cloneUrl, deployConfigs, metadata)
	if err != nil {
		onError(w, jobLog.With(logger.ErrAttr(err)), "failed to clean up obsolete auto-discovered containers", err.Error(), http.StatusInternalServerError, metadata)
	}

	var wg sync.WaitGroup

	resultCh := make(chan error, len(deployConfigs))

	for _, deployConfig := range deployConfigs {
		deployLog := jobLog.
			WithGroup("deploy").
			With(
				slog.String("stack", deployConfig.Name),
				slog.String("reference", deployConfig.Reference))

		// Used to make test deployments unique and prevent conflicts between tests when running in parallel.
		// It is not used in production.
		if testName != "" {
			deployConfig.Name = test.ConvertTestName(testName)
		}

		wg.Add(1)

		go func(dc *config.DeployConfig) {
			defer wg.Done()

			if deployerLimiter != nil {
				deployLog.Debug("queuing deployment")

				unlock, lErr := deployerLimiter.acquire(ctx, repoName, NormalizeReference(dc.Reference))
				if lErr != nil {
					resultCh <- lErr
					return
				}
				defer unlock()
			}

			failNotifyFunc := func(err error, metadata notification.Metadata) {
				// Don't write to HTTP from goroutines — just send notification and log
				go func() {
					notifyErr := notification.Send(notification.Failure, "Deployment Failed", err.Error(), metadata)
					if notifyErr != nil {
						deployLog.Error("failed to send notification", logger.ErrAttr(notifyErr))
					}
				}()

				deployLog.Error("deployment failed", logger.ErrAttr(err))
			}

			stageMgr := stages.NewStageManager(
				metadata.JobID,
				stages.JobTriggerWebhook,
				deployLog,
				failNotifyFunc,
				&stages.RepositoryData{
					CloneURL:     config.HttpUrl(cloneUrl),
					Name:         repoName,
					PathInternal: internalRepoPath,
					PathExternal: externalRepoPath,
				},
				&stages.Docker{
					Cmd:            dockerCli,
					Client:         dockerClient,
					DataMountPoint: dataMountPoint,
				},
				&payload,
				appConfig,
				dc,
				secretProvider,
			)

			err := stageMgr.RunStages(ctx)
			resultCh <- err
		}(deployConfig)
	}

	// Wait for all deployments to complete
	wg.Wait()
	close(resultCh)

	var deployErr error

	for e := range resultCh {
		if e != nil {
			deployErr = e
			// keep looping to drain channel
		}
	}

	if deployErr != nil {
		// In synchronous mode we should return an error to the caller
		// For async mode, w is noopResponseWriter and JSONError is a no-op
		onError(w, jobLog.With(logger.ErrAttr(deployErr)), "deployment failed", deployErr.Error(), http.StatusInternalServerError, metadata)
		return
	}

	msg := "job completed successfully"
	elapsedTime := time.Since(startTime)
	jobLog.Info(msg, slog.String("elapsed_time", elapsedTime.Truncate(time.Millisecond).String()))
	JSONResponse(w, msg, jobID, http.StatusCreated)

	prometheus.WebhookRequestsTotal.WithLabelValues(repoName).Inc()
	prometheus.WebhookDuration.WithLabelValues(repoName).Observe(elapsedTime.Seconds())
}

// WebhookHandler handles incoming webhook requests.
func (h *handlerData) WebhookHandler(w http.ResponseWriter, r *http.Request) {
	ctx := context.WithoutCancel(r.Context())

	customTarget := r.PathValue("customTarget")

	// Add a job id to the context to track deployments in the logs
	jobID := uuid.Must(uuid.NewV7()).String()

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
		var statusCode int

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

		if payload.CloneURL != "" {
			metadata.Repository = git.GetRepoName(payload.CloneURL)
			metadata.Revision = notification.GetRevision(payload.Ref, payload.CommitSHA)
		}

		onError(w, jobLog.With(slog.String("ip", r.RemoteAddr), logger.ErrAttr(err)), errMsg, err.Error(), statusCode, metadata)

		return
	}

	if deletionEvent, eErr := webhook.IsBranchOrTagDeletionEvent(r, payload, provider); eErr == nil && deletionEvent {
		errMsg = "branch or tag deletion event received, skipping webhook event"
		jobLog.Info(errMsg)
		JSONResponse(w, errMsg, jobID, http.StatusAccepted)

		return
	} else if eErr != nil {
		errMsg = "failed to check if event is branch or tag deletion"
		jobLog.Error(errMsg, logger.ErrAttr(eErr))
		JSONError(w, errMsg, eErr.Error(), jobID, http.StatusInternalServerError)

		return
	}

	if metadata.Repository == "" {
		metadata.Repository = git.GetRepoName(payload.CloneURL)
		metadata.Revision = notification.GetRevision(payload.Ref, payload.CommitSHA)
	}

	if wait {
		HandleEvent(ctx, jobLog, w, h.appConfig, h.dataMountPoint, payload, customTarget, jobID, h.dockerCli, h.dockerClient, h.secretProvider, h.testName)
		return
	}

	// Async mode: respond immediately and run the deployment in the background.
	JSONResponse(w, "job accepted", jobID, http.StatusAccepted)

	go func() {
		HandleEvent(ctx, jobLog, noopResponseWriter{}, h.appConfig, h.dataMountPoint, payload, customTarget, jobID, h.dockerCli, h.dockerClient, h.secretProvider, h.testName)
	}()
}

// noopResponseWriter is used when we run HandleEvent asynchronously.
// It prevents writes to the original HTTP connection after we've already responded.
type noopResponseWriter struct{}

func (noopResponseWriter) Header() http.Header       { return http.Header{} }
func (noopResponseWriter) Write([]byte) (int, error) { return 0, nil }
func (noopResponseWriter) WriteHeader(_ int)         {}
