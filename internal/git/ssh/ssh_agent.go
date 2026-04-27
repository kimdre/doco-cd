package ssh

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/kimdre/doco-cd/internal/logger"
)

const (
	SocketAgentSocketEnvVar = "SSH_AUTH_SOCK"
)

var socketAgentSocketPath = filepath.Join(os.TempDir(), "ssh_agent.sock")

var ErrSSHAgentSocketPathEmpty = errors.New("socket path cannot be empty")

// cleanupSocketFile removes the socket file at the specified path.
func cleanupSocketFile(socketPath string) {
	_ = os.Remove(socketPath)
}

// startSSHAgent starts an SSH agent that listens on a Unix domain socket at the specified path.
// The function runs until the provided context is canceled.
func startSSHAgent(ctx context.Context, log *slog.Logger, socketPath string, privateKey []byte, keyPassphrase string) error {
	if socketPath == "" {
		return ErrSSHAgentSocketPathEmpty
	}

	socketPath = filepath.Clean(socketPath)

	// Remove stale socket if it exists
	cleanupSocketFile(socketPath)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("failed to start socket agent listener: %w", err)
	}
	defer listener.Close() // nolint:errcheck

	wg := &sync.WaitGroup{}
	defer wg.Wait()
	// close the listener on context cancellation
	wg.Go(func() {
		defer listener.Close() // nolint:errcheck

		<-ctx.Done()
	})

	// Set the SSH_AUTH_SOCK environment variable to point to the socket
	err = os.Setenv(SocketAgentSocketEnvVar, socketPath)
	if err != nil {
		return fmt.Errorf("failed to set %s environment variable: %w", SocketAgentSocketEnvVar, err)
	}

	defer cleanupSocketFile(socketPath)

	keyring := agent.NewKeyring()
	if err := addKeyToAgent(keyring, privateKey, keyPassphrase); err != nil {
		return err
	}

	// Accept loop with context awareness
	wg.Go(func() {
		for {
			// Non-blocking stop check
			select {
			case <-ctx.Done():
				return
			default:
			}

			conn, err := listener.Accept()
			if err != nil {
				// Break on expected shutdown or EOF
				if ctx.Err() != nil || errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) {
					return
				}
				// Log and continue on transient errors
				log.Warn("Failed to accept SSH agent connection", logger.ErrAttr(err))

				continue
			}

			wg.Go(func() {
				defer conn.Close() // nolint:errcheck

				if err := agent.ServeAgent(keyring, conn); err != nil {
					// Ignore expected close conditions
					if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
						log.Warn("Error serving SSH agent:", logger.ErrAttr(err))
					}
				}
			})
		}
	})

	return nil
}

// addKeyToAgent adds a private key to the SSH agent running at the socket specified.
func addKeyToAgent(agentClient agent.Agent, privateKey []byte, keyPassphrase string) error {
	rawKey, err := getRawPrivateKey(privateKey, keyPassphrase)
	if err != nil {
		return err
	}

	return agentClient.Add(agent.AddedKey{
		PrivateKey:   rawKey,
		Comment:      "added by ssh agent",
		LifetimeSecs: 0,
	})
}

// getRawPrivateKey parses the private key bytes and returns the raw private key object.
func getRawPrivateKey(pemBytes []byte, passphrase string) (any, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("failed to decode PEM block")
	}

	switch block.Type {
	case "ENCRYPTED PRIVATE KEY":
		// Deprecated, but we still use it for compatibility
		der, err := x509.DecryptPEMBlock(block, []byte(passphrase)) // nolint:staticcheck
		if err != nil {
			return nil, err
		}

		return x509.ParsePKCS8PrivateKey(der)
	case "PRIVATE KEY":
		return x509.ParsePKCS8PrivateKey(block.Bytes)
	default:
		// fallback to ssh package for other types
		if passphrase != "" {
			return ssh.ParseRawPrivateKeyWithPassphrase(pemBytes, []byte(passphrase))
		}

		return ssh.ParseRawPrivateKey(pemBytes)
	}
}
