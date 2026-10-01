package commitstatus

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// progressDescriptionPrefix prefixes the phase in the description of a
// running deployment's commit status.
const progressDescriptionPrefix = "In Progress: "

type singleAttemptKey struct{}

// WithSingleAttempt returns a context whose commit status writes are sent once,
// without retries. Best-effort updates use it so they never wait on backoff or
// spend requests against a provider's rate limit.
func WithSingleAttempt(ctx context.Context) context.Context {
	return context.WithValue(ctx, singleAttemptKey{}, true)
}

func isSingleAttempt(ctx context.Context) bool {
	single, _ := ctx.Value(singleAttemptKey{}).(bool)

	return single
}

// ProgressStatus returns the status of a running deployment in phase, for
// example "In Progress: pulling images".
func ProgressStatus(phase string) Status {
	phase = strings.Join(strings.Fields(phase), " ")

	return Status{
		State:       StatePending,
		Outcome:     OutcomeInProgress,
		Description: truncateDescription(progressDescriptionPrefix + phase),
		Phase:       phase,
	}
}

// SupportsProgress reports whether the request's provider can show the phase
// of a running deployment. GitLab cannot: it rejects a running status for a
// pipeline that is already running, so its description cannot change.
func (r Request) SupportsProgress() bool {
	if r.Target != nil && r.Target.Backend == BackendChecks {
		return true
	}

	host, _, err := parseHostAndScheme(r.RepoURL)
	if err != nil {
		return false
	}

	return resolveProvider(r.Provider, host) != ProviderGitLab
}

// IsRateLimited reports whether err is a provider's rate limit response: 429,
// or a 403 whose message mentions a rate limit (GitHub's primary and secondary
// limits).
func IsRateLimited(err error) bool {
	var postErr *commitStatusPostRetryError
	if !errors.As(err, &postErr) {
		return false
	}

	switch postErr.statusCode {
	case http.StatusTooManyRequests:
		return true
	case http.StatusForbidden:
		return strings.Contains(strings.ToLower(postErr.details), "rate limit")
	default:
		return false
	}
}
