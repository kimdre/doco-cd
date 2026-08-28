package docker

import (
	"bytes"
	"context"
	"fmt"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

func Exec(apiClient client.APIClient, containerID string, cmd ...string) (out string, err error) {
	return ExecContext(context.Background(), apiClient, containerID, cmd...)
}

func ExecContext(ctx context.Context, apiClient client.APIClient, containerID string, cmd ...string) (out string, err error) {
	// EXEC COMMAND
	execConfig := client.ExecCreateOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	}

	res, err := apiClient.ExecCreate(ctx, containerID, execConfig)
	if err != nil {
		return "", fmt.Errorf("failed to exec command with error: %w", err)
	}

	// ATTACH TO EXEC PROCESS
	resp, err := apiClient.ExecAttach(ctx, res.ID, client.ExecAttachOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to attach to exec process: %w", err)
	}
	defer resp.Close()

	// READ OUTPUT
	// Use buffers to capture stdout and stderr
	var stdoutBuf, stderrBuf bytes.Buffer

	_, err = stdcopy.StdCopy(&stdoutBuf, &stderrBuf, resp.Reader)
	if err != nil {
		return "", fmt.Errorf("failed to copy output: %w", err)
	}

	return stdoutBuf.String(), nil
}
