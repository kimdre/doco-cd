package stages

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/go-git/go-billy/v5/memfs"

	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
)

func TestAutoDiscoveryConfigLabelDriftServices(t *testing.T) {
	expected := "{enabled: true, depth: 0, delete: false, remove_volumes: true, remove_images: true}"

	disabled := "{enabled: false, depth: 0, delete: false, remove_volumes: false, remove_images: true}"

	tests := []struct {
		name           string
		expected       string // defaults to expected when empty
		status         map[docker.Service]docker.ServiceStatus
		wantServices   []string
		wantFirstLabel string
	}{
		{
			name: "matching labels",
			status: map[docker.Service]docker.ServiceStatus{
				"web": {
					Labels: docker.Labels{
						docker.DocoCDLabels.Deployment.AutoDiscoveryConfig: expected,
					},
				},
			},
			wantServices:   nil,
			wantFirstLabel: expected,
		},
		{
			// Serialization-only changes must not cause drift (#1818).
			name: "same config, different key order",
			status: map[docker.Service]docker.ServiceStatus{
				"web": {
					Labels: docker.Labels{
						docker.DocoCDLabels.Deployment.AutoDiscoveryConfig: "{depth: 0, enabled: true, delete: false, remove_volumes: true, remove_images: true}",
					},
				},
			},
			wantServices:   nil,
			wantFirstLabel: expected,
		},
		{
			name: "mismatched labels",
			status: map[docker.Service]docker.ServiceStatus{
				"web": {
					Labels: docker.Labels{
						docker.DocoCDLabels.Deployment.AutoDiscoveryConfig: "{enabled: true, depth: 0, delete: true, remove_volumes: true, remove_images: true}",
					},
				},
			},
			wantServices:   []string{"web"},
			wantFirstLabel: "{enabled: true, depth: 0, delete: true, remove_volumes: true, remove_images: true}",
		},
		{
			name: "missing label",
			status: map[docker.Service]docker.ServiceStatus{
				"web": {
					Labels: docker.Labels{},
				},
			},
			wantServices:   []string{"web"},
			wantFirstLabel: "",
		},
		{
			name: "multiple services sorted",
			status: map[docker.Service]docker.ServiceStatus{
				"z-api": {
					Labels: docker.Labels{},
				},
				"a-web": {
					Labels: docker.Labels{},
				},
			},
			wantServices:   []string{"a-web", "z-api"},
			wantFirstLabel: "",
		},
		{
			// A changed default must not recreate the stack while auto-discovery is off,
			// because the label steers auto-discovery cleanup only.
			name:     "disabled on both sides, only a default differs",
			expected: disabled,
			status: map[docker.Service]docker.ServiceStatus{
				"web": {
					Labels: docker.Labels{
						docker.DocoCDLabels.Deployment.AutoDiscoveryConfig: "{enabled: false, depth: 0, delete: true, remove_volumes: false, remove_images: true}",
					},
				},
			},
			wantServices:   nil,
			wantFirstLabel: disabled,
		},
		{
			name:     "disabled now, deployed while enabled",
			expected: disabled,
			status: map[docker.Service]docker.ServiceStatus{
				"web": {
					Labels: docker.Labels{
						docker.DocoCDLabels.Deployment.AutoDiscoveryConfig: "{enabled: true, depth: 0, delete: true, remove_volumes: false, remove_images: true}",
					},
				},
			},
			wantServices:   []string{"web"},
			wantFirstLabel: "{enabled: true, depth: 0, delete: true, remove_volumes: false, remove_images: true}",
		},
		{
			name:     "disabled with no label yet",
			expected: disabled,
			status: map[docker.Service]docker.ServiceStatus{
				"web": {
					Labels: docker.Labels{},
				},
			},
			wantServices:   nil,
			wantFirstLabel: disabled,
		},
		{
			name:     "disabled now, legacy deployment was enabled",
			expected: disabled,
			status: map[docker.Service]docker.ServiceStatus{
				"web": {
					Labels: docker.Labels{
						docker.DocoCDLabels.Deployment.AutoDiscovery: "true",
					},
				},
			},
			wantServices:   []string{"web"},
			wantFirstLabel: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expected := expected
			if tt.expected != "" {
				expected = tt.expected
			}

			expectedCfg := docker.ParseAutoDiscoveryConfig(expected)

			gotServices, gotFirst := autoDiscoveryConfigLabelDriftServices(tt.status, expectedCfg)
			if !slices.Equal(gotServices, tt.wantServices) {
				t.Fatalf("autoDiscoveryConfigLabelDriftServices() services = %v, want %v", gotServices, tt.wantServices)
			}

			wantFirstLabel := tt.wantFirstLabel
			if wantFirstLabel == expected {
				wantFirstLabel = docker.MarshalAutoDiscoveryConfig(expectedCfg)
			}

			if gotFirst != wantFirstLabel {
				t.Fatalf("autoDiscoveryConfigLabelDriftServices() first label = %q, want %q", gotFirst, wantFirstLabel)
			}
		})
	}
}

