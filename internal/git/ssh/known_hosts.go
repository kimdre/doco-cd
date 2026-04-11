package ssh

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
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

var (
	KnownHostsFilePath   = filepath.Join(os.TempDir(), "known_hosts")
	fetchHostPublicKeyFn = fetchHostPublicKey
)

type sshEndpoint struct {
	host string
	port string
}

func (e sshEndpoint) dialAddress() string {
	return net.JoinHostPort(e.host, e.port)
}

// fetchHostPublicKey connects to the SSH server and returns its public key.

func fetchHostPublicKey(endpoint sshEndpoint) (ssh.PublicKey, error) {
	addr := endpoint.dialAddress()

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to %s: %w", addr, err)
	}

	defer func() { _ = conn.Close() }()

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

		defer func() { _ = file.Close() }()
	}

	return os.Setenv(KnownHostsEnvVar, KnownHostsFilePath)
}

// formatKnownHostLine returns a known_hosts formatted line for the given address and key.
func formatKnownHostLine(address string, key ssh.PublicKey) string {
	return knownhosts.Line([]string{address}, key)
}

// knownHostsContainsEndpoint checks if the known_hosts content contains an entry for the given SSH endpoint.
func knownHostsContainsEndpoint(content string, endpoint sshEndpoint) bool {
	target := knownhosts.Normalize(endpoint.dialAddress())

	for _, rawLine := range strings.Split(content, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		hostsFieldIndex := 0

		if strings.HasPrefix(fields[0], "@") {
			if len(fields) < 3 {
				continue
			}

			hostsFieldIndex = 1
		}

		for _, hostEntry := range strings.Split(fields[hostsFieldIndex], ",") {
			if hostEntry == target {
				return true
			}
		}
	}

	return false
}

// rewriteKnownHostsWithoutEndpoint removes any entries for the given SSH endpoint from the known_hosts content.
func rewriteKnownHostsWithoutEndpoint(content string, endpoint sshEndpoint) (string, bool) {
	target := knownhosts.Normalize(endpoint.dialAddress())
	removed := false

	updatedLines := make([]string, 0)

	for _, rawLine := range strings.Split(content, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "#") {
			updatedLines = append(updatedLines, rawLine)
			continue
		}

		fields := strings.Fields(rawLine)
		if len(fields) < 2 {
			updatedLines = append(updatedLines, rawLine)
			continue
		}

		hostsFieldIndex := 0

		if strings.HasPrefix(fields[0], "@") {
			if len(fields) < 3 {
				updatedLines = append(updatedLines, rawLine)
				continue
			}

			hostsFieldIndex = 1
		}

		hostEntries := strings.Split(fields[hostsFieldIndex], ",")

		filteredHosts := make([]string, 0, len(hostEntries))
		for _, hostEntry := range hostEntries {
			if hostEntry == target {
				removed = true
				continue
			}

			filteredHosts = append(filteredHosts, hostEntry)
		}

		if len(filteredHosts) == 0 {
			continue
		}

		if len(filteredHosts) != len(hostEntries) {
			fields[hostsFieldIndex] = strings.Join(filteredHosts, ",")
			updatedLines = append(updatedLines, strings.Join(fields, " "))

			continue
		}

		updatedLines = append(updatedLines, rawLine)
	}

	return strings.Join(updatedLines, "\n"), removed
}

func overwriteKnownHosts(content string) error {
	if content != "" {
		content += "\n"
	}

	if err := os.WriteFile(KnownHostsFilePath, []byte(content), filesystem.PermOwner); err != nil { // #nosec G304
		return fmt.Errorf("failed to write known_hosts: %w", err)
	}

	return nil
}

// IsHostKeyMismatchError returns true when an SSH operation failed due to host key mismatch.
func IsHostKeyMismatchError(err error) bool {
	if err == nil {
		return false
	}

	message := strings.ToLower(err.Error())

	return strings.Contains(message, "knownhosts: key mismatch")
}

