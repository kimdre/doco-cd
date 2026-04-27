package ssh

import (
	"context"
	"log/slog"

	"github.com/kimdre/doco-cd/internal/graceful"
)

func RegisterSSHAgent(ctx context.Context, log *slog.Logger, privateKey string, passphrase string) {
	agentCtx, agentCancel := context.WithCancel(ctx)
	serveFunc := func(_ context.Context) error {
		return startSSHAgent(agentCtx, log, socketAgentSocketPath, []byte(privateKey), passphrase)
	}

	graceful.RegisterServerFunc("SSH Agent", serveFunc, func(_ context.Context) error {
		agentCancel()
		return nil
	})
}
