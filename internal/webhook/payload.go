package webhook

import (
	"encoding/json"
)

// GithubPushPayload is a struct that represents the payload sent by GitHub or Gitea, as they have the same structure
type GithubPushPayload struct {
	Ref        string `json:"ref"`
	Repository struct {
		Name     string `json:"name"`
		FullName string `json:"full_name"`
		CloneURL string `json:"clone_url"`
		Private  bool   `json:"private"`
	} `json:"repository"`
}

// GitlabPushPayload is a struct that represents the payload sent by GitLab
type GitlabPushPayload struct {
	Ref        string `json:"ref"`
	Repository struct {
		Name              string `json:"name"`
		PathWithNamespace string `json:"path_with_namespace"`
		CloneURL          string `json:"http_url"`
		VisibilityLevel   int64  `json:"visibility_level"`
	} `json:"project"`
}

// ParsedPayload is a struct that contains the parsed payload data
type ParsedPayload struct {
	Ref      string
	Name     string
	FullName string
	CloneURL string
	Private  bool
}

// ParsePayload parses the payload and returns a ParsedPayload struct
func parsePayload(payload []byte, provider string) (ParsedPayload, error) {
	var githubPayload GithubPushPayload

	var gitlabPayload GitlabPushPayload

	switch provider {
	case "github", "gitea":
		err := json.Unmarshal(payload, &githubPayload)
		if err != nil {
			return ParsedPayload{}, err
		}

		parsedPayload := ParsedPayload{
			Ref:      githubPayload.Ref,
			Name:     githubPayload.Repository.Name,
			FullName: githubPayload.Repository.FullName,
			CloneURL: githubPayload.Repository.CloneURL,
			Private:  githubPayload.Repository.Private,
		}

		return parsedPayload, nil
	case "gitlab":
		err := json.Unmarshal(payload, &gitlabPayload)
		if err != nil {
			return ParsedPayload{}, err
		}

		parsedPayload := ParsedPayload{
			Ref:      gitlabPayload.Ref,
			Name:     gitlabPayload.Repository.Name,
			FullName: gitlabPayload.Repository.PathWithNamespace,
			CloneURL: gitlabPayload.Repository.CloneURL,
			Private:  gitlabPayload.Repository.VisibilityLevel == 0,
		}

		return parsedPayload, nil
	}

	return ParsedPayload{}, ErrParsingPayload
}
