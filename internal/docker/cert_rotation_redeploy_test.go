package docker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/lock"
	"github.com/kimdre/doco-cd/internal/test"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func TestRotateProjectCertificates_MissingLabels(t *testing.T) {
	err := RotateProjectCertificates(context.Background(), "", nil, map[string]string{}, nil, false, CertificateRotationOptions{})
	if err == nil {
		t.Fatal("expected an error for missing deployment labels")
	}
}

// TestPruneSwarmStackRevisions_RetentionHonoredIndependentOfEnvVar proves that Swarm
// config/secret revision retention is controlled solely by
// CertificateRotationOptions.DockerSwarmConfigRetention/DockerSwarmSecretRetention, not by
// reading the DOCKER_SWARM_CONFIG_RETENTION/DOCKER_SWARM_SECRET_RETENTION environment variables
// directly. A stale environment value that would enable pruning must have no effect: when the
// options explicitly disable pruning (-1) and the deploy config has no override, no attempt is
// made to reach the Docker daemon at all. dockerCli is passed as nil here; if the retention
// resolution incorrectly fell back to reading the environment variables (which are set to a
// pruning-enabled value below), calling dockerCli.Client() on the nil interface would panic and
// fail the test.
func TestPruneSwarmStackRevisions_RetentionHonoredIndependentOfEnvVar(t *testing.T) {
	t.Setenv("DOCKER_SWARM_CONFIG_RETENTION", "5")
	t.Setenv("DOCKER_SWARM_SECRET_RETENTION", "5")

	opts := CertificateRotationOptions{
		DockerSwarmConfigRetention: -1,
		DockerSwarmSecretRetention: -1,
	}
	deployConfig := &deploy.Config{}

	pruneSwarmStackRevisions(context.Background(), nil, "stack", deployConfig, opts)
}

func TestRotateProjectCertificates_SwarmMissingLabels(t *testing.T) {
	err := RotateProjectCertificates(context.Background(), "", nil, map[string]string{}, nil, true, CertificateRotationOptions{})
	if err == nil {
		t.Fatal("expected an error for missing deployment labels in swarm mode")
	}
}

// TestRotateProjectCertificates_ContextNamespacesLock verifies that RotateProjectCertificates
// locks using lock.StackKey(contextName, stack), so a rotation on a named Docker context does not
// serialize behind a same-named stack rotation on a different context (or the default context).
// It uses the Swarm-labels path (composeScheduledServiceRefFromSwarmLabels) since it only requires
// the doco-cd deployment name label to reach the lock acquisition, unlike the Compose path which
// also requires the Compose service label.
func TestRotateProjectCertificates_ContextNamespacesLock(t *testing.T) {
	stackName := test.ConvertTestName(t.Name())

	labels := map[string]string{
		DocoCDLabels.Deployment.Name: stackName,
	}

	keyDefault := lock.StackKey("", stackName)
	keyRemote := lock.StackKey("docker01", stackName)

	if keyDefault == keyRemote {
		t.Fatalf("expected different lock keys for default and remote contexts, both got %q", keyDefault)
	}

	lock.LockStack(keyDefault)
	defer lock.UnlockStack(keyDefault)

	// With the default-context lock held, RotateProjectCertificates for the same stack name on a
	// different (remote) context must still be able to acquire its own (different) lock key. It
	// will fail quickly afterwards because the deployment labels carry no working directory, but
	// that failure must happen right after acquiring the lock, not be blocked by it.
	done := make(chan error, 1)

	go func() {
		done <- RotateProjectCertificates(context.Background(), "docker01", nil, labels, nil, true, CertificateRotationOptions{})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error since the deployment labels carry no working directory")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out: rotation for a different context blocked on the default context's lock")
	}
}

