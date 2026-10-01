package githubapp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/sync/singleflight"
)

const (
	tokenRenewalBuffer = 30 * time.Second
	// installationCacheTTL bounds how long a looked-up installation ID is
	// reused. A reinstalled App is also detected earlier, when minting a token
	// for the cached ID fails with 404.
	installationCacheTTL = time.Hour
)

// Config contains credentials used to mint short-lived GitHub App installation tokens.
type Config struct {
	ID             string
	PrivateKey     string
	InstallationID int64
}

var (
	apiHTTPClient = &http.Client{Timeout: 15 * time.Second}
	nowFn         = time.Now

	tokenCacheMu sync.RWMutex
	tokenCache   = map[string]cachedToken{}

	installationCacheMu sync.RWMutex
	installationCache   = map[string]cachedInstallation{}

	// lookupGroup and mintGroup collapse concurrent identical API calls, so
	// deployments starting together share one request.
	lookupGroup singleflight.Group
	mintGroup   singleflight.Group
)

type cachedToken struct {
	Token     string
	ExpiresAt time.Time
}

type cachedInstallation struct {
	ID        int64
	ExpiresAt time.Time
}

// apiStatusError is returned when the GitHub API answers with a non-2xx status.
type apiStatusError struct {
	StatusCode int
	Body       string
}

func (e *apiStatusError) Error() string {
	return fmt.Sprintf("GitHub API request failed with status %d: %s", e.StatusCode, e.Body)
}

func isNotFound(err error) bool {
	var statusErr *apiStatusError

	return errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusNotFound
}

type installationResponse struct {
	ID int64 `json:"id"`
}

type accessTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ResolveInstallationToken mints (or reuses a cached) installation access token for a repository URL.
func ResolveInstallationToken(repoURL string, cfg Config) (string, error) {
	appID := strings.TrimSpace(cfg.ID)

	privateKey := strings.TrimSpace(cfg.PrivateKey)
	if appID == "" || privateKey == "" {
		return "", errors.New("github app id and private key are required")
	}

	host := parseGitHost(repoURL)
	if host == "" {
		return "", errors.New("failed to parse host from repository URL")
	}

	owner, repo, err := ownerRepoFromURL(repoURL)
	if err != nil {
		return "", err
	}

	if cfg.InstallationID != 0 {
		return resolveToken(host, appID, privateKey, cfg.InstallationID)
	}

	installKey := installationCacheKey(host, appID, owner, repo)

	installationID, cached, err := resolveInstallationID(installKey, host, owner, repo, appID, privateKey)
	if err != nil {
		return "", err
	}

	token, err := resolveToken(host, appID, privateKey, installationID)
	if err == nil || !cached || !isNotFound(err) {
		return token, err
	}

	// The cached installation no longer exists (the App was uninstalled or
	// reinstalled), so look it up once more.
	forgetInstallationID(installKey, installationID)

	installationID, _, err = resolveInstallationID(installKey, host, owner, repo, appID, privateKey)
	if err != nil {
		return "", err
	}

	return resolveToken(host, appID, privateKey, installationID)
}

// resolveInstallationID returns the installation ID for a repository, from
// the cache when possible. cached reports whether the ID came from the cache.
func resolveInstallationID(key, host, owner, repo, appID, privateKey string) (int64, bool, error) {
	if id, ok := getCachedInstallationID(key); ok {
		return id, true, nil
	}

	lookup := func() (int64, error) {
		id, err := lookupInstallationID(host, owner, repo, appID, privateKey)
		if err != nil {
			return 0, err
		}

		cacheInstallationID(key, id)

		return id, nil
	}

	type lookupResult struct {
		id     int64
		cached bool
	}

	value, err, _ := lookupGroup.Do(key, func() (any, error) {
		if id, ok := getCachedInstallationID(key); ok {
			return lookupResult{id: id, cached: true}, nil
		}

		id, err := lookup()

		return lookupResult{id: id}, err
	})
	if err != nil {
		return 0, false, err
	}

	result, ok := value.(lookupResult)
	if !ok {
		id, err := lookup()

		return id, false, err
	}

	return result.id, result.cached, nil
}

