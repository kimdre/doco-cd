package docker

import (
	"testing"
)

func TestExtractOciArtifactTag(t *testing.T) {
	tests := []struct {
		name      string
		reference string
		want      string
	}{
		{
			name:      "OCI artifact with tag",
			reference: "ghcr.io/kimdre/doco-cd_tests:main",
			want:      "main",
		},
		{
			name:      "OCI artifact with version tag",
			reference: "ghcr.io/kimdre/doco-cd_tests:v1.0.0",
			want:      "v1.0.0",
		},
		{
			name:      "OCI artifact without tag (digest only)",
			reference: "ghcr.io/kimdre/doco-cd_tests@sha256:1234567890",
			want:      "1234567890",
		},
		{
			name:      "Git reference with heads (kept as-is)",
			reference: "refs/heads/main",
			want:      "refs/heads/main",
		},
		{
			name:      "Git reference with tags (kept as-is)",
			reference: "refs/tags/v1.0.0",
			want:      "refs/tags/v1.0.0",
		},
		{
			name:      "Git branch with slashes (kept as-is)",
			reference: "feat/app/this",
			want:      "feat/app/this",
		},
		{
			name:      "Git version tag (kept as-is)",
			reference: "v1.0.0-rc.1",
			want:      "v1.0.0-rc.1",
		},
		{
			name:      "Docker Hub without tag (kept as-is)",
			reference: "my/app",
			want:      "my/app",
		},
		{
			name:      "Docker Hub with tag",
			reference: "my/app:latest",
			want:      "latest",
		},
		{
			name:      "Simple git reference",
			reference: "main",
			want:      "main",
		},
		{
			name:      "Empty reference",
			reference: "",
			want:      "",
		},
		{
			name:      "Whitespace reference",
			reference: "  ",
			want:      "",
		},
		{
			name:      "Complex OCI reference with registry port",
			reference: "registry.example.com:5000/namespace/repo:develop",
			want:      "develop",
		},
		{
			name:      "Nested path with tag",
			reference: "gcr.io/my-project/my-image:1.2.3",
			want:      "1.2.3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractOciArtifactTag(tt.reference)
			if got != tt.want {
				t.Errorf("ExtractOciArtifactTag(%q) = %q, want %q", tt.reference, got, tt.want)
			}
		})
	}
}

// TestSourceCloneURLFromLabels verifies that the resolved-clone-URL label is
// preferred over the browsable Source.URL label, and that containers deployed
// before the CloneURL label existed (i.e. carrying only Source.URL) still
// resolve correctly. Regression coverage for
// https://github.com/kimdre/doco-cd/issues/1850.
func TestSourceCloneURLFromLabels(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{
			name: "prefers clone url label when both are present and diverge",
			labels: map[string]string{
				DocoCDLabels.Source.URL:      "https://git.example.com/owner/repo",
				DocoCDLabels.Source.CloneURL: "ssh://git@gits.example.com:222/owner/repo.git",
			},
			want: "ssh://git@gits.example.com:222/owner/repo.git",
		},
		{
			name: "falls back to url label when clone url label is absent",
			labels: map[string]string{
				DocoCDLabels.Source.URL: "https://git.example.com/owner/repo",
			},
			want: "https://git.example.com/owner/repo",
		},
		{
			name:   "returns empty string when neither label is present",
			labels: map[string]string{},
			want:   "",
		},
		{
			name:   "returns empty string for nil labels",
			labels: nil,
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SourceCloneURLFromLabels(tt.labels); got != tt.want {
				t.Errorf("SourceCloneURLFromLabels(%v) = %q, want %q", tt.labels, got, tt.want)
			}
		})
	}
}