func TestShouldSkipDeployment(t *testing.T) {
	tests := []struct {
		name                      string
		retryAfterFailure         bool
		composeChanged            bool
		autoDiscoveryLabelChanged bool
		changedServices           []docker.Change
		ignoredInfo               docker.IgnoredInfo
		imagesChanged             bool
		mismatchServices          []docker.ServiceMismatch
		want                      bool
	}{
		{
			name:                      "retry after failed deployment",
			retryAfterFailure:         true,
			composeChanged:            false,
			autoDiscoveryLabelChanged: false,
			changedServices:           nil,
			ignoredInfo:               docker.IgnoredInfo{},
			imagesChanged:             false,
			mismatchServices:          nil,
			want:                      false,
		},
		{
			name:                      "no changes",
			composeChanged:            false,
			autoDiscoveryLabelChanged: false,
			changedServices:           nil,
			ignoredInfo:               docker.IgnoredInfo{},
			imagesChanged:             false,
			mismatchServices:          nil,
			want:                      true,
		},
		{
			name:                      "compose file changed",
			composeChanged:            true,
			autoDiscoveryLabelChanged: false,
			changedServices:           nil,
			ignoredInfo:               docker.IgnoredInfo{},
			imagesChanged:             false,
			mismatchServices:          nil,
			want:                      false,
		},
		{
			name:                      "services changed",
			composeChanged:            false,
			autoDiscoveryLabelChanged: false,
			changedServices: []docker.Change{{
				Type:     "configs",
				Services: []string{"web"},
			}},
			ignoredInfo:      docker.IgnoredInfo{},
			imagesChanged:    false,
			mismatchServices: nil,
			want:             false,
		},
		{
			name:                      "ignored changes",
			composeChanged:            false,
			autoDiscoveryLabelChanged: false,
			changedServices:           nil,
			ignoredInfo:               docker.IgnoredInfo{Ignored: []string{"web"}},
			imagesChanged:             false,
			mismatchServices:          nil,
			want:                      true,
		},
		{
			name:                      "ignored changes but need send signal",
			composeChanged:            false,
			autoDiscoveryLabelChanged: false,
			changedServices:           nil,
			ignoredInfo: docker.IgnoredInfo{NeedSendSignal: []docker.SignalService{
				{ServiceName: "web", Signal: "SIGHUP"},
			}},
			imagesChanged:    false,
			mismatchServices: nil,
			want:             false,
		},
		{
			name:                      "images changed",
			composeChanged:            false,
			autoDiscoveryLabelChanged: false,
			changedServices:           nil,
			ignoredInfo:               docker.IgnoredInfo{},
			imagesChanged:             true,
			mismatchServices:          nil,
			want:                      false,
		},
		{
			name:                      "missing services",
			composeChanged:            false,
			autoDiscoveryLabelChanged: false,
			changedServices:           nil,
			ignoredInfo:               docker.IgnoredInfo{},
			imagesChanged:             false,
			mismatchServices: []docker.ServiceMismatch{
				{
					ServiceName: "web",
					Reasons: []docker.ServiceMismatchReason{
						{
							Reason: docker.ServiceMismatchReasonNotDeployed,
						},
					},
				},
			},
			want: false,
		},
		{
			name:                      "auto discovery label changed",
			composeChanged:            false,
			autoDiscoveryLabelChanged: true,
			changedServices:           nil,
			ignoredInfo:               docker.IgnoredInfo{},
			imagesChanged:             false,
			mismatchServices:          nil,
			want:                      false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldSkipDeployment(tt.retryAfterFailure, tt.composeChanged, tt.autoDiscoveryLabelChanged, tt.changedServices, tt.ignoredInfo, tt.imagesChanged, tt.mismatchServices)
			if got != tt.want {
				t.Errorf("shouldSkipDeployment() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestShouldSkipOCIDeployment(t *testing.T) {
	tests := []struct {
		name          string
		forceRecreate bool
		deployed      string
		resolved      string
		deployedHash  string
		resolvedHash  string
		want          bool
	}{
		{
			name:          "skip when digest and project hash unchanged",
			forceRecreate: false,
			deployed:      "sha256:abc",
			resolved:      "sha256:abc",
			deployedHash:  "project-abc",
			resolvedHash:  "project-abc",
			want:          true,
		},
		{
			name:          "do not skip when digest changed",
			forceRecreate: false,
			deployed:      "sha256:abc",
			resolved:      "sha256:def",
			deployedHash:  "project-abc",
			resolvedHash:  "project-abc",
			want:          false,
		},
		{
			name:          "do not skip when project hash changed",
			forceRecreate: false,
			deployed:      "sha256:abc",
			resolved:      "sha256:abc",
			deployedHash:  "project-abc",
			resolvedHash:  "project-def",
			want:          false,
		},
		{
			name:          "do not skip when deployed digest missing",
			forceRecreate: false,
			deployed:      "",
			resolved:      "sha256:def",
			deployedHash:  "project-abc",
			resolvedHash:  "project-abc",
			want:          false,
		},
		{
			name:          "do not skip when resolved digest missing",
			forceRecreate: false,
			deployed:      "sha256:def",
			resolved:      "",
			deployedHash:  "project-abc",
			resolvedHash:  "project-abc",
			want:          false,
		},
		{
			name:          "do not skip when deployed project hash missing",
			forceRecreate: false,
			deployed:      "sha256:abc",
			resolved:      "sha256:abc",
			resolvedHash:  "project-abc",
			want:          false,
		},
		{
			name:          "do not skip when resolved project hash missing",
			forceRecreate: false,
			deployed:      "sha256:abc",
			resolved:      "sha256:abc",
			deployedHash:  "project-abc",
			want:          false,
		},
		{
			name:          "force recreate disables skip",
			forceRecreate: true,
			deployed:      "sha256:abc",
			resolved:      "sha256:abc",
			deployedHash:  "project-abc",
			resolvedHash:  "project-abc",
			want:          false,
		},
		{
			name:          "trims surrounding whitespace",
			forceRecreate: false,
			deployed:      "  sha256:abc  ",
			resolved:      "sha256:abc",
			deployedHash:  "  project-abc  ",
			resolvedHash:  "project-abc",
			want:          true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldSkipOCIDeployment(tt.forceRecreate, tt.deployed, tt.resolved, tt.deployedHash, tt.resolvedHash)
			if got != tt.want {
				t.Errorf("shouldSkipOCIDeployment() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestShouldSkipOCIDeployment_InterpolationEnvironmentChanged(t *testing.T) {
	t.Parallel()

	makeProject := func(value string) *types.Project {
		return &types.Project{
			Services: types.Services{
				"app": {
					Name:  "app",
					Image: "myapp:latest",
					Environment: types.MappingWithEquals{
						"SECRET": &value,
					},
				},
			},
		}
	}

	deployedHash, err := docker.ProjectHash(makeProject("old-secret"))
	if err != nil {
		t.Fatalf("hash deployed project: %v", err)
	}

	resolvedHash, err := docker.ProjectHash(makeProject("new-secret"))
	if err != nil {
		t.Fatalf("hash resolved project: %v", err)
	}

	if shouldSkipOCIDeployment(false, "sha256:abc", "sha256:abc", deployedHash, resolvedHash) {
		t.Fatal("expected environment-only project change to prevent OCI deployment skip")
	}
}

func TestShouldRecoverFromMissingDeployedCommit(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "object not found",
			err:  plumbing.ErrObjectNotFound,
			want: true,
		},
		{
			name: "wrapped object not found",
			err:  fmt.Errorf("wrapped: %w", plumbing.ErrObjectNotFound),
			want: true,
		},
		{
			name: "reference not found",
			err:  plumbing.ErrReferenceNotFound,
			want: true,
		},
		{
			name: "generic error",
			err:  errors.New("some other git error"),
			want: false,
		},
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldRecoverFromMissingDeployedCommit(tt.err)
			if got != tt.want {
				t.Fatalf("shouldRecoverFromMissingDeployedCommit() = %v, want %v", got, tt.want)
			}
		})
	}
}

// commitN creates n sequential empty commits on wt starting at message/time
// offset startIndex, and returns their hashes, oldest first. startIndex lets
// two separate calls on branches created from the same base (e.g. to build
// diverged history) produce distinct, non-colliding commits instead of two
// identical ones. This is a minimal local stand-in for internal/git's
// unexported test helper of the same name, since it isn't visible from this
// package.
func commitN(t *testing.T, wt *gogit.Worktree, n int, startIndex int) []plumbing.Hash {
	t.Helper()

	hashes := make([]plumbing.Hash, 0, n)
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for i := range n {
		idx := startIndex + i
		sig := &object.Signature{Name: "Jane Doe", Email: "jane@example.com", When: when.Add(time.Duration(idx) * time.Minute)}

		h, err := wt.Commit(fmt.Sprintf("commit %d", idx), &gogit.CommitOptions{
			AllowEmptyCommits: true,
			Author:            sig,
			Committer:         sig,
		})
		if err != nil {
			t.Fatalf("commit %d: %v", idx, err)
		}

		hashes = append(hashes, h)
	}

	return hashes
}

// TestIsStaleDeployment_OutOfOrderOlderRevisionIsSkipped is the stage-level
// regression test for the latest-revision-wins guard (removing the
// repository-wide webhook lock lets two events for the same stack run out of
// arrival order): if the revision an out-of-order run resolves to is a proven
// ancestor of what is already deployed, it must be reported as stale so the
// caller skips it instead of reverting a newer deployment.
func TestIsStaleDeployment_OutOfOrderOlderRevisionIsSkipped(t *testing.T) {
	repo, err := gogit.Init(memory.NewStorage(), memfs.New())
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	h := commitN(t, wt, 2, 0) // h[0] older, h[1] newer

	stageLog := slog.New(slog.NewTextHandler(io.Discard, nil))

	// An out-of-order run resolving to the older commit, while the newer
	// commit is already deployed, must be reported stale.
	if !isStaleDeployment(repo, h[0], h[1], stageLog) {
		t.Fatal("expected an older, already-superseded revision to be reported stale")
	}

	// The in-order case (latest is newer than deployed) must never be
	// reported stale.
	if isStaleDeployment(repo, h[1], h[0], stageLog) {
		t.Fatal("expected a newer revision to not be reported stale")
	}
}

// TestIsStaleDeployment_DivergedHistoryFailsOpen covers a force-push/rebase:
// neither commit is reachable from the other, so ancestry is unproven and the
// guard must fail open (never skip) rather than guess.
func TestIsStaleDeployment_DivergedHistoryFailsOpen(t *testing.T) {
	repo, err := gogit.Init(memory.NewStorage(), memfs.New())
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	base := commitN(t, wt, 1, 0)

	tip1 := commitN(t, wt, 1, 1) // parented on base

	if err := wt.Checkout(&gogit.CheckoutOptions{Hash: base[0]}); err != nil {
		t.Fatalf("checkout: %v", err)
	}

	tip2 := commitN(t, wt, 1, 2) // also parented on base, diverged from tip1

	stageLog := slog.New(slog.NewTextHandler(io.Discard, nil))

	if isStaleDeployment(repo, tip1[0], tip2[0], stageLog) {
		t.Fatal("expected diverged history to fail open (not stale)")
	}
}

// TestIsStaleDeployment_MissingCommitFailsOpen covers a shallow mirror
// missing one of the two commits: ancestry cannot be determined, so the
// guard must fail open rather than skip a deployment it cannot prove is
// stale.
func TestIsStaleDeployment_MissingCommitFailsOpen(t *testing.T) {
	repo, err := gogit.Init(memory.NewStorage(), memfs.New())
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	h := commitN(t, wt, 1, 0)

	stageLog := slog.New(slog.NewTextHandler(io.Discard, nil))

	if isStaleDeployment(repo, plumbing.NewHash("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"), h[0], stageLog) {
		t.Fatal("expected a missing commit to fail open (not stale)")
	}
}

func TestPkiRoleNormMap_BuildsStablePlaceholders(t *testing.T) {
	t.Parallel()

	certPEM := "-----BEGIN CERTIFICATE-----\nMIIFake...\n-----END CERTIFICATE-----\n"
	keyPEM := "-----BEGIN EC PRIVATE KEY-----\nMIIFakeKey...\n-----END EC PRIVATE KEY-----\n" // #nosec G101
	ref := "pki-role:pki:my-role:app.example.com"

	externalSecrets := map[string]secrettypes.ExternalSecretRef{
		"CERT": {LegacyRef: ref},
	}
	env := map[string]string{
		"CERT":     certPEM,
		"CERT_KEY": keyPEM,
	}

	norm := pkiRoleNormMap(externalSecrets, env)

	if norm[certPEM] != ref {
		t.Errorf("cert placeholder: want %q, got %q", ref, norm[certPEM])
	}

	if norm[keyPEM] != ref+"_KEY" {
		t.Errorf("key placeholder: want %q, got %q", ref+"_KEY", norm[keyPEM])
	}
}

func TestPkiRoleNormMap_SkipsNonPKIRoleRefs(t *testing.T) {
	t.Parallel()

	externalSecrets := map[string]secrettypes.ExternalSecretRef{
		"DB_PASSWORD": {LegacyRef: "kv/secret/db#password"},
		"API_KEY":     {LegacyRef: "kv/secret/api#key"},
	}
	env := map[string]string{
		"DB_PASSWORD": "s3cr3t",
		"API_KEY":     "apikey123",
	}

	norm := pkiRoleNormMap(externalSecrets, env)

	if len(norm) != 0 {
		t.Errorf("expected empty norm map for non-pki-role refs, got %v", norm)
	}
}

// TestPkiRoleNormMap_HashStability is an end-to-end check: using pkiRoleNormMap
// + docker.WithNormalizedEnvValues, consecutive cert re-issues should not change
// the project hash.
func TestPkiRoleNormMap_HashStability(t *testing.T) {
	t.Parallel()

	ref := "pki-role:pki:my-role:app.example.com"
	cert1 := "-----BEGIN CERTIFICATE-----\nserial-1\n-----END CERTIFICATE-----\n"
	cert2 := "-----BEGIN CERTIFICATE-----\nserial-2\n-----END CERTIFICATE-----\n"
	key1 := "-----BEGIN EC PRIVATE KEY-----\nkey-1\n-----END EC PRIVATE KEY-----\n" // #nosec G101
	key2 := "-----BEGIN EC PRIVATE KEY-----\nkey-2\n-----END EC PRIVATE KEY-----\n" // #nosec G101

	externalSecrets := map[string]secrettypes.ExternalSecretRef{"CERT": {LegacyRef: ref}}

	env1 := map[string]string{"CERT": cert1, "CERT_KEY": key1}
	env2 := map[string]string{"CERT": cert2, "CERT_KEY": key2}

	norm1 := pkiRoleNormMap(externalSecrets, env1)
	norm2 := pkiRoleNormMap(externalSecrets, env2)

	strPtr := func(s string) *string { return &s }

	makeProject := func(cert, key string) *types.Project {
		return &types.Project{
			Services: types.Services{
				"app": {
					Name:  "app",
					Image: "myapp:latest",
					Environment: types.MappingWithEquals{
						"CERT":     strPtr(cert),
						"CERT_KEY": strPtr(key),
					},
				},
			},
		}
	}

	h1, err := docker.ProjectHash(docker.WithNormalizedEnvValues(makeProject(cert1, key1), norm1))
	if err != nil {
		t.Fatalf("hash 1: %v", err)
	}

	h2, err := docker.ProjectHash(docker.WithNormalizedEnvValues(makeProject(cert2, key2), norm2))
	if err != nil {
		t.Fatalf("hash 2: %v", err)
	}

	if h1 != h2 {
		t.Errorf("hash changed despite same pki-role ref: %q vs %q", h1, h2)
	}
}

func TestGetAbsWorkingDirContainment(t *testing.T) {
	t.Parallel()

	repoPath := filepath.Join(t.TempDir(), "repository")

	tests := []struct {
		name       string
		workingDir string
		wantPath   string
		wantErr    bool
	}{
		{name: "repository root", workingDir: ".", wantPath: repoPath},
		{name: "nested directory", workingDir: "deploy/production", wantPath: filepath.Join(repoPath, "deploy/production")},
		{name: "normalized nested directory", workingDir: "deploy/../production", wantPath: filepath.Join(repoPath, "production")},
		{name: "parent traversal", workingDir: "..", wantPath: filepath.Dir(repoPath), wantErr: true},
		{name: "sibling prefix", workingDir: "../repository-backup", wantPath: repoPath + "-backup", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := getAbsWorkingDir(repoPath, tt.workingDir)
			if (err != nil) != tt.wantErr {
				t.Fatalf("getAbsWorkingDir() error = %v, wantErr %v", err, tt.wantErr)
			}

			if got != tt.wantPath {
				t.Fatalf("getAbsWorkingDir() = %q, want %q", got, tt.wantPath)
			}
		})
	}
}

func TestLoadComposeProjectHashCachesProjectAndHash(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()

	composePath := filepath.Join(repoPath, "compose.yaml")
	if err := os.WriteFile(composePath, []byte("services:\n  app:\n    image: busybox:latest\n"), 0o600); err != nil {
		t.Fatalf("write compose file: %v", err)
	}

	config := deploy.New("app", "main")
	config.WorkingDirectory = "."
	config.ComposeFiles = []string{"compose.yaml"}
	config.EnvFiles = nil

	manager := &StageManager{
		AppConfig:    &app.Config{},
		DeployConfig: config,
		Docker:       &Docker{},
		Repository: &RepositoryData{
			PathInternal: repoPath,
			PathExternal: repoPath,
		},
	}

	got, err := manager.loadComposeProjectHash(context.Background())
	if err != nil {
		t.Fatalf("loadComposeProjectHash() error = %v", err)
	}

	if manager.Docker.Project == nil {
		t.Fatal("loadComposeProjectHash() did not cache the Compose project")
	}

	if got == "" || manager.Docker.ProjectHash != got {
		t.Fatalf("cached project hash = %q, returned %q", manager.Docker.ProjectHash, got)
	}
}

// fakeSchedulerHolds is a SchedulerStopHolds stub keyed by context/project/service.
type fakeSchedulerHolds struct {
	held  map[string]bool
	calls int
}

func (f *fakeSchedulerHolds) IsSchedulerStopHeld(contextName, project, service string) bool {
	f.calls++

	return f.held[contextName+"/"+project+"/"+service]
}

// TestDropSchedulerHeldMismatches_SkipsDeploymentDuringJobStopWindow reproduces #1856:
// a scheduled job stops its cd.doco.job.stop_services targets, a poll tick lands inside
// that stop window, and the stopped service reports 0 running replicas. Without the
// scheduler-hold check the replicas mismatch makes shouldSkipDeployment return false and
// doco-cd runs a full deploy cycle for a service it stopped itself.
func TestDropSchedulerHeldMismatches_SkipsDeploymentDuringJobStopWindow(t *testing.T) {
	project := &types.Project{
		Name: "db",
		Services: types.Services{
			"db": types.ServiceConfig{
				Name:    "db",
				Restart: "unless-stopped",
			},
		},
	}

	// Service is present but stopped, so no container counts as a running replica.
	deployedStatus := map[docker.Service]docker.ServiceStatus{
		"db": {Labels: docker.Labels{}},
	}

	mismatches := docker.CheckServiceMismatch(false, deployedStatus, project.Services)
	if len(mismatches) != 1 {
		t.Fatalf("expected the stopped service to report a mismatch, got %v", mismatches)
	}

	if shouldSkipDeployment(false, false, false, nil, docker.IgnoredInfo{}, false, mismatches) {
		t.Fatal("expected an unfiltered replicas mismatch to force a deployment")
	}

	holds := &fakeSchedulerHolds{held: map[string]bool{"/db/db": true}}

	s := &StageManager{
		Docker:         &Docker{Project: project},
		DeployConfig:   &deploy.Config{Name: "db"},
		SchedulerHolds: holds,
	}

	filtered := s.dropSchedulerHeldMismatches(mismatches, nil)
	if len(filtered) != 0 {
		t.Fatalf("expected mismatch of scheduler-held service to be dropped, got %v", filtered)
	}

	if !shouldSkipDeployment(false, false, false, nil, docker.IgnoredInfo{}, false, filtered) {
		t.Fatal("expected deployment to be skipped while the job scheduler holds the service stopped")
	}
}

func TestDropSchedulerHeldMismatches(t *testing.T) {
	mismatches := []docker.ServiceMismatch{
		{ServiceName: "db", Reasons: []docker.ServiceMismatchReason{{Reason: docker.ServiceMismatchReasonReplicas, Want: 1, Got: uint64(0)}}},
		{ServiceName: "web", Reasons: []docker.ServiceMismatchReason{{Reason: docker.ServiceMismatchReasonReplicas, Want: 1, Got: uint64(0)}}},
	}

	project := &types.Project{
		Name: "stack",
		Services: types.Services{
			"db":  types.ServiceConfig{Name: "db", Restart: "unless-stopped"},
			"web": types.ServiceConfig{Name: "web", Restart: "unless-stopped"},
		},
	}

	tests := []struct {
		name      string
		swarmMode bool
		context   string
		holds     *fakeSchedulerHolds
		want      []string
		wantCalls bool
	}{
		{
			name:      "held service is dropped, others stay",
			holds:     &fakeSchedulerHolds{held: map[string]bool{"/stack/db": true}},
			want:      []string{"web"},
			wantCalls: true,
		},
		{
			name:      "nothing held keeps every mismatch",
			holds:     &fakeSchedulerHolds{held: map[string]bool{}},
			want:      []string{"db", "web"},
			wantCalls: true,
		},
		{
			name:      "holds are keyed by docker context",
			context:   "remote",
			holds:     &fakeSchedulerHolds{held: map[string]bool{"/stack/db": true}},
			want:      []string{"db", "web"},
			wantCalls: true,
		},
		{
			// Holds are only registered for compose-mode jobs, same scope as the
			// reconciliation event listener uses.
			name:      "swarm deployments are untouched",
			swarmMode: true,
			holds:     &fakeSchedulerHolds{held: map[string]bool{"/stack/db": true}},
			want:      []string{"db", "web"},
			wantCalls: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &StageManager{
				Docker:         &Docker{Project: project, SwarmMode: tc.swarmMode},
				DeployConfig:   &deploy.Config{Name: "stack", Context: tc.context},
				SchedulerHolds: tc.holds,
			}

			got := s.dropSchedulerHeldMismatches(mismatches, nil)

			names := make([]string, 0, len(got))
			for _, m := range got {
				names = append(names, m.ServiceName)
			}

			if !slices.Equal(names, tc.want) {
				t.Fatalf("expected remaining mismatches %v, got %v", tc.want, names)
			}

			if tc.wantCalls != (tc.holds.calls > 0) {
				t.Fatalf("expected scheduler holds queried=%v, got %d calls", tc.wantCalls, tc.holds.calls)
			}
		})
	}
}

// TestDropSchedulerHeldMismatches_NoTracker covers deployments created without a
// scheduler hold tracker, e.g. from tests or an embedding that does not run a scheduler.
func TestDropSchedulerHeldMismatches_NoTracker(t *testing.T) {
	mismatches := []docker.ServiceMismatch{{ServiceName: "db"}}

	s := &StageManager{
		Docker:       &Docker{Project: &types.Project{Name: "stack"}},
		DeployConfig: &deploy.Config{Name: "stack"},
	}

	if got := s.dropSchedulerHeldMismatches(mismatches, nil); len(got) != 1 {
		t.Fatalf("expected mismatches to pass through unchanged, got %v", got)
	}
}
