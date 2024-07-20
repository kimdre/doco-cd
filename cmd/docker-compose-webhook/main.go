package main

import (
	"errors"
	"fmt"
	"github.com/go-playground/webhooks/v6/gitea"
	"github.com/go-playground/webhooks/v6/gitlab"
	"log/slog"
	"net/http"
	"os"

	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/kimdre/docker-compose-webhook/internal/config"
	"github.com/kimdre/docker-compose-webhook/internal/git"
	"github.com/kimdre/docker-compose-webhook/internal/logger"

	gitHttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"github.com/go-playground/webhooks/v6/github"
)

const (
	path = "/webhooks"
)

func main() {
	// Set default log level to debug
	log := logger.GetLogger(slog.LevelDebug)

	// Get the application configuration
	c, err := config.GetAppConfig()
	if err != nil {
		log.Error(fmt.Sprintf("failed to parse environment variables: %+v", err))
		os.Exit(1)
	}

	// Parse the log level from the app configuration
	logLevel, err := logger.ParseLevel(c.LogLevel)
	if err != nil {
		logLevel = slog.LevelInfo
	}

	// Set the actual log level
	log = logger.GetLogger(logLevel)

	log.Info("Starting application", slog.String("log_level", c.LogLevel))

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
				slog.String("reference", event.Ref),
			)

			repoPath := "/tmp/" + event.Repository.Name

			var auth transport.AuthMethod = nil

			if event.Repository.Private {
				log.Info("Repository is private")

				if c.GitAccessToken == "" {
					log.Error("Missing access token for private repository")
					return
				}

				// Basic auth examples:
				// https://YOUR-USERNAME:GENERATED-TOKEN@github.com/YOUR-USERNAME/YOUR-REPOSITORY
				// Or
				// https://GENERATED-TOKEN@github.com/YOUR-USERNAME/YOUR-REPOSITORY
				auth = &gitHttp.BasicAuth{
					Password: c.GitAccessToken,
				}
			}

			// Clone the repository
			log.Info(
				"Cloning repository",
				slog.String("url", event.Repository.CloneURL),
				slog.String("reference", event.Ref),
			)

			repo, err := git.CloneRepository(event.Repository.CloneURL, event.Ref, auth)
			if err != nil {
				return
			}

			log.Debug("Repository cloned successfully", slog.String("path", repoPath))

			// Get the worktree from the repository
			worktree, err := repo.Worktree()
			if err != nil {
				return
			}

			// Get the filesystem from the worktree
			fs := worktree.Filesystem

			// Get the deployment config from the repository
			deployConfig, err := config.GetDeployConfig(fs, event)
			if deployConfig == nil && err != nil {
				log.Error("Failed to get deploy config: " + err.Error())
				return
			}

			log.Debug("Deployment config retrieved", slog.Any("config", deployConfig))

			// TODO docker-compose deployment logic here

		case gitlab.PushEventPayload:
			// TODO: Implement GitLab webhook handling
			log.Error("GitLab webhook event not yet implemented")

		case gitea.PushPayload:
			// TODO: Implement Gitea webhook handling
			log.Error("Gitea webhook event not yet implemented")

		default:
			log.Warn("Event not supported")
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
