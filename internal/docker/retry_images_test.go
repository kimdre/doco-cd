package docker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
)

func TestDesiredProjectImageDigestsInspectWithoutRuntimeResources(t *testing.T) {
	previous := registryDigestLookup

	t.Cleanup(func() { registryDigestLookup = previous })

	var calls int

	registryDigestLookup = func(_ context.Context, _ command.Cli, ref string) (string, error) {
		calls++

		if ref == "unavailable:latest" {
			return "", errors.New("registry unavailable")
		}

		return "sha256:remote", nil
	}
	project := &types.Project{
		Services: types.Services{
			"web":      {Name: "web", Image: "app:latest"},
			"worker":   {Name: "worker", Image: "app:latest"},
			"pinned":   {Name: "pinned", Image: "app@sha256:pinned"},
			"missing":  {Name: "missing", Image: "unavailable:latest"},
			"disabled": {Name: "disabled", Image: "unused:latest", Scale: new(0)},
		},
	}
	got := DesiredProjectImageDigests(t.Context(), nil, project, slog.New(slog.NewTextHandler(io.Discard, nil)))

	want := map[string]string{"web": "sha256:remote", "worker": "sha256:remote", "pinned": "sha256:pinned"}
	if !maps.Equal(got, want) || calls != 2 {
		t.Fatalf("desired digest inspection = %v, calls=%d", got, calls)
	}
}

func TestWithNormalizedEnvValuesStabilizesSecretContentForRetries(t *testing.T) {
	t.Parallel()

	project := func(cert string) *types.Project {
		return &types.Project{
			Services: types.Services{"web": {Name: "web", Image: "app:latest"}},
			Secrets:  types.Secrets{"cert": {Name: "cert", Content: cert}},
		}
	}
	a, b := project("first-cert"), project("rotated-cert")
	normA := WithNormalizedEnvValues(a, map[string]string{"first-cert": "pki-role:issuer:role"})
	normB := WithNormalizedEnvValues(b, map[string]string{"rotated-cert": "pki-role:issuer:role"})

	hashA, err := ProjectHash(normA, "")
	if err != nil {
		t.Fatal(err)
	}

	hashB, err := ProjectHash(normB, "")
	if err != nil {
		t.Fatal(err)
	}

	if hashA != hashB || a.Secrets["cert"].Content != "first-cert" || b.Secrets["cert"].Content != "rotated-cert" {
		t.Fatal("PKI secret rotation changed desired identity or normalization mutated project")
	}
}
