package ssh

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
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
		errChan <- ListenSocketAgent(ctx, socketPath)
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
