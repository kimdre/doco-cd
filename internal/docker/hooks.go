package docker

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/client"
)

// OnCrash listens for container events and executes a callback function when the specified container crashes or stops.
func OnCrash(client client.APIClient, containerID string, do func(), onErr func(err error)) {
	client.NegotiateAPIVersion(context.TODO())

	eventChan, errChan := client.Events(context.TODO(), events.ListOptions{})

	for {
		select {
		case event := <-eventChan:
			// fmt.Printf("received '%v' event: event '%v' requests '%v'", event.Type, event.ID, event.Action)
			if event.Type == "container" && event.Actor.ID == containerID {
				switch event.Action {
				case "die", "kill", "stop", "oom", "destroy":
					containerData, err := client.ContainerInspect(context.TODO(), containerID)
					if err != nil {
						onErr(fmt.Errorf("failed to inspect container: %w", err))
						return
					}

					if !containerData.State.Restarting {
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
