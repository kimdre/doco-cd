package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"testing"
	"time"

	containerTypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/test"
)

func TestGetContainerRunAction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		state  *containerTypes.State
		want   containerRunAction
		baseOK bool
	}{
		{
			name:   "missing inspect state defaults to restart",
			want:   containerRunActionRestart,
			baseOK: false,
		},
		{
			name:   "nil state defaults to restart",
			want:   containerRunActionRestart,
			baseOK: true,
		},
		{
			name:   "running container restarts",
			state:  &containerTypes.State{Running: true},
			want:   containerRunActionRestart,
			baseOK: true,
		},
		{
			name:   "created container starts",
			state:  &containerTypes.State{Running: false, Status: "created"},
			want:   containerRunActionStart,
			baseOK: true,
		},
		{
			name:   "exited container starts",
			state:  &containerTypes.State{Running: false, Status: "exited"},
			want:   containerRunActionStart,
			baseOK: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			inspect := containerTypes.InspectResponse{}
			if tt.baseOK {
				inspect.State = tt.state
			}

			if got := getContainerRunAction(inspect); got != tt.want {
				t.Fatalf("getContainerRunAction() = %q, want %q", got, tt.want)
			}
		})
	}
}

// containerWaitTestImage is the image used for integration tests that need a
// minimal shell with a sleep command.
const containerWaitTestImage = "alpine:latest"

// setupContainerWaitTest creates a Docker CLI and pulls the test image. It
// skips the test if Docker is unavailable or the image cannot be pulled (e.g.
// no network in CI).
func setupContainerWaitTest(t *testing.T) client.APIClient {
	t.Helper()

	dockerCli, err := test.NewDockerCli()
	if err != nil {
		t.Skipf("docker unavailable: %v", err)
	}

	apiClient := dockerCli.Client()

	ctx := context.Background()

	reader, err := apiClient.ImagePull(ctx, containerWaitTestImage, client.ImagePullOptions{})
	if err != nil {
		t.Skipf("cannot pull %s: %v", containerWaitTestImage, err)
	}

	_, _ = io.Copy(io.Discard, reader)
	_ = reader.Close()

	return apiClient
}

// createTestContainer creates a container (not started) with the given command
// and registers cleanup. Returns the container ID.
func createTestContainer(ctx context.Context, t *testing.T, apiClient client.APIClient, cmd []string) string { //nolint:contextcheck
	t.Helper()

	name := fmt.Sprintf("doco-test-%s-%d", test.ConvertTestName(t.Name()), time.Now().UnixNano())

	result, err := apiClient.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &containerTypes.Config{
			Image: containerWaitTestImage,
			Cmd:   cmd,
		},
		Name: name,
	})
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}

	cleanupCtx := context.Background()

	t.Cleanup(func() {
		_, _ = apiClient.ContainerRemove(cleanupCtx, result.ID, client.ContainerRemoveOptions{Force: true})
	})

	return result.ID
}

// TestRunContainerOneOffFromExisting_WaitsForCompletion verifies that
// RunContainerOneOffFromExisting blocks until the spawned container exits,
// rather than returning as soon as the container is created (the
// WaitConditionNotRunning regression).
func TestRunContainerOneOffFromExisting_WaitsForCompletion(t *testing.T) {
	ctx := context.Background()
	apiClient := setupContainerWaitTest(t)

	const sleepSeconds = 1

	// Create a source container (not started) that sleeps for sleepSeconds.
	srcID := createTestContainer(ctx, t, apiClient, []string{"sleep", strconv.Itoa(sleepSeconds)})

	start := time.Now()

	if err := RunContainerOneOffFromExisting(ctx, apiClient, srcID); err != nil {
		t.Fatalf("RunContainerOneOffFromExisting: %v", err)
	}

	elapsed := time.Since(start)

	if elapsed < sleepSeconds*time.Second {
		t.Fatalf("RunContainerOneOffFromExisting returned too early: elapsed=%s, want >= %ds (WaitConditionNextExit regression?)", elapsed, sleepSeconds)
	}
}

