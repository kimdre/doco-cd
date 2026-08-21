package docker

import (
	"log/slog"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/config"
	"github.com/docker/cli/cli/config/configfile"
	"github.com/docker/cli/cli/config/credentials"
)

// reloadConfigCli re-reads the docker config file from disk on every
// ConfigFile() call instead of returning the snapshot the docker cli caches at
// startup. Registries with short-lived tokens (ECR, GCR, ACR) refresh the
// mounted config.json while the daemon runs; with the cached snapshot every
// pull fails after the startup token expires until the daemon restarts.
// All auth lookups (compose pulls, distribution inspects, swarm) resolve the
// config at call time, so they all pick up the fresh file through this wrapper.
type reloadConfigCli struct {
	command.Cli
}

// ConfigFile loads the docker config fresh from disk. On a read or parse
// error (e.g. the file is being rewritten at that moment) it falls back to
// the snapshot cached by the underlying cli.
func (c reloadConfigCli) ConfigFile() *configfile.ConfigFile {
	fresh, err := config.Load(config.Dir())
	if err != nil {
		slog.Warn("failed to reload docker config, using cached snapshot", slog.String("error", err.Error()))
		return c.Cli.ConfigFile()
	}

	// Mirror config.LoadDefaultConfigFile: detect the platform default
	// credentials store when the file configures no auth at all.
	if !fresh.ContainsAuth() {
		fresh.CredentialsStore = credentials.DetectDefaultStore(fresh.CredentialsStore)
	}

	return fresh
}
