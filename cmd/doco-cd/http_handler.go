package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/compose/v2/pkg/api"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"

	"github.com/docker/cli/cli/command"
	"github.com/google/uuid"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/webhook"
)

type handlerData struct {
	appConfig      *config.AppConfig    // Application configuration
	appVersion     string               // Application version
	dataMountPoint container.MountPoint // Mount point for the data directory
	dockerCli      command.Cli          // Docker CLI client
	dockerClient   *client.Client       // Docker client
	log            *logger.Logger       // Logger for logging messages
}

// HandleEvent handles the incoming webhook event
func HandleEvent(ctx context.Context, jobLog *slog.Logger, w http.ResponseWriter, appConfig *config.AppConfig, dataMountPoint container.MountPoint, payload webhook.ParsedPayload, customTarget, jobID string, dockerCli command.Cli, dockerClient *client.Client) {
	jobLog = jobLog.With(slog.String("repository", payload.FullName), slog.Group("trigger", slog.String("commit", payload.CommitSHA), slog.String("ref", payload.Ref)))

	if customTarget != "" {
		jobLog = jobLog.With(slog.String("custom_target", customTarget))
	}

	jobLog.Info("received new job")

	// Clone the repository
	jobLog.Debug(
		"get repository",
		slog.String("url", payload.CloneURL))

	// TODO: Check edge case: public repo - empty access token
	if payload.Private {
		jobLog.Debug("authenticating to private repository")

		if appConfig.GitAccessToken == "" {
			errMsg = "missing access token for private repository"
			jobLog.Error(errMsg)
			JSONError(w,
				errMsg,
				"",
				jobID,
				http.StatusInternalServerError)

			return
		}

		payload.CloneURL = git.GetAuthUrl(payload.CloneURL, appConfig.AuthType, appConfig.GitAccessToken)
	} else if appConfig.GitAccessToken != "" {
		// Always use the access token for public repositories if it is set to avoid rate limiting
		payload.CloneURL = git.GetAuthUrl(payload.CloneURL, appConfig.AuthType, appConfig.GitAccessToken)
	}

	// Validate payload.FullName to prevent directory traversal
	if strings.Contains(payload.FullName, "..") {
		errMsg = "invalid repository name"
		jobLog.Error(errMsg, slog.String("repository", payload.FullName))
		JSONError(w, errMsg, "", jobID, http.StatusBadRequest)

		return
	}

	internalRepoPath := filepath.Join(dataMountPoint.Destination, payload.FullName) // Path inside the container
	externalRepoPath := filepath.Join(dataMountPoint.Source, payload.FullName)      // Path on the host

	// Try to clone the repository
	_, err := git.CloneRepository(internalRepoPath, payload.CloneURL, payload.Ref, appConfig.SkipTLSVerification)
	if err != nil {
		// If the repository already exists, check it out to the specified commit SHA
		if errors.Is(err, git.ErrRepositoryAlreadyExists) {
			jobLog.Debug("repository already exists, checking out commit "+payload.CommitSHA, slog.String("host_path", externalRepoPath))

			_, err = git.CheckoutRepository(internalRepoPath, payload.Ref, payload.CommitSHA, appConfig.SkipTLSVerification)
			if err != nil {
				errMsg = "failed to checkout repository"
				jobLog.Error(errMsg, logger.ErrAttr(err))
				JSONError(w,
					errMsg,
					err.Error(),
					jobID,
					http.StatusInternalServerError)

				return
			}
		} else {
			errMsg = "failed to clone repository"
			jobLog.Error(errMsg, logger.ErrAttr(err))
			JSONError(w,
				errMsg,
				err.Error(),
				jobID,
				http.StatusInternalServerError)

			return
		}
	} else {
		jobLog.Debug("repository cloned", slog.String("path", externalRepoPath))
	}

	jobLog.Debug("retrieving deployment configuration")

	// Get the deployment configs from the repository
	deployConfigs, err := config.GetDeployConfigs(internalRepoPath, payload.Name, customTarget)
	if err != nil {
		if errors.Is(err, config.ErrDeprecatedConfig) {
			jobLog.Warn(err.Error())
		} else {
			errMsg = "failed to get deploy configuration"
			jobLog.Error(errMsg, logger.ErrAttr(err))
			JSONError(w,
				errMsg,
				err.Error(),
				jobID,
				http.StatusInternalServerError)

			return
		}
	}

	for _, deployConfig := range deployConfigs {
		jobLog = jobLog.With("stack", deployConfig.Name, slog.String("reference", deployConfig.Reference))

		jobLog.Debug("deployment configuration retrieved", slog.Any("config", deployConfig))

		if deployConfig.Reference != "" && deployConfig.Reference != payload.Ref {
			jobLog.Debug("checking out reference "+deployConfig.Reference, slog.String("host_path", externalRepoPath))

			_, err = git.CheckoutRepository(internalRepoPath, deployConfig.Reference, "", appConfig.SkipTLSVerification)
			if err != nil {
				errMsg = "failed to checkout repository"
				jobLog.Error(errMsg, logger.ErrAttr(err))
				JSONError(w,
					errMsg,
					err.Error(),
					jobID,
					http.StatusInternalServerError)

				return
			}
		}

		if deployConfig.Destroy {
			jobLog.Debug("destroying stack")

			// Check if doco-cd manages the project before destroying the stack
			containers, err := docker.GetLabeledContainers(ctx, dockerClient, api.ProjectLabel, deployConfig.Name)
			if err != nil {
				errMsg = "failed to retrieve containers"
				jobLog.Error(errMsg, logger.ErrAttr(err))
				JSONError(w,
					errMsg,
					err.Error(),
					jobID,
					http.StatusInternalServerError)

				return
			}

			// If no containers are found, skip the destruction step
			if len(containers) == 0 {
				jobLog.Debug("no containers found for stack, skipping...")
				continue
			}

			// Check if doco-cd manages the stack
			managed := false
			correctRepo := false

			for _, cont := range containers {
				if cont.Labels[docker.DocoCDLabels.Metadata.Manager] == "doco-cd" {
					managed = true

					if cont.Labels[docker.DocoCDLabels.Repository.Name] == payload.FullName {
						correctRepo = true
					}

					break
				}
			}

			if !managed {
				errMsg = "stack " + deployConfig.Name + " is not managed by doco-cd, aborting destruction"
				jobLog.Error(errMsg)
				JSONError(w,
					errMsg,
					map[string]string{
						"stack": deployConfig.Name,
					},
					jobID,
					http.StatusInternalServerError)

				return
			}

			if !correctRepo {
				errMsg = "stack " + deployConfig.Name + " is not managed by this repository, aborting destruction"
				jobLog.Error(errMsg)
				JSONError(w,
					errMsg,
					map[string]string{
						"stack": deployConfig.Name,
					},
					jobID,
					http.StatusInternalServerError)

				return
			}

			err = docker.DestroyStack(jobLog, &ctx, &dockerCli, deployConfig)
			if err != nil {
				errMsg = "failed to destroy stack"
				jobLog.Error(errMsg, logger.ErrAttr(err))
				JSONError(w,
					errMsg,
					err.Error(),
					jobID,
					http.StatusInternalServerError)

				return
			}

			if deployConfig.DestroyOpts.RemoveRepoDir {
				// Remove the repository directory after destroying the stack
				jobLog.Debug("removing deployment directory", slog.String("path", externalRepoPath))
				// Check if the parent directory has multiple subdirectories/repos
				parentDir := filepath.Dir(internalRepoPath)

				subDirs, err := os.ReadDir(parentDir)
				if err != nil {
					jobLog.Error("failed to read parent directory", logger.ErrAttr(err))
					JSONError(w, "failed to read parent directory", err.Error(), jobID, http.StatusInternalServerError)

					return
				}

				if len(subDirs) > 1 {
					// Do not remove the parent directory if it has multiple subdirectories
					jobLog.Debug("remove deployment directory but keep parent directory as it has multiple subdirectories", slog.String("path", internalRepoPath))

					// Remove only the repository directory
					err = os.RemoveAll(internalRepoPath)
					if err != nil {
						jobLog.Error("failed to remove deployment directory", logger.ErrAttr(err))
						JSONError(w, "failed to remove deployment directory", err.Error(), jobID, http.StatusInternalServerError)

						return
					}
				} else {
					// Remove the parent directory if it has only one subdirectory
					err = os.RemoveAll(parentDir)
					if err != nil {
						jobLog.Error("failed to remove deployment directory", logger.ErrAttr(err))
						JSONError(w, "failed to remove deployment directory", err.Error(), jobID, http.StatusInternalServerError)

						return
					}

					jobLog.Debug("removed directory", slog.String("path", parentDir))
				}
			}
		} else {
			// Skip deployment if another project with the same name already exists
			containers, err := docker.GetLabeledContainers(ctx, dockerClient, api.ProjectLabel, deployConfig.Name)
			if err != nil {
				errMsg = "failed to retrieve containers"
				jobLog.Error(errMsg, logger.ErrAttr(err))
				JSONError(w,
					errMsg,
					err.Error(),
					jobID,
					http.StatusInternalServerError)

				return
			}

			// Check if containers do not belong to this repository or if doco-cd does not manage the stack
			correctRepo := true

			for _, cont := range containers {
				repoName, ok := cont.Labels[docker.DocoCDLabels.Repository.Name]
				if !ok || repoName != payload.FullName {
					correctRepo = false
					break
				}
			}

			if !correctRepo {
				errMsg = "another stack with the name " + deployConfig.Name + " already exists, skipping deployment"
				jobLog.Error(errMsg)
				JSONError(w,
					errMsg,
					map[string]string{
						"stack": deployConfig.Name,
					},
					jobID,
					http.StatusInternalServerError)

				return
			}

			err = docker.DeployStack(jobLog, internalRepoPath, externalRepoPath, &ctx, &dockerCli, &payload, deployConfig, Version)
			if err != nil {
				msg := "deployment failed"
				jobLog.Error(msg)
				JSONError(w, err, msg, jobID, http.StatusInternalServerError)

				return
			}
		}
	}

	msg := "job completed successfully"
	jobLog.Info(msg)
	JSONResponse(w, msg, jobID, http.StatusCreated)
}

