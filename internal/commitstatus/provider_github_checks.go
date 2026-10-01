package commitstatus

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// githubCheckLocks is a global lock map to prevent concurrent updates to the same GitHub check run.
var githubCheckLocks = struct {
	sync.Mutex
	targets map[string]chan struct{}
}{targets: make(map[string]chan struct{})}

// lockGitHubCheck locks a GitHub check run for the given key.
// It returns an unlock function that must be called to release the lock.
func lockGitHubCheck(ctx context.Context, key string) (func(), error) {
	githubCheckLocks.Lock()

	lock := githubCheckLocks.targets[key]
	if lock == nil {
		lock = make(chan struct{}, 1)
		githubCheckLocks.targets[key] = lock
	}
	githubCheckLocks.Unlock()

	select {
	case lock <- struct{}{}:
		return func() { <-lock }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// githubCheck represents a GitHub check run as returned by the GitHub API.
type githubCheck struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	ExternalID string     `json:"external_id"`
	Status     string     `json:"status"`
	Conclusion string     `json:"conclusion"`
	DetailsURL string     `json:"details_url"`
	StartedAt  *time.Time `json:"started_at"`
	App        struct {
		ID       int64  `json:"id"`
		ClientID string `json:"client_id"`
	} `json:"app"`
	Output struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
	} `json:"output"`
}

// githubCheckOutput represents the output of a GitHub check run as sent to the GitHub API.
type githubCheckOutput struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
}

// githubCheckRequest represents a request to create or update a GitHub check run.
type githubCheckRequest struct {
	Name        string             `json:"name"`
	HeadSHA     string             `json:"head_sha,omitempty"`
	ExternalID  string             `json:"external_id"`
	Status      string             `json:"status"`
	Conclusion  string             `json:"conclusion,omitempty"`
	DetailsURL  string             `json:"details_url,omitempty"`
	StartedAt   *time.Time         `json:"started_at,omitempty"`
	CompletedAt *time.Time         `json:"completed_at,omitempty"`
	Output      *githubCheckOutput `json:"output,omitempty"`
}

// Check output titles that complement, rather than repeat, the state GitHub
// renders next to them. GitHub keeps a check's output until it is overwritten,
// so every state that follows one with output needs its own title.
const (
	checkTitleQueued    = "Waiting for a deployment slot"
	checkTitleDeploying = "Deploying"
	checkTitleDeployed  = "Deployed"
	checkTitleFailed    = "Deployment failed"
	checkTitleTimedOut  = "Deployment timed out"
)

// githubAPIURL returns the GitHub API URL for the given request.
func (r Request) githubAPIURL() (string, error) {
	host, scheme, err := parseHostAndScheme(r.RepoURL)
	if err != nil {
		return "", err
	}

	if resolveProvider(r.Provider, host) != ProviderGitHub {
		return "", errors.New("check runs require the GitHub provider")
	}

	if strings.TrimSpace(r.APIBaseURL) != "" {
		base, err := parseAPIBaseURL(r.APIBaseURL)
		if err != nil {
			return "", err
		}

		parsed, err := url.Parse(base)
		if err != nil {
			return "", err
		}

		if bareHost(host) != "github.com" && parsed.Path == "" {
			base += "/api/v3"
		}

		return base, nil
	}

	if bareHost(host) == "github.com" {
		return "https://api.github.com", nil
	}

	return scheme + "://" + host + "/api/v3", nil
}

// deferredExternalID returns a deferred external ID for the GitHub check run.
func (r Request) deferredExternalID() string {
	key := r.RepoFullName + "\x00" + r.CommitSHA + "\x00" + r.Context + "\x00" + r.Target.AppID + "\x00" + r.Target.Scope

	return fmt.Sprintf("doco-cd:deferred:%x", sha256.Sum256([]byte(key)))
}