// TestRunContainerOneOffFromExisting_NonZeroExit verifies that a non-zero exit
// code from the spawned container is surfaced as a ContainerExitError.
func TestRunContainerOneOffFromExisting_NonZeroExit(t *testing.T) {
	ctx := context.Background()
	apiClient := setupContainerWaitTest(t)

	srcID := createTestContainer(ctx, t, apiClient, []string{"sh", "-c", "exit 42"})

	err := RunContainerOneOffFromExisting(ctx, apiClient, srcID)
	if err == nil {
		t.Fatal("expected error for non-zero exit, got nil")
	}

	var exitErr *ContainerExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected ContainerExitError, got %T: %v", err, err)
	}

	if exitErr.ExitCode != 42 {
		t.Fatalf("expected exit code 42, got %d", exitErr.ExitCode)
	}
}

// TestRestartContainerAndWait_StoppedContainer verifies that
// RestartContainerAndWait blocks until a stopped (not-yet-started) container
// exits after being started.
func TestRestartContainerAndWait_StoppedContainer(t *testing.T) {
	ctx := context.Background()
	apiClient := setupContainerWaitTest(t)

	const sleepSeconds = 1

	containerID := createTestContainer(ctx, t, apiClient, []string{"sleep", strconv.Itoa(sleepSeconds)})

	start := time.Now()

	if err := RestartContainerAndWait(ctx, apiClient, containerID); err != nil {
		t.Fatalf("RestartContainerAndWait: %v", err)
	}

	elapsed := time.Since(start)

	if elapsed < sleepSeconds*time.Second {
		t.Fatalf("RestartContainerAndWait returned too early: elapsed=%s, want >= %ds", elapsed, sleepSeconds)
	}
}

// TestRestartContainerAndWait_RunningContainer verifies that
// RestartContainerAndWait waits for the container to complete its run after
// being restarted (i.e. it does not fire on the stop step of the restart cycle).
func TestRestartContainerAndWait_RunningContainer(t *testing.T) {
	ctx := context.Background()
	apiClient := setupContainerWaitTest(t)

	// Use a sleep long enough that the container is definitely still running
	// when RestartContainerAndWait is called. After restart it will sleep again
	// and the function must wait for that second run to complete.
	const sleepSeconds = 2

	containerID := createTestContainer(ctx, t, apiClient, []string{"sleep", strconv.Itoa(sleepSeconds)})

	// Start the container so it is in "running" state.
	if _, err := apiClient.ContainerStart(ctx, containerID, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}

	start := time.Now()

	// Container is running. RestartContainerAndWait should: restart → wait for
	// the post-restart run to finish.
	if err := RestartContainerAndWait(ctx, apiClient, containerID); err != nil {
		t.Fatalf("RestartContainerAndWait: %v", err)
	}

	elapsed := time.Since(start)

	// The restart cycle adds some overhead but the post-restart sleep is the
	// dominant component; require at least sleepSeconds to confirm we waited.
	if elapsed < sleepSeconds*time.Second {
		t.Fatalf("RestartContainerAndWait returned too early: elapsed=%s, want >= %ds", elapsed, sleepSeconds)
	}
}

// TestRestartContainerAndWait_NonZeroExit verifies that a non-zero exit code
// is surfaced as a ContainerExitError in the restart-and-wait flow.
func TestRestartContainerAndWait_NonZeroExit(t *testing.T) {
	ctx := context.Background()
	apiClient := setupContainerWaitTest(t)

	containerID := createTestContainer(ctx, t, apiClient, []string{"sh", "-c", "exit 7"})

	err := RestartContainerAndWait(ctx, apiClient, containerID)
	if err == nil {
		t.Fatal("expected error for non-zero exit, got nil")
	}

	var exitErr *ContainerExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected ContainerExitError, got %T: %v", err, err)
	}

	if exitErr.ExitCode != 7 {
		t.Fatalf("expected exit code 7, got %d", exitErr.ExitCode)
	}
}
