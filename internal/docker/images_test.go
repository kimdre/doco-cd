package docker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
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
