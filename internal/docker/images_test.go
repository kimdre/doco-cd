package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/config/configfile"
)

func TestDigestFromReference(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ref  string
		want string
	}{
		{name: "reference with digest", ref: "nginx@sha256:abc", want: "sha256:abc"},
		{name: "reference without digest", ref: "nginx:latest", want: ""},
		{name: "empty", ref: "", want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := digestFromReference(tc.ref); got != tc.want {
				t.Fatalf("digestFromReference() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDigestFromRepoDigests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		repoDigests []string
		want        string
	}{
		{name: "first valid digest", repoDigests: []string{"nginx@sha256:abc", "nginx@sha256:def"}, want: "sha256:abc"},
		{name: "skips invalid entry", repoDigests: []string{"not-a-digest", "nginx@sha256:def"}, want: "sha256:def"},
		{name: "none", repoDigests: []string{"not-a-digest"}, want: ""},
		{name: "empty", repoDigests: nil, want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := digestFromRepoDigests(tc.repoDigests); got != tc.want {
				t.Fatalf("digestFromRepoDigests() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRegistryManifestURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		imageRef    string
		wantURL     string
		wantAuthKey string
		wantScope   string
	}{
		{
			name:        "gcr.io",
			imageRef:    "gcr.io/google-containers/pause:3.9",
			wantURL:     "https://gcr.io/v2/google-containers/pause/manifests/3.9",
			wantAuthKey: "gcr.io",
			wantScope:   "repository:google-containers/pause:pull",
		},
		{
			name:        "ghcr.io",
			imageRef:    "ghcr.io/octo-org/octo-image:1.2.3",
			wantURL:     "https://ghcr.io/v2/octo-org/octo-image/manifests/1.2.3",
			wantAuthKey: "ghcr.io",
			wantScope:   "repository:octo-org/octo-image:pull",
		},
		{
			name:        "registry.gitlab.com",
			imageRef:    "registry.gitlab.com/group/project/image:latest",
			wantURL:     "https://registry.gitlab.com/v2/group/project/image/manifests/latest",
			wantAuthKey: "registry.gitlab.com",
			wantScope:   "repository:group/project/image:pull",
		},
		{
			name:        "docker.io maps to Docker Hub registry host and auth key",
			imageRef:    "docker.io/library/nginx:latest",
			wantURL:     "https://registry-1.docker.io/v2/library/nginx/manifests/latest",
			wantAuthKey: "https://index.docker.io/v1/",
			wantScope:   "repository:library/nginx:pull",
		},
		{
			name:        "index.docker.io maps to Docker Hub registry host and auth key",
			imageRef:    "index.docker.io/library/nginx:latest",
			wantURL:     "https://registry-1.docker.io/v2/library/nginx/manifests/latest",
			wantAuthKey: "https://index.docker.io/v1/",
			wantScope:   "repository:library/nginx:pull",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gotURL, gotAuthKey, gotScope, err := registryManifestURL(tc.imageRef)
			if err != nil {
				t.Fatalf("registryManifestURL(%q) error = %v", tc.imageRef, err)
			}

			if gotURL != tc.wantURL {
				t.Fatalf("registryManifestURL(%q) url = %q, want %q", tc.imageRef, gotURL, tc.wantURL)
			}

			if gotAuthKey != tc.wantAuthKey {
				t.Fatalf("registryManifestURL(%q) auth key = %q, want %q", tc.imageRef, gotAuthKey, tc.wantAuthKey)
			}

			if gotScope != tc.wantScope {
				t.Fatalf("registryManifestURL(%q) scope = %q, want %q", tc.imageRef, gotScope, tc.wantScope)
			}
		})
	}
}

func TestHaveDeployedServiceImageDigestsChanged(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	expectedLookupErr := errors.New("deployed lookup failed")

	oldRegistryLookup := registryDigestLookup
	oldDeployedLookup := deployedServiceDigestLookup

	t.Cleanup(func() {
		registryDigestLookup = oldRegistryLookup
		deployedServiceDigestLookup = oldDeployedLookup
	})

	tests := []struct {
		name                 string
		project              *types.Project
		registryDigest       string
		deployedDigests      map[string]string
		deployedLookupErr    error
		wantChanged          bool
		wantErr              error
		wantRegistryCalls    int
		wantRegistryCallRefs []string
	}{
		{
			name: "no configured images",
			project: &types.Project{
				Name: "test",
				Services: types.Services{
					"web": {Name: "web"},
				},
			},
			wantChanged:       false,
			wantRegistryCalls: 0,
		},
		{
			name: "returns true when deployed digest missing",
			project: &types.Project{
				Name: "test",
				Services: types.Services{
					"web": {Name: "web", Image: "nginx:latest"},
				},
			},
			registryDigest:       "sha256:new",
			deployedDigests:      map[string]string{},
			wantChanged:          true,
			wantRegistryCalls:    1,
			wantRegistryCallRefs: []string{"nginx:latest"},
		},
		{
			name: "returns true on digest mismatch",
			project: &types.Project{
				Name: "test",
				Services: types.Services{
					"web": {Name: "web", Image: "nginx:latest"},
				},
			},
			registryDigest:       "sha256:new",
			deployedDigests:      map[string]string{"web": "sha256:old"},
			wantChanged:          true,
			wantRegistryCalls:    1,
			wantRegistryCallRefs: []string{"nginx:latest"},
		},
		{
			name: "returns false when digests match",
			project: &types.Project{
				Name: "test",
				Services: types.Services{
					"web": {Name: "web", Image: "nginx:latest"},
				},
			},
			registryDigest:       "sha256:same",
			deployedDigests:      map[string]string{"web": "sha256:same"},
			wantChanged:          false,
			wantRegistryCalls:    1,
			wantRegistryCallRefs: []string{"nginx:latest"},
		},
		{
			name: "returns deployed lookup error",
			project: &types.Project{
				Name: "test",
				Services: types.Services{
					"web": {Name: "web", Image: "nginx:latest"},
				},
			},
			registryDigest:       "sha256:same",
			deployedLookupErr:    expectedLookupErr,
			wantErr:              expectedLookupErr,
			wantRegistryCalls:    1,
			wantRegistryCallRefs: []string{"nginx:latest"},
		},
		{
			name: "deduplicates registry lookups by image ref",
			project: &types.Project{
				Name: "test",
				Services: types.Services{
					"web":    {Name: "web", Image: "nginx:latest"},
					"worker": {Name: "worker", Image: "nginx:latest"},
				},
			},
			registryDigest:       "sha256:same",
			deployedDigests:      map[string]string{"web": "sha256:same", "worker": "sha256:same"},
			wantChanged:          false,
			wantRegistryCalls:    1,
			wantRegistryCallRefs: []string{"nginx:latest"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			registryCalls := make([]string, 0)
			registryDigestLookup = func(_ context.Context, _ command.Cli, ref string) (string, error) {
				registryCalls = append(registryCalls, ref)
				return tc.registryDigest, nil
			}
			deployedServiceDigestLookup = func(context.Context, command.Cli, string, *slog.Logger) (map[string]string, error) {
				if tc.deployedLookupErr != nil {
					return nil, tc.deployedLookupErr
				}

				return tc.deployedDigests, nil
			}

			changed, err := HaveDeployedServiceImageDigestsChanged(ctx, nil, tc.project, logger)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected error %v, got %v", tc.wantErr, err)
			}

			if changed != tc.wantChanged {
				t.Fatalf("changed = %v, want %v", changed, tc.wantChanged)
			}

			if len(registryCalls) != tc.wantRegistryCalls {
				t.Fatalf("expected %d registry lookup(s), got %d", tc.wantRegistryCalls, len(registryCalls))
			}

			for i, wantRef := range tc.wantRegistryCallRefs {
				if i >= len(registryCalls) {
					t.Fatalf("missing registry lookup ref at index %d, want %q", i, wantRef)
				}

				if registryCalls[i] != wantRef {
					t.Fatalf("registry lookup ref at index %d = %q, want %q", i, registryCalls[i], wantRef)
				}
			}
		})
	}
}

func TestRegistryDigestForRefPrefersHEAD(t *testing.T) {
	t.Parallel()

	oldHeadLookup := registryDigestHeadLookup
	oldDistributionLookup := registryDigestDistributionLookup

	t.Cleanup(func() {
		registryDigestHeadLookup = oldHeadLookup
		registryDigestDistributionLookup = oldDistributionLookup
	})

	registryDigestHeadLookup = func(context.Context, command.Cli, string) (string, error) {
		return "sha256:head", nil
	}
	registryDigestDistributionLookup = func(context.Context, command.Cli, string) (string, error) {
		t.Fatal("distribution inspect fallback should not be called")
		return "", nil
	}

	got, err := registryDigestForRef(context.Background(), nil, "nginx:latest")
	if err != nil {
		t.Fatalf("registryDigestForRef() unexpected error: %v", err)
	}

	if got != "sha256:head" {
		t.Fatalf("registryDigestForRef() = %q, want %q", got, "sha256:head")
	}
}

func TestRegistryDigestForRefFallsBackToDistributionInspect(t *testing.T) {
	t.Parallel()

	oldHeadLookup := registryDigestHeadLookup
	oldDistributionLookup := registryDigestDistributionLookup

	t.Cleanup(func() {
		registryDigestHeadLookup = oldHeadLookup
		registryDigestDistributionLookup = oldDistributionLookup
	})

	registryDigestHeadLookup = func(context.Context, command.Cli, string) (string, error) {
		return "", errors.New("head failed")
	}
	registryDigestDistributionLookup = func(context.Context, command.Cli, string) (string, error) {
		return "sha256:fallback", nil
	}

	got, err := registryDigestForRef(context.Background(), nil, "nginx:latest")
	if err != nil {
		t.Fatalf("registryDigestForRef() unexpected error: %v", err)
	}

	if got != "sha256:fallback" {
		t.Fatalf("registryDigestForRef() = %q, want %q", got, "sha256:fallback")
	}
}

func TestRegistryDigestForRefFallsBackWhenHEADMissingDigestHeader(t *testing.T) {
	t.Parallel()

	// HEAD server returns 200 but omits Docker-Content-Digest, so the HEAD path errors.
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// deliberately omit the Docker-Content-Digest header
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("failed to parse test server URL: %v", err)
	}

	imageRef := parsed.Host + "/team/app:latest"

	// Confirm the HEAD path alone returns an error.
	_, headErr := registryDigestForRefViaHEADWithClient(context.Background(), &configfile.ConfigFile{}, imageRef, server.Client())
	if headErr == nil {
		t.Fatal("expected HEAD lookup to fail without Docker-Content-Digest header, but it succeeded")
	}

	// Now wire up the full registryDigestForRef call:
	// - override the HEAD lookup to use the test server's HTTP client
	// - override the distribution lookup to return a known digest (simulating Docker Engine fallback)
	oldHeadLookup := registryDigestHeadLookup
	oldDistributionLookup := registryDigestDistributionLookup

	t.Cleanup(func() {
		registryDigestHeadLookup = oldHeadLookup
		registryDigestDistributionLookup = oldDistributionLookup
	})

	registryDigestHeadLookup = func(ctx context.Context, _ command.Cli, ref string) (string, error) {
		return registryDigestForRefViaHEADWithClient(ctx, &configfile.ConfigFile{}, ref, server.Client())
	}
	registryDigestDistributionLookup = func(_ context.Context, _ command.Cli, _ string) (string, error) {
		return "sha256:from-distribution-fallback", nil
	}

	got, err := registryDigestForRef(context.Background(), nil, imageRef)
	if err != nil {
		t.Fatalf("registryDigestForRef() unexpected error: %v", err)
	}

	if got != "sha256:from-distribution-fallback" {
		t.Fatalf("registryDigestForRef() = %q, want %q", got, "sha256:from-distribution-fallback")
	}
}

func TestRegistryDigestForRefViaHEADWithClientUsesHEAD(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Fatalf("unexpected method: got %s, want %s", r.Method, http.MethodHead)
		}

		if r.URL.Path != "/v2/team/app/manifests/latest" {
			t.Fatalf("unexpected path: got %s", r.URL.Path)
		}

		w.Header().Set(dockerContentDigest, "sha256:from-head")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("failed to parse test server URL: %v", err)
	}

	imageRef := parsed.Host + "/team/app:latest"

	got, err := registryDigestForRefViaHEADWithClient(context.Background(), &configfile.ConfigFile{}, imageRef, server.Client())
	if err != nil {
		t.Fatalf("registryDigestForRefViaHEADWithClient() unexpected error: %v", err)
	}

	if got != "sha256:from-head" {
		t.Fatalf("registryDigestForRefViaHEADWithClient() = %q, want %q", got, "sha256:from-head")
	}
}

