package commitstatus

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// postGitLab posts a commit status using the GitLab API.
func postGitLab(ctx context.Context, baseURL, repoFullName, commitSHA, token string, status Status) error {
	// GitLab requires the namespace/path to be URL-encoded with %2F separating path components.
	encodedPath := strings.ReplaceAll(repoFullName, "/", "%2F")
	apiURL := fmt.Sprintf("%s/api/v4/projects/%s/statuses/%s", baseURL, encodedPath, commitSHA)

	type gitlabRequest struct {
		State       string `json:"state"`
		Name        string `json:"name"`
		Description string `json:"description"`
		TargetURL   string `json:"target_url,omitempty"`
	}

	body := gitlabRequest{
		State:       commitStatusToGitLabState(status),
		Name:        status.Context,
		Description: status.Description,
		TargetURL:   status.TargetURL,
	}

	err := doPost(ctx, apiURL, bearerAuthToken(token), body)
	if err == nil || !isGitLabInvalidTransition(err) {
		return err
	}

	switch body.State {
	case "pending", "running":
		// GitLab cannot re-enter an active state, so the status is already pending or running.
		return nil
	case "skipped":
		// GitLab cannot skip a running status, so finish it with the legacy result instead.
		body.State = "success"

		return doPost(ctx, apiURL, bearerAuthToken(token), body)
	default:
		return err
	}
}

// commitStatusToGitLabState maps a status to GitLab's native commit status states.
// GitLab has no timed-out state, so timeouts are reported as failed.
func commitStatusToGitLabState(status Status) string {
	switch status.State {
	case StateFailure, StateError:
		return "failed"
	case StatePending:
		if status.Outcome == OutcomeQueued || status.Outcome == OutcomeDeferred {
			return "pending"
		}

		return "running"
	case StateSuccess:
		if status.Outcome == OutcomeSkipped {
			return "skipped"
		}
	}

	return string(status.State)
}

// isGitLabInvalidTransition reports whether GitLab rejected a status because
// the existing status for the context cannot move to the requested state.
func isGitLabInvalidTransition(err error) bool {
	var postErr *commitStatusPostRetryError

	return errors.As(err, &postErr) && postErr.statusCode == http.StatusBadRequest &&
		strings.Contains(postErr.details, "Cannot transition status")
}

func getGitLab(ctx context.Context, baseURL, repoFullName, commitSHA, token, contextName string) (Status, bool, error) {
	encodedPath := strings.ReplaceAll(repoFullName, "/", "%2F")
	apiURL := fmt.Sprintf("%s/api/v4/projects/%s/repository/commits/%s/statuses", baseURL, encodedPath, commitSHA)

	type gitlabStatus struct {
		Status      string `json:"status"`
		Name        string `json:"name"`
		Description string `json:"description"`
		TargetURL   string `json:"target_url"`
	}

	var statuses []gitlabStatus
	if err := doGet(ctx, apiURL, bearerAuthToken(token), &statuses); err != nil {
		return Status{}, false, err
	}

	for _, status := range statuses {
		if status.Name != contextName {
			continue
		}

		state, outcome := gitLabStateToCommitStatus(status.Status)

		return Status{
			State:       state,
			Outcome:     outcome,
			Description: status.Description,
			Context:     status.Name,
			TargetURL:   status.TargetURL,
		}, true, nil
	}

	return Status{}, false, nil
}

func gitLabStateToCommitStatus(state string) (State, Outcome) {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "pending":
		return StatePending, OutcomeQueued
	case "running":
		return StatePending, OutcomeInProgress
	case "success":
		return StateSuccess, ""
	case "skipped":
		return StateSuccess, OutcomeSkipped
	case "failed", "failure":
		return StateFailure, ""
	default:
		return StateError, ""
	}
}