// addHostToKnownHosts retrieves the host key and adds it to known_hosts.
func addHostToKnownHosts(endpoint sshEndpoint) error {
	// Skip probing if we already have an entry for this endpoint.
	content, err := os.ReadFile(KnownHostsFilePath) // #nosec G304
	if err != nil {
		return fmt.Errorf("failed to read known_hosts: %w", err)
	}

	if knownHostsContainsEndpoint(string(content), endpoint) {
		return nil
	}

	serverKey, err := fetchHostPublicKeyFn(endpoint)
	if err != nil {
		return err
	}

	knownHostLine := formatKnownHostLine(endpoint.dialAddress(), serverKey)

	if strings.Contains(string(content), knownHostLine) {
		return nil // Host already exists
	}

	f, err := os.OpenFile(KnownHostsFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, filesystem.PermOwner) // #nosec G304
	if err != nil {
		return fmt.Errorf("failed to open known_hosts: %w", err)
	}

	defer func() { _ = f.Close() }()

	if _, err := f.WriteString(knownHostLine + "\n"); err != nil {
		return fmt.Errorf("failed to write to known_hosts: %w", err)
	}

	return os.Setenv(KnownHostsEnvVar, KnownHostsFilePath)
}

// RefreshKnownHost replaces the known_hosts entry for the given SSH URL and fetches the current server key.
func RefreshKnownHost(url string) error {
	if err := createKnownHostsFile(); err != nil {
		return fmt.Errorf("failed to create known_hosts file: %w", err)
	}

	endpoint, err := extractHostAndPortFromSSHUrl(url)
	if err != nil {
		return fmt.Errorf("failed to extract host from SSH URL: %w", err)
	}

	content, err := os.ReadFile(KnownHostsFilePath) // #nosec G304
	if err != nil {
		return fmt.Errorf("failed to read known_hosts: %w", err)
	}

	updatedContent, removed := rewriteKnownHostsWithoutEndpoint(string(content), endpoint)
	if removed {
		if err = overwriteKnownHosts(updatedContent); err != nil {
			return err
		}
	}

	if err = addHostToKnownHosts(endpoint); err != nil {
		return fmt.Errorf("failed to refresh host in known_hosts: %w", err)
	}

	return nil
}

// extractHostAndPortFromSSHUrl extracts the host and port from an SSH URL.
func extractHostAndPortFromSSHUrl(sshUrl string) (sshEndpoint, error) {
	if strings.HasPrefix(sshUrl, "ssh://") {
		u, err := url.Parse(sshUrl)
		if err != nil {
			return sshEndpoint{}, err
		}

		host := u.Hostname()
		if host == "" {
			return sshEndpoint{}, errors.New("invalid SSH URL: missing host")
		}

		port := u.Port()
		if port == "" {
			port = strconv.Itoa(DefaultGitSSHPort)
		}

		return sshEndpoint{host: host, port: port}, nil
	}

	// Handle [user@]host:path format
	atIndex := strings.Index(sshUrl, "@")

	colonIndex := strings.Index(sshUrl, ":")
	if colonIndex == -1 {
		return sshEndpoint{}, errors.New("invalid SSH URL: missing ':' after host")
	}

	var host string

	switch {
	case atIndex == -1:
		// host:path
		host = sshUrl[:colonIndex]
	case atIndex < colonIndex:
		// user@host:path
		host = sshUrl[atIndex+1 : colonIndex]
	default:
		return sshEndpoint{}, errors.New("invalid SSH URL format")
	}

	return sshEndpoint{host: host, port: strconv.Itoa(DefaultGitSSHPort)}, nil
}

// AddToKnownHosts adds the host from the SSH URL to the known_hosts file.
func AddToKnownHosts(url string) error {
	err := createKnownHostsFile()
	if err != nil {
		return fmt.Errorf("failed to create known_hosts file: %w", err)
	}

	endpoint, err := extractHostAndPortFromSSHUrl(url)
	if err != nil {
		return fmt.Errorf("failed to extract host from SSH URL: %w", err)
	}

	return addHostToKnownHosts(endpoint)
}
