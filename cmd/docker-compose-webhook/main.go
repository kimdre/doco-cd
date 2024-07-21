package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/kimdre/docker-compose-webhook/internal/compose"

	"github.com/go-playground/webhooks/v6/gitea"
	"github.com/go-playground/webhooks/v6/gitlab"

	"github.com/kimdre/docker-compose-webhook/internal/config"
	"github.com/kimdre/docker-compose-webhook/internal/git"
	"github.com/kimdre/docker-compose-webhook/internal/logger"

	"github.com/go-playground/webhooks/v6/github"
)

const (
	path = "/webhooks"
)

func main() {
	// Set default log level to debug
	log := logger.New(slog.LevelDebug)

	// Get the application configuration
	c, err := config.GetAppConfig()
	if err != nil {
		log.Error("Failed to get application configuration", log.ErrAttr(err))
		os.Exit(1)
	}

	// Parse the log level from the app configuration
	logLevel, err := logger.ParseLevel(c.LogLevel)
	if err != nil {
		logLevel = slog.LevelInfo
	}

	// Set the actual log level
	log = logger.New(logLevel)

	log.Info("starting application", slog.String("log_level", c.LogLevel))

	err = compose.VerifySocketConnection()
	if err != nil {
		log.Error(compose.ErrDockerSocketConnectionFailed.Error(), log.ErrAttr(err))
		os.Exit(1)
	}

	log.Debug("connection to docker socket was successful")

	hook, _ := github.New(github.Options.Secret(c.WebhookSecret))

	http.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		payload, err := hook.Parse(r, github.PushEvent) // , github.PullRequestEvent)
		if err != nil {
			if errors.Is(err, github.ErrEventNotFound) {
				// ok event wasn't one of the ones asked to be parsed
				log.Error("Event not found")
			}
		}

		switch event := payload.(type) {
		case github.PushPayload:
			log.Debug(
				"Push event received",
				slog.String("repository", event.Repository.FullName),
				slog.String("reference", event.Ref))

			// Clone the repository
			log.Info(
				"Cloning repository",
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

			log.Debug(
				"Using clone URL",
				slog.String("url", cloneUrl),
				slog.String("repository", event.Repository.FullName),
				slog.String("reference", event.Ref))

			repo, err := git.CloneRepository(cloneUrl, event.Ref)
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

			// Get the filesystem from the worktree
			fs := worktree.Filesystem

			// Get the deployment config from the repository
			deployConfig, err := config.GetDeployConfig(fs, event)
			if deployConfig == nil && err != nil {
				log.Error(
					"Failed to get deploy config",
					log.ErrAttr(err),
					slog.String("repository", event.Repository.FullName))

				return
			}

			log.Debug(
				"Deployment config retrieved",
				slog.Any("config", deployConfig),
				slog.String("repository", event.Repository.FullName))

			log.Debug("Deploying", slog.String("repository", event.Repository.FullName))
			// TODO docker-compose deployment logic here
			//err = compose.LoadComposeFile("test", "docker-compose.yml")
			//if err != nil {
			//	log.Error(
			//		"Failed to load compose file",
			//		logger.ErrAttr(err),
			//		slog.String("repository", event.Repository.FullName))
			//
			//	return
			//}

			log.Debug(
				"Cleaning up",
				slog.String("repository", event.Repository.FullName))

			repo = nil

		case gitlab.PushEventPayload:
			// TODO: Implement GitLab webhook handling
			log.Error("GitLab webhook event not yet implemented")

		case gitea.PushPayload:
			// TODO: Implement Gitea webhook handling
			log.Error("Gitea webhook event not yet implemented")

		default:
			log.Warn("Event not supported", slog.Any("event", event))
		}
	})

	log.Info(
		"Listening for webhooks",
		slog.Int("http_port", int(c.HttpPort)),
		slog.String("path", path),
	)

	err = http.ListenAndServe(fmt.Sprintf(":%d", c.HttpPort), nil)
	if err != nil {
		return
	}
}
