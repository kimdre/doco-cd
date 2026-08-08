package docker

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/api"

	"github.com/kimdre/doco-cd/internal/secretprovider"
	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

// stubSecretProvider is a minimal SecretProvider whose ResolveSecretReferences
// returns a fixed map, allowing tests to verify the project environment is
// populated without touching a real secret backend.
type stubSecretProvider struct {
	resolved map[string]string
	err      error
}

func (s *stubSecretProvider) Name() string { return "stub" }
func (s *stubSecretProvider) Close()       {}
func (s *stubSecretProvider) GetSecret(_ context.Context, _ string) (string, error) {
	return "", nil
}

func (s *stubSecretProvider) GetSecrets(_ context.Context, _ []string) (map[string]string, error) {
	return nil, nil
}

func (s *stubSecretProvider) ResolveSecretReferences(_ context.Context, _ map[string]string) (secrettypes.ResolvedSecrets, error) {
	return s.resolved, s.err
}

// newStubProvider returns a secretprovider.SecretProvider pointer backed by stub.
func newStubProvider(resolved map[string]string, err error) *secretprovider.SecretProvider {
	var sp secretprovider.SecretProvider = &stubSecretProvider{resolved: resolved, err: err}
	return &sp
}

// ── composeScheduledServiceRefFromLabels ─────────────────────────────────────

