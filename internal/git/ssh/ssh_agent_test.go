package ssh

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

func TestListenSocketAgentAddKeyToAgent(t *testing.T) {
	socketPath := shortSocketPath(t, "one")

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

	wg := &sync.WaitGroup{}
	defer wg.Wait()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	startErr := make(chan error, 1)
	// Start the SSH agent
	wg.Go(func() {
		startErr <- startSSHAgent(ctx, slog.Default(), socketPath, []KeyRecord{{PrivateKey: string(privateKeyPEM)}})
	})

	// Wait until the socket appears or timeout
	deadline := time.Now().Add(2 * time.Second)

	for {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}

		if time.Now().After(deadline) {
			select {
			case err := <-startErr:
				t.Fatalf("failed to start SSH agent: %v", err)
			default:
			}

			t.Fatalf("SSH agent socket file does not exist: %s", socketPath)
		}

		time.Sleep(10 * time.Millisecond)
	}

	// Connect to the agent and verify the key was added
	agentConn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("Failed to connect to SSH agent socket: %v", err)
	}
	defer agentConn.Close() // nolint:errcheck

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

func TestListenSocketAgentAddsMultipleKeys(t *testing.T) {
	socketPath := shortSocketPath(t, "multiple")
	keys := make([]KeyRecord, 0, 2)

	for range 2 {
		_, privateKey, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("generate test key: %v", err)
		}

		privateKeyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
		if err != nil {
			t.Fatalf("marshal test key: %v", err)
		}

		keys = append(keys, KeyRecord{PrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKeyDER}))})
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	startErr := make(chan error, 1)
	go func() {
		startErr <- startSSHAgent(ctx, slog.Default(), socketPath, keys)
	}()

	deadline := time.Now().Add(2 * time.Second)

	for {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}

		if time.Now().After(deadline) {
			select {
			case err := <-startErr:
				t.Fatalf("failed to start SSH agent: %v", err)
			default:
			}

			t.Fatalf("SSH agent socket file does not exist: %s", socketPath)
		}

		time.Sleep(10 * time.Millisecond)
	}

	agentConn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("connect to agent: %v", err)
	}
	defer agentConn.Close() // nolint:errcheck

	addedKeys, err := agent.NewClient(agentConn).List()
	if err != nil {
		t.Fatalf("list agent keys: %v", err)
	}

	if len(addedKeys) != 2 {
		t.Fatalf("expected 2 keys in SSH agent, got %d", len(addedKeys))
	}
}

func shortSocketPath(t *testing.T, name string) string {
	t.Helper()

	// Unix socket paths are limited to ~104 bytes, which t.TempDir() and some
	// temp directories exceed, so fall back to /tmp for long paths.
	fileName := fmt.Sprintf("dc-agent-%d-%s.sock", os.Getpid(), name)

	socketPath := filepath.Join(os.TempDir(), fileName)
	if len(socketPath) > 100 {
		socketPath = filepath.Join("/tmp", fileName)
	}

	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove stale socket: %v", err)
	}

	t.Cleanup(func() {
		_ = os.Remove(socketPath)
	})

	return socketPath
}

