package git_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gittransportssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	cryptossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/kimdre/doco-cd/internal/git"
	internalssh "github.com/kimdre/doco-cd/internal/git/ssh"
)

func TestSSHHostKeyMismatchFailsClosed(t *testing.T) {
	originalKnownHostsFilePath := internalssh.KnownHostsFilePath

	t.Cleanup(func() {
		internalssh.KnownHostsFilePath = originalKnownHostsFilePath

		if err := internalssh.ConfigureKnownHostsFile(""); err != nil {
			t.Errorf("reset known_hosts configuration: %v", err)
		}
	})

	// CloneOrUpdateBareMirror resolves auth from key material itself (unlike
	// FetchRepository below, which takes a pre-built transport.AuthMethod), so
	// the mismatch must be detected via the client's real HostKeyCallback
	// fallback to SSH_KNOWN_HOSTS rather than an explicitly injected one.
	clientKeyPEM := newSSHPrivateKeyPEM(t)

	tests := []struct {
		name      string
		operation func(*testing.T, string, transport.AuthMethod) error
	}{
		{
			name: "clone",
			operation: func(t *testing.T, url string, _ transport.AuthMethod) error {
				_, err := git.CloneOrUpdateBareMirror(
					nil,
					url,
					git.MainBranch,
					filepath.Join(t.TempDir(), "repository"),
					true,
					clientKeyPEM, "", "",
					false,
					transport.ProxyOptions{},
					0,
				)

				return err
			},
		},
		{
			name: "fetch",
			operation: func(t *testing.T, url string, auth transport.AuthMethod) error {
				repo, err := gogit.PlainInit(filepath.Join(t.TempDir(), "repository"), false)
				if err != nil {
					t.Fatalf("PlainInit() error = %v", err)
				}

				if _, err = repo.CreateRemote(&config.RemoteConfig{
					Name: git.RemoteName,
					URLs: []string{url},
				}); err != nil {
					t.Fatalf("CreateRemote() error = %v", err)
				}

				return git.FetchRepository(repo, url, false, transport.ProxyOptions{}, auth, 0)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url, address := startHostKeyMismatchSSHServer(t)

			trustedSigner := newSSHSigner(t)
			knownHostsContent := knownhosts.Line([]string{address}, trustedSigner.PublicKey()) + "\n"
			knownHostsPath := filepath.Join(t.TempDir(), "known_hosts")

			if err := os.WriteFile(knownHostsPath, []byte(knownHostsContent), 0o600); err != nil {
				t.Fatalf("write known_hosts: %v", err)
			}

			if err := internalssh.ConfigureKnownHostsFile(knownHostsPath); err != nil {
				t.Fatalf("ConfigureKnownHostsFile() error = %v", err)
			}

			hostKeyCallback, err := knownhosts.New(knownHostsPath)
			if err != nil {
				t.Fatalf("knownhosts.New() error = %v", err)
			}

			auth := &gittransportssh.PublicKeys{
				User:   internalssh.DefaultGitSSHUser,
				Signer: newSSHSigner(t),
			}
			auth.HostKeyCallback = hostKeyCallback

			err = tt.operation(t, url, auth)
			if err == nil {
				t.Fatal("SSH operation error = nil, want host-key mismatch")
			}

			if !internalssh.IsHostKeyMismatchError(err) {
				t.Fatalf("SSH operation error = %v, want explicit host-key mismatch", err)
			}

			got, readErr := os.ReadFile(knownHostsPath)
			if readErr != nil {
				t.Fatalf("read known_hosts: %v", readErr)
			}

			if string(got) != knownHostsContent {
				t.Fatalf("known_hosts changed after mismatch:\ngot  %q\nwant %q", got, knownHostsContent)
			}
		})
	}
}

func startHostKeyMismatchSSHServer(t *testing.T) (string, string) {
	t.Helper()

	serverConfig := &cryptossh.ServerConfig{NoClientAuth: true}
	serverConfig.AddHostKey(newSSHSigner(t))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}

	t.Cleanup(func() {
		_ = listener.Close()
	})

	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}

			go func() {
				defer func() { _ = conn.Close() }()

				_, _, _, _ = cryptossh.NewServerConn(conn, serverConfig)
			}()
		}
	}()

	address := listener.Addr().String()

	host, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("net.SplitHostPort() error = %v", err)
	}

	return fmt.Sprintf("ssh://git@%s:%s/repository.git", host, port), address
}

func newSSHSigner(t *testing.T) cryptossh.Signer {
	t.Helper()

	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey() error = %v", err)
	}

	signer, err := cryptossh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatalf("ssh.NewSignerFromKey() error = %v", err)
	}

	return signer
}

// newSSHPrivateKeyPEM generates a fresh ed25519 key and PEM-encodes it
// (PKCS#8) for use as client key material with functions that build their
// own transport.AuthMethod from a private key string, such as
// CloneOrUpdateBareMirror.
func newSSHPrivateKeyPEM(t *testing.T) string {
	t.Helper()

	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey() error = %v", err)
	}

	der, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("x509.MarshalPKCS8PrivateKey() error = %v", err)
	}

	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}
