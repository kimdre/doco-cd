//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/moby/moby/client"
)

func TestMain(m *testing.M) {
	if _, err := suiteIncludesStandalone(os.Getenv(suiteModeEnv)); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)

		os.Exit(2)
	}

	code := m.Run()

	teardownSuiteHarnesses()
	terminateSharedOCIRegistry()
	removeGitServerImage()
	removeDaemonImage()

	os.Exit(code)
}

func removeGitServerImage() {
	dockerCli, err := client.New(client.FromEnv)
	if err != nil {
		return
	}

	_, _ = dockerCli.ImageRemove(context.Background(), gitServerImageName(), client.ImageRemoveOptions{Force: true})
}

func removeDaemonImage() {
	dockerCli, err := client.New(client.FromEnv)
	if err != nil {
		return
	}

	_, _ = dockerCli.ImageRemove(context.Background(), daemonImageName(), client.ImageRemoveOptions{Force: true})
}
