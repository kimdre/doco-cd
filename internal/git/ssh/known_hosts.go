package ssh

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/kimdre/doco-cd/internal/filesystem"
)

const (
	KnownHostsEnvVar  = "SSH_KNOWN_HOSTS"
	DefaultGitSSHUser = "git" // Default SSH user for Git servers
	DefaultGitSSHPort = 22    // Default SSH port for Git servers
)

var KnownHostsFilePath = filepath.Join(os.TempDir(), "known_hosts")

// fetchHostPublicKey connects to the SSH server and returns its public key.
func fetchHostPublicKey(host string) (ssh.PublicKey, error) {
	addr := host
	if !strings.Contains(host, ":") {
		addr = host + fmt.Sprintf(":%d", DefaultGitSSHPort)
	}

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to %s: %w", addr, err)
	}
	defer conn.Close() // nolint:errcheck

	hostKeyCallback, err := knownhosts.New(KnownHostsFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to create hostkey callback: %w", err)
	}

	var serverKey ssh.PublicKey

	sshConn, _, _, err := ssh.NewClientConn(conn, addr, &ssh.ClientConfig{
		User: DefaultGitSSHUser,
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			serverKey = key
			return hostKeyCallback(hostname, remote, key)
		},
		Timeout: 5 * time.Second,
	})
	if err != nil && serverKey == nil {
		return nil, fmt.Errorf("failed to get host key: %w", err)
	}

	if sshConn != nil {
		err = sshConn.Close()
		if err != nil {
			return nil, fmt.Errorf("failed to close ssh connection: %w", err)
		}
	}

	return serverKey, nil
}

// createKnownHostsFile ensures that the known_hosts file exists
// and sets the SSH_KNOWN_HOSTS environment variable.
func createKnownHostsFile() error {
	if _, err := os.Stat(KnownHostsFilePath); errors.Is(err, os.ErrNotExist) {
		file, err := os.Create(KnownHostsFilePath) // #nosec G304
		if err != nil {
			return fmt.Errorf("failed to create known_hosts file: %w", err)
		}
		defer file.Close() // nolint:errcheck
	}

	return os.Setenv(KnownHostsEnvVar, KnownHostsFilePath)
}

// formatKnownHostLine returns a known_hosts formatted line for the given host and key.
func formatKnownHostLine(host string, key ssh.PublicKey) string {
	return fmt.Sprintf("%s %s %s", host, key.Type(), base64.StdEncoding.EncodeToString(key.Marshal()))
}

// addHostToKnownHosts retrieves the host key and adds it to known_hosts.
func addHostToKnownHosts(host string) error {
	serverKey, err := fetchHostPublicKey(host)
	if err != nil {
		return err
	}

	knownHostLine := formatKnownHostLine(host, serverKey)

	// Check if the host is already in known_hosts
	content, err := os.ReadFile(KnownHostsFilePath) // #nosec G304
	if err != nil {
		return fmt.Errorf("failed to read known_hosts: %w", err)
	}

	if strings.Contains(string(content), knownHostLine) {
		return nil // Host already exists
	}

	f, err := os.OpenFile(KnownHostsFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, filesystem.PermOwner) // #nosec G304
	if err != nil {
		return fmt.Errorf("failed to open known_hosts: %w", err)
	}
	defer f.Close() // nolint:errcheck

	if _, err := f.WriteString(knownHostLine + "\n"); err != nil {
		return fmt.Errorf("failed to write to known_hosts: %w", err)
	}

	return os.Setenv(KnownHostsEnvVar, KnownHostsFilePath)
}

// extractHostFromSSHUrl extracts the host/domain from an SSH URL.
func extractHostFromSSHUrl(sshUrl string) (string, error) {
	if strings.HasPrefix(sshUrl, "ssh://") {
		u, err := url.Parse(sshUrl)
		if err != nil {
			return "", err
		}

		if u.Host == "" {
			return "", errors.New("invalid SSH URL: missing host")
		}

		host := u.Host
		// Remove port if present
		if idx := strings.Index(host, ":"); idx != -1 {
			host = host[:idx]
		}

		return host, nil
	}

	// Handle [user@]host:path format
	atIndex := strings.Index(sshUrl, "@")

	colonIndex := strings.Index(sshUrl, ":")
	if colonIndex == -1 {
		return "", errors.New("invalid SSH URL: missing ':' after host")
	}

	if atIndex != -1 && atIndex < colonIndex {
		// user@host:path
		host := sshUrl[atIndex+1 : colonIndex]
		return host, nil
	} else if atIndex == -1 {
		// host:path
		host := sshUrl[:colonIndex]
		return host, nil
	}

	return "", errors.New("invalid SSH URL format")
}

// AddToKnownHosts adds the host from the SSH URL to the known_hosts file.
func AddToKnownHosts(url string) error {
	err := createKnownHostsFile()
	if err != nil {
		return fmt.Errorf("failed to create known_hosts file: %w", err)
	}

	host, err := extractHostFromSSHUrl(url)
	if err != nil {
		return fmt.Errorf("failed to extract host from SSH URL: %w", err)
	}

	return addHostToKnownHosts(host)
}