func TestGetRawPrivateKey(t *testing.T) {
	testCases := []struct {
		name          string
		generateKey   func() ([]byte, error)
		keyPassphrase string
		wantErr       bool
	}{
		{
			name: "Unencrypted ED25519 key",
			generateKey: func() ([]byte, error) {
				_, privateKey, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					return nil, err
				}

				privateKeyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
				if err != nil {
					return nil, err
				}

				privateKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKeyDER})

				return privateKeyPEM, nil
			},
			keyPassphrase: "",
			wantErr:       false,
		},
		{
			name: "Malformed private key",
			generateKey: func() ([]byte, error) {
				return []byte("malformed key data"), nil
			},
			keyPassphrase: "",
			wantErr:       true,
		},
		{
			name: "OpenSSH private key",
			generateKey: func() ([]byte, error) {
				// This is a sample unencrypted ed25519 OpenSSH private key
				// ssh-keygen -t ed25519 -f test_ed25519_openssh -N ""
				privateKeyPEM := []byte(`-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW
QyNTUxOQAAACCU6Sk58h0kd2bUvHHvyS1JQiLgBf6yKaIbpGlK8TEfVAAAAJgBQMSpAUDE
qQAAAAtzc2gtZWQyNTUxOQAAACCU6Sk58h0kd2bUvHHvyS1JQiLgBf6yKaIbpGlK8TEfVA
AAAEBBVspZHjWj6Np5szQQHB6w+1X3ZOatDcMmcnm1+R9J9pTpKTnyHSR3ZtS8ce/JLUlC
IuAF/rIpohukaUrxMR9UAAAADmtpbUBraW0tZmVkb3JhAQIDBAUGBw==
-----END OPENSSH PRIVATE KEY-----`)

				return privateKeyPEM, nil
			},
			keyPassphrase: "",
			wantErr:       false,
		},
		{
			name: "OpenSSH private key with passphrase",
			generateKey: func() ([]byte, error) {
				// This is a sample ed25519 OpenSSH private key encrypted with passphrase "doco-cd"
				// ssh-keygen -t ed25519 -f test_ed25519_openssh_pass -N "doco-cd"
				privateKeyPEM := []byte(`-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAACmFlczI1Ni1jdHIAAAAGYmNyeXB0AAAAGAAAABA+Zz/91P
rp2u7NvTWBtLI0AAAAGAAAAAEAAAAzAAAAC3NzaC1lZDI1NTE5AAAAIFyEIiKcYAJl82Ga
40hVJoKO1qOvVfekORkGLSsKFnF7AAAAoBgOn6fvoLqNvcj0QMyuZTYVJEm9YXs8zNkG+9
suGsdNHOvMRQWLzq9VJiJUyOG29zayIQ4Q3pZlcoRINpUI9yl4/eFza7P4MEHDVBLF531K
X3nAnZomTg2czfus92AmR+3kYDWvBE1WkpieAaRfVTuBtNcB41rOAZMLQ001zhVF2qdb+D
+tvLTkrbIyLPEbZOBHuCH+mVgPefYCRXsB9Nw=
-----END OPENSSH PRIVATE KEY-----`)

				return privateKeyPEM, nil
			},
			keyPassphrase: "doco-cd",
			wantErr:       false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			privateKeyPEM, err := tc.generateKey()
			if err != nil {
				t.Fatalf("Failed to generate test key: %v", err)
			}

			_, err = getRawPrivateKey(privateKeyPEM, tc.keyPassphrase)
			if (err != nil) != tc.wantErr {
				t.Errorf("getRawPrivateKey() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestStartSSHAgentFailsWhenNoKeyIsUsable(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	err := startSSHAgent(ctx, slog.Default(), shortSocketPath(t, "invalid"), []KeyRecord{{PrivateKey: "not-a-key"}})
	if !errors.Is(err, ErrNoUsableSSHKeys) {
		t.Fatalf("expected %v, got %v", ErrNoUsableSSHKeys, err)
	}
}

func TestCollectKeyRecordsSkipsEmptyAndDuplicateKeys(t *testing.T) {
	keys := collectKeyRecords(
		KeyRecord{PrivateKey: " global-key ", Passphrase: "global-passphrase"},
		KeyRecord{PrivateKey: "scoped-key", Passphrase: "scoped-passphrase"}, // #nosec G101 -- test fixture, not a credential
		KeyRecord{PrivateKey: "global-key", Passphrase: "global-passphrase"},
		KeyRecord{PrivateKey: "  "},
	)

	if len(keys) != 2 {
		t.Fatalf("expected two unique SSH key records, got %d", len(keys))
	}

	if keys[0].PrivateKey != "global-key" || keys[1].PrivateKey != "scoped-key" {
		t.Fatalf("unexpected key records: %#v", keys)
	}
}

func TestCollectKeyRecordsSameKeyDifferentPassphraseAreDistinct(t *testing.T) {
	keys := collectKeyRecords(
		KeyRecord{PrivateKey: "mykey", Passphrase: "passphrase-a"},
		KeyRecord{PrivateKey: "mykey", Passphrase: "passphrase-b"},
	)

	if len(keys) != 2 {
		t.Fatalf("expected two records (same key, different passphrase), got %d", len(keys))
	}
}

func TestCollectKeyRecordsPreservesOrder(t *testing.T) {
	keys := collectKeyRecords(
		KeyRecord{PrivateKey: "first"},
		KeyRecord{PrivateKey: "second"},
		KeyRecord{PrivateKey: "third"},
	)

	if len(keys) != 3 {
		t.Fatalf("expected 3 records, got %d", len(keys))
	}

	for i, want := range []string{"first", "second", "third"} {
		if keys[i].PrivateKey != want {
			t.Errorf("keys[%d]=%q, want %q", i, keys[i].PrivateKey, want)
		}
	}
}

func TestRegisterSSHAgentWithoutKeys(t *testing.T) {
	if RegisterSSHAgent(t.Context(), slog.Default(), []KeyRecord{{PrivateKey: " "}}) {
		t.Fatal("expected no SSH agent to be registered without usable keys")
	}
}

func TestRegisterSSHAgentWithValidKeyReturnsTrue(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	privateKeyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}

	pemBytes := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKeyDER}))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	if !RegisterSSHAgent(ctx, slog.Default(), []KeyRecord{{PrivateKey: pemBytes}}) {
		t.Fatal("expected RegisterSSHAgent to return true with a valid key")
	}
}
