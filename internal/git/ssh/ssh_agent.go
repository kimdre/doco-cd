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
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/kimdre/doco-cd/internal/logger"
)

const (
	SocketAgentSocketEnvVar = "SSH_AUTH_SOCK"
)

var socketAgentSocketPath = filepath.Join(os.TempDir(), "ssh_agent.sock")

var (
	ErrSSHAgentSocketPathEmpty = errors.New("socket path cannot be empty")
	ErrNoUsableSSHKeys         = errors.New("no usable SSH keys could be added to the agent")
)

// KeyRecord contains an SSH private key and its optional passphrase.
type KeyRecord struct {
	PrivateKey string
	Passphrase string
}

// collectKeyRecords returns the given keys without empty entries and duplicates,
// preserving the order in which they were provided.
func collectKeyRecords(keys ...KeyRecord) []KeyRecord {
	collected := make([]KeyRecord, 0, len(keys))
	seen := make(map[KeyRecord]struct{}, len(keys))

	for _, key := range keys {
		key.PrivateKey = strings.TrimSpace(key.PrivateKey)
		if key.PrivateKey == "" {
			continue
		}

		if _, ok := seen[key]; ok {
			continue
		}

		seen[key] = struct{}{}

		collected = append(collected, key)
	}

	return collected
}

// cleanupSocketFile removes the socket file at the specified path.
func cleanupSocketFile(socketPath string) {
	_ = os.Remove(socketPath)
}

// startSSHAgent starts an SSH agent that listens on a Unix domain socket at the specified path.
// The function runs until the provided context is canceled.
func startSSHAgent(ctx context.Context, log *slog.Logger, socketPath string, keys []KeyRecord) error {
	if socketPath == "" {
		return ErrSSHAgentSocketPathEmpty
	}

	socketPath = filepath.Clean(socketPath)

	// The keyring is populated before the listener is started so that failures
	// return immediately instead of leaving a running agent behind.
	keyring := agent.NewKeyring()

	var added int

	for _, key := range keys {
		if err := addKeyToAgent(keyring, []byte(key.PrivateKey), key.Passphrase); err != nil {
			// A single unusable key must not prevent the remaining keys from being served.
			log.Warn("failed to add SSH key to agent", logger.ErrAttr(err))

			continue
		}

		added++
	}

	if added == 0 {
		return ErrNoUsableSSHKeys
	}

	// Remove stale socket if it exists
	cleanupSocketFile(socketPath)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("failed to start socket agent listener: %w", err)
	}
	defer listener.Close() // nolint:errcheck

	// Set the SSH_AUTH_SOCK environment variable to point to the socket
	if err = os.Setenv(SocketAgentSocketEnvVar, socketPath); err != nil {
		cleanupSocketFile(socketPath)

		return fmt.Errorf("failed to set %s environment variable: %w", SocketAgentSocketEnvVar, err)
	}

	defer cleanupSocketFile(socketPath)

	wg := &sync.WaitGroup{}
	defer wg.Wait()
	// close the listener on context cancellation
	wg.Go(func() {
		defer listener.Close() // nolint:errcheck

		<-ctx.Done()
	})

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

				err := agent.ServeAgent(keyring, conn)
				// Ignore expected close conditions
				if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
					log.Warn("Error serving SSH agent:", logger.ErrAttr(err))
				}
			})
		}
	})

	// Wait for context cancellation
	<-ctx.Done()

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
