package commitstatus

import (
	"context"
	"fmt"
)

// postGitHub posts a commit status using the GitHub REST API.
// For github.com the public API (api.github.com) is used.
// For any other host (GitHub Enterprise Server) the /api/v3 endpoint is used.
func postGitHub(ctx context.Context, baseURL, host, repoFullName, commitSHA, token string, status Status) error {
	var apiURL string

	if bareHost(host) == "github.com" {
		apiURL = fmt.Sprintf("https://api.github.com/repos/%s/statuses/%s", repoFullName, commitSHA)
	} else {
		// GitHub Enterprise Server: https://{hostname}/api/v3/repos/{owner}/{repo}/statuses/{sha}
		apiURL = fmt.Sprintf("%s/api/v3/repos/%s/statuses/%s", baseURL, repoFullName, commitSHA)
	}

	type githubRequest struct {
		State       string `json:"state"`
		Description string `json:"description"`
		Context     string `json:"context"`
		TargetURL   string `json:"target_url,omitempty"`
	}

	body := githubRequest{
		State:       string(status.State),
		Description: status.Description,
		Context:     status.Context,
		TargetURL:   status.TargetURL,
	}

	return doPost(ctx, apiURL, bearerAuthToken(token), body)
}

func getGitHub(ctx context.Context, baseURL, host, repoFullName, commitSHA, token, contextName string) (Status, bool, error) {
	var apiURL string

	if bareHost(host) == "github.com" {
		apiURL = fmt.Sprintf("https://api.github.com/repos/%s/statuses/%s", repoFullName, commitSHA)
	} else {
		apiURL = fmt.Sprintf("%s/api/v3/repos/%s/statuses/%s", baseURL, repoFullName, commitSHA)
	}

	return getGitHubStyle(ctx, apiURL, bearerAuthToken(token), contextName)
}

func getGitHubStyle(ctx context.Context, apiURL, token, contextName string) (Status, bool, error) {
	type githubStatus struct {
		State       string `json:"state"`
		Description string `json:"description"`
		Context     string `json:"context"`
		TargetURL   string `json:"target_url"`
	}

	var statuses []githubStatus
	if err := doGet(ctx, apiURL, token, &statuses); err != nil {
		return Status{}, false, err
	}

	for _, status := range statuses {
		if status.Context != contextName {
			continue
		}

		return Status{
			State:       State(status.State),
			Description: status.Description,
			Context:     status.Context,
			TargetURL:   status.TargetURL,
		}, true, nil
	}

	return Status{}, false, nil
}
