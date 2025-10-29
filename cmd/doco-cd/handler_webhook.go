package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"

	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/google/uuid"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
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

// getRepoName extracts the repository name from the clone URL.
func getRepoName(cloneURL string) string {
	repoName := strings.SplitAfter(cloneURL, "://")[1]

	if strings.Contains(repoName, "@") {
		repoName = strings.SplitAfter(repoName, "@")[1]
	}

	return strings.TrimSuffix(repoName, ".git")
}

// HandleEvent executes the deployment process for a given webhook event.
func HandleEvent(ctx context.Context, jobLog *slog.Logger, w http.ResponseWriter, appConfig *config.AppConfig,
	dataMountPoint container.MountPoint, payload webhook.ParsedPayload, customTarget, jobID string,
	dockerCli command.Cli, dockerClient *client.Client, secretProvider *secretprovider.SecretProvider,
) {
	var err error

	startTime := time.Now()
	repoName := getRepoName(payload.CloneURL)

	jobLog = jobLog.With(slog.String("repository", repoName))

	if customTarget != "" {
		jobLog = jobLog.With(slog.String("custom_target", customTarget))
	}

	jobLog.Info("received new job",
		slog.Group("trigger",
			slog.String("commit", payload.CommitSHA), slog.String("ref", payload.Ref),
			slog.String("event", "webhook")))

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

	// Clone the repository
	jobLog.Debug(
		"get repository",
		slog.String("url", payload.CloneURL))

	if payload.Private {
		jobLog.Debug("authenticating to private repository")

		if appConfig.GitAccessToken == "" {
			onError(w, jobLog, "missing access token for private repository", "", http.StatusInternalServerError, metadata)

			return
		}

		payload.CloneURL = git.GetAuthUrl(payload.CloneURL, appConfig.AuthType, appConfig.GitAccessToken)
	} else if appConfig.GitAccessToken != "" {
		// Always use the access token for public repositories if it is set to avoid rate limiting
		payload.CloneURL = git.GetAuthUrl(payload.CloneURL, appConfig.AuthType, appConfig.GitAccessToken)
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

	externalRepoPath, err := filesystem.VerifyAndSanitizePath(filepath.Join(dataMountPoint.Destination, repoName), dataMountPoint.Destination) // Path on the host
	if err != nil {
		onError(w, jobLog.With(logger.ErrAttr(err)), "failed to verify and sanitize external filesystem path", err.Error(), http.StatusBadRequest, metadata)

		return
	}

	// Try to clone the repository
	_, err = git.CloneRepository(internalRepoPath, payload.CloneURL, payload.Ref, appConfig.SkipTLSVerification, appConfig.HttpProxy)
	if err != nil {
		// If the repository already exists, check it out to the specified commit SHA
		if errors.Is(err, git.ErrRepositoryAlreadyExists) {
			jobLog.Debug("repository already exists, checking out reference "+payload.Ref, slog.String("host_path", externalRepoPath))

			_, err = git.UpdateRepository(internalRepoPath, payload.CloneURL, payload.Ref, appConfig.SkipTLSVerification, appConfig.HttpProxy)
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
	deployConfigs, err := config.GetDeployConfigs(internalRepoPath, payload.Name, customTarget, payload.Ref)
	if err != nil {
		onError(w, jobLog.With(logger.ErrAttr(err)), "failed to get deploy configuration", err.Error(), http.StatusInternalServerError, metadata)

		return
	}

	for _, deployConfig := range deployConfigs {
		subJobLog := jobLog.With()

		// Validate the webhook event reference against the WebhookEventFilter in the deployment config
		if deployConfig.WebhookEventFilter != "" {
			filter := regexp.MustCompile(deployConfig.WebhookEventFilter)
			if !filter.MatchString(payload.Ref) {
				subJobLog.Debug("reference does not match the webhook event filter, skipping deployment", slog.String("webhook_filter", deployConfig.WebhookEventFilter), slog.String("ref", payload.Ref))
				continue
			}

			subJobLog.Debug("reference matches the webhook event filter, proceeding with deployment", slog.String("webhook_filter", deployConfig.WebhookEventFilter), slog.String("ref", payload.Ref))
		}

		repoName = getRepoName(payload.CloneURL)
		if deployConfig.RepositoryUrl != "" {
			repoName = getRepoName(string(deployConfig.RepositoryUrl))
		}

		metadata.Repository = repoName
		metadata.Revision = notification.GetRevision(deployConfig.Reference, "")
		metadata.Stack = deployConfig.Name

		internalRepoPath, err = filesystem.VerifyAndSanitizePath(filepath.Join(dataMountPoint.Destination, repoName), dataMountPoint.Destination) // Path inside the container
		if err != nil {
			onError(w, subJobLog.With(logger.ErrAttr(err)), "invalid repository name", err.Error(), http.StatusBadRequest, metadata)

			return
		}

		externalRepoPath, err = filesystem.VerifyAndSanitizePath(filepath.Join(dataMountPoint.Source, repoName), dataMountPoint.Source) // Path on the host
		if err != nil {
			onError(w, subJobLog.With(logger.ErrAttr(err)), "invalid repository name", err.Error(), http.StatusBadRequest, metadata)

			return
		}

		subJobLog = subJobLog.With(
			slog.String("stack", deployConfig.Name),
			slog.String("reference", deployConfig.Reference),
			slog.String("repository", repoName),
		)

		subJobLog.Debug("deployment configuration retrieved", slog.Any("config", deployConfig))

		var cloneUrl string

		if deployConfig.RepositoryUrl != "" {
			cloneUrl = string(deployConfig.RepositoryUrl)
			if appConfig.GitAccessToken != "" {
				cloneUrl = git.GetAuthUrl(string(deployConfig.RepositoryUrl), appConfig.AuthType, appConfig.GitAccessToken)
			}

			subJobLog.Debug("repository URL provided, cloning remote repository")
			// Try to clone the remote repository
			_, err = git.CloneRepository(internalRepoPath, cloneUrl, deployConfig.Reference, appConfig.SkipTLSVerification, appConfig.HttpProxy)
			if err != nil && !errors.Is(err, git.ErrRepositoryAlreadyExists) {
				onError(w, subJobLog.With(logger.ErrAttr(err)), "failed to clone remote repository", err.Error(), http.StatusInternalServerError, metadata)

				return
			}

			subJobLog.Debug("remote repository cloned", slog.String("path", externalRepoPath))
		}

		subJobLog.Debug("checking out reference "+deployConfig.Reference, slog.String("host_path", externalRepoPath))

		repo, err := git.UpdateRepository(internalRepoPath, cloneUrl, deployConfig.Reference, appConfig.SkipTLSVerification, appConfig.HttpProxy)
		if err != nil {
			onError(w, subJobLog.With(logger.ErrAttr(err)), "failed to checkout repository", err.Error(), http.StatusInternalServerError, metadata)

			return
		}

		latestCommit, err := git.GetLatestCommit(repo, deployConfig.Reference)
		if err != nil {
			onError(w, subJobLog.With(logger.ErrAttr(err)), "failed to get latest commit", err.Error(), http.StatusInternalServerError, metadata)

			return
		}

		metadata.Revision = notification.GetRevision(deployConfig.Reference, latestCommit)

		filterLabel := api.ProjectLabel
		if swarm.ModeEnabled {
			filterLabel = swarm.StackNamespaceLabel
		}

		if deployConfig.Destroy {
			subJobLog.Debug("destroying stack")

			// Check if doco-cd manages the project before destroying the stack
			containers, err := docker.GetLabeledContainers(ctx, dockerClient, filterLabel, deployConfig.Name)
			if err != nil {
				onError(w, subJobLog.With(logger.ErrAttr(err)), "failed to retrieve containers", err.Error(), http.StatusInternalServerError, metadata)

				return
			}

			// If no containers are found, skip the destruction step
			if len(containers) == 0 {
				subJobLog.Debug("no containers found for stack, skipping...")

				continue
			}

			// Check if doco-cd manages the stack
			managed := false
			correctRepo := false

			for _, cont := range containers {
				if cont.Labels[docker.DocoCDLabels.Metadata.Manager] == config.AppName {
					managed = true

					if cont.Labels[docker.DocoCDLabels.Repository.Name] == payload.FullName {
						correctRepo = true
					}

					break
				}
			}

			if !managed {
				onError(w, subJobLog, fmt.Errorf("%w: %s: aborting destruction", ErrNotManagedByDocoCD, deployConfig.Name).Error(),
					"", http.StatusInternalServerError, metadata)

				return
			}

			if !correctRepo {
				onError(w, subJobLog, fmt.Errorf("%w: %s: aborting destruction", ErrDeploymentConflict, deployConfig.Name).Error(),
					map[string]string{"stack": deployConfig.Name}, http.StatusInternalServerError, metadata)

				return
			}

			err = docker.DestroyStack(subJobLog, &ctx, &dockerCli, deployConfig)
			if err != nil {
				onError(w, subJobLog.With(logger.ErrAttr(err)), "failed to destroy stack", err.Error(), http.StatusInternalServerError, metadata)

				return
			}

			if swarm.ModeEnabled && deployConfig.DestroyOpts.RemoveVolumes {
				err = docker.RemoveLabeledVolumes(ctx, dockerClient, deployConfig.Name, filterLabel)
				if err != nil {
					onError(w, subJobLog.With(logger.ErrAttr(err)), "failed to remove volumes", err.Error(), http.StatusInternalServerError, metadata)

					return
				}

				subJobLog.Debug("failed to remove volumes", slog.String("stack", deployConfig.Name))
			}

			if deployConfig.DestroyOpts.RemoveRepoDir {
				// Remove the repository directory after destroying the stack
				subJobLog.Debug("removing deployment directory", slog.String("path", externalRepoPath))
				// Check if the parent directory has multiple subdirectories/repos
				parentDir := filepath.Dir(internalRepoPath)

				subDirs, err := os.ReadDir(parentDir)
				if err != nil {
					onError(w, subJobLog.With(logger.ErrAttr(err)), "failed to read parent directory", err.Error(), http.StatusInternalServerError, metadata)

					return
				}

				if len(subDirs) > 1 {
					// Do not remove the parent directory if it has multiple subdirectories
					subJobLog.Debug("remove deployment directory but keep parent directory as it has multiple subdirectories", slog.String("path", internalRepoPath))

					// Remove only the repository directory
					err = os.RemoveAll(internalRepoPath)
					if err != nil {
						onError(w, subJobLog.With(logger.ErrAttr(err)), "failed to remove deployment directory", err.Error(), http.StatusInternalServerError, metadata)

						return
					}
				} else {
					// Remove the parent directory if it has only one subdirectory
					err = os.RemoveAll(parentDir)
					if err != nil {
						onError(w, subJobLog.With(logger.ErrAttr(err)), "failed to remove deployment directory", err.Error(), http.StatusInternalServerError, metadata)

						return
					}

					subJobLog.Debug("removed directory", slog.String("path", parentDir))
				}
			}
		} else {
			// Skip deployment if another project with the same name already exists
			containers, err := docker.GetLabeledContainers(ctx, dockerClient, filterLabel, deployConfig.Name)
			if err != nil {
				onError(w, subJobLog.With(logger.ErrAttr(err)), "failed to retrieve containers", err.Error(), http.StatusInternalServerError, metadata)

				return
			}

			// Check if containers do not belong to this repository or if doco-cd does not manage the stack
			correctRepo := true
			deployedCommit := ""
			deployedSecretHash := ""

			for _, cont := range containers {
				name, ok := cont.Labels[docker.DocoCDLabels.Repository.Name]
				if !ok || name != payload.FullName {
					correctRepo = false

					break
				}

				deployedCommit = cont.Labels[docker.DocoCDLabels.Deployment.CommitSHA]
				deployedSecretHash = cont.Labels[docker.DocoCDLabels.Deployment.ExternalSecretsHash]
			}

			if !correctRepo {
				onError(w, subJobLog, fmt.Errorf("%w: %s: skipping deployment", ErrDeploymentConflict, deployConfig.Name).Error(),
					map[string]string{"stack": deployConfig.Name}, http.StatusInternalServerError, metadata)

				return
			}

			secretsChanged := false // Flag to indicate if external secrets have changed

			resolvedSecrets := make(secrettypes.ResolvedSecrets)

			if secretProvider != nil && *secretProvider != nil && len(deployConfig.ExternalSecrets) > 0 {
				subJobLog.Debug("resolving external secrets", slog.Any("external_secrets", deployConfig.ExternalSecrets))

				// Resolve external secrets
				resolvedSecrets, err = (*secretProvider).ResolveSecretReferences(ctx, deployConfig.ExternalSecrets)
				if err != nil {
					onError(w, subJobLog.With(logger.ErrAttr(err)), "failed to resolve external secrets", err.Error(), http.StatusInternalServerError, metadata)

					return
				}

				secretHash := secretprovider.Hash(resolvedSecrets)
				if deployedSecretHash != "" && deployedSecretHash != secretHash {
					subJobLog.Debug("external secrets have changed, proceeding with deployment")

					secretsChanged = true
				}
			}

			subJobLog.Debug("comparing commits",
				slog.String("deployed_commit", deployedCommit),
				slog.String("latest_commit", latestCommit))

			var changedFiles []git.ChangedFile
			if deployedCommit != "" {
				changedFiles, err = git.GetChangedFilesBetweenCommits(repo, plumbing.NewHash(deployedCommit), plumbing.NewHash(latestCommit))
				if err != nil {
					onError(w, subJobLog.With(logger.ErrAttr(err)), "failed to get changed files between commits", err.Error(), http.StatusInternalServerError, metadata)

					return
				}

				hasChanged, err := git.HasChangesInSubdir(changedFiles, deployConfig.WorkingDirectory)
				if err != nil {
					onError(w, subJobLog, fmt.Errorf("failed to compare commits in subdirectory: %w", err).Error(),
						map[string]string{"stack": deployConfig.Name}, http.StatusInternalServerError, metadata)

					return
				}

				if !hasChanged {
					jobLog.Debug("no changes detected in subdirectory, skipping deployment",
						slog.String("directory", deployConfig.WorkingDirectory),
						slog.String("last_commit", latestCommit),
						slog.String("deployed_commit", deployedCommit))

					continue
				}

				subJobLog.Debug("changes detected in subdirectory, proceeding with deployment",
					slog.String("directory", deployConfig.WorkingDirectory),
					slog.String("last_commit", latestCommit),
					slog.String("deployed_commit", deployedCommit))
			}

			err = docker.DeployStack(subJobLog, internalRepoPath, externalRepoPath, &ctx, &dockerCli, dockerClient,
				&payload, deployConfig, changedFiles, latestCommit, Version, "webhook", false, metadata,
				resolvedSecrets, secretsChanged)
			if err != nil {
				onError(w, subJobLog.With(logger.ErrAttr(err)), "deployment failed", err.Error(), http.StatusInternalServerError, metadata)

				return
			}
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
			repoName = getRepoName(payload.CloneURL)
			metadata.Repository = repoName
			metadata.Revision = notification.GetRevision(payload.Ref, payload.CommitSHA)
		}

		onError(w, jobLog.With(slog.String("ip", r.RemoteAddr), logger.ErrAttr(err)), errMsg, err.Error(), statusCode, metadata)

		return
	}

	if metadata.Repository == "" {
		repoName = getRepoName(payload.CloneURL)
		metadata.Repository = repoName
		metadata.Revision = notification.GetRevision(payload.Ref, payload.CommitSHA)
	}

	lock := getRepoLock(repoName)
	locked := lock.TryLock()

	if !locked {
		onError(w, jobLog, "Another job is still in progress for this repository", nil, http.StatusTooManyRequests, metadata)
		return
	}

	defer lock.Unlock()

	HandleEvent(ctx, jobLog, w, h.appConfig, h.dataMountPoint, payload, customTarget, jobID, h.dockerCli, h.dockerClient, h.secretProvider)
}
