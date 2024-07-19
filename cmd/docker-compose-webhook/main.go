package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/go-playground/webhooks/v6/github"
)

const (
	path = "/webhooks"
)

func main() {
	log := GetLogger()

	githubWebhookSecret := os.Getenv("GITHUB_WEBHOOK_SECRET")
	httpPort := os.Getenv("HTTP_PORT")

	if githubWebhookSecret == "" {
		log.Error("GITHUB_WEBHOOK_SECRET is required")
		return
	}

	if httpPort == "" {
		log.Error("HTTP_PORT is required")
		return
	}

	hook, _ := github.New(github.Options.Secret(githubWebhookSecret))

	http.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		payload, err := hook.Parse(r, github.PushEvent, github.PullRequestEvent)
		if err != nil {
			if errors.Is(err, github.ErrEventNotFound) {
				// ok event wasn't one of the ones asked to be parsed
				log.Error("Event not found")
			}
		}

		switch event := payload.(type) {
		case github.PushPayload:
			log.Info("Push event received for " + event.Repository.FullName)

			if !DirectoryExists(fmt.Sprintf("/tmp/%v", event.Repository.ID)) {
				repo, err := CloneRepository(
					event.Repository.CloneURL,
					event.Ref,
					event.Repository.Private,
				)
				if err != nil {
					return
				}

				fmt.Println(repo)
			}

		case github.PingPayload:
			log.Info("Ping event received")

			ping := payload.(github.PingPayload)
			// Do whatever you want from here...
			fmt.Printf("%+v", ping)

		default:
			log.Warn("Event not supported")
		}
	})
	log.Info("Server listening on port " + httpPort)

	err := http.ListenAndServe(":"+httpPort, nil)
	if err != nil {
		return
	}
}
