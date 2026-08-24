//go:build e2e

package e2e

import (
	"context"
	"os"
	"testing"

	"github.com/moby/moby/client"
)

func TestMain(m *testing.M) {
	code := m.Run()

	teardownSuiteHarnesses()
	removeGitServerImage()

	os.Exit(code)
}

func removeGitServerImage() {
	dockerCli, err := client.New(client.FromEnv)
	if err != nil {
		return
	}

	_, _ = dockerCli.ImageRemove(context.Background(), gitServerImageName(), client.ImageRemoveOptions{Force: true})
}