// TestRotateProjectCertificates_SwarmReloadFallsBackToConfiguredComposeFiles verifies that
// reloading a rotatable Swarm deployment works from doco-cd's own labels alone: Swarm services
// carry no com.docker.compose.config_files label (docker stack deploy never sets Compose's own
// tracking labels), so composeScheduledServiceRefFromSwarmLabels leaves ConfigFiles empty and
// loadComposeScheduledProjectAll must fall back to the freshly reloaded deploy config's compose
// file list to find the project at all.
func TestRotateProjectCertificates_SwarmReloadFallsBackToConfiguredComposeFiles(t *testing.T) {
	dataMountPath := t.TempDir()
	scheduledOpts := ScheduledComposeOptions{
		ComposeLoad:         ComposeLoadOptions{DataMountPath: dataMountPath},
		DeployConfigBaseDir: "/",
	}

	repoRoot := filepath.Join(dataMountPath, "example.com", "owner", "repo")

	workingDir := filepath.Join(repoRoot, "stacks", "nas", "mtls-app")
	if err := os.MkdirAll(filepath.Join(repoRoot, ".git"), filesystem.PermDir); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(workingDir, filesystem.PermDir); err != nil {
		t.Fatal(err)
	}

	createComposeFile(t, filepath.Join(workingDir, "compose.yml"), `services:
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

	labels := map[string]string{
		DocoCDLabels.Deployment.Name:       "mtls-app",
		DocoCDLabels.Deployment.WorkingDir: workingDir,
		DocoCDLabels.Source.URL:            "https://example.com/owner/repo",
		DocoCDLabels.Deployment.TargetRef:  "refs/heads/main",
	}

	ref, err := composeScheduledServiceRefFromSwarmLabels(labels)
	if err != nil {
		t.Fatalf("unexpected error building swarm ref: %v", err)
	}

	if len(ref.ConfigFiles) != 0 {
		t.Fatalf("expected swarm ref to have no config files, got %v", ref.ConfigFiles)
	}

	expiry := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	certPEM := generateTestCertPEM(t, expiry)

	provider := newStubProvider(map[string]string{"CERT": certPEM}, nil)

	project, _, err := loadComposeScheduledProjectAll(context.Background(), nil, ref, provider, scheduledOpts)
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
}

// TestRotateProjectCertificates_ReloadReissuesCertAndRelabels verifies that reloading a rotatable
// deployment's compose project re-resolves its external secrets (reissuing the pki-role
// certificate through the secret provider) and that relabeling the reloaded project stamps fresh
// cert expiry/rotatable labels. These are the two steps RotateProjectCertificates performs before handing
// off to deployCompose (which requires a live Docker daemon and is out of scope for this unit test).
func TestRotateProjectCertificates_ReloadReissuesCertAndRelabels(t *testing.T) {
	dataMountPath := t.TempDir()
	scheduledOpts := ScheduledComposeOptions{
		ComposeLoad:         ComposeLoadOptions{DataMountPath: dataMountPath},
		DeployConfigBaseDir: "/",
	}

	repoRoot := filepath.Join(dataMountPath, "example.com", "owner", "repo")

	workingDir := filepath.Join(repoRoot, "stacks", "nas", "mtls-app")
	if err := os.MkdirAll(filepath.Join(repoRoot, ".git"), filesystem.PermDir); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(workingDir, filesystem.PermDir); err != nil {
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

	project, deployConfig, err := loadComposeScheduledProjectAll(context.Background(), nil, ref, provider, scheduledOpts)
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

	if svc.CustomLabels[DocoCDLabels.Deployment.CertState] == "" {
		t.Fatal("expected a non-empty CertState label")
	}
}

// TestRotateProjectCertificates_ReloadPreservesConfigTargetLabel verifies that reloading a
// rotatable deployment via a custom config target keeps the resulting deploy config's
// Internal.ConfigTarget (and therefore the cd.doco.deployment.config.target label applied to the
// recreated service) consistent with the target that was used to select the config file. Without
// this, deploy.GetConfigs never sets Internal.ConfigTarget itself, so a rotation redeploy would
// blank out the config target label on the affected service, breaking any subsequent reload that
// relies on that label to find the correct .doco-cd.<target>.yml file.
func TestRotateProjectCertificates_ReloadPreservesConfigTargetLabel(t *testing.T) {
	dataMountPath := t.TempDir()
	scheduledOpts := ScheduledComposeOptions{
		ComposeLoad:         ComposeLoadOptions{DataMountPath: dataMountPath},
		DeployConfigBaseDir: "/",
	}

	repoRoot := filepath.Join(dataMountPath, "example.com", "owner", "repo")

	workingDir := filepath.Join(repoRoot, "stacks", "nas", "mtls-app")
	if err := os.MkdirAll(filepath.Join(repoRoot, ".git"), filesystem.PermDir); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(workingDir, filesystem.PermDir); err != nil {
		t.Fatal(err)
	}

	composePath := filepath.Join(workingDir, "compose.yml")
	createComposeFile(t, composePath, `services:
  app:
    image: busybox:latest
    environment:
      CERT: ${CERT}
`)

	createComposeFile(t, filepath.Join(repoRoot, ".doco-cd.nas.yml"), `name: mtls-app
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
		ConfigTarget:   "nas",
		Reference:      "refs/heads/main",
	}

	expiry := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	certPEM := generateTestCertPEM(t, expiry)

	provider := newStubProvider(map[string]string{"CERT": certPEM}, nil)

	project, deployConfig, err := loadComposeScheduledProjectAll(context.Background(), nil, ref, provider, scheduledOpts)
	if err != nil {
		t.Fatalf("unexpected error reloading project: %v", err)
	}

	if deployConfig.Internal.ConfigTarget != "nas" {
		t.Fatalf("expected reloaded deploy config to keep ConfigTarget %q, got %q", "nas", deployConfig.Internal.ConfigTarget)
	}

	payload := &webhook.ParsedPayload{Trigger: certRotationTrigger}

	addComposeServiceLabels(project, deployConfig, payload, ref.WorkingDir, "test", time.Now().UTC().Format(time.RFC3339), ComposeVersion, "", "")

	svc, err := project.GetService("app")
	if err != nil {
		t.Fatalf("failed to get app service after relabeling: %v", err)
	}

	if svc.CustomLabels[DocoCDLabels.Deployment.ConfigTarget] != "nas" {
		t.Fatalf("expected config target label %q, got %q", "nas", svc.CustomLabels[DocoCDLabels.Deployment.ConfigTarget])
	}
}

