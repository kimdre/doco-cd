package webhook

import (
	"encoding/json"
)

// GithubPushPayload is a struct that represents the payload sent by GitHub or Gitea, as they have the same structure.
type GithubPushPayload struct {
	Ref        string `json:"ref"`
	CommitSHA  string `json:"after"`
	Repository struct {
		Name     string `json:"name"`
		FullName string `json:"full_name"`
		CloneURL string `json:"clone_url"`
		WebURL   string `json:"html_url"`
		Private  bool   `json:"private"`
	} `json:"repository"`
}

// GitlabPushPayload is a struct that represents the payload sent by GitLab.
type GitlabPushPayload struct {
	Ref        string `json:"ref"`
	CommitSHA  string `json:"after"`
	Repository struct {
		Name              string `json:"name"`
		PathWithNamespace string `json:"path_with_namespace"`
		CloneURL          string `json:"http_url"`
		WebURL            string `json:"web_url"`
		VisibilityLevel   int64  `json:"visibility_level"`
	} `json:"project"`
}

// ParsedPayload is a struct that contains the parsed payload data.
type ParsedPayload struct {
	Ref       string // Ref is the branch or tag that triggered the webhook
	CommitSHA string // CommitSHA is the SHA of the commit that triggered the webhook
	Name      string // Name is the short name of the repository (without owner or organization)
	FullName  string // FullName is the full name of the repository (e.g., owner/repo)
	CloneURL  string // CloneURL is the URL to clone the repository
	WebURL    string // WebURL is the URL to view the repository in a web browser
	Private   bool   // Private indicates whether the repository is private or public
}

// ParsePayload parses the payload and returns a ParsedPayload struct.
func parsePayload(payload []byte, provider string) (ParsedPayload, error) {
	var (
		githubPayload GithubPushPayload
		gitlabPayload GitlabPushPayload
	)

	switch provider {
	case "github", "gitea":
		err := json.Unmarshal(payload, &githubPayload)
		if err != nil {
			return ParsedPayload{}, err
		}

		parsedPayload := ParsedPayload{
			Ref:       githubPayload.Ref,
			CommitSHA: githubPayload.CommitSHA,
			Name:      githubPayload.Repository.Name,
			FullName:  githubPayload.Repository.FullName,
			CloneURL:  githubPayload.Repository.CloneURL,
			WebURL:    githubPayload.Repository.WebURL,
			Private:   githubPayload.Repository.Private,
		}

		return parsedPayload, nil
	case "gitlab":
		err := json.Unmarshal(payload, &gitlabPayload)
		if err != nil {
			return ParsedPayload{}, err
		}

		parsedPayload := ParsedPayload{
			Ref:       gitlabPayload.Ref,
			CommitSHA: gitlabPayload.CommitSHA,
			Name:      gitlabPayload.Repository.Name,
			FullName:  gitlabPayload.Repository.PathWithNamespace,
			CloneURL:  gitlabPayload.Repository.CloneURL,
			WebURL:    gitlabPayload.Repository.WebURL,
			Private:   gitlabPayload.Repository.VisibilityLevel == 0,
		}

		return parsedPayload, nil
	}

	return ParsedPayload{}, ErrParsingPayload
}
