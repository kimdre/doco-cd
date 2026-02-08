package webhook

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/kimdre/doco-cd/internal/git"
)

var ErrUnknownProvider = errors.New("unknown SCM provider")

// ScmProviderEventHeaders maps ScmProvider to their respective event header names.
var ScmProviderEventHeaders = map[ScmProvider]string{
	Github:  "X-GitHub-Event",
	Gitlab:  "X-Gitlab-Event",
	Gitea:   "X-Gitea-Event",
	Gogs:    "X-Gogs-Event",
	Forgejo: "X-Forgejo-Event",
}

// IsBranchOrTagDeletionEvent checks if the incoming webhook event is a branch or tag deletion event for the given provider.
func IsBranchOrTagDeletionEvent(r *http.Request, payload ParsedPayload, provider ScmProvider) (bool, error) {
	event := r.Header.Get(ScmProviderEventHeaders[provider])
	if event == "" {
		return false, fmt.Errorf("missing event header for provider %v", provider)
	}

	switch provider {
	case Github, Gitea, Gogs, Forgejo:
		if payload.Before != git.ZeroSHA && payload.After == git.ZeroSHA {
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

		if payload.After != git.ZeroSHA {
			return false, nil
		}
		// Also verify checkout_sha is null for deletion events
		return payload.CommitSHA == "", nil
	default:
		return false, ErrUnknownProvider
	}
}
