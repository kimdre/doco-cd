package docker

import (
	"testing"

	"github.com/kimdre/doco-cd/internal/webhook"
)

func TestSourceURLLabelValue(t *testing.T) {
	t.Parallel()

	gitPayload := &webhook.ParsedPayload{
		Source:   webhook.PayloadSourceGit,
		CloneURL: "ssh://git@example.com/owner/config.git",
		WebURL:   "https://example.com/owner/config",
	}
	if got := sourceURLLabelValue(gitPayload); got != gitPayload.CloneURL {
		t.Fatalf("expected resolved clone URL, got %q", got)
	}

	ociPayload := &webhook.ParsedPayload{
		Source:   webhook.PayloadSourceOCI,
		Artifact: "registry.example.com/owner/config:main",
		WebURL:   "https://registry.example.com/owner/config",
	}
	if got := sourceURLLabelValue(ociPayload); got != ociPayload.Artifact {
		t.Fatalf("expected OCI artifact URL, got %q", got)
	}
}
