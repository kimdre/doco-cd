package ssh

import (
	"context"
	"crypto/x509"
	"encoding/pem"
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

// cleanupSocketFile removes the socket file at the specified path.
func cleanupSocketFile(socketPath string) {
	if socketPath == "" {
		socketPath = SocketAgentSocketPath
	}

	_ = os.Remove(socketPath)
}

// StartSSHAgent starts an SSH agent that listens on a Unix domain socket at the specified path.
// If no path is provided, it defaults to SocketAgentSocketPath.
// The function runs until the provided context is canceled.
func StartSSHAgent(ctx context.Context, socketPath string) error {
	if socketPath == "" {
		socketPath = SocketAgentSocketPath
	}

	socketPath = filepath.Clean(socketPath)

	// Remove stale socket if it exists
	cleanupSocketFile(socketPath)

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

	defer cleanupSocketFile(socketPath)

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
func getRawPrivateKey(pemBytes []byte, passphrase string) (interface{}, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("failed to decode PEM block")
	}

	fmt.Println("Type: ", block.Type)

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
