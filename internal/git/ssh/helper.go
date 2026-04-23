package ssh

import (
	"context"

	"github.com/kimdre/doco-cd/internal/graceful"
)

func RegisterSSHAgent(ctx context.Context, privateKey string, passphrase string) {
	agentCtx, agentCancel := context.WithCancel(ctx)
	serveFunc := func(_ context.Context) error {
		return startSSHAgent(agentCtx, socketAgentSocketPath, []byte(privateKey), passphrase)
	}

	graceful.RegisterServerFunc("SSH Agent", serveFunc, func(_ context.Context) error {
		agentCancel()
		return nil
	})
}
