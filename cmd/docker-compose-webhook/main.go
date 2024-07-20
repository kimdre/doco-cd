package main

import (
	"errors"
	"fmt"
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
	log := logger.GetLogger()

	c, err := config.GetAppConfig()
	if err != nil {
		log.Error(fmt.Sprintf("failed to parse environment variables: %+v", err))
		os.Exit(1)
	}

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
			log.Info("Push event received for repository " + event.Repository.FullName + " (" + event.Ref + ")")

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
					Username: c.GitUsername,
					Password: c.GitAccessToken,
				}
			}

			// Clone the repository
			repo, err := git.CloneRepository(event.Repository.CloneURL, event.Ref, auth)
			if err != nil {
				return
			}

			log.Info("Repository cloned successfully", slog.String("path", repoPath))

			// Show files in the repository
			worktree, err := repo.Worktree()
			if err != nil {
				return
			}

			fs := worktree.Filesystem

			deployConfig, err := config.GetDeployConfig(fs, event)
			if deployConfig == nil && err != nil {
				log.Error("Failed to get deploy config: " + err.Error())
				return
			}

			fmt.Println(*deployConfig)

		case github.PingPayload:
			log.Info("Ping event received")

			ping := payload.(github.PingPayload)
			// Do whatever you want from here...
			log.Info("Ping event received for repository " + ping.Repository.FullName)

		default:
			log.Warn("Event not supported")
		}
	})
	log.Info("Server listening on port " + c.HttpPort)

	err = http.ListenAndServe(":"+c.HttpPort, nil)
	if err != nil {
		return
	}
}