func TestRegistryDigestForRefViaHEADWithClientBearerChallenge(t *testing.T) {
	t.Parallel()

	var tokenURL string

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"registry-token"}`))
		case "/v2/team/app/manifests/latest":
			if !strings.HasPrefix(r.Header.Get("Authorization"), registryAuthBearer+" ") || r.Header.Get("Authorization") != registryAuthBearer+" registry-token" {
				w.Header().Set(wwwAuthenticateHeader, fmt.Sprintf(`Bearer realm=%q,service=%q,scope=%q`, tokenURL, "test-registry", "repository:team/app:pull"))
				w.WriteHeader(http.StatusUnauthorized)

				return
			}

			w.Header().Set(dockerContentDigest, "sha256:from-bearer")
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	tokenURL = server.URL + "/token"

	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("failed to parse test server URL: %v", err)
	}

	imageRef := parsed.Host + "/team/app:latest"

	got, err := registryDigestForRefViaHEADWithClient(context.Background(), &configfile.ConfigFile{}, imageRef, server.Client())
	if err != nil {
		t.Fatalf("registryDigestForRefViaHEADWithClient() unexpected error: %v", err)
	}

	if got != "sha256:from-bearer" {
		t.Fatalf("registryDigestForRefViaHEADWithClient() = %q, want %q", got, "sha256:from-bearer")
	}
}
