package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/google/uuid"

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
) {
	var err error

	startTime := time.Now()
	repoName := stages.GetRepoName(payload.CloneURL)

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
		onError(w, jobLog, "no reference provided in webhook payload, skipping event", "", http.StatusBadRequest, metadata)

		return
	}

	if appConfig.DockerSwarmFeatures {
		// Check if docker host is running in swarm mode
		swarm.ModeEnabled, err = swarm.CheckDaemonIsSwarmManager(ctx, dockerCli)
		if err != nil {
			onError(w, jobLog.With(logger.ErrAttr(err)), "failed to check if docker host is running in swarm mode", err.Error(), http.StatusInternalServerError, metadata)

			return
		}
	}

	cloneUrl := payload.CloneURL
	if appConfig.SSHPrivateKey != "" {
		cloneUrl = payload.SSHUrl
	}

	// Clone the repository
	jobLog.Debug(
		"get repository",
		slog.String("url", cloneUrl))

	// Determine the authenticated clone URL if needed
	auth := transport.AuthMethod(nil)
	if git.IsSSH(cloneUrl) {
		// SSH authentication
		auth, err = git.SSHAuth(appConfig.SSHPrivateKey, appConfig.SSHPrivateKeyPassphrase)
		if err != nil {
			onError(w, jobLog.With(logger.ErrAttr(err)), "failed to set up SSH authentication", err.Error(), http.StatusInternalServerError, metadata)

			return
		}
	} else {
		// HTTPS authentication
		if appConfig.GitAccessToken != "" {
			auth = git.HttpTokenAuth(appConfig.GitAccessToken)
		} else if payload.Private {
			onError(w, jobLog, "missing access token for private repository", "", http.StatusInternalServerError, metadata)

			return
		}
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
	configDir := filepath.Join(internalRepoPath, appConfig.DeployConfigBaseDir)

	deployConfigs, err := config.GetDeployConfigs(configDir, payload.Name, customTarget, payload.Ref)
	if err != nil {
		onError(w, jobLog.With(logger.ErrAttr(err)), "failed to get deploy configuration", err.Error(), http.StatusInternalServerError, metadata)

		return
	}

	err = cleanupObsoleteAutoDiscoveredContainers(ctx, jobLog, dockerClient, dockerCli, cloneUrl, deployConfigs, metadata)
	if err != nil {
		onError(w, jobLog.With(logger.ErrAttr(err)), "failed to clean up obsolete auto-discovered containers", err.Error(), http.StatusInternalServerError, metadata)
	}

	for _, deployConfig := range deployConfigs {
		deployLog := jobLog.WithGroup("deploy")

		failNotifyFunc := func(err error, metadata notification.Metadata) {
			onError(w, deployLog.With(logger.ErrAttr(err)), "deployment failed", err.Error(), http.StatusInternalServerError, metadata)
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
			deployConfig,
			secretProvider,
		)

		err = stageMgr.RunStages(ctx)
		if err != nil {
			return
		}
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

	metadata := notification.Metadata{
		JobID:      jobID,
		Repository: "",
		Stack:      "",
		Revision:   "",
	}

	repoName := "unknown"

	// Limit the request body size
	r.Body = http.MaxBytesReader(w, r.Body, h.appConfig.MaxPayloadSize)

	payload, err := webhook.Parse(r, h.appConfig.WebhookSecret)
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
			repoName = stages.GetRepoName(payload.CloneURL)
			metadata.Repository = repoName
			metadata.Revision = notification.GetRevision(payload.Ref, payload.CommitSHA)
		}

		onError(w, jobLog.With(slog.String("ip", r.RemoteAddr), logger.ErrAttr(err)), errMsg, err.Error(), statusCode, metadata)

		return
	}

	if metadata.Repository == "" {
		repoName = stages.GetRepoName(payload.CloneURL)
		metadata.Repository = repoName
		metadata.Revision = notification.GetRevision(payload.Ref, payload.CommitSHA)
	}

	lock := GetRepoLock(repoName)
	locked := lock.TryLock(jobID)

	if !locked {
		errMsg = "another job is still in progress for this repository"
		h.log.Warn(errMsg,
			slog.String("repository", repoName),
			slog.String("locked_by_job", lock.Holder()),
		)
		JSONError(w,
			errMsg,
			fmt.Sprintf("repsoitory '%s' is currently locked by job '%s'", repoName, lock.Holder()),
			jobID,
			http.StatusTooManyRequests)

		return
	}

	defer lock.Unlock()

	HandleEvent(ctx, jobLog, w, h.appConfig, h.dataMountPoint, payload, customTarget, jobID, h.dockerCli, h.dockerClient, h.secretProvider)
}
