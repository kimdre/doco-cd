package docker

import (
	"context"

	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/client"
)

func OnCrash(client client.APIClient, do func(), onErr func(err error)) {
	client.NegotiateAPIVersion(context.TODO())

	eventChan, errChan := client.Events(context.TODO(), events.ListOptions{})

	for {
		select {
		case event := <-eventChan:
			if event.Type == "container" {
				switch event.Action {
				case "die", "kill", "stop", "oom", "destroy":
					do()
					return
				}
			}
		case err := <-errChan:
			if err != nil {
				onErr(err)
			}
			return
		}
	}
}