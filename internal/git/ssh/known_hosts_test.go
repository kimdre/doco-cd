package ssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestAddToKnownHosts(t *testing.T) {
	host, port := startTestSSHServer(t)

	originalKnownHostsFilePath := KnownHostsFilePath
	KnownHostsFilePath = filepath.Join(t.TempDir(), "known_hosts_test")
	t.Cleanup(func() {
		KnownHostsFilePath = originalKnownHostsFilePath
	})

	testCases := []struct {
		name         string
		url          string
		expectedHost string
		wantErr      bool
	}{
		{
			name:         "SSH URL with non-default port",
			url:          fmt.Sprintf("ssh://git@%s:%s/admin/test.git", host, port),
			expectedHost: knownhosts.Normalize(net.JoinHostPort(host, port)),
			wantErr:      false,
		},
		{
			name:         "Invalid host",
			url:          "ssh://git@invalid.host.example:2222/admin/test.git",
			expectedHost: "",
			wantErr:      true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := AddToKnownHosts(tc.url)
			if (err != nil) != tc.wantErr {
				t.Fatalf("AddToKnownHosts(%q) error = %v, wantErr %v", tc.url, err, tc.wantErr)
			}

			// Get the known_hosts file content
			data, readErr := os.ReadFile(KnownHostsFilePath) // #nosec G304
			if readErr != nil {
				t.Fatalf("Failed to read known_hosts file: %v", readErr)
			}

			content := string(data)
			// Check size of content based on expectation
			if tc.wantErr {
				if strings.TrimSpace(content) != "" {
					t.Errorf("Expected known_hosts to be empty for invalid host, got: %q", content)
				}
			} else {
				if strings.TrimSpace(content) == "" {
					t.Errorf("Expected known_hosts to contain entry for valid host, got empty content")
				}

				if !strings.Contains(content, tc.expectedHost+" ") {
					t.Fatalf("Expected known_hosts to contain %q, got %q", tc.expectedHost, content)
				}

				if strings.Contains(content, host+" ") {
					t.Fatalf("Expected non-default port entry to be written with known_hosts port syntax, got %q", content)
				}
			}

			_ = os.WriteFile(KnownHostsFilePath, nil, 0o600)
		})
	}
}

func TestAddToKnownHosts_SkipsProbeWhenEndpointAlreadyExists(t *testing.T) {
	host, port, stopServer := startTestSSHServerWithStop(t)

	originalKnownHostsFilePath := KnownHostsFilePath
	KnownHostsFilePath = filepath.Join(t.TempDir(), "known_hosts_test")
	t.Cleanup(func() {
		KnownHostsFilePath = originalKnownHostsFilePath
	})

	url := fmt.Sprintf("ssh://git@%s:%s/admin/test.git", host, port)

	if err := AddToKnownHosts(url); err != nil {
		t.Fatalf("first AddToKnownHosts(%q) failed: %v", url, err)
	}

	// Stop the server to prove the second call does not need another probe.
	stopServer()

	if err := AddToKnownHosts(url); err != nil {
		t.Fatalf("second AddToKnownHosts(%q) should not re-probe known endpoint, got: %v", url, err)
	}
}