// findCheck searches for a GitHub check run that matches the given criteria.
func (r Request) findCheck(ctx context.Context, baseURL string, matches func(githubCheck) bool) (githubCheck, bool, error) {
	var latest githubCheck

	for page := 1; ; page++ {
		query := url.Values{
			"check_name": {r.Context},
			"filter":     {"all"},
			"per_page":   {"100"},
			"page":       {strconv.Itoa(page)},
		}
		apiURL := fmt.Sprintf("%s/repos/%s/commits/%s/check-runs?%s",
			baseURL, r.RepoFullName, url.PathEscape(r.CommitSHA), query.Encode())

		var response struct {
			TotalCount int           `json:"total_count"`
			CheckRuns  []githubCheck `json:"check_runs"`
		}
		if err := doGet(ctx, apiURL, bearerAuthToken(r.Token), &response); err != nil {
			return githubCheck{}, false, err
		}

		for _, check := range response.CheckRuns {
			if check.Name != r.Context || check.ID <= 0 {
				continue
			}

			// GitHub Apps may authenticate with either their numeric ID or client ID.
			if r.Target.AppID != "" && strconv.FormatInt(check.App.ID, 10) != r.Target.AppID &&
				check.App.ClientID != r.Target.AppID {
				continue
			}

			if matches(check) && check.ID > latest.ID {
				latest = check
			}
		}

		if len(response.CheckRuns) == 0 || page*100 >= response.TotalCount {
			break
		}
	}

	return latest, latest.ID > 0, nil
}

// postCheck creates or updates a GitHub check run with the given status.
func (r Request) postCheck(ctx context.Context, status Status) error {
	baseURL, err := r.githubAPIURL()
	if err != nil {
		return err
	}

	if r.Target.ExternalID == "" {
		return errors.New("check run target has no external identity")
	}

	// Claim a deferred check before another local attempt can resume it.
	unlock, err := lockGitHubCheck(ctx, baseURL+"\x00"+r.RepoFullName+"\x00"+r.Context+"\x00"+r.Target.AppID+"\x00"+r.Target.Scope)
	if err != nil {
		return err
	}
	defer unlock()

	if r.Target.CheckRunID == 0 {
		check, found, err := r.findCheck(ctx, baseURL, func(check githubCheck) bool {
			return check.ExternalID == r.Target.ExternalID ||
				(check.ExternalID == r.deferredExternalID() && check.Status == "queued")
		})
		if err != nil {
			return err
		}

		if found {
			r.Target.CheckRunID = check.ID
			// A resumed deferral starts when its deployment is admitted, not when it was queued.
			if check.StartedAt != nil && check.Status != "queued" {
				r.Target.StartedAt = *check.StartedAt
			}
		}
	}

	if status.Outcome == OutcomeDeferred {
		r.Target.ExternalID = r.deferredExternalID()
	}

	body, err := r.checkRequest(status)
	if err != nil {
		return err
	}

	apiURL := fmt.Sprintf("%s/repos/%s/check-runs", baseURL, r.RepoFullName)
	if r.Target.CheckRunID != 0 {
		return doWrite(ctx, http.MethodPatch, fmt.Sprintf("%s/%d", apiURL, r.Target.CheckRunID),
			bearerAuthToken(r.Token), body, nil)
	}

	body.HeadSHA = r.CommitSHA

	first := true

	return retryWrite(ctx, func() error {
		// A failed response may still have created the check. Recover its exact
		// identity before retrying rather than creating an orphaned duplicate.
		if !first {
			check, found, err := r.findCheck(ctx, baseURL, func(check githubCheck) bool {
				return check.ExternalID == r.Target.ExternalID
			})
			if err != nil {
				return err
			}

			if found {
				r.Target.CheckRunID = check.ID
				body.HeadSHA = ""

				return doWriteRequest(ctx, http.MethodPatch, fmt.Sprintf("%s/%d", apiURL, check.ID),
					bearerAuthToken(r.Token), body, nil)
			}
		}

		first = false

		var created githubCheck
		if err := doWriteRequest(ctx, http.MethodPost, apiURL, bearerAuthToken(r.Token), body, &created); err != nil {
			return err
		}

		if created.ID <= 0 {
			return errors.New("GitHub created a check run without returning its ID")
		}

		r.Target.CheckRunID = created.ID

		return nil
	})
}

