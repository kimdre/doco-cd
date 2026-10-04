package reconciliation

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/docker/cli/cli/command"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/common/types/set"
	"github.com/kimdre/doco-cd/internal/config"
	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/source/oci"
	"github.com/kimdre/doco-cd/internal/stages"
	"github.com/kimdre/doco-cd/internal/syncwindow"
	"github.com/kimdre/doco-cd/internal/webhook"
)

const cleanupTestRepoURL = "https://example.com/organization/repository.git"

type cleanupTestCLI struct {
	command.Cli
	apiClient client.APIClient
}

func (c cleanupTestCLI) Client() client.APIClient {
	return c.apiClient
}

// cleanupTestClient lists containers matching the label filters of a request.
type cleanupTestClient struct {
	client.APIClient
	containers []container.Summary
	// listErr is returned for requests filtering on this label.
	listErr map[string]error
}

func (c *cleanupTestClient) ContainerList(_ context.Context, options client.ContainerListOptions) (client.ContainerListResult, error) {
	for filter := range options.Filters["label"] {
		if err := c.listErr[filter]; err != nil {
			return client.ContainerListResult{}, err
		}
	}

	var items []container.Summary

	for _, cont := range c.containers {
		matches := true

		for filter := range options.Filters["label"] {
			key, value, hasValue := strings.Cut(filter, "=")
			if got, ok := cont.Labels[key]; !ok || hasValue && got != value {
				matches = false
			}
		}

		if matches {
			items = append(items, cont)
		}
	}

	return client.ContainerListResult{Items: items}, nil
}

func cleanupTestRequest() DeployRequest {
	return DeployRequest{
		JobTrigger: stages.JobTriggerPoll,
		Repository: stages.RepositoryData{SourceUrl: cleanupTestRepoURL},
	}
}

// cleanupTestStack returns a container of an auto-discovered stack deployed from relDir of cleanupTestRepoURL.
func cleanupTestStack(name, relDir string, labels map[string]string) container.Summary {
	stackLabels := map[string]string{
		docker.DocoCDLabels.Deployment.Name:          name,
		docker.DocoCDLabels.Deployment.AutoDiscovery: "true",
		docker.DocoCDLabels.Deployment.WorkingDir:    "/var/lib/doco-cd/example.com/organization/repository/artifacts/0123456789abcdef0123456789abcdef01234567/" + relDir,
		docker.DocoCDLabels.Source.URL:               cleanupTestRepoURL,
	}

	maps.Copy(stackLabels, labels)

	return container.Summary{Names: []string{"/" + name + "-app"}, Labels: stackLabels}
}

// cleanupTestDiscovered returns a deploy config discovered by origin.
func cleanupTestDiscovered(name string, origin deployConfig.AutoDiscoveryOrigin) *deployConfig.Config {
	cfg := &deployConfig.Config{Name: name}
	cfg.AutoDiscovery = origin.Settings
	cfg.Internal.AutoDiscoveryOrigin = &origin

	return cfg
}

// runCleanupDecision runs the cleanup and returns the stacks it would remove. The removal predicate rejects
// every stack, so nothing is destroyed.
func runCleanupDecision(t *testing.T, req DeployRequest, containers []container.Summary, configs []*deployConfig.Config) []string {
	t.Helper()

	var removable []string

	err := cleanupObsoleteAutoDiscoveredContainers(
		t.Context(),
		slog.New(slog.DiscardHandler),
		cleanupTestCLI{apiClient: &cleanupTestClient{containers: containers}},
		false,
		"",
		req,
		configs,
		nil,
		func(_ *slog.Logger, stackName string) bool {
			removable = append(removable, stackName)

			return false
		},
	)
	if err != nil {
		t.Fatalf("cleanupObsoleteAutoDiscoveredContainers() error = %v", err)
	}

	return removable
}