// resolveToken returns a cached installation token or mints a new one.
func resolveToken(host, appID, privateKey string, installationID int64) (string, error) {
	cacheKey := fmt.Sprintf("%s|%s|%d", host, appID, installationID)
	if token, ok := getCachedToken(cacheKey); ok {
		return token, nil
	}

	mint := func() (string, error) {
		tokenResp, err := createInstallationToken(host, installationID, appID, privateKey)
		if err != nil {
			return "", err
		}

		cacheToken(cacheKey, tokenResp.Token, tokenResp.ExpiresAt)

		return tokenResp.Token, nil
	}

	value, err, _ := mintGroup.Do(cacheKey, func() (any, error) {
		if token, ok := getCachedToken(cacheKey); ok {
			return token, nil
		}

		return mint()
	})
	if err != nil {
		return "", err
	}

	token, ok := value.(string)
	if !ok {
		return mint()
	}

	return token, nil
}

func installationCacheKey(host, appID, owner, repo string) string {
	return strings.ToLower(fmt.Sprintf("%s|%s|%s/%s", host, appID, owner, repo))
}

func getCachedInstallationID(key string) (int64, bool) {
	installationCacheMu.RLock()

	entry, ok := installationCache[key]

	installationCacheMu.RUnlock()

	if !ok || !nowFn().Before(entry.ExpiresAt) {
		return 0, false
	}

	return entry.ID, true
}

func cacheInstallationID(key string, id int64) {
	installationCacheMu.Lock()
	defer installationCacheMu.Unlock()

	installationCache[key] = cachedInstallation{ID: id, ExpiresAt: nowFn().Add(installationCacheTTL)}
}

// forgetInstallationID drops a cached installation ID unless another caller
// already replaced it with a different one.
func forgetInstallationID(key string, id int64) {
	installationCacheMu.Lock()
	defer installationCacheMu.Unlock()

	if entry, ok := installationCache[key]; ok && entry.ID == id {
		delete(installationCache, key)
	}
}

func getCachedToken(cacheKey string) (string, bool) {
	tokenCacheMu.RLock()

	entry, ok := tokenCache[cacheKey]

	tokenCacheMu.RUnlock()

	if !ok {
		return "", false
	}

	if nowFn().Add(tokenRenewalBuffer).After(entry.ExpiresAt) {
		return "", false
	}

	return entry.Token, true
}

func cacheToken(cacheKey, token string, expiresAt time.Time) {
	tokenCacheMu.Lock()
	defer tokenCacheMu.Unlock()

	tokenCache[cacheKey] = cachedToken{Token: token, ExpiresAt: expiresAt}
}

func ownerRepoFromURL(repoURL string) (string, string, error) {
	u := strings.TrimSpace(repoURL)
	if u == "" {
		return "", "", errors.New("failed to parse owner/repo from URL")
	}

	if strings.Contains(u, "@") && strings.Contains(u, ":") && !strings.Contains(u, "://") {
		parts := strings.SplitN(u, "@", 2)
		if len(parts) != 2 {
			return "", "", errors.New("failed to parse owner/repo from URL")
		}

		hostPath := strings.SplitN(parts[1], ":", 2)
		if len(hostPath) != 2 {
			return "", "", errors.New("failed to parse owner/repo from URL")
		}

		return splitOwnerRepo(hostPath[1])
	}

	parsed, err := url.Parse(u)
	if err != nil {
		return "", "", errors.New("failed to parse owner/repo from URL")
	}

	return splitOwnerRepo(strings.TrimPrefix(parsed.Path, "/"))
}

