package docker

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/client"
)

func OnCrash(client client.APIClient, containerID string, do func(), onErr func(err error)) {
	client.NegotiateAPIVersion(context.TODO())

	eventChan, errChan := client.Events(context.TODO(), events.ListOptions{})

	for {
		select {
		case event := <-eventChan:
			fmt.Printf("received '%v' event: event '%v' requests '%v'", event.Type, event.ID, event.Action)
			if event.Type == "container" && event.ID == containerID {
				switch event.Action {
				case "die", "kill", "stop", "oom", "destroy":
					containerJSON, err := client.ContainerInspect(context.TODO(), containerID)
					if err != nil {
						onErr(fmt.Errorf("failed to inspect container: %w", err))
						return
					}

					if !containerJSON.State.Restarting {
						do()
						return
					}
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