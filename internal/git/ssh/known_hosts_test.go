package ssh

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAddHostToKnownHosts(t *testing.T) {
	testCases := []struct {
		name    string
		host    string
		wantErr bool
	}{
		{
			name:    "Valid host",
			host:    "github.com",
			wantErr: false,
		},
		{
			name:    "Invalid host",
			host:    "invalid.host.example",
			wantErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			KnownHostsFilePath = filepath.Join(t.TempDir(), "known_hosts_test")

			err := createKnownHostsFile()
			if err != nil {
				t.Fatalf("Failed to create known_hosts file: %v", err)
			}

			err = addHostToKnownHosts(tc.host)
			if (err != nil) != tc.wantErr {
				t.Errorf("addHostToKnownHosts(%q) error = %v, wantErr %v", tc.host, err, tc.wantErr)
			}

			// Get the known_hosts file content
			data, readErr := os.ReadFile(KnownHostsFilePath) // #nosec G304
			if readErr != nil {
				t.Fatalf("Failed to read known_hosts file: %v", readErr)
			}

			content := string(data)
			// Check size of content based on expectation
			if tc.wantErr {
				if len(content) != 0 {
					t.Errorf("Expected known_hosts to be empty for invalid host, got: %q", content)
				}
			} else {
				if len(content) == 0 {
					t.Errorf("Expected known_hosts to contain entry for valid host, got empty content")
				}
			}
		})
	}
}

func TestExtractHostFromSSHUrl(t *testing.T) {
	testCases := []struct {
		sshURL   string
		expected string
	}{
		{"git@github.com:user/repo.git", "github.com"},
		{"ssh://git@github.com/user/repo.git", "github.com"},
		{"ssh://github.com/user/repo.git", "github.com"},
		{"github.com:user/repo.git", "github.com"},
		{"invalid-url", ""},
	}

	for _, tc := range testCases {
		t.Run(tc.sshURL, func(t *testing.T) {
			host, err := extractHostFromSSHUrl(tc.sshURL)
			if tc.expected == "" {
				if err == nil {
					t.Errorf("Expected error for invalid URL %q, got host %q", tc.sshURL, host)
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error for URL %q: %v", tc.sshURL, err)
				}

				if host != tc.expected {
					t.Errorf("Extracted host = %q, want %q", host, tc.expected)
				}
			}
		})
	}
}
