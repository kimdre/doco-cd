package githubapp

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testRepoURL = "https://github.com/acme/deploy.git"

var (
	testKeyOnce sync.Once
	testKeyPEM  string
)

func testPrivateKey(t *testing.T) string {
	t.Helper()

	testKeyOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}

		testKeyPEM = string(pem.EncodeToMemory(&pem.Block{
			Type:  "RSA PRIVATE KEY",
			Bytes: x509.MarshalPKCS1PrivateKey(key),
		}))
	})

	return testKeyPEM
}

// fakeGitHub serves the installation lookup and token mint endpoints.
type fakeGitHub struct {
	installationID atomic.Int64
	lookups        atomic.Int32
	mints          atomic.Int32
	lookupStatus   atomic.Int32
	lookupDelay    time.Duration
	now            time.Time
	nowMu          sync.Mutex
}

func (f *fakeGitHub) advance(d time.Duration) {
	f.nowMu.Lock()
	defer f.nowMu.Unlock()

	f.now = f.now.Add(d)
}

func (f *fakeGitHub) currentTime() time.Time {
	f.nowMu.Lock()
	defer f.nowMu.Unlock()

	return f.now
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/deploy/installation":
		f.lookups.Add(1)
		time.Sleep(f.lookupDelay)

		if status := int(f.lookupStatus.Load()); status != 0 {
			http.Error(w, `{"message":"Not Found"}`, status)

			return
		}

		_ = json.NewEncoder(w).Encode(installationResponse{ID: f.installationID.Load()})
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/app/installations/"):
		f.mints.Add(1)

		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/app/installations/"), "/access_tokens")
		if id != strconv.FormatInt(f.installationID.Load(), 10) {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)

			return
		}

		_ = json.NewEncoder(w).Encode(accessTokenResponse{
			Token:     "token-" + id,
			ExpiresAt: f.currentTime().Add(time.Hour),
		})
	default:
		http.NotFound(w, r)
	}
}

// rewriteTransport sends every request to the test server, keeping the path.
type rewriteTransport struct {
	host string
}

func (rt rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme = "http"
	req.URL.Host = rt.host

	return http.DefaultTransport.RoundTrip(req)
}

// setupResolver points the resolver at a fake GitHub API with empty caches.
func setupResolver(t *testing.T) *fakeGitHub {
	t.Helper()

	fake := &fakeGitHub{now: time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)}
	fake.installationID.Store(42)

	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)

	prevClient, prevNow := apiHTTPClient, nowFn

	apiHTTPClient = &http.Client{
		Timeout:   5 * time.Second,
		Transport: rewriteTransport{host: strings.TrimPrefix(server.URL, "http://")},
	}
	nowFn = fake.currentTime

	tokenCacheMu.Lock()
	tokenCache = map[string]cachedToken{}
	tokenCacheMu.Unlock()

	installationCacheMu.Lock()
	installationCache = map[string]cachedInstallation{}
	installationCacheMu.Unlock()

	t.Cleanup(func() {
		apiHTTPClient, nowFn = prevClient, prevNow
	})

	return fake
}

func resolve(t *testing.T, cfg Config) string {
	t.Helper()

	token, err := ResolveInstallationToken(testRepoURL, cfg)
	if err != nil {
		t.Fatalf("ResolveInstallationToken() error = %v", err)
	}

	return token
}

func appConfig(t *testing.T) Config {
	t.Helper()

	return Config{ID: "123", PrivateKey: testPrivateKey(t)}
}

func expectCalls(t *testing.T, fake *fakeGitHub, lookups, mints int32) {
	t.Helper()

	if got := fake.lookups.Load(); got != lookups {
		t.Errorf("installation lookups = %d, want %d", got, lookups)
	}

	if got := fake.mints.Load(); got != mints {
		t.Errorf("token mints = %d, want %d", got, mints)
	}
}

func TestResolveInstallationToken_CachesInstallationLookup(t *testing.T) {
	fake := setupResolver(t)
	cfg := appConfig(t)

	if got := resolve(t, cfg); got != "token-42" {
		t.Fatalf("token = %q, want token-42", got)
	}

	if got := resolve(t, cfg); got != "token-42" {
		t.Fatalf("token = %q, want token-42", got)
	}

	// The same repository with a different case shares the cached lookup.
	if _, err := ResolveInstallationToken("https://github.com/Acme/Deploy.git", cfg); err != nil {
		t.Fatalf("ResolveInstallationToken() error = %v", err)
	}

	expectCalls(t, fake, 1, 1)
}

