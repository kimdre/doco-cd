package stages

import (
	"fmt"
	"strings"

	"github.com/kimdre/doco-cd/internal/docker"
)

// successfulCommitStatusSummary describes the deployment using the same immutable
// commit as its check run, without additional Git or Docker reads.
func (s *StageManager) successfulCommitStatusSummary(commitSHA string) string {
	var b strings.Builder

	stack := MarkdownCode(strings.TrimSpace(s.DeployConfig.Name))
	_, _ = fmt.Fprintf(&b, "Successfully deployed stack %s.\n\n| Detail | Value |\n| --- | --- |\n", markdownTableCell(stack))

	writeDetail := func(name, value string) {
		_, _ = fmt.Fprintf(&b, "| %s | %s |\n", name, markdownTableCell(value))
	}

	writeDetail("Stack", stack)

	if target := strings.TrimSpace(s.DeployConfig.Internal.ConfigTarget); target != "" {
		writeDetail("Target", MarkdownCode(target))
	}

	writeDetail("Docker context", MarkdownCode(docker.DisplayContextName(s.DeployConfig.Context)))

	reference := strings.TrimSpace(s.DeployConfig.Reference)
	if s.Repository != nil && strings.TrimSpace(s.Repository.ResolvedReference) != "" {
		reference = strings.TrimSpace(s.Repository.ResolvedReference)
	}

	if reference != "" {
		writeDetail("Reference", MarkdownCode(reference))
	}

	if commitSHA = strings.TrimSpace(commitSHA); commitSHA != "" {
		writeDetail("Commit", MarkdownCode(shortCommit(commitSHA)))
	}

	if s.Docker != nil {
		mode := "Compose"
		if s.Docker.SwarmMode {
			mode = "Swarm"
		}

		writeDetail("Deployment mode", mode)
	}

	switch s.JobTrigger {
	case JobTriggerWebhook:
		writeDetail("Trigger", "Webhook")
	case JobTriggerPoll:
		writeDetail("Trigger", "Poll")
	}

	services := s.DeployState.changedServiceNames()
	if len(services) == 0 {
		writeDetail("Detected changed services", "Not individually tracked")
	} else {
		formatted := make([]string, len(services))
		for i, service := range services {
			formatted[i] = MarkdownCode(service)
		}

		writeDetail("Detected changed services", strings.Join(formatted, ", "))
	}

	return b.String()
}
