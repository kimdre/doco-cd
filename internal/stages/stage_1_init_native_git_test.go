package stages

import (
	"testing"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
)

func TestDestroySourceIdentity_NativeGit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		url  string
		name string
	}{
		{url: "git://example.com/org/repo.git", name: "example.com/org/repo"},
		{url: "GIT://example.com/org/repo.git", name: "example.com/org/repo"},
		{url: "git://example.com:19418/org/repo.git", name: "example.com:19418/org/repo"},
		{url: "git://example.com"},
		{url: "git:///org/repo.git"},
		{url: "git://user:password@example.com/org/repo.git"}, // #nosec G101 -- Invalid URL fixture, not credentials.
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			t.Parallel()

			source, name := destroySourceIdentity(tt.url)
			if name != tt.name {
				t.Fatalf("destroySourceIdentity() name = %q, want %q", name, tt.name)
			}

			if tt.name != "" && source != config.SourceTypeGit {
				t.Fatalf("destroySourceIdentity() source = %q, want Git", source)
			}

			if tt.name == "" && source != "" {
				t.Fatalf("invalid native Git URL produced source %q", source)
			}
		})
	}
}

func TestDestroySourceIdentity_OCIDigest(t *testing.T) {
	t.Parallel()

	source, name := destroySourceIdentity("ghcr.io/org/repo@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if source != config.SourceTypeOCI || name != "ghcr.io/org/repo" {
		t.Fatalf("destroySourceIdentity() = %q, %q, want OCI repository identity", source, name)
	}
}

func TestServicesBelongToRepository_NativeGit(t *testing.T) {
	t.Parallel()

	repository := &RepositoryData{
		Source:          config.SourceTypeGit,
		Name:            "example.com/org/repo",
		SourceUrl:       "git://example.com/org/repo.git",
		ConfigSourceUrl: "git://example.com/org/repo.git",
	}

	tests := []struct {
		url        string
		sourceType string
		want       bool
	}{
		{url: repository.SourceUrl, sourceType: "git", want: true},
		{url: "git://foreign.example.com/org/repo.git", sourceType: "git"},
		{url: "git://example.com:19418/org/repo.git", sourceType: "git"},
		{url: repository.SourceUrl, sourceType: "oci"},
		{url: "git://example.com", sourceType: "git"},
	}

	for _, tt := range tests {
		t.Run(tt.url+"/"+tt.sourceType, func(t *testing.T) {
			t.Parallel()

			services := map[docker.Service]docker.Labels{
				"web": {
					docker.DocoCDLabels.Source.Name: repository.Name,
					docker.DocoCDLabels.Source.URL:  tt.url,
					docker.DocoCDLabels.Source.Type: tt.sourceType,
				},
			}
			if got := servicesBelongToRepository(services, repository); got != tt.want {
				t.Fatalf("servicesBelongToRepository() = %t, want %t", got, tt.want)
			}
		})
	}
}
