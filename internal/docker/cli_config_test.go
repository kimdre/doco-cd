package docker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/config"
	"github.com/docker/cli/cli/config/configfile"
	configtypes "github.com/docker/cli/cli/config/types"
)

// staticConfigCli stands in for the underlying docker cli with its
// startup-cached config snapshot.
type staticConfigCli struct {
	command.Cli
	cfg *configfile.ConfigFile
}

func (s staticConfigCli) ConfigFile() *configfile.ConfigFile { return s.cfg }

func useDockerConfigDir(t *testing.T, dir string) {
	t.Helper()

	orig := config.Dir()

	config.SetDir(dir)
	t.Cleanup(func() { config.SetDir(orig) })
}

func writeDockerConfig(t *testing.T, dir, content string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, config.ConfigFileName), []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write docker config: %v", err)
	}
}

func TestReloadConfigCliPicksUpConfigChanges(t *testing.T) {
	dir := t.TempDir()
	useDockerConfigDir(t, dir)

	writeDockerConfig(t, dir, `{"auths":{"registry.example.com":{"auth":"dXNlcjp0b2tlbi1vbmU="}}}`)

	cli := reloadConfigCli{staticConfigCli{cfg: configfile.New("startup-snapshot")}}

	// LoadFromReader decodes the base64 `auth` field into Username/Password.
	if got := cli.ConfigFile().AuthConfigs["registry.example.com"].Password; got != "token-one" {
		t.Fatalf("expected password from initial config file, got %q", got)
	}

	// Simulate a credential refresh on disk (e.g. an ECR token rotation cron).
	writeDockerConfig(t, dir, `{"auths":{"registry.example.com":{"auth":"dXNlcjp0b2tlbi10d28="}}}`)

	if got := cli.ConfigFile().AuthConfigs["registry.example.com"].Password; got != "token-two" {
		t.Fatalf("expected password from refreshed config file, got %q", got)
	}
}

func TestReloadConfigCliFallsBackToSnapshotOnUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	useDockerConfigDir(t, dir)

	writeDockerConfig(t, dir, `{malformed`)

	snapshot := configfile.New("startup-snapshot")
	snapshot.AuthConfigs = map[string]configtypes.AuthConfig{
		"registry.example.com": {Auth: "c25hcHNob3Q="},
	}

	cli := reloadConfigCli{staticConfigCli{cfg: snapshot}}

	if got := cli.ConfigFile(); got != snapshot {
		t.Fatalf("expected fallback to the cached snapshot, got %+v", got)
	}
}

func TestReloadConfigCliMissingFileReturnsEmptyConfig(t *testing.T) {
	dir := t.TempDir()
	useDockerConfigDir(t, dir)

	snapshot := configfile.New("startup-snapshot")
	snapshot.AuthConfigs = map[string]configtypes.AuthConfig{
		"registry.example.com": {Auth: "c25hcHNob3Q="},
	}

	cli := reloadConfigCli{staticConfigCli{cfg: snapshot}}

	// Missing file is not an error: the daemon must see the credential removal,
	// not keep serving the stale snapshot.
	got := cli.ConfigFile()
	if got == snapshot {
		t.Fatal("expected a fresh config, got the cached snapshot")
	}

	if len(got.AuthConfigs) != 0 {
		t.Fatalf("expected no auth configs from a missing file, got %+v", got.AuthConfigs)
	}
}
