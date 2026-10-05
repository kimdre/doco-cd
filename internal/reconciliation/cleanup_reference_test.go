package reconciliation

import (
	"slices"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/moby/moby/api/types/container"

	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/stages"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func TestObsoleteStackCleanupScopesAncestryToDeploymentReference(t *testing.T) {
	t.Parallel()

	repoDir, mainRevision, deployedRevision := newTestRepoWithTwoCommits(t)

	repo, err := gogit.PlainOpen(repoDir)
	if err != nil {
		t.Fatal(err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	newer, err := wt.Commit("feature removes obsolete stack", &gogit.CommitOptions{
		AllowEmptyCommits: true,
		Author:            &object.Signature{Name: "Jane Doe", Email: "jane@example.com", When: time.Date(2026, 1, 1, 0, 2, 0, 0, time.UTC)},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name         string
		mainRef      string
		featureRef   string
		deployedRef  string
		mainDelete   bool
		mainFilter   string
		featureStale bool
		wantRemove   bool
	}{
		{name: "overlapping main origin does not veto feature cleanup", mainRef: "main", featureRef: "feature", deployedRef: "feature", mainDelete: true, wantRemove: true},
		{name: "qualified branch references", mainRef: "refs/heads/main", featureRef: "feature", deployedRef: "refs/heads/feature", mainDelete: true, wantRemove: true},
		{name: "feature scan predates deployment", mainRef: "main", featureRef: "refs/heads/feature", deployedRef: "feature", mainDelete: true, featureStale: true},
		{name: "missing deployment reference stays conservative", mainRef: "main", featureRef: "feature", mainDelete: true},
		{name: "unknown origin reference stays conservative", featureRef: "feature", deployedRef: "feature", mainDelete: true},
		{name: "no matching reference owner stays conservative", mainRef: "main", featureRef: "other", deployedRef: "feature", mainDelete: true},
		{name: "current removal permission remains authoritative", mainRef: "main", featureRef: "feature", deployedRef: "feature"},
		{name: "current webhook filter remains authoritative", mainRef: "main", featureRef: "feature", deployedRef: "feature", mainDelete: true, mainFilter: "^refs/heads/main$"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			featureRevision := newer.String()
			if tt.featureStale {
				featureRevision = mainRevision
			}

			configs := []*deployConfig.Config{
				cleanupTestDiscovered("main-keep", deployConfig.AutoDiscoveryOrigin{
					WorkingDirectory: "services", Reference: tt.mainRef, Revision: mainRevision, MirrorDir: repoDir,
					WebhookEventFilter: tt.mainFilter,
					Settings:           deployConfig.AutoDiscoveryConfig{Enabled: true, Delete: tt.mainDelete},
				}),
				cleanupTestDiscovered("feature-keep", deployConfig.AutoDiscoveryOrigin{
					WorkingDirectory: "services", Reference: tt.featureRef, Revision: featureRevision, MirrorDir: repoDir,
					Settings: deployConfig.AutoDiscoveryConfig{Enabled: true, Delete: true},
				}),
			}
			req := cleanupTestRequest()
			req.JobTrigger = stages.JobTriggerWebhook
			req.Payload = &webhook.ParsedPayload{Ref: "refs/heads/feature"}
			stack := cleanupTestStack("feature-old", "services/feature-old", map[string]string{
				docker.DocoCDLabels.Deployment.CommitSHA: deployedRevision,
				docker.DocoCDLabels.Deployment.TargetRef: tt.deployedRef,
			})
			got := runCleanupDecision(t, req, []container.Summary{stack}, configs)

			var want []string
			if tt.wantRemove {
				want = []string{"feature-old"}
			}

			if !slices.Equal(got, want) {
				t.Fatalf("removable stacks = %v, want %v", got, want)
			}
		})
	}
}

func TestObsoleteStackCleanupRemovedOriginRetainsStaleGuardAcrossReferences(t *testing.T) {
	t.Parallel()

	repoDir, older, newer := newTestRepoWithTwoCommits(t)
	req := cleanupTestRequest()
	req.Repository.MirrorDir = repoDir
	req.Repository.Revision = older
	req.Repository.ResolvedReference = "main"

	stack := cleanupTestStack("feature-old", "services/feature-old", map[string]string{
		docker.DocoCDLabels.Deployment.CommitSHA: newer,
		docker.DocoCDLabels.Deployment.TargetRef: "feature",
		docker.DocoCDLabels.Deployment.AutoDiscoveryConfig: docker.MarshalAutoDiscoveryConfig(
			deployConfig.AutoDiscoveryConfig{Enabled: true, Delete: true}),
	})
	if got := runCleanupDecision(t, req, []container.Summary{stack}, nil); len(got) != 0 {
		t.Fatalf("newer deployment was removable without a current matching origin: %v", got)
	}
}