func (h *handlerData) WebhookHandler(w http.ResponseWriter, r *http.Request) {
	ctx := context.Background()

	customTarget := r.PathValue("customTarget")

	// Add job id to the context to track deployments in the logs
	jobID := uuid.Must(uuid.NewRandom()).String()
	jobLog := h.log.With(slog.String("job_id", jobID))

	jobLog.Debug("received webhook event")

	payload, err := webhook.Parse(r, h.appConfig.WebhookSecret)
	if err != nil {
		switch {
		case errors.Is(err, webhook.ErrHMACVerificationFailed):
			errMsg = "incorrect webhook secret"
			jobLog.Debug(errMsg, slog.String("ip", r.RemoteAddr), logger.ErrAttr(err))
			JSONError(w, errMsg, err.Error(), jobID, http.StatusUnauthorized)
		case errors.Is(err, webhook.ErrGitlabTokenVerificationFailed):
			errMsg = webhook.ErrGitlabTokenVerificationFailed.Error()
			jobLog.Debug(errMsg, slog.String("ip", r.RemoteAddr), logger.ErrAttr(err))
			JSONError(w, errMsg, err.Error(), jobID, http.StatusUnauthorized)
		case errors.Is(err, webhook.ErrMissingSecurityHeader):
			errMsg = webhook.ErrMissingSecurityHeader.Error()
			jobLog.Debug(errMsg, slog.String("ip", r.RemoteAddr), logger.ErrAttr(err))
			JSONError(w, errMsg, err.Error(), jobID, http.StatusBadRequest)
		case errors.Is(err, webhook.ErrParsingPayload):
			errMsg = webhook.ErrParsingPayload.Error()
			jobLog.Debug(errMsg, slog.String("ip", r.RemoteAddr), logger.ErrAttr(err))
			JSONError(w, errMsg, err.Error(), jobID, http.StatusInternalServerError)
		case errors.Is(err, webhook.ErrInvalidHTTPMethod):
			errMsg = webhook.ErrInvalidHTTPMethod.Error()
			jobLog.Debug(errMsg, slog.String("ip", r.RemoteAddr), logger.ErrAttr(err))
			JSONError(w, errMsg, "", jobID, http.StatusMethodNotAllowed)
		default:
			jobLog.Debug(webhook.ErrParsingPayload.Error(), slog.String("ip", r.RemoteAddr), logger.ErrAttr(err))
			JSONError(w, errMsg, err.Error(), jobID, http.StatusInternalServerError)
		}

		return
	}

	HandleEvent(ctx, jobLog, w, h.appConfig, h.dataMountPoint, payload, customTarget, jobID, h.dockerCli, h.dockerClient)
}

func (h *handlerData) HealthCheckHandler(w http.ResponseWriter, _ *http.Request) {
	err := docker.VerifySocketConnection()
	if err != nil {
		h.log.Error(docker.ErrDockerSocketConnectionFailed.Error(), logger.ErrAttr(err))
		JSONError(w, "unhealthy", err.Error(), "", http.StatusServiceUnavailable)

		return
	}

	h.log.Debug("health check successful")
	JSONResponse(w, "healthy", "", http.StatusOK)
}