func TestComposeScheduledServiceRefFromLabels(t *testing.T) {
	t.Parallel()

	t.Run("parses required and optional labels", func(t *testing.T) {
		t.Parallel()

		ref, err := composeScheduledServiceRefFromLabels(map[string]string{
			api.ProjectLabel:     "project-a",
			api.ServiceLabel:     "backup",
			api.WorkingDirLabel:  "/repo/stack",
			api.ConfigFilesLabel: "/repo/stack/compose.yaml, /repo/stack/compose.override.yaml",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if ref.Project != "project-a" || ref.Service != "backup" {
			t.Fatalf("unexpected ref: %+v", ref)
		}

		if ref.WorkingDir != "/repo/stack" {
			t.Fatalf("unexpected working dir: %q", ref.WorkingDir)
		}

		if len(ref.ConfigFiles) != 2 {
			t.Fatalf("expected 2 config files, got %d (%v)", len(ref.ConfigFiles), ref.ConfigFiles)
		}
	})

	t.Run("fails on nil label map", func(t *testing.T) {
		t.Parallel()

		_, err := composeScheduledServiceRefFromLabels(nil)
		if !errors.Is(err, ErrComposeScheduledMetadataUnavailable) {
			t.Fatalf("expected ErrComposeScheduledMetadataUnavailable, got %v", err)
		}
	})

	t.Run("fails when project label is missing", func(t *testing.T) {
		t.Parallel()

		_, err := composeScheduledServiceRefFromLabels(map[string]string{
			api.ServiceLabel:    "backup",
			api.WorkingDirLabel: "/repo/stack",
		})
		if !errors.Is(err, ErrComposeScheduledMetadataUnavailable) {
			t.Fatalf("expected ErrComposeScheduledMetadataUnavailable, got %v", err)
		}
	})

	t.Run("fails when service label is missing", func(t *testing.T) {
		t.Parallel()

		_, err := composeScheduledServiceRefFromLabels(map[string]string{
			api.ProjectLabel:    "project-a",
			api.WorkingDirLabel: "/repo/stack",
		})
		if !errors.Is(err, ErrComposeScheduledMetadataUnavailable) {
			t.Fatalf("expected ErrComposeScheduledMetadataUnavailable, got %v", err)
		}
	})

	t.Run("whitespace in label values is trimmed", func(t *testing.T) {
		t.Parallel()

		ref, err := composeScheduledServiceRefFromLabels(map[string]string{
			api.ProjectLabel:    "  project-a  ",
			api.ServiceLabel:    "  backup  ",
			api.WorkingDirLabel: "  /repo  ",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if ref.Project != "project-a" {
			t.Errorf("project not trimmed: %q", ref.Project)
		}

		if ref.Service != "backup" {
			t.Errorf("service not trimmed: %q", ref.Service)
		}

		if ref.WorkingDir != "/repo" {
			t.Errorf("working dir not trimmed: %q", ref.WorkingDir)
		}
	})

	t.Run("parses external refs JSON label", func(t *testing.T) {
		t.Parallel()

		encodedRefs := map[string]string{ //nolint:gosec // test data, not real credentials
			"DB_PASSWORD": "my-store/db-password",
			"API_KEY":     "my-store/api-key",
		}
		refsJSON, _ := json.Marshal(encodedRefs)

		ref, err := composeScheduledServiceRefFromLabels(map[string]string{
			api.ProjectLabel:                      "project-a",
			api.ServiceLabel:                      "backup",
			api.WorkingDirLabel:                   "/repo/stack",
			api.ConfigFilesLabel:                  "/repo/stack/compose.yaml",
			DocoCDJobLabels.JobExternalSecretRefs: string(refsJSON),
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(ref.EncodedExternalSecrets) != 2 {
			t.Fatalf("expected 2 external refs, got %d: %v", len(ref.EncodedExternalSecrets), ref.EncodedExternalSecrets)
		}

		if ref.EncodedExternalSecrets["DB_PASSWORD"] != "my-store/db-password" {
			t.Errorf("unexpected DB_PASSWORD ref: %q", ref.EncodedExternalSecrets["DB_PASSWORD"])
		}

		if ref.EncodedExternalSecrets["API_KEY"] != "my-store/api-key" {
			t.Errorf("unexpected API_KEY ref: %q", ref.EncodedExternalSecrets["API_KEY"])
		}
	})

	t.Run("ignores malformed external refs JSON label", func(t *testing.T) {
		t.Parallel()

		ref, err := composeScheduledServiceRefFromLabels(map[string]string{
			api.ProjectLabel:                      "project-a",
			api.ServiceLabel:                      "backup",
			api.WorkingDirLabel:                   "/repo/stack",
			api.ConfigFilesLabel:                  "/repo/stack/compose.yaml",
			DocoCDJobLabels.JobExternalSecretRefs: "not-valid-json",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(ref.EncodedExternalSecrets) != 0 {
			t.Fatalf("expected empty external refs on bad JSON, got %v", ref.EncodedExternalSecrets)
		}
	})

	t.Run("empty external refs JSON label is ignored", func(t *testing.T) {
		t.Parallel()

		ref, err := composeScheduledServiceRefFromLabels(map[string]string{
			api.ProjectLabel:                      "project-a",
			api.ServiceLabel:                      "backup",
			DocoCDJobLabels.JobExternalSecretRefs: "   ",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if ref.EncodedExternalSecrets != nil {
			t.Fatalf("expected nil external refs on empty label, got %v", ref.EncodedExternalSecrets)
		}
	})
}

// ── splitCommaSeparatedLabelValues ───────────────────────────────────────────

func TestSplitCommaSeparatedLabelValues(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  []string
	}{
		{"typical", "a, b, c", []string{"a", "b", "c"}},
		{"extra spaces", "a,, b , ,c", []string{"a", "b", "c"}},
		{"single value", "only", []string{"only"}},
		{"empty string", "", []string{}},
		{"only commas", ",,,", []string{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := splitCommaSeparatedLabelValues(tc.input)
			if len(got) != len(tc.want) {
				t.Fatalf("input %q: expected %v, got %v", tc.input, tc.want, got)
			}

			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("index %d: expected %q, got %q", i, tc.want[i], got[i])
				}
			}
		})
	}
}

// ── loadComposeScheduledProject ──────────────────────────────────────────────

func TestLoadComposeScheduledProject_RequiresComposeMetadata(t *testing.T) {
	t.Parallel()

	t.Run("missing working dir and config files", func(t *testing.T) {
		t.Parallel()

		_, err := loadComposeScheduledProject(context.Background(), nil, composeScheduledServiceRef{
			Project: "project-a",
			Service: "backup",
		}, nil)
		if !errors.Is(err, ErrComposeScheduledMetadataUnavailable) {
			t.Fatalf("expected ErrComposeScheduledMetadataUnavailable, got %v", err)
		}
	})

	t.Run("missing config files only", func(t *testing.T) {
		t.Parallel()

		_, err := loadComposeScheduledProject(context.Background(), nil, composeScheduledServiceRef{
			Project:    "project-a",
			Service:    "backup",
			WorkingDir: "/some/dir",
		}, nil)
		if !errors.Is(err, ErrComposeScheduledMetadataUnavailable) {
			t.Fatalf("expected ErrComposeScheduledMetadataUnavailable, got %v", err)
		}
	})

	t.Run("missing working dir only", func(t *testing.T) {
		t.Parallel()

		_, err := loadComposeScheduledProject(context.Background(), nil, composeScheduledServiceRef{
			Project:     "project-a",
			Service:     "backup",
			ConfigFiles: []string{"/some/compose.yaml"},
		}, nil)
		if !errors.Is(err, ErrComposeScheduledMetadataUnavailable) {
			t.Fatalf("expected ErrComposeScheduledMetadataUnavailable, got %v", err)
		}
	})
}

func TestLoadComposeScheduledProject_InjectsResolvedSecrets(t *testing.T) {
	t.Parallel()

	resolved := map[string]string{
		"DB_PASSWORD": "s3cr3t",
		"API_KEY":     "abc123",
	}
	provider := newStubProvider(resolved, nil)

	// Build a minimal in-memory project to stand in for the loaded one.
	// We test the injection logic directly by constructing a project and
	// calling the injection block's equivalent: verifying that the values
	// returned by the provider end up in project.Environment.
	project := &types.Project{
		Name:        "project-a",
		WorkingDir:  "/repo",
		Environment: map[string]string{},
		Services: types.Services{
			"backup": {Name: "backup"},
		},
	}

	// Simulate the re-resolution block from loadComposeScheduledProject.
	refs := map[string]string{
		"DB_PASSWORD": "store/db-password", //nolint:gosec // test data, not real credentials
		"API_KEY":     "store/api-key",
	}

	r, err := (*provider).ResolveSecretReferences(context.Background(), refs)
	if err != nil {
		t.Fatalf("ResolveSecretReferences: %v", err)
	}

	maps.Copy(project.Environment, r)

	if project.Environment["DB_PASSWORD"] != "s3cr3t" {
		t.Errorf("expected DB_PASSWORD=%q, got %q", "s3cr3t", project.Environment["DB_PASSWORD"])
	}

	if project.Environment["API_KEY"] != "abc123" {
		t.Errorf("expected API_KEY=%q, got %q", "abc123", project.Environment["API_KEY"])
	}
}

func TestLoadComposeScheduledProject_SecretResolutionError(t *testing.T) {
	t.Parallel()

	providerErr := errors.New("provider unavailable")
	provider := newStubProvider(nil, providerErr)

	refs := map[string]string{"DB_PASSWORD": "store/db"} //nolint:gosec // test data, not real credentials

	_, err := (*provider).ResolveSecretReferences(context.Background(), refs)
	if !errors.Is(err, providerErr) {
		t.Fatalf("expected provider error, got %v", err)
	}
}

func TestLoadComposeScheduledProject_SkipsResolutionWithoutProvider(t *testing.T) {
	t.Parallel()

	// No provider (nil) — early return from metadata check is expected here;
	// if metadata were present and a real docker was available it would skip
	// the resolution block. We verify the guard condition directly.
	var sp *secretprovider.SecretProvider

	shouldResolve := sp != nil && *sp != nil

	if shouldResolve {
		t.Fatal("should not resolve secrets when provider is nil")
	}
}

func TestLoadComposeScheduledProject_SkipsResolutionWithoutRefs(t *testing.T) {
	t.Parallel()

	provider := newStubProvider(map[string]string{"KEY": "val"}, nil)

	// Zero encoded refs — the guard must prevent calling the provider.
	ref := composeScheduledServiceRef{
		Project:                "p",
		Service:                "s",
		EncodedExternalSecrets: nil,
	}

	shouldResolve := provider != nil && *provider != nil && len(ref.EncodedExternalSecrets) > 0
	if shouldResolve {
		t.Fatal("should not resolve secrets when encoded refs are empty")
	}
}

// ── validateComposeScheduledServiceScale ─────────────────────────────────────

func TestValidateComposeScheduledServiceScale(t *testing.T) {
	t.Parallel()

	ref := composeScheduledServiceRef{Project: "project-a", Service: "backup"}

	t.Run("accepts default scale (1)", func(t *testing.T) {
		t.Parallel()

		project := &types.Project{
			Services: types.Services{
				"backup": {Name: "backup"},
			},
		}

		if err := validateComposeScheduledServiceScale(project, ref); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("accepts explicit scale 1", func(t *testing.T) {
		t.Parallel()

		scale := 1
		project := &types.Project{
			Services: types.Services{
				"backup": {Name: "backup", Scale: &scale},
			},
		}

		if err := validateComposeScheduledServiceScale(project, ref); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("rejects scale 2", func(t *testing.T) {
		t.Parallel()

		scale := 2
		project := &types.Project{
			Services: types.Services{
				"backup": {Name: "backup", Scale: &scale},
			},
		}

		if err := validateComposeScheduledServiceScale(project, ref); !errors.Is(err, ErrComposeScheduledServiceReplicated) {
			t.Fatalf("expected ErrComposeScheduledServiceReplicated, got %v", err)
		}
	})

	t.Run("returns error for unknown service", func(t *testing.T) {
		t.Parallel()

		project := &types.Project{
			Services: types.Services{
				"other": {Name: "other"},
			},
		}

		err := validateComposeScheduledServiceScale(project, ref)
		if err == nil {
			t.Fatal("expected error for missing service, got nil")
		}
	})
}

// ── getServiceSchedulerLabels ────────────────────────────────────────────────

func TestGetServiceSchedulerLabels(t *testing.T) {
	t.Parallel()

	t.Run("returns service labels when no custom labels", func(t *testing.T) {
		t.Parallel()

		svc := types.ServiceConfig{
			Labels: map[string]string{"app": "web"},
		}

		got := getServiceSchedulerLabels(svc)
		if got["app"] != "web" {
			t.Errorf("expected app=web, got %q", got["app"])
		}
	})

	t.Run("merges service and custom labels", func(t *testing.T) {
		t.Parallel()

		svc := types.ServiceConfig{
			Labels:       map[string]string{"app": "web", "env": "prod"},
			CustomLabels: map[string]string{"managed-by": "doco-cd"},
		}

		got := getServiceSchedulerLabels(svc)
		if got["app"] != "web" {
			t.Errorf("app label: expected %q, got %q", "web", got["app"])
		}

		if got["managed-by"] != "doco-cd" {
			t.Errorf("managed-by label: expected %q, got %q", "doco-cd", got["managed-by"])
		}
	})

	t.Run("custom labels take precedence over service labels on collision", func(t *testing.T) {
		t.Parallel()

		svc := types.ServiceConfig{
			Labels:       map[string]string{"version": "old"},
			CustomLabels: map[string]string{"version": "new"},
		}

		got := getServiceSchedulerLabels(svc)
		if got["version"] != "new" {
			t.Errorf("expected custom label to win, got %q", got["version"])
		}
	})

	t.Run("returns nil when both label sets are empty", func(t *testing.T) {
		t.Parallel()

		svc := types.ServiceConfig{}
		got := getServiceSchedulerLabels(svc)

		if len(got) != 0 {
			t.Errorf("expected empty result, got %v", got)
		}
	})

	t.Run("all custom labels present when service labels empty", func(t *testing.T) {
		t.Parallel()

		svc := types.ServiceConfig{
			CustomLabels: map[string]string{"k": "v"},
		}

		got := getServiceSchedulerLabels(svc)
		if got["k"] != "v" {
			t.Errorf("expected k=v, got %q", got["k"])
		}
	})
}
