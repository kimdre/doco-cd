package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path"
	"reflect"

	"github.com/docker/cli/cli/command"

	"github.com/google/uuid"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/docker/docker/client"
	"github.com/kimdre/docker-compose-webhook/internal/utils"

	"github.com/kimdre/docker-compose-webhook/internal/docker"

	"github.com/go-playground/webhooks/v6/gitea"
	"github.com/go-playground/webhooks/v6/gitlab"

	"github.com/kimdre/docker-compose-webhook/internal/config"
	"github.com/kimdre/docker-compose-webhook/internal/git"
	"github.com/kimdre/docker-compose-webhook/internal/logger"

	"github.com/go-playground/webhooks/v6/github"
)

const (
	webhookPath = "/v1/webhook"
)

var errMsg string

func handleEvent(ctx context.Context, jobLog *slog.Logger, w http.ResponseWriter, c *config.AppConfig, repoName, repoFullName, cloneUrl, ref, jobID string, private bool, dockerCli command.Cli) {
	jobLog = jobLog.With(slog.String("repository", repoFullName))

	jobLog.Info("preparing project deployment")

	// Clone the repository
	jobLog.Debug(
		"cloning repository to temporary directory",
		slog.String("url", cloneUrl))

	// var auth transport.AuthMethod = nil

	if private {
		errMsg = "missing access token for private repository"
		if c.GitAccessToken == "" {
			jobLog.Error(errMsg)
			utils.JSONError(w,
				errMsg,
				"",
				jobID,
				http.StatusInternalServerError)

			return
		}

		// Basic auth examples:
		// https://YOUR-USERNAME:GENERATED-TOKEN@github.com/YOUR-USERNAME/YOUR-REPOSITORY
		// Or
		// https://GENERATED-TOKEN@github.com/YOUR-USERNAME/YOUR-REPOSITORY
		//auth = &gitHttp.BasicAuth{
		//	Username: "",
		//	Password: c.GitAccessToken,
		//}

		cloneUrl = git.GetAuthUrl(cloneUrl, c.GitAccessToken)
	}

	repo, err := git.CloneRepository(repoFullName, cloneUrl, ref, c.SkipTLSVerification)
	if err != nil {
		errMsg = "failed to clone repository"
		jobLog.Error(errMsg, logger.ErrAttr(err))
		utils.JSONError(w,
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
		utils.JSONError(w,
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
			utils.JSONError(w,
				errMsg,
				err.Error(),
				jobID,
				http.StatusInternalServerError)
		}
	}(fs.Root())

	jobLog.Debug("retrieving deployment configuration")

	// Get the deployment config from the repository
	deployConfig, err := config.GetDeployConfig(fs.Root(), repoName)
	if err != nil {
		errMsg = "failed to get deploy configuration"
		jobLog.Error(errMsg, logger.ErrAttr(err))
		utils.JSONError(w,
			errMsg,
			err.Error(),
			jobID,
			http.StatusInternalServerError)

		return
	}

	jobLog = jobLog.With(slog.String("reference", deployConfig.Reference))

	jobLog.Debug("deployment configuration retrieved", slog.Any("config", deployConfig))

	workingDir := path.Join(fs.Root(), deployConfig.WorkingDirectory)

	err = os.Chdir(workingDir)
	if err != nil {
		errMsg = "failed to change working directory"
		jobLog.Error(errMsg, logger.ErrAttr(err), slog.String("path", workingDir))
		utils.JSONError(w,
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
			utils.JSONError(w,
				errMsg,
				err.Error(),
				jobID,
				http.StatusInternalServerError)

			return
		}

		deployConfig.ComposeFiles = tmpComposeFiles
	}

	project, err := docker.LoadCompose(ctx, workingDir, repoName, deployConfig.ComposeFiles)
	if err != nil {
		errMsg = "failed to load project"
		jobLog.Error(errMsg,
			logger.ErrAttr(err),
			slog.Group("compose_files", slog.Any("files", deployConfig.ComposeFiles)))
		utils.JSONError(w,
			errMsg,
			err.Error(),
			jobID,
			http.StatusInternalServerError)

		return
	}

	jobLog.Info("deploying project")

	err = docker.DeployCompose(ctx, dockerCli, project, deployConfig)
	if err != nil {
		errMsg = "failed to deploy project"
		jobLog.Error(errMsg,
			logger.ErrAttr(err),
			slog.Group("compose_files", slog.Any("files", deployConfig.ComposeFiles)))
		utils.JSONError(w,
			errMsg,
			err.Error(),
			jobID,
			http.StatusInternalServerError)

		return
	}

	msg := "project deployment successful"
	jobLog.Info(msg)
	utils.JSONResponse(w, msg, jobID, http.StatusCreated)
}

func main() {
	// Set default log level to debug
	log := logger.New(slog.LevelDebug)

	// Get the application configuration
	c, err := config.GetAppConfig()
	if err != nil {
		log.Critical("failed to get application configuration", logger.ErrAttr(err))
	}

	// Parse the log level from the app configuration
	logLevel, err := logger.ParseLevel(c.LogLevel)
	if err != nil {
		logLevel = slog.LevelInfo
	}

	// Set the actual log level
	log = logger.New(logLevel)

	log.Info("starting application", slog.String("log_level", c.LogLevel))

	// Test/verify the connection to the docker socket
	err = docker.VerifySocketConnection()
	if err != nil {
		log.Critical(docker.ErrDockerSocketConnectionFailed.Error(), logger.ErrAttr(err))
	}

	log.Debug("connection to docker socket was successful")

	dockerCli, err := docker.CreateDockerCli()
	if err != nil {
		log.Critical("failed to create docker client", logger.ErrAttr(err))
		return
	}
	defer func(client client.APIClient) {
		log.Debug("closing docker client")

		err = client.Close()
		if err != nil {
			log.Error("failed to close docker client", logger.ErrAttr(err))
		}
	}(dockerCli.Client())

	log.Debug("docker client created")

	githubHook, _ := github.New(github.Options.Secret(c.WebhookSecret))
	giteaHook, _ := gitea.New(gitea.Options.Secret(c.WebhookSecret))

	http.HandleFunc(webhookPath, func(w http.ResponseWriter, r *http.Request) {
		ctx := context.Background()

		// Add job id to the context to track deployments in the logs
		jobID := uuid.Must(uuid.NewRandom()).String()
		jobLog := log.With(slog.String("job_id", jobID))

		githubPayload, githubHookErr := githubHook.Parse(r, github.PushEvent)
		giteaPayload, giteaHookErr := giteaHook.Parse(r, gitea.PushEvent)

		if githubHookErr != nil {
			err = githubHookErr
		} else if giteaHookErr != nil {
			err = giteaHookErr
		}

		if githubHookErr != nil && giteaHookErr != nil {
			switch {
			case errors.Is(err, github.ErrHMACVerificationFailed), errors.Is(err, gitea.ErrHMACVerificationFailed):
				errMsg = "incorrect webhook secret"
				jobLog.Debug(errMsg, slog.String("ip", r.RemoteAddr), logger.ErrAttr(err))
				utils.JSONError(w, errMsg, err.Error(), jobID, http.StatusUnauthorized)
			case errors.Is(err, github.ErrEventNotFound), errors.Is(err, gitea.ErrEventNotFound):
				errMsg = "event not found"
				jobLog.Debug(errMsg, slog.String("ip", r.RemoteAddr), logger.ErrAttr(err))
				utils.JSONError(w, errMsg, err.Error(), jobID, http.StatusNotFound)
			case errors.Is(err, github.ErrInvalidHTTPMethod), errors.Is(err, gitea.ErrInvalidHTTPMethod):
				errMsg = "invalid HTTP method"
				jobLog.Debug(errMsg, slog.String("ip", r.RemoteAddr), logger.ErrAttr(err))
				utils.JSONError(w, errMsg, err.Error(), jobID, http.StatusMethodNotAllowed)
			default:
				errMsg = "failed to parse webhook"
				jobLog.Debug(errMsg, slog.String("ip", r.RemoteAddr), logger.ErrAttr(err))
				utils.JSONError(w, errMsg, err.Error(), jobID, http.StatusInternalServerError)
			}

			return
		}

		var payload any
		if githubPayload != nil {
			payload = githubPayload
		} else if giteaPayload != nil {
			payload = giteaPayload
		} else {
			errMsg = "webhook payload not found"
			jobLog.Error(errMsg)
			utils.JSONError(w, errMsg, err.Error(), jobID, http.StatusInternalServerError)
		}

		switch event := payload.(type) {
		case github.PushPayload:
			handleEvent(ctx, jobLog, w, c, event.Repository.Name, event.Repository.FullName, event.Repository.CloneURL, event.Ref, jobID, event.Repository.Private, dockerCli)

		case gitea.PushPayload:
			handleEvent(ctx, jobLog, w, c, event.Repo.Name, event.Repo.FullName, event.Repo.CloneURL, event.Ref, jobID, event.Repo.Private, dockerCli)

		case gitlab.PushEventPayload:
			// TODO: Implement GitLab webhook handling
			errMsg = "gitLab webhook event not implemented"
			jobLog.Error(errMsg)
			http.Error(w, errMsg, http.StatusNotImplemented)

		default:
			errMsg = "event not supported"
			jobLog.Debug(errMsg,
				slog.Any("event", event))
			http.Error(w, errMsg, http.StatusNotImplemented)
		}
	})

	log.Info(
		"listening for events",
		slog.Int("http_port", int(c.HttpPort)),
		slog.String("path", webhookPath),
	)

	err = http.ListenAndServe(fmt.Sprintf(":%d", c.HttpPort), nil)
	if err != nil {
		return
	}
}
