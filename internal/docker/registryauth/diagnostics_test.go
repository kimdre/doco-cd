package registryauth

import (
	"errors"
	"strings"
	"testing"

	"github.com/docker/cli/cli/config/configfile"
	configtypes "github.com/docker/cli/cli/config/types"
)

func TestIsAuthRelatedError(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		err  error
		want bool
	}{
		{err: nil, want: false},
		{err: errors.New("network timeout"), want: false},
		{err: errors.New("pull access denied for private/app"), want: true},
		{err: errors.New("Error response from daemon: unauthorized"), want: true},
		{err: errors.New("error getting credentials - err: exit status 1"), want: true},
	}

	for _, tc := range testCases {
		if got := IsAuthRelatedError(tc.err); got != tc.want {
			t.Fatalf("IsAuthRelatedError(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestBuildFailureHint(t *testing.T) {
	t.Parallel()

	cfg := &configfile.ConfigFile{
		Filename:         "/root/.docker/config.json",
		CredentialsStore: "__missing_helper__",
	}

	hint := BuildFailureHint(cfg, []string{"ghcr.io/example/private:latest"}, errors.New("pull access denied"))
	if !strings.Contains(hint, "registry auth hint:") {
		t.Fatalf("BuildFailureHint() missing prefix: %q", hint)
	}

	if !strings.Contains(hint, "docker config path: /root/.docker/config.json") {
		t.Fatalf("BuildFailureHint() missing config path hint: %q", hint)
	}

	if !strings.Contains(hint, "missing helper binaries in container:") {
		t.Fatalf("BuildFailureHint() missing helper-binary hint: %q", hint)
	}

	if !strings.Contains(hint, "helper-based credentials require matching docker-credential-* binaries") {
		t.Fatalf("BuildFailureHint() missing helper requirement hint: %q", hint)
	}
}

func TestBuildFailureHint_NoAuthError(t *testing.T) {
	t.Parallel()

	cfg := &configfile.ConfigFile{
		AuthConfigs: map[string]configtypes.AuthConfig{
			"ghcr.io": {Username: "u", Password: "p"},
		},
	}

	if hint := BuildFailureHint(cfg, []string{"ghcr.io/example/private:latest"}, errors.New("connection reset by peer")); hint != "" {
		t.Fatalf("BuildFailureHint() = %q, want empty hint", hint)
	}
}

func TestWrapLookupError(t *testing.T) {
	t.Parallel()

	cfg := &configfile.ConfigFile{
		Filename:         "/root/.docker/config.json",
		CredentialsStore: "__missing_helper__",
	}

	err := WrapLookupError(cfg, "ghcr.io/example/private:latest", errors.New("error getting credentials"))
	if err == nil {
		t.Fatal("WrapLookupError() returned nil error")
	}

	if !strings.Contains(err.Error(), "retrieve auth token from image") {
		t.Fatalf("WrapLookupError() missing base context: %v", err)
	}

	if !strings.Contains(err.Error(), "registry auth hint:") {
		t.Fatalf("WrapLookupError() missing hint: %v", err)
	}
}