func splitOwnerRepo(path string) (string, string, error) {
	p := strings.TrimSpace(path)
	p = strings.TrimPrefix(p, "/")
	p = strings.TrimSuffix(p, "/")
	p = strings.TrimSuffix(p, ".git")

	parts := strings.Split(p, "/")
	if len(parts) < 2 {
		return "", "", errors.New("failed to parse owner/repo from URL")
	}

	owner := strings.TrimSpace(parts[0])

	repo := strings.TrimSpace(parts[1])
	if owner == "" || repo == "" {
		return "", "", errors.New("failed to parse owner/repo from URL")
	}

	return owner, repo, nil
}

func lookupInstallationID(host, owner, repo, appID, privateKey string) (int64, error) {
	jwtToken, err := createAppJWT(appID, privateKey)
	if err != nil {
		return 0, err
	}

	apiBase := apiBaseURL(host)
	endpoint := fmt.Sprintf("%s/repos/%s/%s/installation", apiBase, owner, repo)

	var resp installationResponse
	if err := doAPIRequest(http.MethodGet, endpoint, jwtToken, nil, &resp); err != nil {
		return 0, err
	}

	if resp.ID == 0 {
		return 0, errors.New("github installation id not found for repository")
	}

	return resp.ID, nil
}

func createInstallationToken(host string, installationID int64, appID, privateKey string) (accessTokenResponse, error) {
	jwtToken, err := createAppJWT(appID, privateKey)
	if err != nil {
		return accessTokenResponse{}, err
	}

	apiBase := apiBaseURL(host)
	endpoint := fmt.Sprintf("%s/app/installations/%d/access_tokens", apiBase, installationID)

	var resp accessTokenResponse
	if err := doAPIRequest(http.MethodPost, endpoint, jwtToken, map[string]string{}, &resp); err != nil {
		return accessTokenResponse{}, err
	}

	if resp.Token == "" {
		return accessTokenResponse{}, errors.New("github installation access token is empty")
	}

	if resp.ExpiresAt.IsZero() {
		resp.ExpiresAt = nowFn().Add(1 * time.Minute)
	}

	return resp, nil
}

func createAppJWT(appID, privateKey string) (string, error) {
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(privateKey))
	if err != nil {
		return "", fmt.Errorf("failed to parse GitHub App private key: %w", err)
	}

	now := nowFn()
	claims := jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(now.Add(-30 * time.Second)),
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),
		Issuer:    appID,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)

	signed, err := token.SignedString(key)
	if err != nil {
		return "", fmt.Errorf("failed to sign GitHub App JWT: %w", err)
	}

	return signed, nil
}

func doAPIRequest(method, endpoint, jwtToken string, payload any, out any) error {
	var body io.Reader

	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("failed to encode GitHub API payload: %w", err)
		}

		body = bytes.NewBuffer(raw)
	}

	req, err := http.NewRequest(method, endpoint, body)
	if err != nil {
		return fmt.Errorf("failed to create GitHub API request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+jwtToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := apiHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("GitHub API request failed: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &apiStatusError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(bodyBytes))}
	}

	if out == nil {
		return nil
	}

	if err := json.Unmarshal(bodyBytes, out); err != nil {
		return fmt.Errorf("failed to decode GitHub API response: %w", err)
	}

	return nil
}

func apiBaseURL(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "github.com" || h == "www.github.com" {
		return "https://api.github.com"
	}

	if strings.HasPrefix(h, "api.") {
		return "https://" + h
	}

	return "https://" + h + "/api/v3"
}

func parseGitHost(rawURL string) string {
	u := strings.TrimSpace(rawURL)
	if u == "" {
		return ""
	}

	if strings.Contains(u, "@") && strings.Contains(u, ":") && !strings.Contains(u, "://") {
		parts := strings.SplitN(u, "@", 2)
		if len(parts) != 2 {
			return ""
		}

		hostPath := strings.SplitN(parts[1], ":", 2)
		if len(hostPath) != 2 {
			return ""
		}

		return normalizeHost(hostPath[0])
	}

	parsed, err := url.Parse(u)
	if err != nil {
		return ""
	}

	if parsed.Host == "" {
		return ""
	}

	return normalizeHost(parsed.Hostname())
}

func normalizeHost(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))

	return strings.TrimSuffix(host, ".")
}
