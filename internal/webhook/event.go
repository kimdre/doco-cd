package webhook

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/go-git/go-git/v5/plumbing"
)

var ErrUnknownProvider = errors.New("unknown SCM provider")

// ScmProviderEventHeaders maps ScmProvider to their respective event header names.
var ScmProviderEventHeaders = map[ScmProvider]string{
	Github:      "X-GitHub-Event",
	Gitlab:      "X-Gitlab-Event",
	Gitea:       "X-Gitea-Event",
	Gogs:        "X-Gogs-Event",
	Forgejo:     "X-Forgejo-Event",
	OCIRegistry: "X-Doco-OCI-Event",
}

// IsBranchOrTagDeletionEvent checks if the incoming webhook event is a branch or tag deletion event for the given provider.
func IsBranchOrTagDeletionEvent(r *http.Request, payload ParsedPayload, provider ScmProvider) (bool, error) {
	event := r.Header.Get(ScmProviderEventHeaders[provider])
	if event == "" && provider != OCIRegistry {
		return false, fmt.Errorf("missing event header for %v", provider)
	}

	switch provider {
	case Github, Gitea, Gogs, Forgejo:
		if payload.Before != plumbing.ZeroHash && payload.After == plumbing.ZeroHash {
			return true, nil
		}

		if event == "delete" {
			return payload.RefType == "branch" || payload.RefType == "tag", nil
		}

		return false, nil
	case Gitlab:
		if event != "Push Hook" && event != "Tag Push Hook" {
			return false, nil
		}

		if payload.After != plumbing.ZeroHash {
			return false, nil
		}
		// Also verify checkout_sha is null for deletion events
		return payload.CommitSHA == plumbing.ZeroHash, nil
	case OCIRegistry:
		// OCI events do not encode branch/tag deletion semantics.
		return false, nil
	default:
		return false, ErrUnknownProvider
	}
}