func TestReconciliationCleanupUsesCurrentOwnershipAfterDeferral(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		webhook    bool
		filter     string
		policy     deployConfig.AutoDiscoveryConfig
		wantRemove bool
	}{
		{
			name:    "webhook filter and delete disabled",
			webhook: true,
			filter:  "^refs/heads/dev$",
			policy:  deployConfig.AutoDiscoveryConfig{Enabled: true},
		},
		{
			name:    "webhook filter keeps obsolete stacks even with delete enabled",
			webhook: true,
			filter:  "^refs/heads/dev$",
			policy:  deployConfig.AutoDiscoveryConfig{Enabled: true, Delete: true, RemoveVolumes: true},
		},
		{
			name:   "sync window and delete disabled",
			filter: "^refs/heads/main$",
			policy: deployConfig.AutoDiscoveryConfig{Enabled: true},
		},
		{
			name:       "sync window keeps current resource removal policy",
			filter:     "^refs/heads/main$",
			policy:     deployConfig.AutoDiscoveryConfig{Enabled: true, Delete: true},
			wantRemove: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			oldOrigin := deployConfig.AutoDiscoveryOrigin{
				WorkingDirectory:   ".",
				WebhookEventFilter: "^refs/heads/main$",
				Revision:           "rev-1",
				Settings: deployConfig.AutoDiscoveryConfig{
					Enabled: true, Delete: true, RemoveVolumes: true, RemoveImages: true,
				},
			}
			oldAlpha := cleanupTestDiscovered("alpha", oldOrigin)
			oldAlpha.WebhookEventFilter = oldOrigin.WebhookEventFilter
			oldAlpha.Reconciliation.Enabled = true
			oldAlpha.Reconciliation.Events = []string{"die"}
			oldOriginPtr := oldAlpha.Internal.AutoDiscoveryOrigin
			oldReq := cleanupTestRequest()
			oldReq.Repository.Revision = "rev-1"
			oldReq.DeployConfigs = []*deployConfig.Config{oldAlpha, cleanupTestDiscovered("beta", oldOrigin)}
			previous := newJob(nil, oldReq, nil)

			manager := newSyncWindowTestManager(t, strings.ReplaceAll(syncWindowTestPolicy, "web*", "*"))
			stack := cleanupTestStack("beta", "beta", map[string]string{
				docker.DocoCDLabels.Deployment.AutoDiscoveryConfig: docker.MarshalAutoDiscoveryConfig(oldOrigin.Settings),
			})

			var recoverySource *DeployRequest

			for _, revision := range []string{"rev-2", "rev-3"} {
				origin := oldOrigin
				origin.Revision = revision
				origin.WebhookEventFilter = tt.filter
				origin.Settings = tt.policy
				alpha := cleanupTestDiscovered("alpha", origin)
				alpha.WebhookEventFilter = tt.filter
				req := cleanupTestRequest()
				req.Logger = slog.New(slog.DiscardHandler)
				req.Repository.Revision = revision
				req.DeployConfigs = []*deployConfig.Config{alpha}

				var deferred map[*deployConfig.Config]struct{}

				if tt.webhook {
					req.JobTrigger = stages.JobTriggerWebhook
					req.Payload = &webhook.ParsedPayload{Ref: "refs/heads/main"}
					deferred = withWebhookFilteredConfigs(req, nil)
				} else {
					gate := manager.newSyncWindowGate(req, syncWindowTestNow)
					if err := gate.admit(req.Logger, alpha, nil); !errors.Is(err, stages.ErrSyncWindowBlocked) {
						t.Fatalf("admit() = %v, want a sync-window deferral", err)
					}

					deferred = gate.deferred()
				}

				current := newReconciliationJob(manager, req, deferred, previous)
				if len(current.info.DeployConfigs) != 1 || current.info.DeployConfigs[0] != oldAlpha {
					t.Fatal("recovery did not retain the original deployment config")
				}

				source := current.carried[oldAlpha]
				if source == nil || source.Repository != oldReq.Repository {
					t.Fatalf("recovery source = %+v, want rev-1", source)
				}

				if recoverySource != nil && source != recoverySource {
					t.Fatal("a repeated deferral replaced the original recovery source")
				}

				recoverySource = source

				if oldAlpha.Internal.AutoDiscoveryOrigin != oldOriginPtr ||
					*oldAlpha.Internal.AutoDiscoveryOrigin != oldOrigin {
					t.Fatal("cleanup mutated the original recovery ownership metadata")
				}

				configs := current.cleanupConfigsForContextMode("", false)

				removable := runCleanupDecision(t, current.info, []container.Summary{stack}, configs)
				if tt.wantRemove != slices.Equal(removable, []string{"beta"}) ||
					!tt.wantRemove && len(removable) != 0 {
					t.Fatalf("removable stacks = %v, want removal = %v", removable, tt.wantRemove)
				}

				cleanup := newObsoleteStackCleanup(req.Logger, nil, false, "", current.info, configs)

				policy, remove := cleanup.removalPolicy(req.Logger, map[docker.Service]map[string]string{"beta-app": stack.Labels})
				if remove != tt.wantRemove || remove && (policy.RemoveVolumes || policy.RemoveImages) {
					t.Fatalf("cleanup policy = %+v, remove = %v, want current policy without resource removal", policy, remove)
				}

				if len(configs) != 1 || configs[0] != alpha {
					t.Fatal("cleanup did not retain the current request's ownership metadata")
				}
				// The sync window still gates deletion, but does not change ownership.
				if !tt.webhook && tt.wantRemove {
					removalReq := current.info
					removalReq.Origin = syncwindow.OriginAutomatic
					removalReq.DeployConfigs = nil

					gate := manager.newSyncWindowGate(removalReq, syncWindowTestNow)
					if gate.allowRemoval(req.Logger, "", "beta") {
						t.Fatal("cleanup bypassed the sync window")
					}
				}

				previous = current
			}
		})
	}
}

