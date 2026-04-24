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

	oldRegistryLookup := registryDigestLookup
	oldDeployedLookup := deployedServiceDigestLookup

	t.Cleanup(func() {
		registryDigestLookup = oldRegistryLookup
		deployedServiceDigestLookup = oldDeployedLookup
	})

	t.Run("no configured images", func(t *testing.T) {
		registryDigestLookup = func(context.Context, command.Cli, string) (string, error) {
			t.Fatal("registry lookup should not be called")
			return "", nil
		}
		deployedServiceDigestLookup = func(context.Context, command.Cli, string, *slog.Logger) (map[string]string, error) {
			t.Fatal("deployed lookup should not be called")
			return nil, nil
		}

		changed, err := HaveDeployedServiceImageDigestsChanged(ctx, nil, &types.Project{
			Name: "test",
			Services: types.Services{
				"web": {Name: "web"},
			},
		}, logger)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if changed {
			t.Fatal("expected changed=false")
		}
	})

	t.Run("returns true when deployed digest missing", func(t *testing.T) {
		registryDigestLookup = func(context.Context, command.Cli, string) (string, error) {
			return "sha256:new", nil
		}
		deployedServiceDigestLookup = func(context.Context, command.Cli, string, *slog.Logger) (map[string]string, error) {
			return map[string]string{}, nil
		}

		changed, err := HaveDeployedServiceImageDigestsChanged(ctx, nil, &types.Project{
			Name: "test",
			Services: types.Services{
				"web": {Name: "web", Image: "nginx:latest"},
			},
		}, logger)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !changed {
			t.Fatal("expected changed=true")
		}
	})

	t.Run("returns true on digest mismatch", func(t *testing.T) {
		registryDigestLookup = func(context.Context, command.Cli, string) (string, error) {
			return "sha256:new", nil
		}
		deployedServiceDigestLookup = func(context.Context, command.Cli, string, *slog.Logger) (map[string]string, error) {
			return map[string]string{"web": "sha256:old"}, nil
		}

		changed, err := HaveDeployedServiceImageDigestsChanged(ctx, nil, &types.Project{
			Name: "test",
			Services: types.Services{
				"web": {Name: "web", Image: "nginx:latest"},
			},
		}, logger)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !changed {
			t.Fatal("expected changed=true")
		}
	})

	t.Run("returns false when digests match", func(t *testing.T) {
		registryDigestLookup = func(context.Context, command.Cli, string) (string, error) {
			return "sha256:same", nil
		}
		deployedServiceDigestLookup = func(context.Context, command.Cli, string, *slog.Logger) (map[string]string, error) {
			return map[string]string{"web": "sha256:same"}, nil
		}

		changed, err := HaveDeployedServiceImageDigestsChanged(ctx, nil, &types.Project{
			Name: "test",
			Services: types.Services{
				"web": {Name: "web", Image: "nginx:latest"},
			},
		}, logger)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if changed {
			t.Fatal("expected changed=false")
		}
	})

	t.Run("returns deployed lookup error", func(t *testing.T) {
		registryDigestLookup = func(context.Context, command.Cli, string) (string, error) {
			return "sha256:same", nil
		}
		expectedErr := errors.New("deployed lookup failed")
		deployedServiceDigestLookup = func(context.Context, command.Cli, string, *slog.Logger) (map[string]string, error) {
			return nil, expectedErr
		}

		_, err := HaveDeployedServiceImageDigestsChanged(ctx, nil, &types.Project{
			Name: "test",
			Services: types.Services{
				"web": {Name: "web", Image: "nginx:latest"},
			},
		}, logger)
		if !errors.Is(err, expectedErr) {
			t.Fatalf("expected error %v, got %v", expectedErr, err)
		}
	})

	t.Run("deduplicates registry lookups by image ref", func(t *testing.T) {
		calls := 0
		registryDigestLookup = func(_ context.Context, _ command.Cli, ref string) (string, error) {
			if ref != "nginx:latest" {
				t.Fatalf("unexpected ref: %s", ref)
			}

			calls++

			return "sha256:same", nil
		}
		deployedServiceDigestLookup = func(context.Context, command.Cli, string, *slog.Logger) (map[string]string, error) {
			return map[string]string{
				"web":    "sha256:same",
				"worker": "sha256:same",
			}, nil
		}

		changed, err := HaveDeployedServiceImageDigestsChanged(ctx, nil, &types.Project{
			Name: "test",
			Services: types.Services{
				"web":    {Name: "web", Image: "nginx:latest"},
				"worker": {Name: "worker", Image: "nginx:latest"},
			},
		}, logger)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if changed {
			t.Fatal("expected changed=false")
		}

		if calls != 1 {
			t.Fatalf("expected 1 registry lookup, got %d", calls)
		}
	})
}
