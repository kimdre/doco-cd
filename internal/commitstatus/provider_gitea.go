package commitstatus

import (
	"context"
	"fmt"
)

// postGitHubCompatible posts a commit status using the GitHub-compatible API.
// This covers Gitea, Forgejo, and Gogs which share the same /api/v1 endpoint shape.
func postGitHubCompatible(ctx context.Context, baseURL, _ /* host */, repoFullName, commitSHA, token string, status Status) error {
	apiURL := fmt.Sprintf("%s/api/v1/repos/%s/statuses/%s", baseURL, repoFullName, commitSHA)

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

	return doPost(ctx, apiURL, giteaAuthToken(token), body)
}

func getGitHubCompatible(ctx context.Context, baseURL, repoFullName, commitSHA, token, contextName string) (Status, bool, error) {
	apiURL := fmt.Sprintf("%s/api/v1/repos/%s/statuses/%s", baseURL, repoFullName, commitSHA)
	return getGitHubStyle(ctx, apiURL, giteaAuthToken(token), contextName)
}
