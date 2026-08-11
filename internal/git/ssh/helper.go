package ssh

import (
	"context"
	"log/slog"

	"github.com/kimdre/doco-cd/internal/graceful"
)

// RegisterSSHAgent registers an SSH agent serving the given keys, ignoring empty
// and duplicate entries. It reports whether an agent was registered.
func RegisterSSHAgent(ctx context.Context, log *slog.Logger, keys []KeyRecord) bool {
	keys = collectKeyRecords(keys...)
	if len(keys) == 0 {
		return false
	}

	agentCtx, agentCancel := context.WithCancel(ctx)
	serveFunc := func(_ context.Context) error {
		return startSSHAgent(agentCtx, log, socketAgentSocketPath, keys)
	}

	graceful.RegisterServerFunc("SSH Agent", serveFunc, func(_ context.Context) error {
		agentCancel()
		return nil
	})

	return true
}
