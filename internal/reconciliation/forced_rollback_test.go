package reconciliation

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/moby/moby/client"

	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/stages"
)

type unavailableReconciliationClient struct {
	client.APIClient
}

func (*unavailableReconciliationClient) Info(context.Context, client.InfoOptions) (client.SystemInfoResult, error) {
	return client.SystemInfoResult{}, errors.New("test daemon unavailable")
}

func TestManagerAddJobRegistersSuccessfulForcedRollback(t *testing.T) {
	t.Parallel()

	repoDir, older, newer := newTestRepoWithTwoCommits(t)

	for _, tt := range []struct {
		name      string
		outcome   error
		recorded  bool
		enabled   bool
		wantOlder bool
	}{
		{name: "successful rollback", recorded: true, enabled: true, wantOlder: true},
		{name: "failed rollback", recorded: true, enabled: true, outcome: errors.New("deployment failed")},
		{name: "blocked rollback", recorded: true, enabled: true, outcome: &stages.SyncWindowBlockedError{}},
		{name: "filtered rollback", recorded: true, enabled: true, outcome: stages.ErrWebhookFilterMismatch},
		{name: "skipped rollback", recorded: true, enabled: true, outcome: stages.ErrSkipDeployment},
		{name: "no deployment outcome", enabled: true},
		{name: "rollback disables reconciliation", recorded: true, wantOlder: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			manager := newTestManagerWithDependencies(t, Dependencies{
				DockerCLI: cleanupTestCLI{apiClient: &unavailableReconciliationClient{}},
			})
			request := func(revision string, forced, enabled bool) DeployRequest {
				return DeployRequest{
					Logger:     slog.New(slog.DiscardHandler),
					JobTrigger: stages.JobTriggerPoll,
					Repository: stages.RepositoryData{
						Name: "repo", MirrorDir: repoDir, Revision: revision, ResolvedReference: "main",
					},
					DeployConfigs: []*deployConfig.Config{{
						Name: "web", ForceRecreate: forced,
						Reconciliation: deployConfig.ReconciliationConfig{Enabled: enabled, Events: []string{"die"}},
					}},
				}
			}
			current := newReconciliationJob(manager, request(newer, false, true), nil, nil)
			manager.jobs.jobs["repo"] = current
			rollback := request(older, true, tt.enabled)
			var results []stackResult
			if tt.recorded {
				results = []stackResult{{config: rollback.DeployConfigs[0], err: tt.outcome}}
			}

			manager.addJob(t.Context(), rollback, nil, successfulForcedDeployments(results))
			got := manager.jobs.jobs["repo"]
			if !tt.wantOlder {
				if got != current {
					t.Fatal("an unsuccessful rollback replaced the newer reconciliation job")
				}
				return
			}
			select {
			case <-current.closeChan:
			default:
				t.Fatal("successful rollback did not close the newer reconciliation job")
			}
			if !tt.enabled {
				if got != nil {
					t.Fatal("rollback that disables reconciliation retained a job")
				}
				return
			}
			if got == nil || got == current || got.info.Repository.Revision != older {
				t.Fatal("successful rollback did not register its deployed revision")
			}
			groups := got.groupByRequest(got.info.DeployConfigs)
			if len(groups) != 1 || groups[0].request.Repository.Revision != older {
				t.Fatal("reconciliation would restore the wrong revision after rollback")
			}
		})
	}
}

func TestForcedRollbackRequestKeepsOtherStacksAndTheirSources(t *testing.T) {
	t.Parallel()

	oldWeb := &deployConfig.Config{Name: "web"}
	oldAPI := &deployConfig.Config{Name: "api"}
	remoteWeb := &deployConfig.Config{Name: "web", Context: "remote"}
	pinned := &deployConfig.Config{Name: "pinned"}
	newWeb := &deployConfig.Config{Name: "web", ForceRecreate: true}
	failedAPI := &deployConfig.Config{Name: "api", ForceRecreate: true}
	previousSource := &DeployRequest{Repository: stages.RepositoryData{Revision: "carried-source"}}
	previous := newJob(nil, DeployRequest{
		Repository:    stages.RepositoryData{Revision: "newer"},
		DeployConfigs: []*deployConfig.Config{oldWeb, oldAPI, remoteWeb},
	}, nil)
	previous.cleanupConfigs = append(previous.cleanupConfigs, pinned)
	previous.carried = map[*deployConfig.Config]*DeployRequest{remoteWeb: previousSource}

	req, deferred := forcedRollbackRequest(DeployRequest{
		Repository:    stages.RepositoryData{Revision: "older"},
		DeployConfigs: []*deployConfig.Config{newWeb, failedAPI},
	}, previous, map[*deployConfig.Config]struct{}{newWeb: {}})
	next := newReconciliationJob(nil, req, deferred, previous)
	if len(next.info.DeployConfigs) != 3 || len(next.pinned) != 1 || next.pinned[0] != pinned {
		t.Fatal("partial rollback lost the recovery state of other stacks")
	}
	if source := next.carried[oldAPI]; source == nil || source.Repository.Revision != "newer" {
		t.Fatal("failed sibling rollback replaced the sibling's deployed revision")
	}
	if next.carried[remoteWeb] != previousSource {
		t.Fatal("partial rollback lost a carried stack's original source")
	}
	for _, dc := range next.info.DeployConfigs {
		if dc == oldWeb || dc == failedAPI {
			t.Fatal("partial rollback retained superseded or undeployed configuration")
		}
	}
	if _, carried := next.carried[newWeb]; carried {
		t.Fatal("successfully rolled-back stack still restores the newer request")
	}
}

func TestSuccessfulForcedDeploymentsUsesIndividualOutcomes(t *testing.T) {
	t.Parallel()

	forced := &deployConfig.Config{ForceRecreate: true}
	failed := &deployConfig.Config{ForceRecreate: true}
	normal := &deployConfig.Config{}
	destroy := &deployConfig.Config{ForceRecreate: true}
	destroy.Destroy.Enabled = true
	got := successfulForcedDeployments([]stackResult{
		{config: forced},
		{config: failed, err: errors.New("failed")},
		{config: normal},
		{config: destroy},
		{},
	})
	if len(got) != 1 {
		t.Fatalf("successful forced deployments = %v, want one", got)
	}
	if _, ok := got[forced]; !ok {
		t.Fatal("successful forced stack was lost because another stack failed")
	}
}
