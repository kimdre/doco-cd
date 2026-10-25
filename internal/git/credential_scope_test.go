package git_test

import (
	"errors"
	"net"
	"net/http"
	"net/http/cgi" //nolint:gosec // The test serves git http-backend through CGI. Modern Go is not affected by Httpoxy.
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/kimdre/doco-cd/internal/git"
)

const sentinelGitToken = "sentinel-token-must-not-leak"

type recordedGitRequest struct {
	host          string
	authorization string
}

// gitHTTPRecorder serves repositories through git http-backend and records the
// Authorization header of each request. Tests use it to prove which hosts get credentials.
type gitHTTPRecorder struct {
	mu       sync.Mutex
	requests []recordedGitRequest
	port     string
}

func (r *gitHTTPRecorder) requestsForHost(host string) []recordedGitRequest {
	r.mu.Lock()
	defer r.mu.Unlock()

	var out []recordedGitRequest

	for _, req := range r.requests {
		if req.host == host {
			out = append(out, req)
		}
	}

	return out
}

func (r *gitHTTPRecorder) url(host, repo string) string {
	return "http://" + net.JoinHostPort(host, r.port) + "/" + repo
}

func startGitHTTPRecorder(t *testing.T, root string) *gitHTTPRecorder {
	t.Helper()

	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("credential scope tests require the git executable: %v", err)
	}

	backend := &cgi.Handler{
		Path: gitPath,
		Args: []string{"http-backend"},
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"},
	}

	rec := &gitHTTPRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		host, _, splitErr := net.SplitHostPort(req.Host)
		if splitErr != nil {
			host = req.Host
		}

		rec.mu.Lock()
		rec.requests = append(rec.requests, recordedGitRequest{host: host, authorization: req.Header.Get("Authorization")})
		rec.mu.Unlock()

		backend.ServeHTTP(w, req)
	}))
	t.Cleanup(server.Close)

	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split test server address: %v", err)
	}

	rec.port = port

	return rec
}

// setupScopedGitHTTP serves one repository and scopes the global token to 127.0.0.1 only.
// The same server is reachable as "localhost", which has no GIT_AUTH_DOMAINS entry.
func setupScopedGitHTTP(t *testing.T) *gitHTTPRecorder {
	t.Helper()

	root := t.TempDir()
	initLocalTestRepo(t, filepath.Join(root, "repo.git"))

	rec := startGitHTTPRecorder(t, root)

	git.ConfigureAuthResolver([]git.ScopedAuthConfig{{Domains: []string{"127.0.0.1"}}}, "", "", sentinelGitToken, "", git.GitHubAppConfig{})
	t.Cleanup(func() {
		git.ConfigureAuthResolver(nil, "", "", "", "", git.GitHubAppConfig{})
	})

	return rec
}

func cloneMirrorFrom(t *testing.T, url string, private bool, path string) error {
	t.Helper()

	_, err := git.CloneOrUpdateBareMirror(nil, url, git.MainBranch, path, private, "", "", false, transport.ProxyOptions{}, 0)

	return err
}

func TestBareMirror_UnscopedHostReceivesNoCredential(t *testing.T) {
	rec := setupScopedGitHTTP(t)
	path := filepath.Join(t.TempDir(), "mirror")

	// Clone and then fetch, so both discovery and update requests are checked.
	for range 2 {
		if err := cloneMirrorFrom(t, rec.url("localhost", "repo.git"), false, path); err != nil {
			t.Fatalf("anonymous clone of public repository failed: %v", err)
		}
	}

	reqs := rec.requestsForHost("localhost")
	if len(reqs) == 0 {
		t.Fatal("expected requests to the unscoped host")
	}

	for _, req := range reqs {
		if req.authorization != "" {
			t.Fatalf("unscoped host received Authorization header %q", req.authorization)
		}
	}
}

func TestBareMirror_ScopedHostReceivesCredential(t *testing.T) {
	rec := setupScopedGitHTTP(t)
	path := filepath.Join(t.TempDir(), "mirror")

	for range 2 {
		if err := cloneMirrorFrom(t, rec.url("127.0.0.1", "repo.git"), false, path); err != nil {
			t.Fatalf("clone from scoped host failed: %v", err)
		}
	}

	reqs := rec.requestsForHost("127.0.0.1")
	if len(reqs) < 2 {
		t.Fatalf("expected clone and fetch requests, got %d", len(reqs))
	}

	for _, req := range reqs {
		user, pass, ok := (&http.Request{Header: http.Header{"Authorization": {req.authorization}}}).BasicAuth()
		if !ok || user != git.DefaultHTTPAuthUser || pass != sentinelGitToken {
			t.Fatalf("scoped host got Authorization %q, want basic auth for %q with the scoped token", req.authorization, git.DefaultHTTPAuthUser)
		}
	}
}

func TestBareMirror_PrivateRepositoryOnUnscopedHostFails(t *testing.T) {
	rec := setupScopedGitHTTP(t)

	err := cloneMirrorFrom(t, rec.url("localhost", "repo.git"), true, filepath.Join(t.TempDir(), "mirror"))
	if !errors.Is(err, git.ErrMissingAuthToken) {
		t.Fatalf("expected %v, got %v", git.ErrMissingAuthToken, err)
	}

	if !strings.Contains(err.Error(), "GIT_AUTH_DOMAINS") || !strings.Contains(err.Error(), "localhost") {
		t.Fatalf("error must name the host and GIT_AUTH_DOMAINS, got %v", err)
	}

	if reqs := rec.requestsForHost("localhost"); len(reqs) != 0 {
		t.Fatalf("expected no requests to the unscoped host, got %d", len(reqs))
	}
}

