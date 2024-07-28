package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path"

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

	hook, _ := github.New(github.Options.Secret(c.WebhookSecret))

	http.HandleFunc(webhookPath, func(w http.ResponseWriter, r *http.Request) {
		ctx := context.Background()

		payload, err := hook.Parse(r, github.PushEvent) // , github.PullRequestEvent)
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
				if c.GitAccessToken == "" {
					log.Error(
						"Missing access token for private repository",
						slog.String("repository", event.Repository.FullName))

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
				log.Error(
					"Failed to clone repository",
					log.ErrAttr(err),
					slog.String("repository", event.Repository.FullName))

				return
			}

			// Get the worktree from the repository
			worktree, err := repo.Worktree()
			if err != nil {
				log.Error(
					"Failed to get worktree",
					log.ErrAttr(err),
					slog.String("repository", event.Repository.FullName))

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
					log.Error(
						"Failed to remove temporary directory",
						log.ErrAttr(err),
						slog.String("repository", event.Repository.FullName))
				}
			}(fs.Root())

			log.Debug(
				"retrieving deployment configuration",
				slog.String("repository", event.Repository.FullName),
				slog.String("reference", event.Ref))

			// Get the deployment config from the repository
			deployConfig, err := config.GetDeployConfig(fs.Root(), event)
			if err != nil {
				log.Error(
					"failed to get deploy configuration",
					log.ErrAttr(err),
					slog.String("repository", event.Repository.FullName))

				return
			}

			log.Debug(
				"deployment configuration retrieved",
				slog.Any("config", deployConfig),
				slog.String("repository", event.Repository.FullName))

			workingDir := path.Join(fs.Root(), deployConfig.WorkingDirectory)

			err = os.Chdir(workingDir)
			if err != nil {
				log.Error(
					"Failed to change working directory",
					log.ErrAttr(err),
					slog.String("repository", event.Repository.FullName),
					slog.String("path", workingDir))

				return
			}

			project, err := docker.LoadCompose(ctx, workingDir, event.Repository.Name, deployConfig.ComposeFiles)
			if err != nil {
				log.Error(
					"Failed to load project",
					log.ErrAttr(err),
					slog.String("repository", event.Repository.FullName),
					slog.Group("compose_files", slog.Any("files", deployConfig.ComposeFiles)))

				return
			}

			log.Debug("deploying project", slog.String("repository", event.Repository.FullName))
			// TODO docker-compose deployment logic here
			err = docker.DeployCompose(ctx, project)
			if err != nil {
				log.Error(
					"project deployment failed",
					log.ErrAttr(err),
					slog.String("repository", event.Repository.FullName),
					slog.Group("compose_files", slog.Any("files", deployConfig.ComposeFiles)))

				return
			}

			log.Info(
				"project deployment successful",
				slog.String("repository", event.Repository.FullName),
				slog.Group("compose_files", slog.Any("files", deployConfig.ComposeFiles)))

		case gitlab.PushEventPayload:
			// TODO: Implement GitLab webhook handling
			log.Error("gitLab webhook event not implemented")
			http.Error(w, "Gitlab webhook not implemented", http.StatusNotImplemented)

		case gitea.PushPayload:
			// TODO: Implement Gitea webhook handling
			log.Error("gitea webhook event not implemented")
			http.Error(w, "Gitea webhook not implemented", http.StatusNotImplemented)

		default:
			log.Debug("event not supported", slog.Any("event", event))
			http.Error(w, "Event not supported", http.StatusNotImplemented)
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
