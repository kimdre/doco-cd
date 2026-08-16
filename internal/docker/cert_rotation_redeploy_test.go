package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/webhook"
)

func TestRotateProjectCertificates_SwarmUnsupported(t *testing.T) {
	err := RotateProjectCertificates(context.Background(), nil, map[string]string{}, nil, true)
	if err == nil {
		t.Fatal("expected an error for swarm mode")
	}

	if !errors.Is(err, ErrCertRotationSwarmUnsupported) {
		t.Fatalf("expected ErrCertRotationSwarmUnsupported, got: %v", err)
	}
}

func TestRotateProjectCertificates_MissingLabels(t *testing.T) {
	err := RotateProjectCertificates(context.Background(), nil, map[string]string{}, nil, false)
	if err == nil {
		t.Fatal("expected an error for missing deployment labels")
	}
}

// TestRotateProjectCertificates_ReloadReissuesCertAndRelabels verifies that reloading a rotatable
// deployment's compose project re-resolves its external secrets (reissuing the pki-role
// certificate through the secret provider) and that relabeling the reloaded project stamps fresh
// cert expiry/rotatable labels — the two steps RotateProjectCertificates performs before handing
// off to deployCompose (which requires a live Docker daemon and is out of scope for this unit test).
func TestRotateProjectCertificates_ReloadReissuesCertAndRelabels(t *testing.T) {
	dataMountPath := t.TempDir()
	t.Setenv("DATA_MOUNT_PATH", dataMountPath)
	t.Setenv("DEPLOY_CONFIG_BASE_DIR", "/")

	repoRoot := filepath.Join(dataMountPath, "example.com", "owner", "repo")

	workingDir := filepath.Join(repoRoot, "stacks", "nas", "mtls-app")
	if err := os.MkdirAll(filepath.Join(repoRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(workingDir, 0o755); err != nil {
		t.Fatal(err)
	}

	composePath := filepath.Join(workingDir, "compose.yml")
	createComposeFile(t, composePath, `services:
  app:
    image: busybox:latest
    environment:
      CERT: ${CERT}
`)

	createComposeFile(t, filepath.Join(repoRoot, ".doco-cd.yml"), `name: mtls-app
reference: refs/heads/main
working_dir: stacks/nas/mtls-app
compose_files:
  - compose.yml
external_secrets:
  CERT: "pki-role:pki:my-role:app.example.com"
`)

	ref := composeScheduledServiceRef{
		Project:        "mtls-app",
		Service:        "app",
		WorkingDir:     workingDir,
		ConfigFiles:    []string{composePath},
		RepositoryURL:  "https://example.com/owner/repo",
		DeploymentName: "mtls-app",
		Reference:      "refs/heads/main",
	}

	expiry := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	certPEM := generateTestCertPEM(t, expiry)

	provider := newStubProvider(map[string]string{"CERT": certPEM}, nil)

	project, deployConfig, err := loadComposeScheduledProjectAll(context.Background(), nil, ref, provider)
	if err != nil {
		t.Fatalf("unexpected error reloading project: %v", err)
	}

	svc, err := project.GetService("app")
	if err != nil {
		t.Fatalf("failed to get app service: %v", err)
	}

	if svc.Environment == nil || svc.Environment["CERT"] == nil || *svc.Environment["CERT"] != certPEM {
		t.Fatalf("expected CERT to be re-resolved to the fresh certificate, got %v", svc.Environment["CERT"])
	}

	payload := &webhook.ParsedPayload{Trigger: certRotationTrigger}

	addComposeServiceLabels(project, deployConfig, payload, ref.WorkingDir, "test", time.Now().UTC().Format(time.RFC3339), ComposeVersion, "", "")

	svc, err = project.GetService("app")
	if err != nil {
		t.Fatalf("failed to get app service after relabeling: %v", err)
	}

	if svc.CustomLabels[DocoCDLabels.Deployment.CertRotatable] != "true" {
		t.Fatalf("expected CertRotatable=true label, got %q", svc.CustomLabels[DocoCDLabels.Deployment.CertRotatable])
	}

	if svc.CustomLabels[DocoCDLabels.Deployment.CertExpiry] == "" {
		t.Fatal("expected a non-empty CertExpiry label")
	}
}

// TestServicesUsingRotatableCerts verifies that only services actually consuming a pki-role-backed
// certificate or private key are selected for redeploy, whether the value reaches them via a
// direct environment variable, a config using "content: $VAR", or a config using the native
// "environment: VAR" form (see resolveConfigsEnvironment in compose-go's loader) — and that
// services with no relation to the rotated certificate are excluded.
func TestServicesUsingRotatableCerts(t *testing.T) {
	dataMountPath := t.TempDir()
	t.Setenv("DATA_MOUNT_PATH", dataMountPath)
	t.Setenv("DEPLOY_CONFIG_BASE_DIR", "/")

	repoRoot := filepath.Join(dataMountPath, "example.com", "owner", "repo")

	workingDir := filepath.Join(repoRoot, "stacks", "nas", "mtls-app")
	if err := os.MkdirAll(filepath.Join(repoRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(workingDir, 0o755); err != nil {
		t.Fatal(err)
	}

	composePath := filepath.Join(workingDir, "compose.yml")
	createComposeFile(t, composePath, `services:
  direct-env:
    image: busybox:latest
    environment:
      CERT: ${CERT}
  config-content:
    image: busybox:latest
    configs:
      - source: cert-via-content
  config-environment:
    image: busybox:latest
    configs:
      - source: cert-via-environment
  unrelated:
    image: busybox:latest
    environment:
      FOO: bar
configs:
  cert-via-content:
    content: ${CERT}
  cert-via-environment:
    environment: CERT
`)

	createComposeFile(t, filepath.Join(repoRoot, ".doco-cd.yml"), `name: mtls-app
reference: refs/heads/main
working_dir: stacks/nas/mtls-app
compose_files:
  - compose.yml
external_secrets:
  CERT: "pki-role:pki:my-role:app.example.com"
`)

	ref := composeScheduledServiceRef{
		Project:        "mtls-app",
		Service:        "direct-env",
		WorkingDir:     workingDir,
		ConfigFiles:    []string{composePath},
		RepositoryURL:  "https://example.com/owner/repo",
		DeploymentName: "mtls-app",
		Reference:      "refs/heads/main",
	}

	expiry := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	certPEM := generateTestCertPEM(t, expiry)

	provider := newStubProvider(map[string]string{"CERT": certPEM}, nil)

	project, deployConfig, err := loadComposeScheduledProjectAll(context.Background(), nil, ref, provider)
	if err != nil {
		t.Fatalf("unexpected error reloading project: %v", err)
	}

	got := servicesUsingRotatableCerts(project, deployConfig)

	want := map[string]bool{"direct-env": true, "config-content": true, "config-environment": true}
	gotSet := make(map[string]bool, len(got))

	for _, name := range got {
		gotSet[name] = true
	}

	if len(gotSet) != len(want) {
		t.Fatalf("expected services %v, got %v", want, got)
	}

	for name := range want {
		if !gotSet[name] {
			t.Errorf("expected service %q to be selected for rotation redeploy, got %v", name, got)
		}
	}

	if gotSet["unrelated"] {
		t.Errorf("did not expect unrelated service to be selected for rotation redeploy, got %v", got)
	}
}
