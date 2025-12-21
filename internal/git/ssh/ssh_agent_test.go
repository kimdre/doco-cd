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
			name: "Encrypted ED25519 key with correct passphrase",
			generateKey: func() ([]byte, error) {
				_, privateKey, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					return nil, err
				}

				privateKeyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
				if err != nil {
					return nil, err
				}

				block, err := x509.EncryptPEMBlock( // nolint:staticcheck
					rand.Reader,
					"ENCRYPTED PRIVATE KEY",
					privateKeyDER,
					[]byte("testpass"),
					x509.PEMCipherAES256,
				)
				if err != nil {
					return nil, err
				}

				privateKeyPEM := pem.EncodeToMemory(block)

				return privateKeyPEM, nil
			},
			keyPassphrase: "testpass",
			wantErr:       false,
		},
		{
			name: "Encrypted ED25519 key with incorrect passphrase",
			generateKey: func() ([]byte, error) {
				_, privateKey, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					return nil, err
				}

				privateKeyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
				if err != nil {
					return nil, err
				}

				block, err := x509.EncryptPEMBlock( // nolint:staticcheck
					rand.Reader,
					"PRIVATE KEY",
					privateKeyDER,
					[]byte("testpass"),
					x509.PEMCipherAES256,
				)
				if err != nil {
					return nil, err
				}

				privateKeyPEM := pem.EncodeToMemory(block)

				return privateKeyPEM, nil
			},
			keyPassphrase: "wrongpass",
			wantErr:       true,
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
