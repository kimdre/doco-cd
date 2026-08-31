package docker

import (
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/api"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/webhook"
)

/*
addComposeServiceLabels adds the labels docker compose expects to exist on services.
This is required for future compose operations to work, such as finding
containers that are part of a service.
*/
func addComposeServiceLabels(project *types.Project, deployConfig *deploy.Config, payload *webhook.ParsedPayload,
	workingDir, appVersion, timestamp, composeVersion, latestCommit, projectHash string,
) {
	for i, s := range project.Services {
		// Extract service dependencies (depends_on)
		dependencies := make([]string, 0, len(s.DependsOn))
		for dep := range s.DependsOn {
			// https://docs.docker.com/compose/how-tos/startup-order/#control-startup
			// Example: <service>:<condition>:<restart>
			dependencies = append(dependencies, dep)
		}

		s.CustomLabels = map[string]string{
			DocoCDLabels.Metadata.Manager:               app.Name,
			DocoCDLabels.Metadata.Version:               appVersion,
			DocoCDLabels.Deployment.Name:                deployConfig.Name,
			DocoCDLabels.Deployment.Timestamp:           timestamp,
			DocoCDLabels.Deployment.ComposeHash:         projectHash,
			DocoCDLabels.Deployment.WorkingDir:          workingDir,
			DocoCDLabels.Deployment.ConfigTarget:        deployConfig.Internal.ConfigTarget,
			DocoCDLabels.Deployment.Trigger:             payload.TriggerString(),
			DocoCDLabels.Deployment.CommitSHA:           latestCommit,
			DocoCDLabels.Deployment.TargetRef:           ExtractOciArtifactTag(deployConfig.Reference),
			DocoCDLabels.Deployment.ConfigHash:          deployConfig.Internal.Hash,
			DocoCDLabels.Deployment.AutoDiscovery:       strconv.FormatBool(deployConfig.AutoDiscovery.Enabled),
			DocoCDLabels.Deployment.AutoDiscoveryConfig: MarshalAutoDiscoveryConfig(deployConfig.AutoDiscovery),
			DocoCDLabels.Source.Type:                    SourceTypeLabelValue(string(payload.Source), string(deployConfig.Source)),
			DocoCDLabels.Source.Name:                    payload.FullName,
			DocoCDLabels.Source.URL:                     payload.WebURL,
			api.ProjectLabel:                            project.Name,
			api.ServiceLabel:                            s.Name,
			api.WorkingDirLabel:                         project.WorkingDir,
			api.ConfigFilesLabel:                        strings.Join(project.ComposeFiles, ","),
			api.VersionLabel:                            composeVersion,
			api.OneoffLabel:                             "False", // default, will be overridden by docker compose
			api.DependenciesLabel:                       strings.Join(dependencies, ","),
		}

		applyCertRotationLabelsToService(s.CustomLabels, s, project, deployConfig)

		project.Services[i] = s
	}
}

func addComposeVolumeLabels(project *types.Project, deployConfig *deploy.Config, payload *webhook.ParsedPayload,
	appVersion, timestamp, composeVersion, latestCommit, projectHash string,
) {
	for i, v := range project.Volumes {
		v.CustomLabels = map[string]string{
			DocoCDLabels.Metadata.Manager:        app.Name,
			DocoCDLabels.Metadata.Version:        appVersion,
			DocoCDLabels.Deployment.Name:         deployConfig.Name,
			DocoCDLabels.Deployment.Timestamp:    timestamp,
			DocoCDLabels.Deployment.ComposeHash:  projectHash,
			DocoCDLabels.Deployment.Trigger:      payload.TriggerString(),
			DocoCDLabels.Deployment.ConfigTarget: deployConfig.Internal.ConfigTarget,
			DocoCDLabels.Deployment.TargetRef:    ExtractOciArtifactTag(deployConfig.Reference),
			DocoCDLabels.Deployment.CommitSHA:    latestCommit,
			DocoCDLabels.Source.Type:             SourceTypeLabelValue(string(payload.Source), string(deployConfig.Source)),
			DocoCDLabels.Source.Name:             payload.FullName,
			DocoCDLabels.Source.URL:              payload.WebURL,
			api.ProjectLabel:                     project.Name,
			api.VolumeLabel:                      v.Name,
			api.VersionLabel:                     composeVersion,
		}
		project.Volumes[i] = v
	}
}

// hasIPv6NetworkWithoutExplicitSubnet reports whether a project enables IPv6 on
// at least one network but omits an explicit IPAM subnet. In that case docker
// compose diverged-recreate can fail while parsing daemon-reported IPv6 gateway
// values that include CIDR suffixes (e.g. "...::1/64").
func hasIPv6NetworkWithoutExplicitSubnet(project *types.Project) bool {
	for _, network := range project.Networks {
		if network.EnableIPv6 == nil || !*network.EnableIPv6 {
			continue
		}

		hasSubnet := false

		for _, ipam := range network.Ipam.Config {
			if strings.TrimSpace(ipam.Subnet) != "" {
				hasSubnet = true
				break
			}
		}

		if !hasSubnet {
			return true
		}
	}

	return false
}
