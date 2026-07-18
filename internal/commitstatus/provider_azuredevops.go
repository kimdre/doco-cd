package commitstatus

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"strings"
)

func postAzureDevOps(ctx context.Context, baseURL, host, repoURL, repoFullName, commitSHA, token string, status Status) error {
	apiURL, err := azureDevOpsStatusesURL(baseURL, host, repoURL, repoFullName, commitSHA)
	if err != nil {
		return err
	}

	type azureContext struct {
		Name  string `json:"name"`
		Genre string `json:"genre"`
	}

	type azureRequest struct {
		State       string       `json:"state"`
		Description string       `json:"description"`
		Context     azureContext `json:"context"`
		TargetURL   string       `json:"targetUrl,omitempty"`
	}

	body := azureRequest{
		State:       commitStatusToAzureState(status.State),
		Description: status.Description,
		Context: azureContext{
			Name:  status.Context,
			Genre: "doco-cd",
		},
		TargetURL: status.TargetURL,
	}

	return doPost(ctx, apiURL, azureDevOpsAuthToken(token), body)
}

func getAzureDevOps(ctx context.Context, baseURL, host, repoURL, repoFullName, commitSHA, token, contextName string) (Status, bool, error) {
	apiURL, err := azureDevOpsStatusesURL(baseURL, host, repoURL, repoFullName, commitSHA)
	if err != nil {
		return Status{}, false, err
	}

	type azureContext struct {
		Name  string `json:"name"`
		Genre string `json:"genre"`
	}

	type azureStatus struct {
		State       string       `json:"state"`
		Description string       `json:"description"`
		TargetURL   string       `json:"targetUrl"`
		Context     azureContext `json:"context"`
	}

	type azureListResponse struct {
		Value []azureStatus `json:"value"`
	}

	var response azureListResponse
	if err := doGet(ctx, apiURL, azureDevOpsAuthToken(token), &response); err != nil {
		return Status{}, false, err
	}

	for _, status := range response.Value {
		if status.Context.Name != contextName {
			continue
		}

		return Status{
			State:       azureStateToCommitStatus(status.State),
			Description: status.Description,
			Context:     status.Context.Name,
			TargetURL:   status.TargetURL,
		}, true, nil
	}

	return Status{}, false, nil
}

func azureDevOpsStatusesURL(baseURL, host, repoURL, repoFullName, commitSHA string) (string, error) {
	projectPath, repository, err := parseAzureDevOpsProjectAndRepo(repoURL, repoFullName, host)
	if err != nil {
		return "", err
	}

	apiBase := strings.TrimSuffix(baseURL, "/")
	if bareHost(host) == "ssh.dev.azure.com" {
		apiBase = "https://dev.azure.com"
	}

	return fmt.Sprintf("%s/%s/_apis/git/repositories/%s/commits/%s/statuses?api-version=7.1",
		apiBase,
		projectPath,
		url.PathEscape(repository),
		commitSHA,
	), nil
}

func parseAzureDevOpsProjectAndRepo(repoURL, repoFullName, host string) (string, string, error) {
	segments := parseAzureDevOpsPathSegments(repoURL)

	projectPath, repository := azureDevOpsProjectAndRepoFromSegments(segments)
	if projectPath != "" && repository != "" {
		return projectPath, repository, nil
	}

	fullNameSegments := splitPathSegments(repoFullName)

	projectPath, repository = azureDevOpsProjectAndRepoFromSegments(fullNameSegments)
	if projectPath != "" && repository != "" {
		return projectPath, repository, nil
	}

	return "", "", fmt.Errorf("failed to parse Azure DevOps repository from %q (host=%q, full_name=%q)", repoURL, host, repoFullName)
}

func parseAzureDevOpsPathSegments(rawURL string) []string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil
	}

	if strings.Contains(rawURL, "@") && strings.Contains(rawURL, ":") && !strings.Contains(rawURL, "://") {
		parts := strings.SplitN(rawURL, ":", 2)
		if len(parts) != 2 {
			return nil
		}

		return splitPathSegments(parts[1])
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil
	}

	return splitPathSegments(parsed.Path)
}

func azureDevOpsProjectAndRepoFromSegments(segments []string) (string, string) {
	if len(segments) == 0 {
		return "", ""
	}

	if segments[0] == "v3" && len(segments) >= 4 {
		return path.Join(segments[1], segments[2]), strings.TrimSuffix(segments[3], ".git")
	}

	for i, segment := range segments {
		if segment != "_git" || i == 0 || i+1 >= len(segments) {
			continue
		}

		return path.Join(segments[:i]...), strings.TrimSuffix(segments[i+1], ".git")
	}

	return "", ""
}

func splitPathSegments(value string) []string {
	value = strings.TrimSpace(value)

	value = strings.Trim(value, "/")
	if value == "" {
		return nil
	}

	rawSegments := strings.Split(value, "/")
	segments := make([]string, 0, len(rawSegments))

	for _, segment := range rawSegments {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			continue
		}

		segments = append(segments, segment)
	}

	return segments
}

func commitStatusToAzureState(state State) string {
	switch state {
	case StatePending:
		return "pending"
	case StateSuccess:
		return "succeeded"
	case StateFailure:
		return "failed"
	default:
		return "error"
	}
}

func azureStateToCommitStatus(state string) State {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "pending", "inprogress":
		return StatePending
	case "succeeded", "success":
		return StateSuccess
	case "failed", "failure":
		return StateFailure
	default:
		return StateError
	}
}
