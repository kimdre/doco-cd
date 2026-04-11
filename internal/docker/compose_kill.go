package docker

import (
	"context"
	"fmt"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
)

// ComposeSignal will send signal to service.
func ComposeSignal(ctx context.Context, dockerCli command.Cli, project *types.Project, signal []SignalService) error {
	service, err := compose.NewComposeService(dockerCli)
	if err != nil {
		return err
	}

	for _, s := range signal {
		err := service.Kill(ctx, project.Name, api.KillOptions{
			Signal:   s.Signal,
			Services: []string{s.ServiceName},
		})
		if err != nil {
			return fmt.Errorf("failed to send signal(%s) to service %s: %w", s.Signal, s.ServiceName, err)
		}
	}

	return nil
}
