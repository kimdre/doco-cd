package reconciliation

import (
	"log/slog"
	"maps"
	"testing"

	"github.com/moby/moby/api/types/container"

	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/stages"
)

func TestCleanupRevisionContains(t *testing.T) {
	t.Parallel()

	mirror, older, newer := newTestRepoWithTwoCommits(t)
	for _, tt := range []struct {
		name, mirror, scanned, deployed string
		want, wantErr                   bool
	}{
		{"same revision", "", older, older, true, false},
		{"forward progress", mirror, newer, older, true, false},
		{"older scan", mirror, older, newer, false, false},
		{"missing mirror", "", newer, older, false, true},
		{"unreadable mirror", t.TempDir(), newer, older, false, true},
		{"missing deployed revision", mirror, newer, "", false, true},
		{"missing scanned revision", mirror, "", older, false, true},
		{"unknown commit", mirror, newer, cleanupTestRevision, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := stages.RevisionContains(tt.mirror, tt.scanned, tt.deployed, stages.NewGitAncestryCache())
			if got != tt.want || (err != nil) != tt.wantErr {
				t.Fatalf("RevisionContains() = %v, %v; want %v, error=%v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestCleanupEmptyScopeKeepsCurrentPolicy(t *testing.T) {
	t.Parallel()

	for _, enabled := range []bool{false, true} {
		req := cleanupTestRequest()
		req.ConfigTarget = "test"
		req.AutoDiscoveryOrigins = []deployConfig.AutoDiscoveryOrigin{{
			WorkingDirectory: "services", ConfigTarget: "test", Context: "remote",
			Revision: cleanupTestRevision,
			Settings: deployConfig.AutoDiscoveryConfig{Enabled: true, Delete: enabled},
		}}
		stack := cleanupTestStack("old", "services/old", map[string]string{
			docker.DocoCDLabels.Deployment.ConfigTarget: "test",
			docker.DocoCDLabels.Deployment.AutoDiscoveryConfig: docker.MarshalAutoDiscoveryConfig(
				deployConfig.AutoDiscoveryConfig{Enabled: true, Delete: true}),
		})
		cleanup := newObsoleteStackCleanup(slog.Default(), nil, false, "remote", req, nil)

		_, remove := cleanup.removalPolicy(slog.Default(), map[docker.Service]map[string]string{"old": stack.Labels})
		if remove != enabled {
			t.Fatalf("empty scope delete=%v: removable=%v", enabled, remove)
		}
	}
}

func TestCleanupUnknownRevisionKeepsStackPending(t *testing.T) {
	t.Parallel()

	for _, missing := range []string{
		docker.DocoCDLabels.Source.ConfigRevision,
		docker.DocoCDLabels.Deployment.CommitSHA,
	} {
		t.Run(missing, func(t *testing.T) {
			t.Parallel()

			stack := cleanupTestStack("old", "services/old", nil)
			stack.Labels[docker.DocoCDLabels.Source.ConfigRevision] = ""

			stack.Labels[missing] = ""
			if missing == docker.DocoCDLabels.Source.ConfigRevision {
				stack.Labels[docker.DocoCDLabels.Deployment.WorkingDir] = "/another/source/artifacts/abc/old"
			}

			cleanup := newObsoleteStackCleanup(slog.Default(), nil, false, "", cleanupTestRequest(), nil)
			stack.Labels[docker.DocoCDLabels.Deployment.AutoDiscoveryConfig] = docker.MarshalAutoDiscoveryConfig(
				deployConfig.AutoDiscoveryConfig{Enabled: true, Delete: true})

			_, remove := cleanup.removalPolicy(slog.Default(), map[docker.Service]map[string]string{"old": stack.Labels})
			if remove || !cleanup.pending {
				t.Fatalf("unknown revision: removable=%v, pending=%v", remove, cleanup.pending)
			}
		})
	}
}

func TestCleanupUnknownOwnershipKeepsStackPending(t *testing.T) {
	t.Parallel()

	stack := cleanupTestStack("old", "services/old", map[string]string{
		docker.DocoCDLabels.Source.URL: "",
	})
	cleanup := newObsoleteStackCleanup(slog.Default(), nil, false, "", cleanupTestRequest(), nil)

	_, remove := cleanup.removalPolicy(slog.Default(), map[docker.Service]map[string]string{"old": stack.Labels})
	if remove || !cleanup.pending {
		t.Fatalf("unknown ownership: removable=%v, pending=%v", remove, cleanup.pending)
	}
}

func TestCleanupFingerprintIncludesOwnership(t *testing.T) {
	t.Parallel()

	for _, label := range []string{
		docker.DocoCDLabels.Source.URL, docker.DocoCDLabels.Source.ConfigRevision,
		docker.DocoCDLabels.Deployment.ConfigTarget, docker.DocoCDLabels.Deployment.AutoDiscoveryConfig,
	} {
		original := cleanupTestStack("old", "services/old", nil)
		changed := maps.Clone(original.Labels)

		changed[label] = "changed"
		if deploymentFingerprint(map[docker.Service]map[string]string{"old": original.Labels}) ==
			deploymentFingerprint(map[docker.Service]map[string]string{"old": changed}) {
			t.Fatalf("fingerprint ignores ownership label %s", label)
		}
	}
}

func TestCleanupRechecksRemovalPredicateUnderStackLock(t *testing.T) {
	t.Parallel()

	stack := cleanupTestStack("old", "services/old", map[string]string{
		docker.DocoCDLabels.Deployment.AutoDiscoveryConfig: docker.MarshalAutoDiscoveryConfig(
			deployConfig.AutoDiscoveryConfig{Enabled: true, Delete: true}),
	})
	cleanup := newObsoleteStackCleanup(slog.Default(),
		cleanupTestCLI{apiClient: &cleanupTestClient{containers: []container.Summary{stack}}},
		false, "", cleanupTestRequest(), nil)
	called := false
	cleanup.allowRemoval = func(_ *slog.Logger, _ string) bool {
		called = true
		return false
	}

	err := cleanup.remove(t.Context(), slog.Default(), "old",
		map[docker.Service]map[string]string{"/old-app": stack.Labels},
		deployConfig.AutoDiscoveryConfig{Delete: true}, nil)
	if err != nil || !called || !cleanup.pending {
		t.Fatalf("locked removal check: called=%v, pending=%v, err=%v", called, cleanup.pending, err)
	}
}
