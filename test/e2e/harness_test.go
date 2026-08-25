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
	if got, want := daemonBuildArgs(tag), []string{
		"build", "-t", tag, "--build-arg", "DISABLE_BITWARDEN=true", repoDir,
	}; !slices.Equal(got, want) {
		t.Fatalf("daemonBuildArgs() = %v, want %v", got, want)
	}

	t.Setenv("E2E_BUILD_CACHE_SCOPE", "e2e-standalone")

	if got, want := daemonBuildArgs(tag), []string{
		"buildx", "build", "--load",
		"--cache-from", "type=gha,scope=e2e-standalone",
		"--cache-to", "type=gha,mode=max,scope=e2e-standalone",
		"-t", tag, "--build-arg", "DISABLE_BITWARDEN=true", repoDir,
	}; !slices.Equal(got, want) {
		t.Fatalf("daemonBuildArgs() = %v, want %v", got, want)
	}
}