// TestServicesUsingRotatableCerts verifies that only services actually consuming a pki-role-backed
// certificate or private key are selected for redeploy, whether the value reaches them via a
// direct environment variable, a config using "content: $VAR", or a config using the native
// "environment: VAR" form (see resolveConfigsEnvironment in compose-go's loader), and that
// services with no relation to the rotated certificate are excluded.
func TestServicesUsingRotatableCerts(t *testing.T) {
	dataMountPath := t.TempDir()
	scheduledOpts := ScheduledComposeOptions{
		ComposeLoad:         ComposeLoadOptions{DataMountPath: dataMountPath},
		DeployConfigBaseDir: "/",
	}

	repoRoot := filepath.Join(dataMountPath, "example.com", "owner", "repo")

	workingDir := filepath.Join(repoRoot, "stacks", "nas", "mtls-app")
	if err := os.MkdirAll(filepath.Join(repoRoot, ".git"), filesystem.PermDir); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(workingDir, filesystem.PermDir); err != nil {
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

	project, deployConfig, err := loadComposeScheduledProjectAll(context.Background(), nil, ref, provider, scheduledOpts)
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

// TestRotateSwarmProjectCertificatesIntegration exercises the full Swarm rotation path --
// composeScheduledServiceRefFromSwarmLabels, loadComposeScheduledProjectAll,
// LoadSwarmStack/addSwarm*Labels, DeploySwarmStack and the config/secret prune -- against a real
// Docker daemon in Swarm mode. docker stack deploy is idempotent, so RotateProjectCertificates can
// be called directly to both create and "rotate" the stack, without a separate initial deploy.
func TestRotateSwarmProjectCertificatesIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping swarm cert rotation integration test in short mode")
	}

	dockerCli, err := CreateDockerCli(false)
	if err != nil {
		t.Fatalf("failed to create Docker CLI: %v", err)
	}

	if err := swarm.RefreshModeEnabled(t.Context(), dockerCli.Client()); err != nil {
		t.Skipf("skipping swarm cert rotation integration test: %v", err)
	}

	if !swarm.GetModeEnabled() {
		t.Skip("swarm mode is not enabled, skipping cert rotation integration test")
	}

	stackName := test.ConvertTestName(t.Name())

	dataMountPath := t.TempDir()
	certOpts := CertificateRotationOptions{
		Scheduled: ScheduledComposeOptions{
			ComposeLoad:         ComposeLoadOptions{DataMountPath: dataMountPath},
			DeployConfigBaseDir: "/",
		},
	}

	repoRoot := filepath.Join(dataMountPath, "example.com", "owner", "repo")
	workingDir := filepath.Join(repoRoot, "stacks", stackName)

	if err := os.MkdirAll(filepath.Join(repoRoot, ".git"), filesystem.PermDir); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(workingDir, filesystem.PermDir); err != nil {
		t.Fatal(err)
	}

	createComposeFile(t, filepath.Join(workingDir, "compose.yml"), `services:
  app:
    image: busybox:latest
    command: ["sh", "-c", "sleep 600"]
    environment:
      CERT: ${CERT}
`)

	createComposeFile(t, filepath.Join(repoRoot, ".doco-cd.yml"), fmt.Sprintf(`name: %s
reference: refs/heads/main
working_dir: stacks/%s
compose_files:
  - compose.yml
external_secrets:
  CERT: "pki-role:pki:my-role:app.example.com"
`, stackName, stackName))

	labels := map[string]string{
		DocoCDLabels.Deployment.Name:       stackName,
		DocoCDLabels.Deployment.WorkingDir: workingDir,
		DocoCDLabels.Source.URL:            "https://example.com/owner/repo",
		DocoCDLabels.Deployment.TargetRef:  "refs/heads/main",
	}

	expiry := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	certPEM := generateTestCertPEM(t, expiry)
	provider := newStubProvider(map[string]string{"CERT": certPEM}, nil)

	t.Cleanup(func() {
		if err := RemoveSwarmStack(context.Background(), dockerCli, stackName); err != nil {
			t.Logf("failed to remove swarm stack %s during cleanup: %v", stackName, err)
		}
	})

	if err := RotateProjectCertificates(t.Context(), "", dockerCli, labels, provider, true, certOpts); err != nil {
		t.Fatalf("unexpected error rotating swarm project certificates: %v", err)
	}

	services, err := swarm.GetServicesByLabel(t.Context(), dockerCli.Client(), DocoCDLabels.Deployment.Name, stackName)
	if err != nil {
		t.Fatalf("failed to list swarm services for stack %s: %v", stackName, err)
	}

	if len(services) != 1 {
		t.Fatalf("expected exactly 1 swarm service for stack %s, got %d", stackName, len(services))
	}

	svc := services[0]
	if svc.Spec.Labels[DocoCDLabels.Deployment.CertRotatable] != "true" {
		t.Errorf("expected CertRotatable=true label, got %q", svc.Spec.Labels[DocoCDLabels.Deployment.CertRotatable])
	}

	if svc.Spec.TaskTemplate.ContainerSpec == nil {
		t.Fatal("expected task template to have a container spec")
	}

	var gotCert string

	for _, env := range svc.Spec.TaskTemplate.ContainerSpec.Env {
		if after, ok := strings.CutPrefix(env, "CERT="); ok {
			gotCert = after
		}
	}

	if gotCert != certPEM {
		t.Errorf("expected service environment to carry the freshly issued certificate, got %q", gotCert)
	}
}
