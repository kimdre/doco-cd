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

			err := CreateKnownHostsFile()
			if err != nil {
				t.Fatalf("Failed to create known_hosts file: %v", err)
			}

			err = AddHostToKnownHosts(tc.host)
			if (err != nil) != tc.wantErr {
				t.Errorf("AddHostToKnownHosts(%q) error = %v, wantErr %v", tc.host, err, tc.wantErr)
			}

			// Get the known_hosts file content
			data, readErr := os.ReadFile(KnownHostsFilePath)
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