func TestIsHostKeyMismatchError(t *testing.T) {
	testCases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil error", err: nil, want: false},
		{name: "mismatch error", err: errors.New("ssh: handshake failed: knownhosts: key mismatch"), want: true},
		{name: "unknown key error", err: errors.New("ssh: handshake failed: knownhosts: key is unknown"), want: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsHostKeyMismatchError(tc.err); got != tc.want {
				t.Fatalf("IsHostKeyMismatchError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestRefreshKnownHost_ReplacesEndpointEntry(t *testing.T) {
	originalKnownHostsFilePath := KnownHostsFilePath
	originalFetchHostPublicKeyFn := fetchHostPublicKeyFn
	KnownHostsFilePath = filepath.Join(t.TempDir(), "known_hosts_test")
	t.Cleanup(func() {
		KnownHostsFilePath = originalKnownHostsFilePath
		fetchHostPublicKeyFn = originalFetchHostPublicKeyFn
	})

	if err := createKnownHostsFile(); err != nil {
		t.Fatalf("createKnownHostsFile failed: %v", err)
	}

	endpoint := sshEndpoint{host: "example.com", port: "2222"}
	otherEndpoint := sshEndpoint{host: "other.example.com", port: "22"}

	oldKey := newTestPublicKey(t)
	newKey := newTestPublicKey(t)

	oldEndpointLine := formatKnownHostLine(endpoint.dialAddress(), oldKey)
	otherLine := formatKnownHostLine(otherEndpoint.dialAddress(), oldKey)
	initialContent := strings.Join([]string{oldEndpointLine, otherLine}, "\n") + "\n"

	if err := os.WriteFile(KnownHostsFilePath, []byte(initialContent), 0o600); err != nil { // #nosec G306
		t.Fatalf("failed to seed known_hosts: %v", err)
	}

	fetchHostPublicKeyFn = func(gotEndpoint sshEndpoint) (ssh.PublicKey, error) {
		if gotEndpoint != endpoint {
			t.Fatalf("fetchHostPublicKey called with unexpected endpoint: %+v", gotEndpoint)
		}

		return newKey, nil
	}

	if err := RefreshKnownHost("ssh://git@example.com:2222/admin/test.git"); err != nil {
		t.Fatalf("RefreshKnownHost failed: %v", err)
	}

	data, err := os.ReadFile(KnownHostsFilePath) // #nosec G304
	if err != nil {
		t.Fatalf("failed to read known_hosts: %v", err)
	}

	content := string(data)
	newEndpointLine := formatKnownHostLine(endpoint.dialAddress(), newKey)

	if strings.Contains(content, oldEndpointLine) {
		t.Fatalf("expected old endpoint key line to be removed, got %q", content)
	}

	if !strings.Contains(content, newEndpointLine) {
		t.Fatalf("expected new endpoint key line to be present, got %q", content)
	}

	if !strings.Contains(content, otherLine) {
		t.Fatalf("expected unrelated known_hosts entry to stay intact, got %q", content)
	}

	if got := strings.Count(content, knownhosts.Normalize(endpoint.dialAddress())+" "); got != 1 {
		t.Fatalf("expected one entry for refreshed endpoint, got %d in %q", got, content)
	}
}

func TestExtractHostAndPortFromSSHUrl(t *testing.T) {
	testCases := []struct {
		sshURL       string
		expectedHost string
		expectedPort string
		wantErr      bool
	}{
		{"git@github.com:user/repo.git", "github.com", "22", false},
		{"ssh://git@github.com/user/repo.git", "github.com", "22", false},
		{"ssh://git@gitea/user/repo.git", "gitea", "22", false},
		{"ssh://github.com/user/repo.git", "github.com", "22", false},
		{"ssh://gitea/user/repo.git", "gitea", "22", false},
		{"ssh://git@github.com:2222/user/repo.git", "github.com", "2222", false},
		{"ssh://git@gitea:2222/user/repo.git", "gitea", "2222", false},
		{"ssh://git@[2001:db8::1]:2222/user/repo.git", "2001:db8::1", "2222", false},
		{"github.com:user/repo.git", "github.com", "22", false},
		{"invalid-url", "", "", true},
	}

	for _, tc := range testCases {
		t.Run(tc.sshURL, func(t *testing.T) {
			endpoint, err := extractHostAndPortFromSSHUrl(tc.sshURL)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Expected error for invalid URL %q, got endpoint %+v", tc.sshURL, endpoint)
				}

				return
			}

			if err != nil {
				t.Fatalf("Unexpected error for URL %q: %v", tc.sshURL, err)
			}

			if endpoint.host != tc.expectedHost {
				t.Errorf("Extracted host = %q, want %q", endpoint.host, tc.expectedHost)
			}

			if endpoint.port != tc.expectedPort {
				t.Errorf("Extracted port = %q, want %q", endpoint.port, tc.expectedPort)
			}
		})
	}
}

func startTestSSHServer(t *testing.T) (string, string) {
	host, port, _ := startTestSSHServerWithStop(t)

	return host, port
}

func startTestSSHServerWithStop(t *testing.T) (string, string, func()) {
	t.Helper()

	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("Failed to generate SSH host key: %v", err)
	}

	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatalf("Failed to create SSH signer: %v", err)
	}

	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to start test SSH server: %v", err)
	}

	stopServer := func() {
		_ = listener.Close()
	}
	t.Cleanup(stopServer)

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			go func(conn net.Conn) {
				defer func() { _ = conn.Close() }()

				serverConn, chans, reqs, err := ssh.NewServerConn(conn, config)
				if err != nil {
					return
				}

				defer func() { _ = serverConn.Close() }()

				go ssh.DiscardRequests(reqs)

				for newChannel := range chans {
					_ = newChannel.Reject(ssh.Prohibited, "test server does not accept channels")
				}
			}(conn)
		}
	}()

	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("Failed to parse test SSH server address: %v", err)
	}

	return host, port, stopServer
}

func newTestPublicKey(t *testing.T) ssh.PublicKey {
	t.Helper()

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key pair: %v", err)
	}

	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("failed to create ssh public key: %v", err)
	}

	return key
}
