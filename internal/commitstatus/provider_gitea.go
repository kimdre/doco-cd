package commitstatus

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

const giteaVersionCacheTTL = time.Hour

type giteaSkippedSupport struct {
	supported bool
	expiresAt time.Time
}

var giteaSkippedSupportCache = struct {
	sync.Mutex
	entries map[string]giteaSkippedSupport
}{entries: map[string]giteaSkippedSupport{}}

// postGitHubCompatible posts a commit status using the GitHub-compatible API.
// This covers Gitea, Forgejo, and Gogs which share the same /api/v1 endpoint shape.
func postGitHubCompatible(ctx context.Context, baseURL, _ /* host */, repoFullName, commitSHA, token string, status Status) error {
	apiURL := fmt.Sprintf("%s/api/v1/repos/%s/statuses/%s", baseURL, repoFullName, commitSHA)

	type githubRequest struct {
		State       string `json:"state"`
		Description string `json:"description"`
		Context     string `json:"context"`
		TargetURL   string `json:"target_url,omitempty"`
	}

	body := githubRequest{
		State:       string(status.State),
		Description: status.Description,
		Context:     status.Context,
		TargetURL:   status.TargetURL,
	}

	if status.State == StateSuccess && status.Outcome == OutcomeSkipped && giteaSupportsSkipped(ctx, baseURL, token) {
		body.State = "skipped"
	}

	return doPost(ctx, apiURL, giteaAuthToken(token), body)
}

// giteaSupportsSkipped reports whether the server has a native skipped state.
// Older servers store unknown states without validation, which leaves required
// checks pending, so skipped is only sent after the version has been confirmed.
func giteaSupportsSkipped(ctx context.Context, baseURL, token string) bool {
	giteaSkippedSupportCache.Lock()
	cached, ok := giteaSkippedSupportCache.entries[baseURL]
	giteaSkippedSupportCache.Unlock()

	if ok && time.Now().Before(cached.expiresAt) {
		return cached.supported
	}

	var response struct {
		Version string `json:"version"`
	}

	if err := doGet(ctx, baseURL+"/api/v1/version", giteaAuthToken(token), &response); err != nil {
		return false
	}

	supported := giteaVersionSupportsSkipped(response.Version)

	giteaSkippedSupportCache.Lock()
	giteaSkippedSupportCache.entries[baseURL] = giteaSkippedSupport{
		supported: supported,
		expiresAt: time.Now().Add(giteaVersionCacheTTL),
	}
	giteaSkippedSupportCache.Unlock()

	return supported
}

// giteaVersionSupportsSkipped reports whether a Gitea (1.25+) or Forgejo (16+)
// version supports the skipped commit status state.
func giteaVersionSupportsSkipped(version string) bool {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	minMajor, minMinor := 1, 25

	// Forgejo reports its own version followed by "+gitea-<compatible version>".
	if forgejoVersion, _, isForgejo := strings.Cut(version, "+gitea-"); isForgejo {
		version = forgejoVersion
		minMajor, minMinor = 16, 0
	}

	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return false
	}

	major, majorOK := leadingInt(parts[0])
	minor, minorOK := leadingInt(parts[1])

	if !majorOK || !minorOK {
		return false
	}

	return major > minMajor || (major == minMajor && minor >= minMinor)
}

// leadingInt parses the leading decimal digits of s, e.g. 25 from "25-rc1".
func leadingInt(s string) (int, bool) {
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}

	n, err := strconv.Atoi(s[:end])

	return n, err == nil
}

func getGitHubCompatible(ctx context.Context, baseURL, repoFullName, commitSHA, token, contextName string) (Status, bool, error) {
	apiURL := fmt.Sprintf("%s/api/v1/repos/%s/statuses/%s", baseURL, repoFullName, commitSHA)
	return getGitHubStyle(ctx, apiURL, giteaAuthToken(token), contextName)
}
