package webhook

import (
	"encoding/json"
)

// GithubPushPayload is a struct that represents the payload sent by GitHub or Gitea, as they have the same structure.
type GithubPushPayload struct {
	Ref        string `json:"ref"`
	RefType    string `json:"ref_type,omitempty"` // ref_type is only present in create/delete events
	Before     string `json:"before"`
	After      string `json:"after"`
	Repository struct {
		Name     string `json:"name"`
		FullName string `json:"full_name"`
		CloneURL string `json:"clone_url"`
		SSHUrl   string `json:"ssh_url"`
		WebURL   string `json:"html_url"`
		Private  bool   `json:"private"`
	} `json:"repository"`
}

// GitlabPushPayload is a struct that represents the payload sent by GitLab.
type GitlabPushPayload struct {
	Ref        string `json:"ref"`
	Before     string `json:"before"`
	After      string `json:"after"`
	CommitSHA  string `json:"checkout_sha"`
	Repository struct {
		Name              string `json:"name"`
		PathWithNamespace string `json:"path_with_namespace"`
		CloneURL          string `json:"http_url"`
		SSHUrl            string `json:"ssh_url"`
		WebURL            string `json:"web_url"`
		VisibilityLevel   int64  `json:"visibility_level"`
	} `json:"project"`
}

// ParsedPayload is a struct that contains the parsed payload data.
type ParsedPayload struct {
	Ref       string // Ref is the branch or tag that triggered the webhook
	RefType   string // RefType is the type of ref (branch or tag) that triggered the webhook, only present in delete events
	Before    string // Before is the SHA of the commit before the push, only present in GitLab payloads
	After     string // After is the SHA of the commit after the push, only present in GitLab payloads
	CommitSHA string // CommitSHA is the SHA of the commit that triggered the webhook
	Name      string // Name is the short name of the repository (without owner or organization)
	FullName  string // FullName is the full name of the repository (e.g., owner/repo)
	CloneURL  string // CloneURL is the URL to clone the repository
	SSHUrl    string // SSHUrl is the SSH URL to clone the repository
	WebURL    string // WebURL is the URL to view the repository in a web browser
	Private   bool   // Private indicates whether the repository is private or public
}

// ParsePayload parses the payload and returns a ParsedPayload struct.
func parsePayload(payload []byte, provider ScmProvider) (ParsedPayload, error) {
	var (
		githubPayload GithubPushPayload
		gitlabPayload GitlabPushPayload
	)

	switch provider {
	case Github, Gitea, Gogs:
		err := json.Unmarshal(payload, &githubPayload)
		if err != nil {
			return ParsedPayload{}, err
		}

		parsedPayload := ParsedPayload{
			Ref:       githubPayload.Ref,
			RefType:   githubPayload.RefType,
			Before:    githubPayload.Before,
			After:     githubPayload.After, // GitHub doesn't have an "after" field, so we use the "after" field as the commit SHA
			CommitSHA: githubPayload.After,
			Name:      githubPayload.Repository.Name,
			FullName:  githubPayload.Repository.FullName,
			CloneURL:  githubPayload.Repository.CloneURL,
			SSHUrl:    githubPayload.Repository.SSHUrl,
			WebURL:    githubPayload.Repository.WebURL,
			Private:   githubPayload.Repository.Private,
		}

		return parsedPayload, nil
	case Gitlab:
		err := json.Unmarshal(payload, &gitlabPayload)
		if err != nil {
			return ParsedPayload{}, err
		}

		parsedPayload := ParsedPayload{
			Ref:       gitlabPayload.Ref,
			Before:    gitlabPayload.Before,
			After:     gitlabPayload.After,
			CommitSHA: gitlabPayload.CommitSHA,
			Name:      gitlabPayload.Repository.Name,
			FullName:  gitlabPayload.Repository.PathWithNamespace,
			CloneURL:  gitlabPayload.Repository.CloneURL,
			SSHUrl:    gitlabPayload.Repository.SSHUrl,
			WebURL:    gitlabPayload.Repository.WebURL,
			Private:   gitlabPayload.Repository.VisibilityLevel == 0,
		}

		return parsedPayload, nil
	default:
		return ParsedPayload{}, ErrParsingPayload
	}
}
