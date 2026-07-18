package commitstatus

import (
	"context"
	"fmt"
	"strings"
)

// postGitLab posts a commit status using the GitLab API.
func postGitLab(ctx context.Context, baseURL, repoFullName, commitSHA, token string, status Status) error {
	// GitLab requires the namespace/path to be URL-encoded with %2F separating path components.
	encodedPath := strings.ReplaceAll(repoFullName, "/", "%2F")
	apiURL := fmt.Sprintf("%s/api/v4/projects/%s/statuses/%s", baseURL, encodedPath, commitSHA)

	// Map our states to GitLab pipeline states.
	gitlabState := string(status.State)

	switch status.State {
	case StateFailure, StateError:
		gitlabState = "failed"
	case StatePending:
		gitlabState = "running"
	}

	type gitlabRequest struct {
		State       string `json:"state"`
		Name        string `json:"name"`
		Description string `json:"description"`
		TargetURL   string `json:"target_url,omitempty"`
	}

	body := gitlabRequest{
		State:       gitlabState,
		Name:        status.Context,
		Description: status.Description,
		TargetURL:   status.TargetURL,
	}

	return doPost(ctx, apiURL, bearerAuthToken(token), body)
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

		return Status{
			State:       gitLabStateToCommitStatus(status.Status),
			Description: status.Description,
			Context:     status.Name,
			TargetURL:   status.TargetURL,
		}, true, nil
	}

	return Status{}, false, nil
}

func gitLabStateToCommitStatus(state string) State {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "running", "pending":
		return StatePending
	case "success":
		return StateSuccess
	case "failed", "failure":
		return StateFailure
	default:
		return StateError
	}
}