func TestCleanupObsoleteAutoDiscoveredContainers_SkipsDifferentTargetBeforeRepositoryMatch(t *testing.T) {
	t.Parallel()

	apiClient := &cleanupTestClient{
		containers: []container.Summary{
			cleanupTestStack("test-stack", "test-stack", map[string]string{
				docker.DocoCDLabels.Deployment.ConfigTarget: "dev",
			}),
		},
	}
	deployCfg := &deployConfig.Config{}
	deployCfg.Internal.ConfigTarget = "updater"

	var logs bytes.Buffer

	err := cleanupObsoleteAutoDiscoveredContainers(
		t.Context(),
		slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		cleanupTestCLI{apiClient: apiClient},
		false,
		"",
		cleanupTestRequest(),
		[]*deployConfig.Config{deployCfg},
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("cleanupObsoleteAutoDiscoveredContainers() error = %v", err)
	}

	if strings.Contains(logs.String(), "checking auto-discovered stack for repository match") {
		t.Fatal("foreign target stack was checked for a repository match")
	}
}

func TestCleanupObsoleteAutoDiscoveredContainers_RemovalDecision(t *testing.T) {
	t.Parallel()

	deleteLabel := docker.MarshalAutoDiscoveryConfig(deployConfig.AutoDiscoveryConfig{Enabled: true, Delete: true})
	keepLabel := docker.MarshalAutoDiscoveryConfig(deployConfig.AutoDiscoveryConfig{Enabled: true, Delete: false})

	services := func(deleteStacks bool, filter string) deployConfig.AutoDiscoveryOrigin {
		return deployConfig.AutoDiscoveryOrigin{
			WorkingDirectory:   "services",
			WebhookEventFilter: filter,
			Settings:           deployConfig.AutoDiscoveryConfig{Enabled: true, Delete: deleteStacks},
		}
	}

	webhookRequest := func(ref string) DeployRequest {
		req := cleanupTestRequest()
		req.JobTrigger = stages.JobTriggerWebhook
		req.Payload = &webhook.ParsedPayload{Ref: ref}

		return req
	}

	const ociTestDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	ociRequest := func(artifact string) DeployRequest {
		req := cleanupTestRequest()
		req.Repository = stages.RepositoryData{Source: config.SourceTypeOCI, SourceUrl: artifact}

		return req
	}

	// ociStack returns a stack deployed from services/web-old of the OCI artifact.
	ociStack := func(artifact string) container.Summary {
		return cleanupTestStack("web-old", "", map[string]string{
			docker.DocoCDLabels.Deployment.WorkingDir: "/var/lib/doco-cd/" + oci.RepositoryNameFromArtifact(artifact) +
				"/artifacts/sha256-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef/services/web-old",
			docker.DocoCDLabels.Source.URL:  artifact,
			docker.DocoCDLabels.Source.Type: "oci",
		})
	}

	tests := []struct {
		name       string
		req        DeployRequest
		containers []container.Summary
		configs    []*deployConfig.Config
		want       []string
	}{
		{
			name: "policy of the current config overrides a label that keeps the stack",
			containers: []container.Summary{cleanupTestStack("web-old", "services/web-old", map[string]string{
				docker.DocoCDLabels.Deployment.AutoDiscoveryConfig: keepLabel,
			})},
			configs: []*deployConfig.Config{cleanupTestDiscovered("web", services(true, ""))},
			want:    []string{"web-old"},
		},
		{
			name: "policy of the current config overrides a label that removes the stack",
			containers: []container.Summary{cleanupTestStack("web-old", "services/web-old", map[string]string{
				docker.DocoCDLabels.Deployment.AutoDiscoveryConfig: deleteLabel,
			})},
			configs: []*deployConfig.Config{cleanupTestDiscovered("web", services(false, ""))},
		},
		{
			name: "label policy applies to stacks outside of every current auto-discovery config",
			containers: []container.Summary{cleanupTestStack("db-old", "databases/db-old", map[string]string{
				docker.DocoCDLabels.Deployment.AutoDiscoveryConfig: deleteLabel,
			})},
			configs: []*deployConfig.Config{cleanupTestDiscovered("web", services(false, ""))},
			want:    []string{"db-old"},
		},
		{
			name: "empty label keeps the stack",
			containers: []container.Summary{cleanupTestStack("db-old", "databases/db-old", map[string]string{
				docker.DocoCDLabels.Deployment.AutoDiscoveryConfig: "",
			})},
			configs: []*deployConfig.Config{cleanupTestDiscovered("web", services(true, ""))},
		},
		{
			name:       "stack deployed before the label existed uses the defaults of that version",
			containers: []container.Summary{cleanupTestStack("db-old", "databases/db-old", nil)},
			want:       []string{"db-old"},
		},
		{
			name: "invalid label keeps the stack",
			containers: []container.Summary{cleanupTestStack("db-old", "databases/db-old", map[string]string{
				docker.DocoCDLabels.Deployment.AutoDiscoveryConfig: "{",
			})},
		},
		{
			name: "explicit config with the same name keeps the stack",
			containers: []container.Summary{cleanupTestStack("web-old", "services/web-old", map[string]string{
				docker.DocoCDLabels.Deployment.AutoDiscoveryConfig: deleteLabel,
			})},
			configs: []*deployConfig.Config{
				cleanupTestDiscovered("web", services(true, "")),
				{Name: "web-old"},
			},
		},
		{
			name: "auto-discovery config filtered by the webhook event keeps the stack",
			req:  webhookRequest("refs/heads/dev"),
			containers: []container.Summary{cleanupTestStack("web-old", "services/web-old", map[string]string{
				docker.DocoCDLabels.Deployment.AutoDiscoveryConfig: deleteLabel,
			})},
			configs: []*deployConfig.Config{cleanupTestDiscovered("web", services(true, "^refs/heads/main$"))},
		},
		{
			name:       "auto-discovery config matching the webhook event removes the stack",
			req:        webhookRequest("refs/heads/main"),
			containers: []container.Summary{cleanupTestStack("web-old", "services/web-old", nil)},
			configs:    []*deployConfig.Config{cleanupTestDiscovered("web", services(true, "^refs/heads/main$"))},
			want:       []string{"web-old"},
		},
		{
			name: "stack too deep for the auto-discovery config falls back to its label",
			containers: []container.Summary{cleanupTestStack("web-old", "services/nested/web-old", map[string]string{
				docker.DocoCDLabels.Deployment.AutoDiscoveryConfig: keepLabel,
			})},
			configs: []*deployConfig.Config{cleanupTestDiscovered("web", deployConfig.AutoDiscoveryOrigin{
				WorkingDirectory: "services",
				Settings:         deployConfig.AutoDiscoveryConfig{Enabled: true, Delete: true, ScanDepth: 1},
			})},
		},
		{
			name:       "every auto-discovery config scanning the stack must allow the removal",
			containers: []container.Summary{cleanupTestStack("web-old", "services/web-old", nil)},
			configs: []*deployConfig.Config{
				cleanupTestDiscovered("web", services(true, "")),
				cleanupTestDiscovered("root", deployConfig.AutoDiscoveryOrigin{
					WorkingDirectory: ".",
					Settings:         deployConfig.AutoDiscoveryConfig{Enabled: true, Delete: false},
				}),
			},
		},
		{
			name: "auto-discovery config of another repository does not own the stack",
			containers: []container.Summary{cleanupTestStack("web-old", "services/web-old", map[string]string{
				docker.DocoCDLabels.Deployment.AutoDiscoveryConfig: keepLabel,
			})},
			configs: []*deployConfig.Config{cleanupTestDiscovered("web", deployConfig.AutoDiscoveryOrigin{
				WorkingDirectory: "services",
				RepositoryURL:    "https://example.com/organization/other.git",
				Settings:         deployConfig.AutoDiscoveryConfig{Enabled: true, Delete: true},
			})},
		},
		{
			name: "stack of another repository is kept",
			containers: []container.Summary{cleanupTestStack("web-old", "services/web-old", map[string]string{
				docker.DocoCDLabels.Source.URL: "https://example.com/organization/other.git",
			})},
			configs: []*deployConfig.Config{cleanupTestDiscovered("web", services(true, ""))},
		},
		{
			name: "stopped and running containers of a stack are removed together",
			containers: []container.Summary{
				cleanupTestStack("web-old", "services/web-old", nil),
				func() container.Summary {
					c := cleanupTestStack("web-old", "services/web-old", nil)
					c.Names = []string{"/web-old-db"}
					c.State = container.StateExited

					return c
				}(),
			},
			configs: []*deployConfig.Config{cleanupTestDiscovered("web", services(true, ""))},
			want:    []string{"web-old"},
		},
		{
			name:       "oci stack deployed from another tag is removed",
			req:        ociRequest("ghcr.io/org/app:v2"),
			containers: []container.Summary{ociStack("ghcr.io/org/app:v1")},
			configs:    []*deployConfig.Config{cleanupTestDiscovered("web", services(true, ""))},
			want:       []string{"web-old"},
		},
		{
			name:       "oci stack deployed from a digest is removed",
			req:        ociRequest("ghcr.io/org/app:v2"),
			containers: []container.Summary{ociStack("ghcr.io/org/app@" + ociTestDigest)},
			configs:    []*deployConfig.Config{cleanupTestDiscovered("web", services(true, ""))},
			want:       []string{"web-old"},
		},
		{
			name:       "oci stack of another repository is kept",
			req:        ociRequest("ghcr.io/org/app:v2"),
			containers: []container.Summary{ociStack("ghcr.io/org/other:v2")},
			configs:    []*deployConfig.Config{cleanupTestDiscovered("web", services(true, ""))},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := tt.req
			if req.JobTrigger == "" {
				req = cleanupTestRequest()
			}

			got := runCleanupDecision(t, req, tt.containers, tt.configs)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("removable stacks = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCleanupObsoleteAutoDiscoveredContainers_KeepsNewerDeployments(t *testing.T) {
	t.Parallel()

	repoDir, older, newer := newTestRepoWithTwoCommits(t)

	discovered := func(revision string) []*deployConfig.Config {
		return []*deployConfig.Config{cleanupTestDiscovered("web", deployConfig.AutoDiscoveryOrigin{
			WorkingDirectory: "services",
			Revision:         revision,
			MirrorDir:        repoDir,
			Settings:         deployConfig.AutoDiscoveryConfig{Enabled: true, Delete: true},
		})}
	}

	deployedAt := func(revision string) []container.Summary {
		return []container.Summary{cleanupTestStack("web-old", "services/web-old", map[string]string{
			docker.DocoCDLabels.Deployment.CommitSHA: revision,
			docker.DocoCDLabels.Deployment.AutoDiscoveryConfig: docker.MarshalAutoDiscoveryConfig(
				deployConfig.AutoDiscoveryConfig{Enabled: true, Delete: true}),
		})}
	}

	if got := runCleanupDecision(t, cleanupTestRequest(), deployedAt(newer), discovered(older)); len(got) != 0 {
		t.Fatalf("stack deployed at a newer revision than the scan was removable: %v", got)
	}

	if got := runCleanupDecision(t, cleanupTestRequest(), deployedAt(older), discovered(newer)); !slices.Equal(got, []string{"web-old"}) {
		t.Fatalf("stack deployed at an older revision than the scan was not removable: %v", got)
	}

	// Without a current auto-discovery config, the job's revision guards the stack.
	req := cleanupTestRequest()
	req.Repository.MirrorDir = repoDir
	req.Repository.Revision = older

	if got := runCleanupDecision(t, req, deployedAt(newer), nil); len(got) != 0 {
		t.Fatalf("stack deployed at a newer revision than the job was removable: %v", got)
	}
}

// newTestRepoWithTwoCommits returns a Git repository whose second commit is a child of the first.
func newTestRepoWithTwoCommits(t *testing.T) (dir, older, newer string) {
	t.Helper()

	dir = t.TempDir()

	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	var commits []string

	for i := range 2 {
		sig := &object.Signature{Name: "Jane Doe", Email: "jane@example.com", When: time.Date(2026, 1, 1, 0, i, 0, 0, time.UTC)}

		hash, err := wt.Commit("commit", &gogit.CommitOptions{AllowEmptyCommits: true, Author: sig, Committer: sig})
		if err != nil {
			t.Fatalf("commit: %v", err)
		}

		commits = append(commits, hash.String())
	}

	return dir, commits[0], commits[1]
}

func TestObsoleteStackCleanup_RemovalPolicyCombinesOwners(t *testing.T) {
	t.Parallel()

	origin := func(dir string, settings deployConfig.AutoDiscoveryConfig) *deployConfig.Config {
		return cleanupTestDiscovered(dir, deployConfig.AutoDiscoveryOrigin{WorkingDirectory: dir, Settings: settings})
	}

	c := newObsoleteStackCleanup(slog.New(slog.DiscardHandler), nil, false, "", cleanupTestRequest(), []*deployConfig.Config{
		origin(".", deployConfig.AutoDiscoveryConfig{Enabled: true, Delete: true, RemoveVolumes: true, RemoveImages: false}),
		origin("services", deployConfig.AutoDiscoveryConfig{Enabled: true, Delete: true, RemoveVolumes: true, RemoveImages: true}),
		// Discovering several stacks yields one origin per stack.
		origin("services", deployConfig.AutoDiscoveryConfig{Enabled: true, Delete: true, RemoveVolumes: true, RemoveImages: true}),
	})

	if len(c.origins) != 2 {
		t.Fatalf("origins = %d, want 2", len(c.origins))
	}

	stack := cleanupTestStack("web-old", "services/web-old", nil)

	policy, remove := c.removalPolicy(slog.New(slog.DiscardHandler), map[docker.Service]map[string]string{"web-old-app": stack.Labels})
	if !remove {
		t.Fatal("stack is not removable")
	}

	if !policy.RemoveVolumes || policy.RemoveImages {
		t.Fatalf("policy = %+v, want volumes removed and images kept", policy)
	}
}

func TestObsoleteStackCleanup_RemoveSkipsChangedStacks(t *testing.T) {
	t.Parallel()

	listed := cleanupTestStack("web-old", "services/web-old", map[string]string{
		docker.DocoCDLabels.Deployment.Timestamp: "2026-01-01T00:00:00Z",
	})
	redeployed := cleanupTestStack("web-old", "services/web-old", map[string]string{
		docker.DocoCDLabels.Deployment.Timestamp: "2026-01-01T00:05:00Z",
	})

	services := map[docker.Service]map[string]string{"/web-old-app": listed.Labels}
	listErr := errors.New("list failed")

	tests := []struct {
		name    string
		client  *cleanupTestClient
		wantErr error
	}{
		{name: "stack was removed", client: &cleanupTestClient{}},
		{name: "stack was redeployed", client: &cleanupTestClient{containers: []container.Summary{redeployed}}},
		{
			name:    "stack cannot be listed",
			client:  &cleanupTestClient{listErr: map[string]error{docker.DocoCDLabels.Deployment.Name + "=web-old": listErr}},
			wantErr: listErr,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// The CLI cannot destroy stacks, so a removal would panic.
			c := newObsoleteStackCleanup(slog.New(slog.DiscardHandler), cleanupTestCLI{apiClient: tt.client}, false, "",
				cleanupTestRequest(), nil)

			err := c.remove(t.Context(), slog.New(slog.DiscardHandler), "web-old", services,
				deployConfig.AutoDiscoveryConfig{Delete: true}, nil)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("remove() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestCleanupObsoleteAutoDiscoveredContainers_ContinuesAfterErrors(t *testing.T) {
	t.Parallel()

	apiClient := &cleanupTestClient{
		containers: []container.Summary{
			cleanupTestStack("a-old", "services/a-old", nil),
			cleanupTestStack("b-old", "services/b-old", nil),
		},
		listErr: map[string]error{
			docker.DocoCDLabels.Deployment.Name + "=a-old": errors.New("list a failed"),
			docker.DocoCDLabels.Deployment.Name + "=b-old": errors.New("list b failed"),
		},
	}

	err := cleanupObsoleteAutoDiscoveredContainers(
		t.Context(),
		slog.New(slog.DiscardHandler),
		cleanupTestCLI{apiClient: apiClient},
		false,
		"",
		cleanupTestRequest(),
		[]*deployConfig.Config{cleanupTestDiscovered("web", deployConfig.AutoDiscoveryOrigin{
			WorkingDirectory: "services",
			Settings:         deployConfig.AutoDiscoveryConfig{Enabled: true, Delete: true},
		})},
		nil,
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "list a failed") || !strings.Contains(err.Error(), "list b failed") {
		t.Fatalf("cleanupObsoleteAutoDiscoveredContainers() error = %v, want the errors of both stacks", err)
	}
}

func TestArtifactRelativeDir(t *testing.T) {
	t.Parallel()

	const repo = "example.com/organization/repository"

	tests := []struct {
		name       string
		workingDir string
		storeName  string
		want       string
		wantOK     bool
	}{
		{name: "nested directory", workingDir: "/data/" + repo + "/artifacts/abc/services/web", storeName: repo, want: "services/web", wantOK: true},
		{name: "artifact root", workingDir: "/data/" + repo + "/artifacts/abc", storeName: repo, want: ".", wantOK: true},
		{name: "trailing slash", workingDir: "/data/" + repo + "/artifacts/abc/web/", storeName: repo, want: "web", wantOK: true},
		{name: "other repository", workingDir: "/data/example.com/organization/other/artifacts/abc/web", storeName: repo},
		{name: "repository name suffix", workingDir: "/data/example.com/organization/my-repository/artifacts/abc/web", storeName: repo},
		{name: "legacy checkout", workingDir: "/data/" + repo + "/web", storeName: repo},
		{name: "artifacts directory", workingDir: "/data/" + repo + "/artifacts", storeName: repo},
		{name: "empty working directory", storeName: repo},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := artifactRelativeDir(tt.workingDir, tt.storeName)
			if got != tt.want || ok != tt.wantOK {
				t.Fatalf("artifactRelativeDir() = %q, %v, want %q, %v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestIsCleanupTargetMatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		runConfigTargets set.Set[string]
		stackTarget      string
		want             bool
	}{
		{
			name:             "legacy mode when no run target is available",
			runConfigTargets: set.New[string](),
			stackTarget:      "dev",
			want:             true,
		},
		{
			name:             "custom target matches same target",
			runConfigTargets: set.New("updater"),
			stackTarget:      "updater",
			want:             true,
		},
		{
			name:             "custom target does not match different target",
			runConfigTargets: set.New("updater"),
			stackTarget:      "dev",
			want:             false,
		},
		{
			name:             "custom target does not match unlabeled stack",
			runConfigTargets: set.New("updater"),
			stackTarget:      "",
			want:             false,
		},
		{
			name:             "default target matches unlabeled stack",
			runConfigTargets: set.New(""),
			stackTarget:      "",
			want:             true,
		},
		{
			name:             "default target matches default label",
			runConfigTargets: set.New(""),
			stackTarget:      "  ",
			want:             true,
		},
		{
			name:             "default target does not match custom target stack",
			runConfigTargets: set.New(""),
			stackTarget:      "dev",
			want:             false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := isCleanupTargetMatch(tt.runConfigTargets, tt.stackTarget); got != tt.want {
				t.Fatalf("isCleanupTargetMatch() = %v, want %v", got, tt.want)
			}
		})
	}
}
