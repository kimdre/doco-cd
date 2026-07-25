package docker

import (
	"context"
	"path/filepath"
	"testing"
)

func TestHasIPv6NetworkWithoutExplicitSubnet(t *testing.T) {
	t.Parallel()

	type testCase struct {
		name     string
		compose  string
		expected bool
	}

	tests := []testCase{
		{
			name: "enable ipv6 without subnet",
			compose: `services:
  app:
    image: nginx:latest
    networks:
      - n1

networks:
  n1:
    enable_ipv6: true
`,
			expected: true,
		},
		{
			name: "enable ipv6 with subnet",
			compose: `services:
  app:
    image: nginx:latest
    networks:
      - n1

networks:
  n1:
    enable_ipv6: true
    ipam:
      config:
        - subnet: fde0:8c1:58a::/64
`,
			expected: false,
		},
		{
			name: "ipv4 only network",
			compose: `services:
  app:
    image: nginx:latest
    networks:
      - n1

networks:
  n1:
    enable_ipv6: false
`,
			expected: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tmpDir := t.TempDir()
			filePath := filepath.Join(tmpDir, "compose.yaml")
			createComposeFile(t, filePath, tc.compose)

			project, err := LoadCompose(context.Background(), tmpDir, tmpDir, "test-stack", []string{filePath}, nil, nil, map[string]string{})
			if err != nil {
				t.Fatalf("LoadCompose() error = %v", err)
			}

			if got := hasIPv6NetworkWithoutExplicitSubnet(project); got != tc.expected {
				t.Fatalf("hasIPv6NetworkWithoutExplicitSubnet() = %v, want %v", got, tc.expected)
			}
		})
	}
}
