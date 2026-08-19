package docker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

/*
Failed deployments must be retried and re-reported until they succeed, also
across daemon restarts (discussion #1702). Container labels cannot hold the
outcome: compose stamps them when it creates the containers, before hooks and
later stages run, and docker cannot change labels on an existing container.
So the outcome lives as a marker file in the data volume, which survives
daemon restarts the same way the labels do.
*/

const (
	failedDeploymentsDir = "failed-deployments"

	// ChangeTypeFailedDeployRetry marks a deployment retry after a recorded
	// failure. A Change of this type carries no service scope: the whole
	// project is force-recreated, because the failed part (e.g. a lifecycle
	// hook) is not attributable to one service.
	ChangeTypeFailedDeployRetry = "failed_deploy_retry"

	// maxRecordedErrorLength caps the stored error text, the full error is in the logs.
	maxRecordedErrorLength = 2000
)

// DeploymentFailure records a failed deployment attempt of one stack.
type DeploymentFailure struct {
	Repository string    `json:"repository"`
	Stack      string    `json:"stack"`
	CommitSHA  string    `json:"commit_sha"`
	Stage      string    `json:"stage"`
	Error      string    `json:"error"`
	FailedAt   time.Time `json:"failed_at"`
}

func deploymentFailurePath(dataMountPath, repoName, stack string) string {
	name := sanitizeStateFileName(repoName) + "--" + sanitizeStateFileName(stack) + ".json"
	return filepath.Join(dataMountPath, failedDeploymentsDir, name)
}

// sanitizeStateFileName keeps [a-zA-Z0-9._-] and replaces everything else with '_'.
func sanitizeStateFileName(s string) string {
	if s == "" {
		return "_"
	}

	out := []byte(s)
	for i, c := range out {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			out[i] = '_'
		}
	}

	return string(out)
}

// RecordDeploymentFailure persists the failure marker for a stack, replacing
// any previous one. The write is atomic (temp file + rename).
func RecordDeploymentFailure(dataMountPath, repoName, stack string, failure DeploymentFailure) error {
	path := deploymentFailurePath(dataMountPath, repoName, stack)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("failed to create failure marker directory: %w", err)
	}

	if r := []rune(failure.Error); len(r) > maxRecordedErrorLength {
		failure.Error = string(r[:maxRecordedErrorLength])
	}

	data, err := json.Marshal(failure)
	if err != nil {
		return fmt.Errorf("failed to marshal failure marker: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("failed to create temp failure marker: %w", err)
	}

	if _, err = tmp.Write(data); err == nil {
		err = tmp.Close()
	} else {
		_ = tmp.Close()
	}

	if err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("failed to write failure marker: %w", err)
	}

	if err = os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("failed to store failure marker: %w", err)
	}

	return nil
}

// GetDeploymentFailure reports whether the last deployment attempt of the
// stack failed. A marker that exists but cannot be parsed still counts as a
// failure: presence is the signal, content is informational.
func GetDeploymentFailure(dataMountPath, repoName, stack string) (DeploymentFailure, bool) {
	data, err := os.ReadFile(deploymentFailurePath(dataMountPath, repoName, stack))
	if err != nil {
		return DeploymentFailure{}, false
	}

	var failure DeploymentFailure
	if err = json.Unmarshal(data, &failure); err != nil {
		return DeploymentFailure{}, true
	}

	return failure, true
}

// ClearDeploymentFailure removes the failure marker of a stack, if present.
func ClearDeploymentFailure(dataMountPath, repoName, stack string) error {
	err := os.Remove(deploymentFailurePath(dataMountPath, repoName, stack))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return nil
}
