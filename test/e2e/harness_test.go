//go:build e2e

package e2e

import (
	"slices"
	"strings"
	"testing"
)

func TestHarnessContainerNameUsesReadablePrefix(t *testing.T) {
	h := &Harness{workDir: "/tmp/doco-cd-e2e-failed-deploy-retry-123456"}

	if got := h.containerName("doco-cd"); got != "doco-cd-e2e-doco-cd-failed-deploy-retry-123456" {
		t.Fatalf("containerName(doco-cd) = %q", got)
	}

	if got := h.containerName("gitserver"); got != "doco-cd-e2e-gitserver-failed-deploy-retry-123456" {
		t.Fatalf("containerName(gitserver) = %q", got)
	}
}

func TestGitServerImageUsesReadablePrefix(t *testing.T) {
	image, tag, found := strings.Cut(gitServerImageName(), ":")
	if !found {
		t.Fatalf("gitserver image %q has no tag", image)
	}

	if image != "doco-cd-e2e-gitserver" {
		t.Fatalf("gitserver image repository = %q", image)
	}

	if tag == "" {
		t.Fatal("gitserver image tag must be non-empty")
	}
}

func TestDaemonImageUsesReadablePrefix(t *testing.T) {
	image, tag, found := strings.Cut(daemonImageName(), ":")
	if !found {
		t.Fatalf("daemon image %q has no tag", image)
	}

	if image != "doco-cd-e2e" {
		t.Fatalf("daemon image repository = %q", image)
	}

	if tag == "" {
		t.Fatal("daemon image tag must be non-empty")
	}
}

func TestDaemonBuildArgs(t *testing.T) {
	t.Setenv("E2E_BUILD_CACHE_SCOPE", "")

	tag := "doco-cd-e2e:test"
	binDir := "/tmp/doco-cd-e2e-bin-123"

	if got, want := daemonBuildArgs(tag, binDir), []string{
		"build", "-t", tag,
		"--provenance", "false",
		"--build-arg", "DISABLE_BITWARDEN=true",
		"--build-context", "build=" + binDir,
		repoDir,
	}; !slices.Equal(got, want) {
		t.Fatalf("daemonBuildArgs() = %v, want %v", got, want)
	}

	t.Setenv("E2E_BUILD_CACHE_SCOPE", "e2e-standalone")

	if got, want := daemonBuildArgs(tag, binDir), []string{
		"buildx", "build", "--load",
		"--cache-from", "type=gha,scope=e2e-standalone",
		"--cache-to", "type=gha,mode=max,scope=e2e-standalone",
		"-t", tag,
		"--provenance", "false",
		"--build-arg", "DISABLE_BITWARDEN=true",
		"--build-context", "build=" + binDir,
		repoDir,
	}; !slices.Equal(got, want) {
		t.Fatalf("daemonBuildArgs() = %v, want %v", got, want)
	}
}

func TestLogsSince(t *testing.T) {
	const logs = "first\nsecond\nthird\n"

	tests := []struct {
		name   string
		offset int
		want   string
	}{
		{name: "start", offset: 0, want: logs},
		{name: "negative", offset: -1, want: logs},
		{name: "middle", offset: len("first\n"), want: "second\nthird\n"},
		{name: "end", offset: len(logs), want: ""},
		{name: "past end", offset: len(logs) + 1, want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := logsSince(logs, tc.offset); got != tc.want {
				t.Fatalf("logsSince() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestImageReferenceMatches(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
		ok   bool
	}{
		{name: "exact tag", got: "alpine:3.22", want: "alpine:3.22", ok: true},
		{name: "swarm resolved tag", got: "alpine:3.22@sha256:abc", want: "alpine:3.22", ok: true},
		{name: "different tag", got: "alpine:3.21@sha256:abc", want: "alpine:3.22"},
		{name: "exact digest", got: "alpine:latest@sha256:abc", want: "alpine:latest@sha256:abc", ok: true},
		{name: "different digest", got: "alpine:latest@sha256:def", want: "alpine:latest@sha256:abc"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := imageReferenceMatches(tc.got, tc.want); got != tc.ok {
				t.Fatalf("imageReferenceMatches(%q, %q) = %t, want %t", tc.got, tc.want, got, tc.ok)
			}
		})
	}
}
