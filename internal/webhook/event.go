package webhook

import (
	"errors"
	"fmt"
	"net/http"
)

var ErrUnknownProvider = errors.New("unknown SCM provider")

// ScmProviderEventHeaders maps ScmProvider to their respective event header names.
var ScmProviderEventHeaders = map[ScmProvider]string{
	Github: "X-GitHub-Event",
	Gitlab: "X-Gitlab-Event",
	Gitea:  "X-Gitea-Event",
	Gogs:   "X-Gogs-Event",
}

const ZeroSHA = "0000000000000000000000000000000000000000" // ZeroSHA is the SHA used by Git to indicate a branch or tag deletion event.

// IsBranchOrTagDeletionEvent checks if the incoming webhook event is a branch or tag deletion event for the given provider.
func IsBranchOrTagDeletionEvent(r *http.Request, payload ParsedPayload, provider ScmProvider) (bool, error) {
	event := r.Header.Get(ScmProviderEventHeaders[provider])
	if event == "" {
		return false, fmt.Errorf("missing event header for provider %v", provider)
	}

	switch provider {
	case Github, Gitea, Gogs, Forgejo:
		if event != "delete" {
			return false, nil
		}

		if payload.After != ZeroSHA && payload.Before != ZeroSHA {
			return false, nil
		}

		return payload.RefType == "branch" || payload.RefType == "tag", nil
	case Gitlab:
		if event != "Push Hook" && event != "Tag Push Hook" {
			return false, nil
		}

		if payload.After != ZeroSHA {
			return false, nil
		}
		// Also verify checkout_sha is null for deletion events
		return payload.CommitSHA == "", nil
	default:
		return false, ErrUnknownProvider
	}
}
