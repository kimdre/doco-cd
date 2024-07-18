package main

import (
	"errors"
	"fmt"
	"github.com/go-playground/webhooks/v6/github"
	"net/http"
	"os"
)

const (
	path = "/webhooks"
)

func main() {
	githubWebhookSecret := os.Getenv("GITHUB_WEBHOOK_SECRET")
	httpPort := os.Getenv("HTTP_PORT")

	if githubWebhookSecret == "" {
		fmt.Println("GITHUB_WEBHOOK_SECRET is required")
		return
	}

	if httpPort == "" {
		fmt.Println("HTTP_PORT is required")
		return
	}

	hook, _ := github.New(github.Options.Secret(githubWebhookSecret))

	http.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		payload, err := hook.Parse(r, github.PushEvent, github.PullRequestEvent)
		if err != nil {
			if errors.Is(err, github.ErrEventNotFound) {
				// ok event wasn't one of the ones asked to be parsed
				fmt.Println("nope")
			}
		}

		switch payload.(type) {

		case github.PushPayload:
			push := payload.(github.PushPayload)
			// DO whatever you want from here
			fmt.Printf("%+v", push)

		case github.ReleasePayload:
			release := payload.(github.ReleasePayload)
			// Do whatever you want from here...
			fmt.Printf("%+v", release)

		case github.PullRequestPayload:
			pullRequest := payload.(github.PullRequestPayload)
			// Do whatever you want from here...
			fmt.Printf("%+v", pullRequest)

		case github.PingPayload:
			ping := payload.(github.PingPayload)
			// Do whatever you want from here...
			fmt.Printf("%+v", ping)

		default:
			fmt.Println("Event not supported")
		}
	})
	fmt.Println("Server is running on port " + httpPort)
	err := http.ListenAndServe(":"+httpPort, nil)
	if err != nil {
		return
	}
}