func TestResolveInstallationToken_LooksUpAgainAfterTTL(t *testing.T) {
	fake := setupResolver(t)
	cfg := appConfig(t)

	resolve(t, cfg)
	fake.advance(installationCacheTTL + time.Second)
	resolve(t, cfg)

	// The token expired too, so both endpoints are called again.
	expectCalls(t, fake, 2, 2)
}

func TestResolveInstallationToken_ConfiguredInstallationSkipsLookup(t *testing.T) {
	fake := setupResolver(t)
	cfg := appConfig(t)
	cfg.InstallationID = 42

	resolve(t, cfg)
	resolve(t, cfg)

	expectCalls(t, fake, 0, 1)
}

func TestResolveInstallationToken_ReinstalledAppRefreshesCachedInstallation(t *testing.T) {
	fake := setupResolver(t)
	cfg := appConfig(t)

	resolve(t, cfg)

	// The App is reinstalled and the old token has expired, while the
	// installation ID is still cached.
	fake.installationID.Store(77)

	tokenCacheMu.Lock()
	tokenCache = map[string]cachedToken{}
	tokenCacheMu.Unlock()

	if got := resolve(t, cfg); got != "token-77" {
		t.Fatalf("token = %q, want token-77", got)
	}

	// The first mint for the stale ID fails, then exactly one lookup and one mint follow.
	expectCalls(t, fake, 2, 3)
}

func TestResolveInstallationToken_ReinstalledAppRefreshesRacingCachedInstallation(t *testing.T) {
	fake := setupResolver(t)
	cfg := appConfig(t)
	installKey := installationCacheKey("github.com", cfg.ID, "acme", "deploy")

	cacheInstallationID(installKey, 42)
	fake.advance(installationCacheTTL)
	fake.installationID.Store(77)

	var clockCalls atomic.Int32

	nowFn = func() time.Time {
		now := fake.currentTime()

		if clockCalls.Add(1) == 1 {
			// Another lookup fills the cache after the outer check reads an
			// expired entry, but before the singleflight cache recheck.
			installationCacheMu.Lock()
			installationCache[installKey] = cachedInstallation{
				ID:        42,
				ExpiresAt: now.Add(installationCacheTTL),
			}
			installationCacheMu.Unlock()
		}

		return now
	}

	if got := resolve(t, cfg); got != "token-77" {
		t.Fatalf("token = %q, want token-77", got)
	}

	expectCalls(t, fake, 1, 2)
}

func TestResolveInstallationToken_DoesNotCacheFailedLookups(t *testing.T) {
	fake := setupResolver(t)
	cfg := appConfig(t)

	fake.lookupStatus.Store(http.StatusNotFound)

	for range 2 {
		_, err := ResolveInstallationToken(testRepoURL, cfg)
		if !isNotFound(err) {
			t.Fatalf("ResolveInstallationToken() error = %v, want a 404 API error", err)
		}
	}

	fake.lookupStatus.Store(0)

	if got := resolve(t, cfg); got != "token-42" {
		t.Fatalf("token = %q, want token-42", got)
	}

	expectCalls(t, fake, 3, 1)
}

func TestResolveInstallationToken_DeduplicatesConcurrentResolves(t *testing.T) {
	fake := setupResolver(t)
	fake.lookupDelay = 50 * time.Millisecond
	cfg := appConfig(t)

	const callers = 8

	var wg sync.WaitGroup

	errs := make(chan error, callers)

	for range callers {
		wg.Go(func() {
			token, err := ResolveInstallationToken(testRepoURL, cfg)
			if err == nil && token != "token-42" {
				err = fmt.Errorf("token = %q, want token-42", token)
			}

			errs <- err
		})
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	expectCalls(t, fake, 1, 1)
}

func TestAPIStatusErrorMessage(t *testing.T) {
	err := &apiStatusError{StatusCode: http.StatusNotFound, Body: `{"message":"Not Found"}`}

	want := `GitHub API request failed with status 404: {"message":"Not Found"}`
	if err.Error() != want {
		t.Fatalf("Error() = %q, want %q", err.Error(), want)
	}
}
