package main

import (
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
)

type projectResponse struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	ConfigFiles string `json:"config_files"`
	Reason      string `json:"reason"`
}

type portPublisherResponse struct {
	URL           string `json:"url"`
	TargetPort    int    `json:"target_port"`
	PublishedPort int    `json:"published_port"`
	Protocol      string `json:"protocol"`
}

type containerResponse struct {
	ID           string                   `json:"id"`
	Name         string                   `json:"name"`
	Names        []string                 `json:"names"`
	Image        string                   `json:"image"`
	Command      string                   `json:"command"`
	Project      string                   `json:"project"`
	Service      string                   `json:"service"`
	Created      int64                    `json:"created"`
	State        container.ContainerState `json:"state"`
	Status       string                   `json:"status"`
	Health       container.HealthStatus   `json:"health"`
	ExitCode     int                      `json:"exit_code"`
	Publishers   []portPublisherResponse  `json:"publishers"`
	Labels       map[string]string        `json:"labels"`
	SizeRw       int64                    `json:"size_rw,omitempty"`
	SizeRootFs   int64                    `json:"size_root_fs,omitempty"`
	Mounts       []string                 `json:"mounts"`
	Networks     []string                 `json:"networks"`
	LocalVolumes int                      `json:"local_volumes"`
}

func projectResponses(projects []api.Stack) []projectResponse {
	if projects == nil {
		return nil
	}

	responses := make([]projectResponse, len(projects))
	for i, project := range projects {
		responses[i] = projectResponse{
			ID:          project.ID,
			Name:        project.Name,
			Status:      project.Status,
			ConfigFiles: project.ConfigFiles,
			Reason:      project.Reason,
		}
	}

	return responses
}

func containerResponses(containers []api.ContainerSummary) []containerResponse {
	if containers == nil {
		return nil
	}

	responses := make([]containerResponse, len(containers))
	for i, c := range containers {
		var publishers []portPublisherResponse
		if c.Publishers != nil {
			publishers = make([]portPublisherResponse, len(c.Publishers))
			for j, publisher := range c.Publishers {
				publishers[j] = portPublisherResponse{
					URL:           publisher.URL,
					TargetPort:    publisher.TargetPort,
					PublishedPort: publisher.PublishedPort,
					Protocol:      publisher.Protocol,
				}
			}
		}

		responses[i] = containerResponse{
			ID:           c.ID,
			Name:         c.Name,
			Names:        c.Names,
			Image:        c.Image,
			Command:      c.Command,
			Project:      c.Project,
			Service:      c.Service,
			Created:      c.Created,
			State:        c.State,
			Status:       c.Status,
			Health:       c.Health,
			ExitCode:     c.ExitCode,
			Publishers:   publishers,
			Labels:       c.Labels,
			SizeRw:       c.SizeRw,
			SizeRootFs:   c.SizeRootFs,
			Mounts:       c.Mounts,
			Networks:     c.Networks,
			LocalVolumes: c.LocalVolumes,
		}
	}

	return responses
}
