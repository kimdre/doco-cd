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

func main() {
	// Set default log level to debug
	log := logger.New(slog.LevelDebug)

	// Get the application configuration
	c, err := config.GetAppConfig()
	if err != nil {
		log.Critical("failed to get application configuration", log.ErrAttr(err))
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
		log.Critical(docker.ErrDockerSocketConnectionFailed.Error(), log.ErrAttr(err))
	}

	log.Debug("connection to docker socket was successful")

	dockerCli, err := docker.CreateDockerCli()
	if err != nil {
		log.Critical("failed to create docker client", log.ErrAttr(err))
		return
	}
	defer func(client client.APIClient) {
		log.Debug("closing docker client")

		err := client.Close()
		if err != nil {
			log.Error("failed to close docker client", log.ErrAttr(err))
		}
	}(dockerCli.Client())

	log.Debug("docker client created")

	githubHook, _ := github.New(github.Options.Secret(c.WebhookSecret))

	http.HandleFunc(webhookPath, func(w http.ResponseWriter, r *http.Request) {
		ctx := context.Background()

		payload, err := githubHook.Parse(r, github.PushEvent)
		if err != nil {
			switch {
			case errors.Is(err, github.ErrHMACVerificationFailed):
				log.Debug("incorrect webhook secret", slog.String("ip", r.RemoteAddr), log.ErrAttr(err))
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
			case errors.Is(err, github.ErrEventNotFound):
				log.Debug("event not found", slog.String("ip", r.RemoteAddr), log.ErrAttr(err))
				http.Error(w, "Event not found", http.StatusNotFound)
			case errors.Is(err, github.ErrInvalidHTTPMethod):
				log.Debug("invalid HTTP method", slog.String("ip", r.RemoteAddr), log.ErrAttr(err))
				http.Error(w, "Invalid HTTP method", http.StatusMethodNotAllowed)
			default:
				log.Debug("failed to parse webhook", slog.String("ip", r.RemoteAddr), log.ErrAttr(err))
				http.Error(w, "Failed to parse webhook", http.StatusInternalServerError)
			}

			return
		}

		switch event := payload.(type) {
		case github.PushPayload:
			log.Debug(
				"push event received",
				slog.String("repository", event.Repository.FullName),
				slog.String("reference", event.Ref))

			// Clone the repository
			log.Debug(
				"cloning repository to temporary directory",
				slog.String("url", event.Repository.CloneURL),
				slog.String("reference", event.Ref),
				slog.String("repository", event.Repository.FullName))

			// var auth transport.AuthMethod = nil

			cloneUrl := event.Repository.CloneURL

			if event.Repository.Private {
				errMsg = "missing access token for private repository"
				if c.GitAccessToken == "" {
					log.Error(
						errMsg,
						slog.String("repository", event.Repository.FullName))
					utils.JSONError(w,
						errMsg,
						err.Error(),
						event.Repository.FullName,
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

				cloneUrl = git.GetAuthUrl(event.Repository.CloneURL, c.GitAccessToken)
			}

			repo, err := git.CloneRepository(event.Repository.Name, cloneUrl, event.Ref)
			if err != nil {
				errMsg = "failed to clone repository"
				log.Error(
					errMsg,
					log.ErrAttr(err),
					slog.String("repository", event.Repository.FullName))
				utils.JSONError(w,
					errMsg,
					err.Error(),
					event.Repository.FullName,
					http.StatusInternalServerError)

				return
			}

			// Get the worktree from the repository
			worktree, err := repo.Worktree()
			if err != nil {
				errMsg = "failed to get worktree"
				log.Error(
					errMsg,
					log.ErrAttr(err),
					slog.String("repository", event.Repository.FullName))
				utils.JSONError(w,
					errMsg,
					err.Error(),
					event.Repository.FullName,
					http.StatusInternalServerError)

				return
			}

			fs := worktree.Filesystem

			log.Debug(
				"repository cloned",
				slog.String("repository", event.Repository.FullName),
				slog.String("reference", event.Ref),
				slog.String("path", fs.Root()))

			// Defer removal of the repository
			defer func(workDir string) {
				log.Debug(
					"cleaning up",
					slog.String("repository", event.Repository.FullName),
					slog.String("path", workDir))

				err := os.RemoveAll(workDir)
				if err != nil {
					errMsg = "failed to remove temporary directory"
					log.Error(
						errMsg,
						log.ErrAttr(err),
						slog.String("repository", event.Repository.FullName))
					utils.JSONError(w,
						errMsg,
						err.Error(),
						event.Repository.FullName,
						http.StatusInternalServerError)
				}
			}(fs.Root())

			log.Debug(
				"retrieving deployment configuration",
				slog.String("repository", event.Repository.FullName),
				slog.String("reference", event.Ref))

			// Get the deployment config from the repository
			deployConfig, err := config.GetDeployConfig(fs.Root(), event)
			if err != nil {
				errMsg = "failed to get deploy configuration"
				log.Error(
					errMsg,
					log.ErrAttr(err),
					slog.String("repository", event.Repository.FullName))
				utils.JSONError(w,
					errMsg,
					err.Error(),
					event.Repository.FullName,
					http.StatusInternalServerError)

				return
			}

			log.Debug(
				"deployment configuration retrieved",
				slog.Any("config", deployConfig),
				slog.String("repository", event.Repository.FullName))

			workingDir := path.Join(fs.Root(), deployConfig.WorkingDirectory)

			err = os.Chdir(workingDir)
			if err != nil {
				errMsg = "failed to change working directory"
				log.Error(
					errMsg,
					log.ErrAttr(err),
					slog.String("repository", event.Repository.FullName),
					slog.String("path", workingDir))
				utils.JSONError(w,
					errMsg,
					err.Error(),
					event.Repository.FullName,
					http.StatusInternalServerError)

				return
			}

			// Check if the default compose files are used
			if reflect.DeepEqual(deployConfig.ComposeFiles, cli.DefaultFileNames) {
				var tmpComposeFiles []string

				log.Debug("checking for default compose files", slog.String("repository", event.Repository.FullName))

				// Check if the default compose files exist
				for _, f := range deployConfig.ComposeFiles {
					if _, err := os.Stat(path.Join(workingDir, f)); errors.Is(err, os.ErrNotExist) {
						continue
					}

					tmpComposeFiles = append(tmpComposeFiles, f)
				}

				if len(tmpComposeFiles) == 0 {
					errMsg = "no compose files found"
					log.Error(
						errMsg,
						slog.String("repository", event.Repository.FullName),
						slog.Group("compose_files", slog.Any("files", deployConfig.ComposeFiles)))
					utils.JSONError(w,
						errMsg,
						err.Error(),
						event.Repository.FullName,
						http.StatusInternalServerError)

					return
				}

				deployConfig.ComposeFiles = tmpComposeFiles
			}

			project, err := docker.LoadCompose(ctx, workingDir, event.Repository.Name, deployConfig.ComposeFiles)
			if err != nil {
				errMsg = "failed to load project"
				log.Error(
					errMsg,
					log.ErrAttr(err),
					slog.String("repository", event.Repository.FullName),
					slog.Group("compose_files", slog.Any("files", deployConfig.ComposeFiles)))
				utils.JSONError(w,
					errMsg,
					err.Error(),
					event.Repository.FullName,
					http.StatusInternalServerError)

				return
			}

			log.Debug("deploying project", slog.String("repository", event.Repository.FullName))
			err = docker.DeployCompose(ctx, dockerCli, project, deployConfig)
			if err != nil {
				errMsg = "failed to deploy project"
				log.Error(
					errMsg,
					log.ErrAttr(err),
					slog.String("repository", event.Repository.FullName),
					slog.Group("compose_files", slog.Any("files", deployConfig.ComposeFiles)))
				utils.JSONError(w,
					errMsg,
					err.Error(),
					event.Repository.FullName,
					http.StatusInternalServerError)

				return
			}

			log.Info(
				"project deployment successful",
				slog.String("repository", event.Repository.FullName),
				slog.Group("compose_files", slog.Any("files", deployConfig.ComposeFiles)))

			// Respond with a 204 No Content status
			w.WriteHeader(http.StatusNoContent)

		case gitlab.PushEventPayload:
			// TODO: Implement GitLab webhook handling
			errMsg = "gitLab webhook event not implemented"
			log.Error(errMsg)
			http.Error(w, errMsg, http.StatusNotImplemented)

		case gitea.PushPayload:
			// TODO: Implement Gitea webhook handling
			errMsg = "gitea webhook event not implemented"
			log.Error(errMsg)
			http.Error(w, errMsg, http.StatusNotImplemented)

		default:
			errMsg = "event not supported"
			log.Debug(errMsg, slog.Any("event", event))
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
