package webhook

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

func detectProvider(r *http.Request) (ScmProvider, error) {
	for provider, header := range ScmProviderEventHeaders {
		if r.Header.Get(header) != "" {
			return provider, nil
		}
	}

	return Unknown, ErrUnknownProvider
}

// IsBranchOrTagDeletionEvent checks if the incoming webhook event is a branch or tag deletion event for the given provider.
func IsBranchOrTagDeletionEvent(r *http.Request, l *slog.Logger) (bool, error) {
	provider, err := detectProvider(r)
	if err != nil {
		return false, err
	}

	event := r.Header.Get(ScmProviderEventHeaders[provider])
	if event == "" {
		return false, fmt.Errorf("missing event header for provider %v", provider)
	}

	var payload map[string]any
	if err = json.NewDecoder(r.Body).Decode(&payload); err != nil {
		return false, fmt.Errorf("failed to decode payload: %w", err)
	}

	switch provider {
	case Github, Gitea, Gogs, Forgejo:
		if event != "delete" {
			return false, nil
		}

		refType, ok := payload["ref_type"].(string)

		return ok && (refType == "branch" || refType == "tag"), nil
	case Gitlab:
		if event != "Push Hook" && event != "Tag Push Hook" {
			l.Warn("unexpected event type for GitLab: ", event)
			return false, nil
		}

		after, ok := payload["after"].(string)
		if !ok || after != "0000000000000000000000000000000000000000" {
			l.Warn("unexpected 'after' value for GitLab event: ", after)
			return false, nil
		}
		// Also verify checkout_sha is null for deletion events
		checkoutSha := payload["checkout_sha"]

		l.Warn("GitLab event 'after' value indicates deletion, checkout_sha:", checkoutSha)
		return checkoutSha == nil, nil
	default:
		return false, ErrUnknownProvider
	}
}
