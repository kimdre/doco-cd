package reconciliation

import (
	"log/slog"
	"slices"
	"testing"

	"github.com/moby/moby/api/types/container"

	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/stages"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func TestObsoleteStackCleanupRetainsPausedDiscoveryScopes(t *testing.T) {
	t.Parallel()

	deleteSettings := deployConfig.AutoDiscoveryConfig{Enabled: true, Delete: true, RemoveImages: true, RemoveVolumes: true}
	deleteLabel := docker.MarshalAutoDiscoveryConfig(deleteSettings)

	for _, tt := range []struct {
		name           string
		pause          bool
		dir            string
		target         string
		stackTarget    string
		repositoryURL  string
		rootContext    string
		cleanupContext string
		childContext   string
		depth          int
		childOrigin    bool
		wantRemove     bool
	}{
		{name: "deployment pause protects an empty root", pause: true, dir: "services"},
		{name: "unpaused empty root still cleans up", dir: "services", wantRemove: true},
		{name: "child-carried origin still protects its scope", pause: true, dir: "services", childOrigin: true},
		{name: "pause does not protect other directories", pause: true, dir: "databases", wantRemove: true},
		{name: "pause does not exceed the root scan depth", pause: true, dir: ".", depth: 1, wantRemove: true},
		{name: "pause does not protect another deployment repository", pause: true, dir: "services", repositoryURL: "https://example.com/organization/other.git", wantRemove: true},
		{name: "custom target root does not clean another target", dir: "services", target: "prod", stackTarget: "dev"},
		{name: "default target root does not clean custom targets", dir: "services", stackTarget: "prod"},
		{name: "custom target root cleans its own obsolete stacks", dir: "services", target: "prod", stackTarget: "prod", wantRemove: true},
		{name: "empty root protects its context", pause: true, dir: "services", rootContext: "production", cleanupContext: "production"},
		{name: "empty root does not protect another context", pause: true, dir: "services", rootContext: "production", cleanupContext: "development", wantRemove: true},
		{name: "default context aliases match", pause: true, dir: "services", rootContext: "default"},
		{name: "child overrides are scoped to their destination", pause: true, dir: "services", rootContext: "production", cleanupContext: "development", childContext: "development"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := cleanupTestRequest()
			origin := deployConfig.AutoDiscoveryOrigin{
				WorkingDirectory: tt.dir, ConfigTarget: tt.target, RepositoryURL: tt.repositoryURL,
				Settings: deleteSettings, Pause: tt.pause, Context: tt.rootContext,
				Revision: cleanupTestRevision,
			}
			origin.Settings.ScanDepth = tt.depth

			var configs []*deployConfig.Config

			if tt.childOrigin || tt.childContext != "" {
				cfg := cleanupTestDiscovered("current", origin)
				cfg.Internal.ConfigTarget = tt.target
				cfg.Context = tt.childContext
				configs = []*deployConfig.Config{cfg}
			}

			if !tt.childOrigin {
				req.AutoDiscoveryOrigins = []deployConfig.AutoDiscoveryOrigin{origin}
			}

			stack := cleanupTestStack("obsolete", "services/obsolete", map[string]string{
				docker.DocoCDLabels.Deployment.AutoDiscoveryConfig: deleteLabel,
				docker.DocoCDLabels.Deployment.ConfigTarget:        tt.stackTarget,
			})
			log := slog.New(slog.DiscardHandler)
			c := newObsoleteStackCleanup(log, nil, false, tt.cleanupContext, req, configs)

			_, remove := c.servicePolicy(log, stack.Labels)
			if remove != tt.wantRemove {
				t.Fatalf("remove = %v, want %v", remove, tt.wantRemove)
			}
		})
	}
}

func TestObsoleteStackCleanupEmptyRootRetainsCurrentPolicyAndRevision(t *testing.T) {
	t.Parallel()

	repoDir, older, newer := newTestRepoWithTwoCommits(t)
	label := docker.MarshalAutoDiscoveryConfig(deployConfig.AutoDiscoveryConfig{Enabled: true, Delete: true})

	for _, tt := range []struct {
		name       string
		delete     bool
		filter     string
		scanned    string
		deployed   string
		wantRemove bool
	}{
		{name: "current deletion policy overrides old label", scanned: newer, deployed: older},
		{name: "current filter overrides old label", delete: true, filter: "^refs/heads/feature$", scanned: newer, deployed: older},
		{name: "empty root cannot remove a newer deployment", delete: true, scanned: older, deployed: newer},
		{name: "empty root removes an older deployment", delete: true, scanned: newer, deployed: older, wantRemove: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := cleanupTestRequest()
			req.JobTrigger = stages.JobTriggerWebhook
			req.Payload = &webhook.ParsedPayload{Ref: "refs/heads/main"}
			req.AutoDiscoveryOrigins = []deployConfig.AutoDiscoveryOrigin{{
				WorkingDirectory: "services", Reference: "main", Revision: tt.scanned, MirrorDir: repoDir,
				WebhookEventFilter: tt.filter,
				Settings:           deployConfig.AutoDiscoveryConfig{Enabled: true, Delete: tt.delete},
			}}
			stack := cleanupTestStack("obsolete", "services/obsolete", map[string]string{
				docker.DocoCDLabels.Deployment.AutoDiscoveryConfig: label,
				docker.DocoCDLabels.Deployment.CommitSHA:           tt.deployed,
				docker.DocoCDLabels.Deployment.TargetRef:           "main",
			})
			got := runCleanupDecision(t, req, []container.Summary{stack}, nil)

			var want []string
			if tt.wantRemove {
				want = []string{"obsolete"}
			}

			if !slices.Equal(got, want) {
				t.Fatalf("removable = %v, want %v", got, want)
			}
		})
	}
}
