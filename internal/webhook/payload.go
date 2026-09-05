package webhook

import (
	"encoding/json"
	"path"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/google/go-containerregistry/pkg/name"
)

type PayloadSource string

const (
	PayloadSourceGit PayloadSource = "git"
	PayloadSourceOCI PayloadSource = "oci"
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

// OCIArtifactPayload is a generic webhook payload for OCI artifact events.
type OCIArtifactPayload struct {
	Source   string `json:"source"`
	Digest   string `json:"digest"`
	Artifact string `json:"artifact"`
}

// ParsedPayload is a struct that contains the parsed payload data.
type ParsedPayload struct {
	Source    PayloadSource
	Ref       string        // Ref is the branch or tag that triggered the webhook
	RefType   string        // RefType is the type of ref (branch or tag) that triggered the webhook, only present in delete events
	Trigger   string        // Trigger is the value that triggered the deployment (e.g., "poll", commit SHA, or OCI digest)
	Name      string        // Name is the short name of the repository (without owner or organization)
	FullName  string        // FullName is the full name of the repository (e.g., owner/repo)
	CloneURL  string        // CloneURL is the URL to clone the repository
	SSHUrl    string        // SSHUrl is the SSH URL to clone the repository
	WebURL    string        // WebURL is the URL to view the repository in a web browser
	Artifact  string        // Artifact is the OCI artifact reference that triggered the webhook
	Digest    string        // Digest is the OCI digest that triggered the webhook
	Before    plumbing.Hash // Before is the hash of the commit before the push
	After     plumbing.Hash // After is the hash of the commit after the push
	CommitSHA plumbing.Hash // CommitSHA is the SHA of the commit that triggered the webhook
	Private   bool          // Private indicates whether the repository is private or public
}

// CommitSHAString returns the CommitSHA as a string.
// If the CommitSHA is empty, it returns an empty string.
func (p ParsedPayload) CommitSHAString() string {
	if p.CommitSHA == plumbing.ZeroHash {
		return ""
	}

	return p.CommitSHA.String()
}

// TriggerString returns the trigger value for the payload.
// For Git payloads, it returns the CommitSHA. For OCI payloads, it returns the Digest.
func (p ParsedPayload) TriggerString() string {
	if trigger := strings.TrimSpace(p.Trigger); trigger != "" {
		return trigger
	}

	if p.Source == PayloadSourceOCI {
		return strings.TrimSpace(p.Digest)
	}

	return p.CommitSHAString()
}

// RevisionString returns the revision string for the payload.
// For Git payloads, it returns the CommitSHA. For OCI payloads, it returns the Digest.
func (p ParsedPayload) RevisionString() string {
	if p.Source == PayloadSourceOCI {
		return strings.TrimSpace(p.Digest)
	}

	return p.CommitSHAString()
}

// parsePayload parses the payload and returns a ParsedPayload struct.
func parsePayload(payload []byte, provider ScmProvider) (ParsedPayload, error) {
	var (
		githubPayload GithubPushPayload
		gitlabPayload GitlabPushPayload
		ociPayload    OCIArtifactPayload
	)

	switch provider {
	case Github, Gitea, Gogs:
		err := json.Unmarshal(payload, &githubPayload)
		if err != nil {
			return ParsedPayload{}, err
		}

		parsedPayload := ParsedPayload{
			Source:    PayloadSourceGit,
			Ref:       githubPayload.Ref,
			RefType:   githubPayload.RefType,
			Before:    plumbing.NewHash(githubPayload.Before),
			After:     plumbing.NewHash(githubPayload.After), // GitHub doesn't have an "after" field, so we use the "after" field as the commit SHA
			CommitSHA: plumbing.NewHash(githubPayload.After),
			Trigger:   strings.TrimSpace(githubPayload.After),
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
			Source:    PayloadSourceGit,
			Ref:       gitlabPayload.Ref,
			Before:    plumbing.NewHash(gitlabPayload.Before),
			After:     plumbing.NewHash(gitlabPayload.After),
			CommitSHA: plumbing.NewHash(gitlabPayload.CommitSHA),
			Trigger:   strings.TrimSpace(gitlabPayload.CommitSHA),
			Name:      gitlabPayload.Repository.Name,
			FullName:  gitlabPayload.Repository.PathWithNamespace,
			CloneURL:  gitlabPayload.Repository.CloneURL,
			SSHUrl:    gitlabPayload.Repository.SSHUrl,
			WebURL:    gitlabPayload.Repository.WebURL,
			Private:   gitlabPayload.Repository.VisibilityLevel == 0,
		}

		return parsedPayload, nil
	case OCIRegistry:
		err := json.Unmarshal(payload, &ociPayload)
		if err != nil {
			return ParsedPayload{}, err
		}

		if strings.TrimSpace(ociPayload.Source) != string(PayloadSourceOCI) {
			return ParsedPayload{}, ErrParsingPayload
		}

		repositoryName, ref := parseRepositoryAndReferenceFromArtifact(ociPayload.Artifact)

		parsedPayload := ParsedPayload{
			Source:    PayloadSourceOCI,
			Ref:       ref,
			CommitSHA: plumbing.ZeroHash,
			Trigger:   strings.TrimSpace(ociPayload.Digest),
			Name:      path.Base(repositoryName),
			FullName:  repositoryName,
			Artifact:  ociPayload.Artifact,
			Digest:    ociPayload.Digest,
		}

		return parsedPayload, nil
	default:
		return ParsedPayload{}, ErrParsingPayload
	}
}

func parseRepositoryAndReferenceFromArtifact(artifact string) (string, string) {
	trimmed := strings.TrimSpace(artifact)
	if trimmed == "" {
		return "", ""
	}

	ref, err := name.ParseReference(trimmed, name.WeakValidation)
	if err != nil {
		return trimmed, ""
	}

	return ref.Context().RepositoryStr(), ref.Identifier()
}
