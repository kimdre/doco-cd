package docker

import (
	"log/slog"
	"strings"
)

// SelfDeployInputFromLabels rebuilds the deploy context from a deployed stack's
// labels, for the redeploy paths that have no webhook payload.
func SelfDeployInputFromLabels(labels map[string]string) *SelfDeployInput {
	if len(labels) == 0 {
		return nil
	}

	return &SelfDeployInput{
		RepoName:     strings.TrimSpace(labels[DocoCDLabels.Source.Name]),
		SourceURL:    strings.TrimSpace(labels[DocoCDLabels.Source.URL]),
		SourceType:   strings.TrimSpace(labels[DocoCDLabels.Source.Type]),
		Reference:    strings.TrimSpace(labels[DocoCDLabels.Deployment.TargetRef]),
		ConfigTarget: strings.TrimSpace(labels[DocoCDLabels.Deployment.ConfigTarget]),
		CommitSHA:    strings.TrimSpace(labels[DocoCDLabels.Deployment.CommitSHA]),
		ProjectHash:  strings.TrimSpace(labels[DocoCDLabels.Deployment.ComposeHash]),
		Trigger:      strings.TrimSpace(labels[DocoCDLabels.Deployment.Trigger]),
		Log:          slog.Default(),
	}
}
