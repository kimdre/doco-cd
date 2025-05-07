package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sync"

	"github.com/docker/docker/api/types/container"

	"github.com/compose-spec/compose-go/v2/cli"
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
	dataMountPoint container.MountPoint // Mount point for the data directory
	dockerCli      command.Cli          // Docker CLI client
	log            *logger.Logger       // Logger for logging messages
}

// HandleEvent handles the incoming webhook event
func HandleEvent(ctx context.Context, jobLog *slog.Logger, w http.ResponseWriter, c *config.AppConfig, dataMountPoint container.MountPoint, p webhook.ParsedPayload, customTarget, jobID string, dockerCli command.Cli, wg *sync.WaitGroup) {
	jobLog = jobLog.With(slog.String("repository", p.FullName))

	if customTarget != "" {
		jobLog = jobLog.With(slog.String("custom_target", customTarget))
	}

	jobLog.Info("preparing stack deployment")

	// Clone the repository
	jobLog.Debug(
		"get repository",
		slog.String("url", p.CloneURL))

	// TODO: Check edge case: public repo - empty access token
	if p.Private {
		jobLog.Debug("authenticating to private repository")

		if c.GitAccessToken == "" {
			errMsg = "missing access token for private repository"
			jobLog.Error(errMsg)
			JSONError(w,
				errMsg,
				"",
				jobID,
				http.StatusInternalServerError)

			return
		}

		p.CloneURL = git.GetAuthUrl(p.CloneURL, c.AuthType, c.GitAccessToken)
	} else if c.GitAccessToken != "" {
		// Always use the access token for public repositories if it is set to avoid rate limiting
		p.CloneURL = git.GetAuthUrl(p.CloneURL, c.AuthType, c.GitAccessToken)
	}

	repoPath := filepath.Join(dataMountPoint.Destination, p.FullName)

	// Try to clone the repository
	repo, err := git.CloneRepository(repoPath, p.CloneURL, p.Ref, c.SkipTLSVerification)
	if err != nil {
		// If the repository already exists, check it out to the specified commit SHA
		if errors.Is(err, git.ErrRepositoryAlreadyExists) {
			jobLog.Debug("repository already exists, checking out commit "+p.CommitSHA, slog.String("path", repoPath))

			repo, err = git.CheckoutRepository(repoPath, p.Ref, p.CommitSHA, c.SkipTLSVerification)
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
		jobLog.Debug("repository cloned", slog.String("path", repoPath))
	}

	// Get the worktree from the repository
	worktree, err := repo.Worktree()
	if err != nil {
		errMsg = "failed to get worktree"
		jobLog.Error(errMsg, logger.ErrAttr(err))
		JSONError(w,
			errMsg,
			err.Error(),
			jobID,
			http.StatusInternalServerError)

		return
	}

	fs := worktree.Filesystem
	repoDir := fs.Root()

	jobLog.Debug("retrieving deployment configuration")

	// Get the deployment configs from the repository
	deployConfigs, err := config.GetDeployConfigs(repoDir, p.Name, customTarget)
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
		err = deployStack(jobLog, repoDir, &ctx, &dockerCli, &p, deployConfig)
		if err != nil {
			msg := "deployment failed"
			jobLog.Error(msg)
			JSONError(w, err, msg, jobID, http.StatusInternalServerError)

			return
		}
		// RUN DOCKER HOOKS
		// containerID, err := docker.GetContainerID(dockerCli.Client(), deployConfig.Name)
		//
		//	if err != nil {
		//		jobLog.Error(err.Error())
		//		JSONError(w, err, "failed to get container id", jobID, http.StatusInternalServerError)
		//	}
		//
		// wg.Add(1)
		//
		//	go func() {
		//		defer wg.Done()
		//
		//		docker.OnCrash(
		//			dockerCli.Client(),
		//			containerID,
		//			func() {
		//				jobLog.Info("cleaning up", slog.String("path", repoDir))
		//				_ = os.RemoveAll(repoDir)
		//			},
		//			func(err error) { jobLog.Error("failed to clean up path: "+repoDir, logger.ErrAttr(err)) },
		//		)
		//	}()
	}

	msg := "deployment successful"
	jobLog.Info(msg)
	JSONResponse(w, msg, jobID, http.StatusCreated)
}

func (h *handlerData) WebhookHandler(w http.ResponseWriter, r *http.Request) {
	var wg sync.WaitGroup

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

	HandleEvent(ctx, jobLog, w, h.appConfig, h.dataMountPoint, payload, customTarget, jobID, h.dockerCli, &wg)
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

func deployStack(
	jobLog *slog.Logger, repoDir string, ctx *context.Context,
	dockerCli *command.Cli, p *webhook.ParsedPayload, deployConfig *config.DeployConfig,
) error {
	stackLog := jobLog.
		With(slog.String("stack", deployConfig.Name)).
		With(slog.String("reference", deployConfig.Reference))

	stackLog.Debug("deployment configuration retrieved", slog.Any("config", deployConfig))

	workingDir := path.Join(repoDir, deployConfig.WorkingDirectory)

	err := os.Chdir(workingDir)
	if err != nil {
		errMsg = "failed to change working directory"
		jobLog.Error(errMsg, logger.ErrAttr(err), slog.String("path", workingDir))

		return fmt.Errorf("%s: %w", errMsg, err)
	}

	// Check if the default compose files are used
	if reflect.DeepEqual(deployConfig.ComposeFiles, cli.DefaultFileNames) {
		var tmpComposeFiles []string

		jobLog.Debug("checking for default compose files")

		// Check if the default compose files exist
		for _, f := range deployConfig.ComposeFiles {
			if _, err = os.Stat(path.Join(workingDir, f)); errors.Is(err, os.ErrNotExist) {
				continue
			}

			tmpComposeFiles = append(tmpComposeFiles, f)
		}

		if len(tmpComposeFiles) == 0 {
			errMsg = "no compose files found"
			stackLog.Error(errMsg,
				slog.Group("compose_files", slog.Any("files", deployConfig.ComposeFiles)))

			return fmt.Errorf("%s: %w", errMsg, err)
		}

		deployConfig.ComposeFiles = tmpComposeFiles
	}

	project, err := docker.LoadCompose(*ctx, workingDir, deployConfig.Name, deployConfig.ComposeFiles)
	if err != nil {
		errMsg = "failed to load compose config"
		stackLog.Error(errMsg,
			logger.ErrAttr(err),
			slog.Group("compose_files", slog.Any("files", deployConfig.ComposeFiles)))

		return fmt.Errorf("%s: %w", errMsg, err)
	}

	stackLog.Info("deploying stack")

	err = docker.DeployCompose(*ctx, *dockerCli, project, deployConfig, *p, repoDir)
	if err != nil {
		errMsg = "failed to deploy stack"
		stackLog.Error(errMsg,
			logger.ErrAttr(err),
			slog.Group("compose_files", slog.Any("files", deployConfig.ComposeFiles)))

		return fmt.Errorf("%s: %w", errMsg, err)
	}

	return nil
}
