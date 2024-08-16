package main

import (
	"context"
	"errors"
	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/docker/cli/cli/command"
	"github.com/google/uuid"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/webhook"
	"log/slog"
	"net/http"
	"os"
	"path"
	"reflect"
)

type handlerData struct {
	dockerCli command.Cli
	appConfig *config.AppConfig
	log       *logger.Logger
}

// HandleEvent handles the incoming webhook event
func HandleEvent(ctx context.Context, jobLog *slog.Logger, w http.ResponseWriter, c *config.AppConfig, p webhook.ParsedPayload, jobID string, dockerCli command.Cli) {
	jobLog = jobLog.With(slog.String("repository", p.FullName))

	jobLog.Info("preparing project deployment")

	// Clone the repository
	jobLog.Debug(
		"cloning repository to temporary directory",
		slog.String("url", p.CloneURL))

	if p.Private {
		jobLog.Debug("repository is private")

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

	repo, err := git.CloneRepository(p.FullName, p.CloneURL, p.Ref, c.SkipTLSVerification)
	if err != nil {
		errMsg = "failed to clone repository"
		jobLog.Error(errMsg, logger.ErrAttr(err))
		JSONError(w,
			errMsg,
			err.Error(),
			jobID,
			http.StatusInternalServerError)

		return
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

	jobLog.Debug("repository cloned", slog.String("path", fs.Root()))

	// Defer removal of the repository
	defer func(workDir string) {
		jobLog.Debug("cleaning up", slog.String("path", workDir))

		err = os.RemoveAll(workDir)
		if err != nil {
			errMsg = "failed to remove temporary directory"
			jobLog.Error(errMsg, logger.ErrAttr(err))
			JSONError(w,
				errMsg,
				err.Error(),
				jobID,
				http.StatusInternalServerError)
		}
	}(fs.Root())

	jobLog.Debug("retrieving deployment configuration")

	// Get the deployment config from the repository
	deployConfig, err := config.GetDeployConfig(fs.Root(), p.Name)
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

	jobLog = jobLog.With(slog.String("reference", deployConfig.Reference))

	jobLog.Debug("deployment configuration retrieved", slog.Any("config", deployConfig))

	workingDir := path.Join(fs.Root(), deployConfig.WorkingDirectory)

	err = os.Chdir(workingDir)
	if err != nil {
		errMsg = "failed to change working directory"
		jobLog.Error(errMsg, logger.ErrAttr(err), slog.String("path", workingDir))
		JSONError(w,
			errMsg,
			err.Error(),
			jobID,
			http.StatusInternalServerError)

		return
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
			jobLog.Error(errMsg,
				slog.Group("compose_files", slog.Any("files", deployConfig.ComposeFiles)))
			JSONError(w,
				errMsg,
				err.Error(),
				jobID,
				http.StatusInternalServerError)

			return
		}

		deployConfig.ComposeFiles = tmpComposeFiles
	}

	project, err := docker.LoadCompose(ctx, workingDir, deployConfig.Name, deployConfig.ComposeFiles)
	if err != nil {
		errMsg = "failed to load project"
		jobLog.Error(errMsg,
			logger.ErrAttr(err),
			slog.Group("compose_files", slog.Any("files", deployConfig.ComposeFiles)))
		JSONError(w,
			errMsg,
			err.Error(),
			jobID,
			http.StatusInternalServerError)

		return
	}

	jobLog.Info("deploying project")

	err = docker.DeployCompose(ctx, dockerCli, project, deployConfig, p)
	if err != nil {
		errMsg = "failed to deploy project"
		jobLog.Error(errMsg,
			logger.ErrAttr(err),
			slog.Group("compose_files", slog.Any("files", deployConfig.ComposeFiles)))
		JSONError(w,
			errMsg,
			err.Error(),
			jobID,
			http.StatusInternalServerError)

		return
	}

	msg := "project deployment successful"
	jobLog.Info(msg)
	JSONResponse(w, msg, jobID, http.StatusCreated)
}

func (h *handlerData) WebhookHandler(w http.ResponseWriter, r *http.Request) {
	ctx := context.Background()

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

	HandleEvent(ctx, jobLog, w, h.appConfig, payload, jobID, h.dockerCli)
}

func (h *handlerData) HealthCheckHandler(w http.ResponseWriter, _ *http.Request) {
	err := docker.VerifySocketConnection(h.appConfig.DockerAPIVersion)
	if err != nil {
		h.log.Error(docker.ErrDockerSocketConnectionFailed.Error(), logger.ErrAttr(err))
		JSONError(w, "unhealthy", err.Error(), "", http.StatusServiceUnavailable)

		return
	}

	h.log.Debug("health check successful")
	JSONResponse(w, "healthy", "", http.StatusOK)
}
