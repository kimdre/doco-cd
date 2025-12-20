package ssh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

const (
	SocketAgentSocketEnvVar = "SSH_AUTH_SOCK"
)

var SocketAgentSocketPath = filepath.Join(os.TempDir(), "ssh_agent.sock")

// cleanupSocketAgentSocket removes the socket file at the specified path.
func cleanupSocketAgentSocket(socketPath string) {
	if socketPath == "" {
		socketPath = SocketAgentSocketPath
	}

	_ = os.Remove(socketPath)
}

// ListenSocketAgent starts a UNIX socket SSH agent and listens for incoming connections.
func ListenSocketAgent(ctx context.Context, socketPath string) error {
	if socketPath == "" {
		socketPath = SocketAgentSocketPath
	}

	socketPath = filepath.Clean(socketPath)

	// Remove stale socket if it exists
	cleanupSocketAgentSocket(socketPath)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("failed to start socket agent listener: %w", err)
	}
	defer listener.Close() // nolint:errcheck

	// Set the SSH_AUTH_SOCK environment variable to point to the socket
	err = os.Setenv(SocketAgentSocketEnvVar, socketPath)
	if err != nil {
		return fmt.Errorf("failed to set %s environment variable: %w", SocketAgentSocketEnvVar, err)
	}

	defer cleanupSocketAgentSocket(socketPath)

	keyring := agent.NewKeyring()

	// Accept loop with context awareness
	errCh := make(chan error, 1)

	go func() {
		defer close(errCh)

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
				log.Println(err)

				continue
			}

			go func(c net.Conn) {
				defer c.Close() // nolint:errcheck

				if err := agent.ServeAgent(keyring, c); err != nil {
					// Ignore expected close conditions
					if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
						log.Println("Error serving SSH agent:", err)
					}
				}
			}(conn)
		}
	}()

	// Wait for context cancellation
	<-ctx.Done()

	return nil
}

// AddKeyToAgent adds a private key to the SSH agent running at the socket specified.
func AddKeyToAgent(privateKey []byte, keyPassphrase string) error {
	conn, err := net.Dial("unix", SocketAgentSocketPath)
	if err != nil {
		return fmt.Errorf("failed to connect to SSH agent socket: %w", err)
	}
	defer conn.Close() // nolint:errcheck

	agentClient := agent.NewClient(conn)

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
func getRawPrivateKey(privateKey []byte, keyPassphrase string) (interface{}, error) {
	var (
		rawKey any
		err    error
	)

	if keyPassphrase != "" {
		rawKey, err = ssh.ParseRawPrivateKeyWithPassphrase(privateKey, []byte(keyPassphrase))
		if err != nil {
			return nil, fmt.Errorf("failed to parse encrypted private key: %w", err)
		}
	} else {
		rawKey, err = ssh.ParseRawPrivateKey(privateKey)
		if err != nil {
			return nil, fmt.Errorf("failed to parse private key: %w", err)
		}
	}

	return rawKey, nil
}