// With the explicit opt-out, an unscoped host receives the global credential.
func TestBareMirror_AllowAnyHostSendsCredentialToUnscopedHost(t *testing.T) {
	rec := setupScopedGitHTTP(t)

	git.ConfigureAuthResolver([]git.ScopedAuthConfig{{Domains: []string{"127.0.0.1"}}}, "", "", sentinelGitToken, "",
		git.GitHubAppConfig{}, git.WithAllowAnyHost(true))

	path := filepath.Join(t.TempDir(), "mirror")
	for range 2 {
		if err := cloneMirrorFrom(t, rec.url("localhost", "repo.git"), true, path); err != nil {
			t.Fatalf("clone or fetch from unscoped host with opt-out failed: %v", err)
		}
	}

	reqs := rec.requestsForHost("localhost")
	if len(reqs) < 2 {
		t.Fatalf("expected clone and fetch requests to the unscoped host, got %d", len(reqs))
	}

	for _, req := range reqs {
		_, pass, ok := (&http.Request{Header: http.Header{"Authorization": {req.authorization}}}).BasicAuth()
		if !ok || pass != sentinelGitToken {
			t.Fatalf("unscoped host got Authorization %q, want the global token", req.authorization)
		}
	}
}

func TestExportTree_SubmoduleCredentialScope(t *testing.T) {
	for _, allowAnyHost := range []bool{false, true} {
		name := "strict"
		if allowAnyHost {
			name = "allow any host"
		}

		t.Run(name, func(t *testing.T) {
			testSubmoduleCredentialScope(t, allowAnyHost)
		})
	}
}

func testSubmoduleCredentialScope(t *testing.T, allowAnyHost bool) {
	t.Helper()

	root := t.TempDir()
	rec := startGitHTTPRecorder(t, root)

	git.ConfigureAuthResolver([]git.ScopedAuthConfig{{Domains: []string{"127.0.0.1"}}}, "", "", sentinelGitToken, "",
		git.GitHubAppConfig{}, git.WithAllowAnyHost(allowAnyHost))
	t.Cleanup(func() {
		git.ConfigureAuthResolver(nil, "", "", "", "", git.GitHubAppConfig{})
	})

	siblingPath := filepath.Join(root, "sibling.git")
	sibling := initLocalTestRepo(t, siblingPath)
	pinned := commitLocalTestFile(t, sibling, siblingPath, "marker.txt", "pinned sibling\n", "pinned sibling")

	parentPath := filepath.Join(root, "parent.git")
	parent := initLocalTestRepo(t, parentPath)

	parentHead, err := parent.Head()
	if err != nil {
		t.Fatalf("get parent HEAD: %v", err)
	}

	gitmodules := "[submodule \"sibling\"]\n\tpath = sibling\n\turl = " + rec.url("localhost", "sibling.git") + "\n"
	tree := writeTestTree(t, parent, []object.TreeEntry{
		{Name: ".gitmodules", Mode: filemode.Regular, Hash: writeTestBlob(t, parent, gitmodules)},
		{Name: "sibling", Mode: filemode.Submodule, Hash: pinned},
	})
	signature := object.Signature{Name: "scope-test", Email: "scope-test@example.invalid", When: time.Now()}

	parentCommit := storeTestObject(t, parent, plumbing.CommitObject, func(raw plumbing.EncodedObject) error {
		return (&object.Commit{
			Author:       signature,
			Committer:    signature,
			Message:      "pin submodule on another host\n",
			TreeHash:     tree,
			ParentHashes: []plumbing.Hash{parentHead.Hash()},
		}).Encode(raw)
	})
	if err := parent.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), parentCommit)); err != nil {
		t.Fatalf("advance parent main: %v", err)
	}

	mirror, err := git.CloneOrUpdateBareMirror(nil, rec.url("127.0.0.1", "parent.git"), git.MainBranch,
		filepath.Join(t.TempDir(), "mirror"), false, "", "", false, transport.ProxyOptions{}, 0)
	if err != nil {
		t.Fatalf("clone parent from scoped host: %v", err)
	}

	exportPath := filepath.Join(t.TempDir(), "export")
	if err := git.ExportTree(exportPath, mirror, parentCommit, git.ExportOptions{
		SubmoduleCacheDir: filepath.Join(t.TempDir(), "submodules"),
	}); err != nil {
		t.Fatalf("export submodule: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(exportPath, "sibling", "marker.txt"))
	if err != nil || string(content) != "pinned sibling\n" {
		t.Fatalf("submodule marker = %q, error = %v, want pinned content", content, err)
	}

	if len(rec.requestsForHost("127.0.0.1")) == 0 {
		t.Fatal("expected requests to the scoped parent host")
	}

	subReqs := rec.requestsForHost("localhost")
	if len(subReqs) == 0 {
		t.Fatal("expected submodule requests to the unscoped host")
	}

	for _, req := range subReqs {
		if allowAnyHost {
			_, pass, ok := (&http.Request{Header: http.Header{"Authorization": {req.authorization}}}).BasicAuth()
			if !ok || pass != sentinelGitToken {
				t.Fatalf("submodule host got Authorization %q, want the global token", req.authorization)
			}
		} else if req.authorization != "" {
			t.Fatalf("submodule host received Authorization header %q", req.authorization)
		}
	}
}
