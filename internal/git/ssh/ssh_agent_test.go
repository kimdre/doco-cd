package ssh

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

func TestListenSocketAgent(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "ssh-agent.sock")
	SocketAgentSocketPath = socketPath
	KnownHostsFilePath = filepath.Join(t.TempDir(), "known_hosts_test")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// Start the SSH agent listener in a separate goroutine
	errChan := make(chan error, 1)

	go func() {
		errChan <- StartSSHAgent(ctx, socketPath)
	}()

	// Wait until the socket appears or timeout
	deadline := time.Now().Add(2 * time.Second)

	for {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("SSH agent socket file does not exist: %s", socketPath)
		}

		time.Sleep(10 * time.Millisecond)
	}

	// Try to connect to the agent socket
	dial, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("Failed to connect to SSH agent socket: %v", err)
	}

	err = dial.Close()
	if err != nil {
		t.Fatalf("Failed to close connection to SSH agent socket: %v", err)
	}

	// Stop the agent
	cancel()

	// Drain error (should be nil on normal shutdown)
	select {
	case e := <-errChan:
		if e != nil && ctx.Err() == nil {
			t.Fatalf("Failed to start SSH agent listener: %v", e)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("listener did not exit on cancel")
	}
}

func TestAddKeyToAgent(t *testing.T) {
	// Start the SSH agent
	socketPath := filepath.Join(t.TempDir(), "ssh-agent.sock")
	SocketAgentSocketPath = socketPath

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go func() {
		_ = StartSSHAgent(ctx, socketPath)
	}()

	// Wait until the socket appears or timeout
	deadline := time.Now().Add(2 * time.Second)

	for {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("SSH agent socket file does not exist: %s", socketPath)
		}

		time.Sleep(10 * time.Millisecond)
	}

	// Generate a test SSH key pair
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("Failed to generate test SSH key: %v", err)
	}

	// Serialize the private key to PEM format
	privateKeyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("Failed to marshal private key to PKCS8: %v", err)
	}

	privateKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKeyDER})

	// Add the key to the agent
	err = AddKeyToAgent(privateKeyPEM, "")
	if err != nil {
		t.Fatalf("Failed to add key to SSH agent: %v", err)
	}

	// Connect to the agent and verify the key was added
	agentConn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("Failed to connect to SSH agent socket: %v", err)
	}
	defer agentConn.Close()

	agentClient := agent.NewClient(agentConn)

	keys, err := agentClient.List()
	if err != nil {
		t.Fatalf("Failed to list keys in SSH agent: %v", err)
	}

	if len(keys) != 1 {
		t.Fatalf("Expected 1 key in SSH agent, got %d", len(keys))
	}

	addedKey := keys[0]

	parsedPubKey, err := ssh.NewPublicKey(publicKey)
	if err != nil {
		t.Fatalf("Failed to parse public key: %v", err)
	}

	// Compare the added key with the original public key
	if !bytes.Equal(addedKey.Marshal(), parsedPubKey.Marshal()) {
		t.Fatalf("Added key does not match the original public key")
	}
}

func TestGetRawPrivateKey(t *testing.T) {
	// Generate a test SSH key pair
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("Failed to generate test SSH key: %v", err)
	}

	// Serialize the private key to PEM format
	privateKeyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("Failed to marshal private key to PKCS8: %v", err)
	}

	privateKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKeyDER})

	// Test parsing unencrypted private key
	rawKey, err := getRawPrivateKey(privateKeyPEM, "")
	if err != nil {
		t.Fatalf("Failed to parse unencrypted private key: %v", err)
	}

	if _, ok := rawKey.(ed25519.PrivateKey); !ok {
		t.Fatalf("Parsed key is not of type ed25519.PrivateKey")
	}
}
