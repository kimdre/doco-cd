package reconciliation

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
)

func TestInitContextCLIsLogsLifecycleCancellationAtDebug(t *testing.T) {
	t.Parallel()

	var logOutput bytes.Buffer

	log := slog.New(slog.NewTextHandler(&logOutput, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	reconciliationJob := newJob(&Manager{}, DeployRequest{
		Logger:        log,
		DeployConfigs: []*deployConfig.Config{deployConfig.New("app", "main")},
	}, nil)
	reconciliationJob.initContextCLIs(ctx)

	if len(reconciliationJob.contextCLIs) != 0 {
		t.Fatalf("context CLIs = %#v, want none after lifecycle cancellation", reconciliationJob.contextCLIs)
	}

	output := logOutput.String()
	if !strings.Contains(output, "level=DEBUG") || !strings.Contains(output, "initialization canceled during application shutdown") {
		t.Fatalf("expected lifecycle cancellation debug log, got %q", output)
	}
}

func TestInitContextCLIsLogsResolutionFailuresAtError(t *testing.T) {
	t.Parallel()

	var logOutput bytes.Buffer

	log := slog.New(slog.NewTextHandler(&logOutput, &slog.HandlerOptions{Level: slog.LevelDebug}))
	contexts := docker.NewContextRegistry(nil, docker.ContextRegistryOptions{})

	t.Cleanup(func() { _ = contexts.Close() })

	reconciliationJob := newJob(&Manager{contexts: contexts}, DeployRequest{
		Logger:        log,
		DeployConfigs: []*deployConfig.Config{deployConfig.New("app", "main")},
	}, nil)
	reconciliationJob.initContextCLIs(t.Context())

	output := logOutput.String()
	if !strings.Contains(output, "level=ERROR") || !strings.Contains(output, "failed to create Docker CLI for context") {
		t.Fatalf("expected context resolution error log, got %q", output)
	}
}