// checkRequest creates a GitHub check run request for the given status.
//
// GitHub renders the status, conclusion and duration of a check run next to
// its output title, and a placeholder when a pending check has no output. The
// description is therefore only used when it adds to that (a sync window
// deferral, a failure reason or a skipped run with a summary); other states
// get a complementary title.
func (r Request) checkRequest(status Status) (githubCheckRequest, error) {
	body := githubCheckRequest{
		Name:       r.Context,
		ExternalID: r.Target.ExternalID,
		DetailsURL: status.TargetURL,
	}

	now := time.Now().UTC()

	switch status.Outcome {
	case OutcomeQueued, OutcomeDeferred:
		body.Status = "queued"
	case OutcomeInProgress:
		body.Status = "in_progress"
	case OutcomeSkipped, OutcomeTimedOut:
		body.Status = "completed"
		body.Conclusion = string(status.Outcome)
	case "":
		switch status.State {
		case StatePending:
			body.Status = "in_progress"
		case StateSuccess:
			body.Status = "completed"
			body.Conclusion = "success"
		case StateFailure, StateError:
			body.Status = "completed"
			body.Conclusion = "failure"
		default:
			return githubCheckRequest{}, fmt.Errorf("unsupported check state %q", status.State)
		}
	default:
		return githubCheckRequest{}, fmt.Errorf("unsupported check outcome %q", status.Outcome)
	}

	if body.Status == "in_progress" && r.Target.StartedAt.IsZero() {
		r.Target.StartedAt = now
	}

	if !r.Target.StartedAt.IsZero() {
		startedAt := r.Target.StartedAt.UTC()
		body.StartedAt = &startedAt
	}

	if body.Status == "completed" {
		body.CompletedAt = &now
	}

	title := checkTitle(status, body)
	summary := strings.TrimSpace(status.Summary)

	if title == "" && summary != "" {
		title = strings.TrimSpace(status.Description)
	}

	if title != "" {
		if summary == "" {
			summary = title
		}

		body.Output = &githubCheckOutput{Title: title, Summary: truncateCheckSummary(summary)}
	}

	return body, nil
}

// maxCheckSummaryLength is the maximum number of characters of a check run
// output summary accepted by GitHub.
const maxCheckSummaryLength = 65535

// truncateCheckSummary shortens summary to at most maxCheckSummaryLength
// characters.
func truncateCheckSummary(summary string) string {
	runes := []rune(summary)
	if len(runes) <= maxCheckSummaryLength {
		return summary
	}

	return string(runes[:maxCheckSummaryLength-1]) + "…"
}

// checkTitle returns the output title for a check run request, or "" when
// GitHub's own rendering of the state needs no addition (a skipped run without a summary).
func checkTitle(status Status, body githubCheckRequest) string {
	description := strings.TrimSpace(status.Description)

	switch {
	case status.Outcome == OutcomeDeferred && description != "":
		return description
	case body.Conclusion == "failure" || body.Conclusion == string(OutcomeTimedOut):
		if description != "" {
			return description
		}

		if body.Conclusion == string(OutcomeTimedOut) {
			return checkTitleTimedOut
		}

		return checkTitleFailed
	case body.Status == "queued":
		return checkTitleQueued
	case body.Status == "in_progress":
		if phase := strings.TrimSpace(status.Phase); phase != "" {
			return checkTitleDeploying + ": " + phase
		}

		return checkTitleDeploying
	case body.Conclusion == "success":
		return checkTitleDeployed
	default:
		return ""
	}
}

// getCheck retrieves the status of a GitHub check run.
func (r Request) getCheck(ctx context.Context) (Status, bool, error) {
	baseURL, err := r.githubAPIURL()
	if err != nil {
		return Status{}, false, err
	}

	check, found, err := r.findCheck(ctx, baseURL, func(githubCheck) bool { return true })
	if err != nil || !found {
		return Status{}, found, err
	}

	status := Status{
		Context:     check.Name,
		Description: check.Output.Title,
		TargetURL:   check.DetailsURL,
	}

	switch check.Status {
	case "queued":
		status.State, status.Outcome = StatePending, OutcomeQueued
	case "in_progress":
		status.State, status.Outcome = StatePending, OutcomeInProgress
	case "completed":
		switch check.Conclusion {
		case "success", "neutral":
			status.State = StateSuccess
		case "skipped":
			status.State, status.Outcome = StateSuccess, OutcomeSkipped
		case "timed_out":
			status.State, status.Outcome = StateFailure, OutcomeTimedOut
		default:
			status.State = StateFailure
		}
	default:
		return Status{}, false, fmt.Errorf("unsupported GitHub check status %q", check.Status)
	}

	return status, true, nil
}
