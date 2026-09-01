package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/api"

	"github.com/kimdre/doco-cd/internal/config/deploy"
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

// newStubProvider returns a secretprovider.SecretProvider backed by stub.
func newStubProvider(resolved map[string]string, err error) secretprovider.SecretProvider {
	var sp secretprovider.SecretProvider = &stubSecretProvider{resolved: resolved, err: err}
	return sp
}

func TestComposeScheduledServiceRefFromLabels(t *testing.T) {
	t.Parallel()

	t.Run("parses required and optional labels", func(t *testing.T) {
		t.Parallel()

		ref, err := composeScheduledServiceRefFromLabels(map[string]string{
			api.ProjectLabel:                     "project-a",
			api.ServiceLabel:                     "backup",
			api.WorkingDirLabel:                  "/repo/stack",
			api.ConfigFilesLabel:                 "/repo/stack/compose.yaml, /repo/stack/compose.override.yaml",
			DocoCDLabels.Source.Name:             "owner/repo",
			DocoCDLabels.Source.URL:              "https://example.com/owner/repo",
			DocoCDLabels.Deployment.Name:         "stack-a",
			DocoCDLabels.Deployment.ConfigTarget: "nas",
			DocoCDLabels.Deployment.TargetRef:    "refs/heads/main",
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

		// RepositoryURL must come from the full source URL label, not the
		// short "owner/repo" name label, so that git.GetRepoName() can
		// reconstruct a host-qualified repository path.
		if ref.RepositoryURL != "https://example.com/owner/repo" {
			t.Fatalf("unexpected repository url: %q", ref.RepositoryURL)
		}

		if ref.DeploymentName != "stack-a" {
			t.Fatalf("unexpected deployment name: %q", ref.DeploymentName)
		}

		if ref.ConfigTarget != "nas" {
			t.Fatalf("unexpected config target: %q", ref.ConfigTarget)
		}

		if ref.Reference != "refs/heads/main" {
			t.Fatalf("unexpected reference: %q", ref.Reference)
		}
	})

	t.Run("falls back to source name label when source url label is missing", func(t *testing.T) {
		t.Parallel()

		ref, err := composeScheduledServiceRefFromLabels(map[string]string{
			api.ProjectLabel:             "project-a",
			api.ServiceLabel:             "backup",
			DocoCDLabels.Source.Name:     "owner/repo",
			DocoCDLabels.Deployment.Name: "stack-a",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if ref.RepositoryURL != "owner/repo" {
			t.Fatalf("unexpected repository url: %q", ref.RepositoryURL)
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
}

func TestComposeScheduledServiceRefFromSwarmLabels(t *testing.T) {
	t.Parallel()

	t.Run("parses required and optional labels from doco-cd labels only", func(t *testing.T) {
		t.Parallel()

		ref, err := composeScheduledServiceRefFromSwarmLabels(map[string]string{
			DocoCDLabels.Deployment.Name:         "stack-a",
			DocoCDLabels.Deployment.WorkingDir:   "/repo/stack",
			DocoCDLabels.Source.Name:             "owner/repo",
			DocoCDLabels.Source.URL:              "https://example.com/owner/repo",
			DocoCDLabels.Deployment.ConfigTarget: "nas",
			DocoCDLabels.Deployment.TargetRef:    "refs/heads/main",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if ref.Project != "stack-a" || ref.DeploymentName != "stack-a" {
			t.Fatalf("unexpected ref: %+v", ref)
		}

		if ref.Service != "" {
			t.Fatalf("expected empty service for a swarm-derived ref, got %q", ref.Service)
		}

		if ref.WorkingDir != "/repo/stack" {
			t.Fatalf("unexpected working dir: %q", ref.WorkingDir)
		}

		if len(ref.ConfigFiles) != 0 {
			t.Fatalf("expected no config files for a swarm-derived ref, got %v", ref.ConfigFiles)
		}

		if ref.RepositoryURL != "https://example.com/owner/repo" {
			t.Fatalf("unexpected repository url: %q", ref.RepositoryURL)
		}

		if ref.ConfigTarget != "nas" {
			t.Fatalf("unexpected config target: %q", ref.ConfigTarget)
		}

		if ref.Reference != "refs/heads/main" {
			t.Fatalf("unexpected reference: %q", ref.Reference)
		}
	})

	t.Run("falls back to source name label when source url label is missing", func(t *testing.T) {
		t.Parallel()

		ref, err := composeScheduledServiceRefFromSwarmLabels(map[string]string{
			DocoCDLabels.Deployment.Name: "stack-a",
			DocoCDLabels.Source.Name:     "owner/repo",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if ref.RepositoryURL != "owner/repo" {
			t.Fatalf("unexpected repository url: %q", ref.RepositoryURL)
		}
	})

	t.Run("fails on nil label map", func(t *testing.T) {
		t.Parallel()

		_, err := composeScheduledServiceRefFromSwarmLabels(nil)
		if !errors.Is(err, ErrComposeScheduledMetadataUnavailable) {
			t.Fatalf("expected ErrComposeScheduledMetadataUnavailable, got %v", err)
		}
	})

	t.Run("fails when deployment name label is missing", func(t *testing.T) {
		t.Parallel()

		_, err := composeScheduledServiceRefFromSwarmLabels(map[string]string{
			DocoCDLabels.Deployment.WorkingDir: "/repo/stack",
		})
		if !errors.Is(err, ErrComposeScheduledMetadataUnavailable) {
			t.Fatalf("expected ErrComposeScheduledMetadataUnavailable, got %v", err)
		}
	})
}

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

func TestLoadComposeScheduledProject_RequiresComposeMetadata(t *testing.T) {
	t.Parallel()

	t.Run("missing working dir and config files", func(t *testing.T) {
		t.Parallel()

		_, err := loadComposeScheduledProject(context.Background(), nil, composeScheduledServiceRef{
			Project: "project-a",
			Service: "backup",
		}, nil, ScheduledComposeOptions{})
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
		}, nil, ScheduledComposeOptions{})
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
		}, nil, ScheduledComposeOptions{})
		if !errors.Is(err, ErrComposeScheduledMetadataUnavailable) {
			t.Fatalf("expected ErrComposeScheduledMetadataUnavailable, got %v", err)
		}
	})
}

func TestPrepareComposeScheduledDeployConfig(t *testing.T) {
	t.Parallel()

	t.Run("merges deploy environment and resolved secrets", func(t *testing.T) {
		t.Parallel()

		provider := newStubProvider(map[string]string{
			"SECRET":  "s3cr3t",
			"API_KEY": "abc123",
		}, nil)

		sourceRepoPath := t.TempDir()

		cfg := &deploy.Config{
			Name:             "project-a",
			WorkingDirectory: ".",
			Environment:      map[string]string{"BACKUP_BASE_DIR": "/backups"},
			ExternalSecrets: map[string]secrettypes.ExternalSecretRef{
				"SECRET":  {LegacyRef: "store/secret"},  //nolint:gosec // test data, not real credentials
				"API_KEY": {LegacyRef: "store/api-key"}, //nolint:gosec // test data, not real credentials
			},
		}

		if err := prepareComposeScheduledDeployConfig(context.Background(), cfg, sourceRepoPath, sourceRepoPath, provider, ScheduledComposeOptions{}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if cfg.Internal.Environment["BACKUP_BASE_DIR"] != "/backups" {
			t.Fatalf("expected BACKUP_BASE_DIR to be preserved, got %q", cfg.Internal.Environment["BACKUP_BASE_DIR"])
		}

		if cfg.Internal.Environment["SECRET"] != "s3cr3t" {
			t.Fatalf("expected SECRET from provider, got %q", cfg.Internal.Environment["SECRET"])
		}

		if cfg.Internal.Environment["API_KEY"] != "abc123" {
			t.Fatalf("expected API_KEY from provider, got %q", cfg.Internal.Environment["API_KEY"])
		}
	})

	t.Run("returns provider errors", func(t *testing.T) {
		t.Parallel()

		providerErr := errors.New("provider unavailable")
		provider := newStubProvider(nil, providerErr)

		cfg := &deploy.Config{
			Name:             "project-a",
			WorkingDirectory: ".",
			ExternalSecrets: map[string]secrettypes.ExternalSecretRef{
				"DB_PASSWORD": {LegacyRef: "store/db"}, //nolint:gosec // test data, not real credentials
			},
		}

		err := prepareComposeScheduledDeployConfig(context.Background(), cfg, t.TempDir(), t.TempDir(), provider, ScheduledComposeOptions{})
		if !errors.Is(err, providerErr) {
			t.Fatalf("expected provider error, got %v", err)
		}
	})

	t.Run("loads dotenv values for interpolation", func(t *testing.T) {
		t.Parallel()

		repoPath := t.TempDir()
		if err := os.WriteFile(filepath.Join(repoPath, ".env"), []byte("BACKUP_BASE_DIR=/backups\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		cfg := &deploy.Config{
			Name:             "project-a",
			WorkingDirectory: ".",
			EnvFiles:         []string{".env"},
		}

		if err := prepareComposeScheduledDeployConfig(context.Background(), cfg, repoPath, repoPath, nil, ScheduledComposeOptions{}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if cfg.Internal.Environment["BACKUP_BASE_DIR"] != "/backups" {
			t.Fatalf("expected dotenv interpolation env, got %q", cfg.Internal.Environment["BACKUP_BASE_DIR"])
		}
	})
}

func TestLoadComposeScheduledProject_InterpolatesDeployConfigEnvironment(t *testing.T) {
	dataMountPath := t.TempDir()
	opts := ScheduledComposeOptions{
		ComposeLoad:         ComposeLoadOptions{DataMountPath: dataMountPath},
		DeployConfigBaseDir: "/",
	}

	repoRoot := filepath.Join(dataMountPath, "example.com", "owner", "repo")

	workingDir := filepath.Join(repoRoot, "stacks", "nas", "adguard-dns")
	if err := os.MkdirAll(workingDir, 0o755); err != nil {
		t.Fatal(err)
	}

	composePath := filepath.Join(workingDir, "compose.yml")
	createComposeFile(t, composePath, `services:
  backup:
    image: busybox:latest
    volumes:
      - ${BACKUP_BASE_DIR}/adguard-dns:/backup
`)

	createComposeFile(t, filepath.Join(repoRoot, ".doco-cd.yml"), `name: adguard-dns
reference: refs/heads/main
working_dir: stacks/nas/adguard-dns
compose_files:
  - compose.yml
environment:
  BACKUP_BASE_DIR: /backups
`)

	project, err := loadComposeScheduledProject(context.Background(), nil, composeScheduledServiceRef{
		Project:        "adguard-dns",
		Service:        "backup",
		WorkingDir:     workingDir,
		ConfigFiles:    []string{composePath},
		RepositoryURL:  "https://example.com/owner/repo",
		DeploymentName: "adguard-dns",
		Reference:      "refs/heads/main",
	}, nil, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	svc, err := project.GetService("backup")
	if err != nil {
		t.Fatalf("failed to get backup service: %v", err)
	}

	if len(svc.Volumes) != 1 {
		t.Fatalf("expected 1 volume, got %d", len(svc.Volumes))
	}

	if svc.Volumes[0].Source != "/backups/adguard-dns" {
		t.Fatalf("expected interpolated backup source, got %q", svc.Volumes[0].Source)
	}
}

// TestLoadComposeScheduledProject_FallsBackWhenLabeledComposeFileIsStale is a
// regression test for https://github.com/kimdre/doco-cd/issues/1737.
func TestLoadComposeScheduledProject_FallsBackWhenLabeledComposeFileIsStale(t *testing.T) {
	dataMountPath := t.TempDir()
	opts := ScheduledComposeOptions{
		ComposeLoad:         ComposeLoadOptions{DataMountPath: dataMountPath},
		DeployConfigBaseDir: "/",
	}

	repoRoot := filepath.Join(dataMountPath, "example.com", "owner", "repo")

	workingDir := filepath.Join(repoRoot, "apps", "imap-backup")
	if err := os.MkdirAll(workingDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Only the renamed file exists; the label still points at the old name.
	createComposeFile(t, filepath.Join(workingDir, "compose.yaml"), `services:
  backup:
    image: busybox:latest
`)

	createComposeFile(t, filepath.Join(repoRoot, ".doco-cd.yml"), `name: imap-backup
reference: refs/heads/main
working_dir: apps/imap-backup
compose_files:
  - compose.yaml
`)

	staleComposePath := filepath.Join(workingDir, "docker-compose.yml")

	project, err := loadComposeScheduledProject(context.Background(), nil, composeScheduledServiceRef{
		Project:        "imap-backup",
		Service:        "backup",
		WorkingDir:     workingDir,
		ConfigFiles:    []string{staleComposePath},
		RepositoryURL:  "https://example.com/owner/repo",
		DeploymentName: "imap-backup",
		Reference:      "refs/heads/main",
	}, nil, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := project.GetService("backup"); err != nil {
		t.Fatalf("failed to get backup service: %v", err)
	}
}

// TestLoadComposeScheduledProject_ResolvesExternalSecrets is a regression test
// for https://github.com/kimdre/doco-cd/issues/1674: external secrets must be
// re-resolved and interpolated into the compose service environment when a
// standard (single-repository, non `repository_url`) scheduled job reloads
// its deploy config at run time.
func TestLoadComposeScheduledProject_ResolvesExternalSecrets(t *testing.T) {
	dataMountPath := t.TempDir()
	opts := ScheduledComposeOptions{
		ComposeLoad:         ComposeLoadOptions{DataMountPath: dataMountPath},
		DeployConfigBaseDir: "/",
	}

	repoRoot := filepath.Join(dataMountPath, "example.com", "owner", "repo")

	workingDir := filepath.Join(repoRoot, "stacks", "nas", "backup")
	if err := os.MkdirAll(filepath.Join(repoRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(workingDir, 0o755); err != nil {
		t.Fatal(err)
	}

	composePath := filepath.Join(workingDir, "compose.yml")
	createComposeFile(t, composePath, `services:
  backup:
    image: busybox:latest
    environment:
      MY_SECRET: ${MY_SECRET}
`)

	createComposeFile(t, filepath.Join(repoRoot, ".doco-cd.yml"), `name: backup-job
reference: refs/heads/main
working_dir: stacks/nas/backup
compose_files:
  - compose.yml
external_secrets:
  MY_SECRET:
    store_ref: bitwarden-login
    remote_ref:
      key: my-bitwarden-item-id
      property: password
`)

	project, err := loadComposeScheduledProject(context.Background(), nil, composeScheduledServiceRef{
		Project:        "backup-job",
		Service:        "backup",
		WorkingDir:     workingDir,
		ConfigFiles:    []string{composePath},
		RepositoryURL:  "https://example.com/owner/repo",
		DeploymentName: "backup-job",
		Reference:      "refs/heads/main",
	}, newStubProvider(map[string]string{"MY_SECRET": "resolved-secret"}, nil), opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	svc, err := project.GetService("backup")
	if err != nil {
		t.Fatalf("failed to get backup service: %v", err)
	}

	if svc.Environment == nil || svc.Environment["MY_SECRET"] == nil {
		t.Fatal("expected MY_SECRET to be present in service environment")
	}

	if *svc.Environment["MY_SECRET"] != "resolved-secret" {
		t.Fatalf("expected MY_SECRET to be resolved from external secret provider, got %q", *svc.Environment["MY_SECRET"])
	}
}

// TestLoadComposeScheduledProject_InterpolateExternalSecretsHonoredIndependentOfEnvVar
// proves that legacy external secret reference interpolation is controlled solely by
// ScheduledComposeOptions.InterpolateExternalSecrets, not by reading the
// INTERPOLATE_EXTERNAL_SECRETS environment variable directly. A legacy reference containing
// an unset variable is left untouched (and so resolves without error) when the option is
// false, even if a stale INTERPOLATE_EXTERNAL_SECRETS=true is set in the environment; it only
// fails interpolation (as expected) when the option is explicitly enabled.
func TestLoadComposeScheduledProject_InterpolateExternalSecretsHonoredIndependentOfEnvVar(t *testing.T) {
	dataMountPath := t.TempDir()

	repoRoot := filepath.Join(dataMountPath, "example.com", "owner", "repo")
	workingDir := filepath.Join(repoRoot, "stacks", "nas", "backup")

	if err := os.MkdirAll(filepath.Join(repoRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(workingDir, 0o755); err != nil {
		t.Fatal(err)
	}

	composePath := filepath.Join(workingDir, "compose.yml")
	createComposeFile(t, composePath, `services:
  backup:
    image: busybox:latest
    environment:
      MY_SECRET: ${MY_SECRET}
`)

	// The legacy reference contains an unguarded variable that is never set in the
	// process environment; interpolating it must fail because it becomes a
	// required-but-absent variable.
	createComposeFile(t, filepath.Join(repoRoot, ".doco-cd.yml"), `name: backup-job
reference: refs/heads/main
working_dir: stacks/nas/backup
compose_files:
  - compose.yml
external_secrets:
  MY_SECRET: item-$UNSET_SECRET_SUFFIX
`)

	ref := composeScheduledServiceRef{
		Project:        "backup-job",
		Service:        "backup",
		WorkingDir:     workingDir,
		ConfigFiles:    []string{composePath},
		RepositoryURL:  "https://example.com/owner/repo",
		DeploymentName: "backup-job",
		Reference:      "refs/heads/main",
	}
	provider := newStubProvider(map[string]string{"MY_SECRET": "resolved-secret"}, nil)

	// A stale INTERPOLATE_EXTERNAL_SECRETS=true must have no effect: production code no
	// longer reads this environment variable, only the explicit
	// ScheduledComposeOptions.InterpolateExternalSecrets field.
	t.Setenv("INTERPOLATE_EXTERNAL_SECRETS", "true")

	disabledOpts := ScheduledComposeOptions{
		ComposeLoad:                ComposeLoadOptions{DataMountPath: dataMountPath},
		DeployConfigBaseDir:        "/",
		InterpolateExternalSecrets: false,
	}

	if _, err := loadComposeScheduledProject(context.Background(), nil, ref, provider, disabledOpts); err != nil {
		t.Fatalf("expected no error with InterpolateExternalSecrets=false despite INTERPOLATE_EXTERNAL_SECRETS=true, got: %v", err)
	}

	enabledOpts := disabledOpts
	enabledOpts.InterpolateExternalSecrets = true

	if _, err := loadComposeScheduledProject(context.Background(), nil, ref, provider, enabledOpts); err == nil {
		t.Fatal("expected an error with InterpolateExternalSecrets=true and an unset reference variable, got nil")
	}
}

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

func TestPrepareComposeProjectForOneOffRun(t *testing.T) {
	t.Parallel()

	t.Run("marks target service as ephemeral without mutating input project", func(t *testing.T) {
		t.Parallel()

		project := &types.Project{
			Name:         "adguard-dns",
			WorkingDir:   "/repo/stacks/nas/adguard-dns",
			ComposeFiles: []string{"/repo/stacks/nas/adguard-dns/compose.yml"},
			Services: types.Services{
				"backup": {
					Name:         "backup",
					Labels:       map[string]string{"plain": "label"},
					CustomLabels: map[string]string{DocoCDJobLabels.JobEnabled: "true"},
				},
			},
		}

		got, err := prepareComposeProjectForOneOffRun(project, "backup")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if got == project {
			t.Fatal("expected returned project copy, got original pointer")
		}

		if got.Services["backup"].Labels[DocoCDJobLabels.JobEphemeral] != "true" {
			t.Fatalf("expected service labels to mark one-off run as ephemeral, got %q",
				got.Services["backup"].Labels[DocoCDJobLabels.JobEphemeral])
		}

		if got.Services["backup"].CustomLabels[DocoCDJobLabels.JobEphemeral] != "true" {
			t.Fatalf("expected custom labels to mark one-off run as ephemeral, got %q",
				got.Services["backup"].CustomLabels[DocoCDJobLabels.JobEphemeral])
		}

		if got.Services["backup"].CustomLabels[api.ProjectLabel] != "adguard-dns" {
			t.Fatalf("expected compose project label, got %q",
				got.Services["backup"].CustomLabels[api.ProjectLabel])
		}

		if got.Services["backup"].CustomLabels[api.ServiceLabel] != "backup" {
			t.Fatalf("expected compose service label, got %q",
				got.Services["backup"].CustomLabels[api.ServiceLabel])
		}

		if got.Services["backup"].CustomLabels[api.WorkingDirLabel] != "/repo/stacks/nas/adguard-dns" {
			t.Fatalf("expected working dir label, got %q",
				got.Services["backup"].CustomLabels[api.WorkingDirLabel])
		}

		if got.Services["backup"].CustomLabels[api.ConfigFilesLabel] != "/repo/stacks/nas/adguard-dns/compose.yml" {
			t.Fatalf("expected config files label, got %q",
				got.Services["backup"].CustomLabels[api.ConfigFilesLabel])
		}

		if _, ok := project.Services["backup"].Labels[DocoCDJobLabels.JobEphemeral]; ok {
			t.Fatal("input project labels were mutated")
		}

		if _, ok := project.Services["backup"].CustomLabels[DocoCDJobLabels.JobEphemeral]; ok {
			t.Fatal("input project custom labels were mutated")
		}

		if _, ok := project.Services["backup"].CustomLabels[api.ProjectLabel]; ok {
			t.Fatal("input project compose labels were mutated")
		}
	})

	t.Run("returns error for unknown service", func(t *testing.T) {
		t.Parallel()

		project := &types.Project{
			Services: types.Services{
				"backup": {Name: "backup"},
			},
		}

		_, err := prepareComposeProjectForOneOffRun(project, "missing")
		if err == nil {
			t.Fatal("expected error for missing service, got nil")
		}
	})
}
