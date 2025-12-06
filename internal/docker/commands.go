package docker

import (
	"bytes"
	"context"
	"fmt"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/moby/moby/pkg/stdcopy"
)

func Exec(client client.APIClient, containerID string, cmd ...string) (out string, err error) {
	client.NegotiateAPIVersion(context.TODO())

	// EXEC COMMAND
	execConfig := container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	}

	res, err := client.ContainerExecCreate(context.Background(), containerID, execConfig)
	if err != nil {
		return "", fmt.Errorf("failed to exec command with error: %w", err)
	}

	// ATTACH TO EXEC PROCESS
	resp, err := client.ContainerExecAttach(context.Background(), res.ID, container.ExecAttachOptions{})
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
