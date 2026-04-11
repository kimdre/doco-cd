package docker

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/test"
)

func TestComposeSignal(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "test.compose.yaml")

	createComposeFile(t, filePath, generateComposeContents())

	stackName := test.ConvertTestName(t.Name())

	project, err := LoadCompose(ctx, tmpDir, tmpDir, stackName, []string{filePath}, []string{".env"}, []string{}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}

	dockerCli, err := CreateDockerCli(false, false)
	if err != nil {
		t.Fatal(err)
	}

	stack := test.ComposeUp(ctx, t, test.WithYAML(generateComposeContents()))

	beforetime := time.Now()

	gotErr := ComposeSignal(t.Context(), dockerCli, project, []SignalService{
		{ServiceName: "test", Signal: "SIGHUP"},
	})
	if gotErr != nil {
		t.Errorf("ComposeSignal() failed: %v", gotErr)
		return
	}

	log := stack.ContainerLogs(ctx, t, "test", beforetime)
	if !strings.Contains(log, "signal 1 (SIGHUP) received, reconfiguring") {
		t.Errorf("expected empty log, got: %s", log)
	}
}
