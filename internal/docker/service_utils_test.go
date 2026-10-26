package docker

import (
	"context"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/config/deploy"

	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/test"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func TestGetServiceStatusFromContainerStatus_ExcludesHookContainers(t *testing.T) {
	t.Parallel()

	const project = "test-project"

	realLabels := Labels{
		api.ProjectLabel:                    project,
		api.ServiceLabel:                    "scheduled",
		DocoCDLabels.Source.Name:            "owner/repo",
		DocoCDLabels.Deployment.CommitSHA:   "real-commit",
		DocoCDLabels.Deployment.ComposeHash: "real-compose",
		DocoCDLabels.Deployment.AutoDiscoveryConfig: MarshalAutoDiscoveryConfig(deploy.AutoDiscoveryConfig{
			Enabled: true,
			Delete:  true,
		}),
	}
	source := container.Summary{ID: "source", State: container.StateCreated, Labels: realLabels}

	for _, state := range []container.ContainerState{container.StateCreated, container.StateRunning, container.StateExited} {
		for _, carriesMetadata := range []bool{false, true} {
			hookLabels := Labels{
				api.ProjectLabel: project,
				api.ServiceLabel: "scheduled",
				api.HookLabel:    "pre_start",
			}
			if carriesMetadata {
				hookLabels[DocoCDLabels.Source.Name] = "owner/repo"
				hookLabels[DocoCDLabels.Deployment.CommitSHA] = "hook-commit"
				hookLabels[DocoCDLabels.Deployment.ComposeHash] = "hook-compose"
				hookLabels[DocoCDLabels.Deployment.AutoDiscoveryConfig] = "{enabled: false}"
			}

			hook := container.Summary{ID: "hook", State: state, Labels: hookLabels}
			orders := []struct {
				name       string
				containers []container.Summary
			}{
				{name: "hook first", containers: []container.Summary{hook, source}},
				{name: "source first", containers: []container.Summary{source, hook}},
				{name: "hook only", containers: []container.Summary{hook}},
			}

			for _, order := range orders {
				t.Run(string(state)+"/metadata="+strconv.FormatBool(carriesMetadata)+"/"+order.name, func(t *testing.T) {
					t.Parallel()

					want := map[Service]ServiceStatus{}
					if order.name != "hook only" {
						want["scheduled"] = ServiceStatus{Labels: realLabels, Containers: 1}
					}

					got := getServiceStatusFromContainerStatus(project, order.containers)
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("service status = %#v, want %#v", got, want)
					}
				})
			}
		}
	}
}

func TestGetServiceStatusFromContainerStatus_PreservesRealContainers(t *testing.T) {
	t.Parallel()

	const project = "test-project"

	labels := Labels{api.ProjectLabel: project, api.ServiceLabel: "app"}
	emptyHookLabels := Labels{api.ProjectLabel: project, api.ServiceLabel: "app", api.HookLabel: ""}
	hookLabels := Labels{api.ProjectLabel: project, api.ServiceLabel: "app", api.HookLabel: "pre_start"}
	otherLabels := Labels{api.ProjectLabel: "other-project", api.ServiceLabel: "app"}
	projectOnlyLabels := Labels{api.ProjectLabel: project, "com.example.helper": "true"}
	emptyServiceLabels := Labels{api.ProjectLabel: project, api.ServiceLabel: ""}
	blankServiceLabels := Labels{api.ProjectLabel: project, api.ServiceLabel: "  "}

	tests := []struct {
		name       string
		containers []container.Summary
		want       map[Service]ServiceStatus
	}{
		{
			name: "empty inventory",
			want: map[Service]ServiceStatus{},
		},
		{
			name:       "nil labels",
			containers: []container.Summary{{State: container.StateRunning}},
			want:       map[Service]ServiceStatus{},
		},
		{
			name:       "other project",
			containers: []container.Summary{{State: container.StateRunning, Labels: otherLabels}},
			want:       map[Service]ServiceStatus{},
		},
		{
			name:       "project label without service label",
			containers: []container.Summary{{State: container.StateRunning, Labels: projectOnlyLabels}},
			want:       map[Service]ServiceStatus{},
		},
		{
			name:       "empty service label",
			containers: []container.Summary{{State: container.StateRunning, Labels: emptyServiceLabels}},
			want:       map[Service]ServiceStatus{},
		},
		{
			name:       "blank service label",
			containers: []container.Summary{{State: container.StateRunning, Labels: blankServiceLabels}},
			want:       map[Service]ServiceStatus{},
		},
		{
			name: "container without service label next to real service",
			containers: []container.Summary{
				{State: container.StateRunning, Labels: projectOnlyLabels},
				{State: container.StateRunning, Labels: labels},
			},
			want: map[Service]ServiceStatus{"app": {Labels: labels, Replicas: 1, Containers: 1}},
		},
		{
			name:       "unmanaged running service",
			containers: []container.Summary{{State: container.StateRunning, Labels: labels}},
			want:       map[Service]ServiceStatus{"app": {Labels: labels, Replicas: 1, Containers: 1}},
		},
		{
			name:       "unmanaged created service",
			containers: []container.Summary{{State: container.StateCreated, Labels: labels}},
			want:       map[Service]ServiceStatus{"app": {Labels: labels, Containers: 1}},
		},
		{
			name:       "unmanaged stopped service",
			containers: []container.Summary{{State: container.StateExited, Labels: labels}},
			want:       map[Service]ServiceStatus{"app": {Labels: labels, Containers: 1}},
		},
		{
			name:       "empty hook label is not a runner",
			containers: []container.Summary{{State: container.StateRunning, Labels: emptyHookLabels}},
			want:       map[Service]ServiceStatus{"app": {Labels: emptyHookLabels, Replicas: 1, Containers: 1}},
		},
		{
			name: "running replicas and all real containers count",
			containers: []container.Summary{
				{State: container.StateRunning, Labels: hookLabels},
				{State: container.StateRunning, Labels: labels},
				{State: container.StateExited, Labels: labels},
				{State: container.StateCreated, Labels: labels},
				{State: container.StateRunning, Labels: labels},
			},
			want: map[Service]ServiceStatus{"app": {Labels: labels, Replicas: 2, Containers: 4}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := getServiceStatusFromContainerStatus(project, tt.containers)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("service status = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestGetLatestServiceStatus_IgnoresHookMetadata(t *testing.T) {
	t.Parallel()

	const project = "test-project"

	autoDiscovery := MarshalAutoDiscoveryConfig(deploy.AutoDiscoveryConfig{Enabled: true})
	realLabels := Labels{
		api.ProjectLabel:                            project,
		api.ServiceLabel:                            "scheduled",
		DocoCDLabels.Source.Name:                    "owner/repo",
		DocoCDLabels.Deployment.CommitSHA:           "real-commit",
		DocoCDLabels.Deployment.ComposeHash:         "real-compose",
		DocoCDLabels.Deployment.AutoDiscoveryConfig: autoDiscovery,
		DocoCDLabels.Deployment.Timestamp:           "2026-10-14T07:00:00Z",
	}
	hookLabels := Labels{
		api.ProjectLabel:                            project,
		api.ServiceLabel:                            "scheduled",
		api.HookLabel:                               "pre_start",
		DocoCDLabels.Source.Name:                    "owner/repo",
		DocoCDLabels.Deployment.CommitSHA:           "hook-commit",
		DocoCDLabels.Deployment.ComposeHash:         "hook-compose",
		DocoCDLabels.Deployment.AutoDiscoveryConfig: "{enabled: false}",
		DocoCDLabels.Deployment.Timestamp:           "2026-10-14T08:00:00Z",
	}
	hook := container.Summary{State: container.StateCreated, Labels: hookLabels}
	source := container.Summary{State: container.StateCreated, Labels: realLabels}

	for _, tt := range []struct {
		name       string
		containers []container.Summary
		want       LatestServiceStatus
	}{
		{
			name:       "hook first",
			containers: []container.Summary{hook, source},
			want: LatestServiceStatus{
				deploymentCommitSHA:   "real-commit",
				deploymentComposeHash: "real-compose",
				DeployedStatus:        map[Service]ServiceStatus{"scheduled": {Labels: realLabels, Containers: 1}},
			},
		},
		{
			name:       "source first",
			containers: []container.Summary{source, hook},
			want: LatestServiceStatus{
				deploymentCommitSHA:   "real-commit",
				deploymentComposeHash: "real-compose",
				DeployedStatus:        map[Service]ServiceStatus{"scheduled": {Labels: realLabels, Containers: 1}},
			},
		},
		{
			name:       "hook only",
			containers: []container.Summary{hook},
			want:       LatestServiceStatus{DeployedStatus: map[Service]ServiceStatus{}},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			inventory := getServiceStatusFromContainerStatus(project, tt.containers)

			got := getLatestServiceStatus(&sync.Map{}, inventory, "https://github.com/owner/repo.git", project)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("deployment status = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// Helper containers that other tools start with only the Compose project label must not cause a
// service mismatch. Otherwise, every poll redeploys the stack while the container exists.
func TestServiceInventory_IgnoresContainersWithoutServiceLabel(t *testing.T) {
	t.Parallel()

	const project = "ops"

	appLabels := Labels{api.ProjectLabel: project, api.ServiceLabel: "app"}
	helperLabels := Labels{api.ProjectLabel: project, "com.example.helper": "true"}

	containers := []container.Summary{
		{State: container.StateRunning, Labels: helperLabels},
		{State: container.StateRunning, Labels: appLabels},
	}

	inventory := getServiceStatusFromContainerStatus(project, containers)
	status := getLatestServiceStatus(&sync.Map{}, inventory, "https://github.com/owner/repo.git", project)

	want := map[Service]ServiceStatus{"app": {Labels: appLabels, Replicas: 1, Containers: 1}}
	if !reflect.DeepEqual(status.DeployedStatus, want) {
		t.Fatalf("deployed status = %#v, want %#v", status.DeployedStatus, want)
	}

	mismatches, err := CheckServiceMismatch(false, status.DeployedStatus, types.Services{"app": {Name: "app"}})
	if err != nil {
		t.Fatalf("CheckServiceMismatch() error = %v", err)
	}

	if len(mismatches) != 0 {
		t.Fatalf("CheckServiceMismatch() = %#v, want no mismatches", mismatches)
	}
}

func TestDeploymentTimestampAtLeastSubsecond(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		current, previous string
		want              bool
	}{
		{"2026-01-01T12:00:00.000000001Z", "2026-01-01T12:00:00Z", true},
		{"2026-01-01T12:00:00Z", "2026-01-01T12:00:00.000000001Z", false},
		{"2026-01-01T12:00:00.1Z", "2026-01-01T12:00:00.11Z", false},
		{"2026-01-01T12:00:00.1Z", "2026-01-01T12:00:00.1Z", true},
		{"2026-01-01T12:00:00.1Z", "", true},
	} {
		if got := deploymentTimestampAtLeast(tc.current, tc.previous); got != tc.want {
			t.Fatalf("timestamp %s >= %s = %v, want %v", tc.current, tc.previous, got, tc.want)
		}
	}
}

func Test_getLatestServiceState(t *testing.T) {
	t.Parallel()

	cache := &sync.Map{}
	cache.Store(getDeployStatusCacheKey("github.com/owner/repo", "cache"), deployStatus{
		ComposeHash: "cache_compose_hash",
		CommitSHA:   "cache_commit_sha",
	})

	tests := []struct {
		name          string
		serviceStatus map[Service]ServiceStatus
		repoName      string
		deployName    string
		cache         *sync.Map
		want          LatestServiceStatus
	}{
		{
			name:          "empty serviceLabels",
			serviceStatus: map[Service]ServiceStatus{},
			repoName:      "repo",
			deployName:    "deploy",
			want: LatestServiceStatus{
				DeployedStatus: map[Service]ServiceStatus{},
			},
		},
		{
			name: "single service with no timestamp",
			serviceStatus: map[Service]ServiceStatus{
				"svc1": {
					Labels: Labels{
						DocoCDLabels.Source.Name:            "repo",
						DocoCDLabels.Deployment.CommitSHA:   "commit_sha",
						DocoCDLabels.Deployment.ComposeHash: "compose_hash",
					},
					Replicas: 1,
				},
			},
			repoName: "repo",
			want: LatestServiceStatus{
				deploymentCommitSHA:   "commit_sha",
				deploymentComposeHash: "compose_hash",
				DeployedStatus: map[Service]ServiceStatus{
					"svc1": {
						Labels: Labels{
							DocoCDLabels.Source.Name:            "repo",
							DocoCDLabels.Deployment.CommitSHA:   "commit_sha",
							DocoCDLabels.Deployment.ComposeHash: "compose_hash",
						},
						Replicas: 1,
					},
				},
			},
		},
		{
			name: "single service but repo this is full clone URL",
			serviceStatus: map[Service]ServiceStatus{
				"svc1": {
					Labels: Labels{
						DocoCDLabels.Source.Name:            "owner/repo",
						DocoCDLabels.Deployment.CommitSHA:   "commit_sha",
						DocoCDLabels.Deployment.ComposeHash: "compose_hash",
					},
					Replicas: 1,
				},
			},
			repoName: "https://github.com/owner/repo.git",
			want: LatestServiceStatus{
				deploymentCommitSHA:   "commit_sha",
				deploymentComposeHash: "compose_hash",
				DeployedStatus: map[Service]ServiceStatus{
					"svc1": {
						Labels: Labels{
							DocoCDLabels.Source.Name:            "owner/repo",
							DocoCDLabels.Deployment.CommitSHA:   "commit_sha",
							DocoCDLabels.Deployment.ComposeHash: "compose_hash",
						},
						Replicas: 1,
					},
				},
			},
		},
		{
			name: "single service with OCI artifact reference including tag",
			serviceStatus: map[Service]ServiceStatus{
				"svc1": {
					Labels: Labels{
						DocoCDLabels.Source.Name:            "ghcr.io/kimdre/doco-cd_tests",
						DocoCDLabels.Deployment.CommitSHA:   "sha256:deadbeef",
						DocoCDLabels.Deployment.ComposeHash: "compose_hash",
					},
					Replicas: 1,
				},
			},
			repoName: "ghcr.io/kimdre/doco-cd_tests:main",
			want: LatestServiceStatus{
				deploymentCommitSHA:   "sha256:deadbeef",
				deploymentComposeHash: "compose_hash",
				DeployedStatus: map[Service]ServiceStatus{
					"svc1": {
						Labels: Labels{
							DocoCDLabels.Source.Name:            "ghcr.io/kimdre/doco-cd_tests",
							DocoCDLabels.Deployment.CommitSHA:   "sha256:deadbeef",
							DocoCDLabels.Deployment.ComposeHash: "compose_hash",
						},
						Replicas: 1,
					},
				},
			},
		},
		{
			name: "cache hit, single service with no timestamp",
			serviceStatus: map[Service]ServiceStatus{
				"svc1": {
					Labels: Labels{
						DocoCDLabels.Source.Name:            "owner/repo",
						DocoCDLabels.Deployment.CommitSHA:   "commit_sha",
						DocoCDLabels.Deployment.ComposeHash: "compose_hash",
					},
					Replicas: 1,
				},
			},
			repoName:   "https://github.com/owner/repo.git",
			deployName: "cache",
			cache:      cache,
			want: LatestServiceStatus{
				deploymentCommitSHA:   "cache_commit_sha",
				deploymentComposeHash: "cache_compose_hash",
				DeployedStatus: map[Service]ServiceStatus{
					"svc1": {
						Labels: Labels{
							DocoCDLabels.Source.Name:            "owner/repo",
							DocoCDLabels.Deployment.CommitSHA:   "commit_sha",
							DocoCDLabels.Deployment.ComposeHash: "compose_hash",
						},
						Replicas: 1,
					},
				},
			},
		},
		{
			name: "single service with timestamp",
			serviceStatus: map[Service]ServiceStatus{
				"svc1": {
					Labels: Labels{
						DocoCDLabels.Source.Name:            "repo",
						DocoCDLabels.Deployment.Timestamp:   "2006-01-02T15:04:05Z07:00",
						DocoCDLabels.Deployment.CommitSHA:   "commit_sha",
						DocoCDLabels.Deployment.ComposeHash: "compose_hash",
					},
					Replicas: 1,
				},
			},
			repoName: "repo",
			want: LatestServiceStatus{
				deploymentCommitSHA:   "commit_sha",
				deploymentComposeHash: "compose_hash",
				DeployedStatus: map[Service]ServiceStatus{
					"svc1": {
						Labels: Labels{
							DocoCDLabels.Source.Name:            "repo",
							DocoCDLabels.Deployment.Timestamp:   "2006-01-02T15:04:05Z07:00",
							DocoCDLabels.Deployment.CommitSHA:   "commit_sha",
							DocoCDLabels.Deployment.ComposeHash: "compose_hash",
						},
						Replicas: 1,
					},
				},
			},
		},
		{
			// All project containers are included in DeployedStatus regardless of repo label.
			// The repo-name filter only governs metadata selection (commit SHA, compose hash).
			name: "two service with timestamp but repo not match",
			serviceStatus: map[Service]ServiceStatus{
				"svc1": {
					Labels: Labels{
						DocoCDLabels.Source.Name:          "repo-2",
						DocoCDLabels.Deployment.Timestamp: "2006-01-02T15:04:05Z07:00",
					},
					Replicas: 1,
				},
				"svc2": {
					Labels: Labels{
						DocoCDLabels.Source.Name:          "repo-2",
						DocoCDLabels.Deployment.Timestamp: "2016-01-02T15:04:05Z07:00",
					},
					Replicas: 1,
				},
			},
			repoName: "repo",
			want: LatestServiceStatus{
				deploymentCommitSHA:   "",
				deploymentComposeHash: "",
				DeployedStatus: map[Service]ServiceStatus{
					"svc1": {
						Labels: Labels{
							DocoCDLabels.Source.Name:          "repo-2",
							DocoCDLabels.Deployment.Timestamp: "2006-01-02T15:04:05Z07:00",
						},
						Replicas: 1,
					},
					"svc2": {
						Labels: Labels{
							DocoCDLabels.Source.Name:          "repo-2",
							DocoCDLabels.Deployment.Timestamp: "2016-01-02T15:04:05Z07:00",
						},
						Replicas: 1,
					},
				},
			},
		},
		{
			// svc2 has a different repo label but is still part of the project, so it must appear
			// in DeployedStatus. Only metadata (commit SHA, compose hash) is drawn from svc1.
			name: "two service with timestamp but repo mixed",
			serviceStatus: map[Service]ServiceStatus{
				"svc1": {
					Labels: Labels{
						DocoCDLabels.Source.Name:            "repo",
						DocoCDLabels.Deployment.Timestamp:   "2006-01-02T15:04:05Z07:00",
						DocoCDLabels.Deployment.CommitSHA:   "commit_sha1",
						DocoCDLabels.Deployment.ComposeHash: "compose_hash1",
					},
					Replicas: 1,
				},
				"svc2": {
					Labels: Labels{
						DocoCDLabels.Source.Name:            "repo-2",
						DocoCDLabels.Deployment.Timestamp:   "2016-01-02T15:04:05Z07:00",
						DocoCDLabels.Deployment.CommitSHA:   "commit_sha2",
						DocoCDLabels.Deployment.ComposeHash: "compose_hash2",
					},
					Replicas: 1,
				},
			},
			repoName: "repo",
			want: LatestServiceStatus{
				deploymentCommitSHA:   "commit_sha1",
				deploymentComposeHash: "compose_hash1",
				DeployedStatus: map[Service]ServiceStatus{
					"svc1": {
						Labels: Labels{
							DocoCDLabels.Source.Name:            "repo",
							DocoCDLabels.Deployment.Timestamp:   "2006-01-02T15:04:05Z07:00",
							DocoCDLabels.Deployment.CommitSHA:   "commit_sha1",
							DocoCDLabels.Deployment.ComposeHash: "compose_hash1",
						},
						Replicas: 1,
					},
					"svc2": {
						Labels: Labels{
							DocoCDLabels.Source.Name:            "repo-2",
							DocoCDLabels.Deployment.Timestamp:   "2016-01-02T15:04:05Z07:00",
							DocoCDLabels.Deployment.CommitSHA:   "commit_sha2",
							DocoCDLabels.Deployment.ComposeHash: "compose_hash2",
						},
						Replicas: 1,
					},
				},
			},
		},
		{
			// Containers without a cd.doco.source.name label contribute no metadata but
			// are still counted as deployed so CheckServiceMismatch does not flag them.
			name: "two service with timestamp but repo mismatch",
			serviceStatus: map[Service]ServiceStatus{
				"svc1": {
					Labels: Labels{
						DocoCDLabels.Deployment.Timestamp:   "2006-01-02T15:04:05Z07:00",
						DocoCDLabels.Deployment.CommitSHA:   "commit_sha1",
						DocoCDLabels.Deployment.ComposeHash: "compose_hash1",
					},
					Replicas: 1,
				},
				"svc2": {
					Labels: Labels{
						DocoCDLabels.Deployment.Timestamp:   "2016-01-02T15:04:05Z07:00",
						DocoCDLabels.Deployment.CommitSHA:   "commit_sha2",
						DocoCDLabels.Deployment.ComposeHash: "compose_hash2",
					},
					Replicas: 1,
				},
			},
			repoName: "repo",
			want: LatestServiceStatus{
				deploymentCommitSHA:   "",
				deploymentComposeHash: "",
				DeployedStatus: map[Service]ServiceStatus{
					"svc1": {
						Labels: Labels{
							DocoCDLabels.Deployment.Timestamp:   "2006-01-02T15:04:05Z07:00",
							DocoCDLabels.Deployment.CommitSHA:   "commit_sha1",
							DocoCDLabels.Deployment.ComposeHash: "compose_hash1",
						},
						Replicas: 1,
					},
					"svc2": {
						Labels: Labels{
							DocoCDLabels.Deployment.Timestamp:   "2016-01-02T15:04:05Z07:00",
							DocoCDLabels.Deployment.CommitSHA:   "commit_sha2",
							DocoCDLabels.Deployment.ComposeHash: "compose_hash2",
						},
						Replicas: 1,
					},
				},
			},
		},
		{
			name: "two service with timestamp but empty repo",
			serviceStatus: map[Service]ServiceStatus{
				"svc1": {
					Labels: Labels{
						DocoCDLabels.Source.Name:            "",
						DocoCDLabels.Deployment.Timestamp:   "2006-01-02T15:04:05Z07:00",
						DocoCDLabels.Deployment.CommitSHA:   "commit_sha1",
						DocoCDLabels.Deployment.ComposeHash: "compose_hash1",
					},
					Replicas: 1,
				},
				"svc2": {
					Labels: Labels{
						DocoCDLabels.Source.Name:            "",
						DocoCDLabels.Deployment.Timestamp:   "2016-01-02T15:04:05Z07:00",
						DocoCDLabels.Deployment.CommitSHA:   "commit_sha2",
						DocoCDLabels.Deployment.ComposeHash: "compose_hash2",
					},
					Replicas: 2,
				},
			},
			repoName: "",
			want: LatestServiceStatus{
				deploymentCommitSHA:   "commit_sha2",
				deploymentComposeHash: "compose_hash2",
				DeployedStatus: map[Service]ServiceStatus{
					"svc2": {
						Labels: Labels{
							DocoCDLabels.Source.Name:            "",
							DocoCDLabels.Deployment.Timestamp:   "2016-01-02T15:04:05Z07:00",
							DocoCDLabels.Deployment.CommitSHA:   "commit_sha2",
							DocoCDLabels.Deployment.ComposeHash: "compose_hash2",
						},
						Replicas: 2,
					},
					"svc1": {
						Labels: Labels{
							DocoCDLabels.Source.Name:            "",
							DocoCDLabels.Deployment.Timestamp:   "2006-01-02T15:04:05Z07:00",
							DocoCDLabels.Deployment.CommitSHA:   "commit_sha1",
							DocoCDLabels.Deployment.ComposeHash: "compose_hash1",
						},
						Replicas: 1,
					},
				},
			},
		},
		{
			// Regression test: containers that have no cd.doco.* labels at all (e.g. recreated
			// via the Docker CLI directly or started before doco-cd first deployed the stack)
			// must still appear in DeployedStatus so CheckServiceMismatch does not produce a
			// false "service not deployed" mismatch and trigger an infinite redeployment loop.
			name: "some services missing cd.doco.* labels entirely",
			serviceStatus: map[Service]ServiceStatus{
				"labeled": {
					Labels: Labels{
						DocoCDLabels.Source.Name:            "repo",
						DocoCDLabels.Deployment.Timestamp:   "2026-01-01T00:00:00Z",
						DocoCDLabels.Deployment.CommitSHA:   "commit_sha",
						DocoCDLabels.Deployment.ComposeHash: "compose_hash",
					},
					Replicas: 1,
				},
				"unlabeled": {
					// No cd.doco.* labels simulates a container recreated outside doco-cd.
					Labels:   Labels{},
					Replicas: 1,
				},
			},
			repoName: "repo",
			want: LatestServiceStatus{
				deploymentCommitSHA:   "commit_sha",
				deploymentComposeHash: "compose_hash",
				DeployedStatus: map[Service]ServiceStatus{
					"labeled": {
						Labels: Labels{
							DocoCDLabels.Source.Name:            "repo",
							DocoCDLabels.Deployment.Timestamp:   "2026-01-01T00:00:00Z",
							DocoCDLabels.Deployment.CommitSHA:   "commit_sha",
							DocoCDLabels.Deployment.ComposeHash: "compose_hash",
						},
						Replicas: 1,
					},
					"unlabeled": {
						Labels:   Labels{},
						Replicas: 1,
					},
				},
			},
		},
		{
			name: "two service with timestamp",
			serviceStatus: map[Service]ServiceStatus{
				"svc1": {
					Labels: Labels{
						DocoCDLabels.Source.Name:          "repo",
						DocoCDLabels.Deployment.Timestamp: "2006-01-02T15:04:05Z07:00",
					},
					Replicas: 1,
				},
				"svc2": {
					Labels: Labels{
						DocoCDLabels.Source.Name:            "repo",
						DocoCDLabels.Deployment.Timestamp:   "2016-01-02T15:04:05Z07:00",
						DocoCDLabels.Deployment.CommitSHA:   "commit_sha2",
						DocoCDLabels.Deployment.ComposeHash: "compose_hash2",
					},
					Replicas: 1,
				},
			},
			repoName: "repo",
			want: LatestServiceStatus{
				deploymentCommitSHA:   "commit_sha2",
				deploymentComposeHash: "compose_hash2",
				DeployedStatus: map[Service]ServiceStatus{
					"svc1": {
						Labels: Labels{
							DocoCDLabels.Source.Name:          "repo",
							DocoCDLabels.Deployment.Timestamp: "2006-01-02T15:04:05Z07:00",
						},
						Replicas: 1,
					},
					"svc2": {
						Labels: Labels{
							DocoCDLabels.Source.Name:            "repo",
							DocoCDLabels.Deployment.Timestamp:   "2016-01-02T15:04:05Z07:00",
							DocoCDLabels.Deployment.CommitSHA:   "commit_sha2",
							DocoCDLabels.Deployment.ComposeHash: "compose_hash2",
						},
						Replicas: 1,
					},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache := &sync.Map{}
			if tt.cache != nil {
				cache = tt.cache
			}

			got := getLatestServiceStatus(cache, tt.serviceStatus, tt.repoName, tt.deployName)

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("GetLatestServiceState() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetLatestServiceState(t *testing.T) {
	dockerCli, err := CreateDockerCli(false)
	if err != nil {
		t.Fatalf("Failed to create Docker CLI: %v", err)
	}

	if resolveTestSwarmMode(t.Context(), t, dockerCli.Client()) {
		t.Skip("Swarm mode is enabled, skipping test")
	}
	// t.Parallel()
	// cannot run in parallel, because container_name is set and need unique.
	ctx := t.Context()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "test.compose.yaml")

	cfg := `
services:
  test:
    image: nginx:latest
    scale: 2
    environment:
      TZ: Europe/Berlin
    ports:
      - "80"
    volumes:
      - ./html:/usr/share/nginx/html
  test2:
    image: nginx:latest
    deploy:
      replicas: 2
    environment:
      TZ: Europe/Berlin
    ports:
      - "80"
    volumes:
      - ./html:/usr/share/nginx/html
  nginx:
    image: nginx:latest
    container_name: nginx
    environment:
      TZ: Europe/Berlin
    volumes:
      - ./html:/usr/share/nginx/html
`
	createComposeFile(t, filePath, cfg)

	stackName := test.ConvertTestName(t.Name())

	_, err = LoadCompose(ctx, nil, tmpDir, tmpDir, stackName, []string{filePath}, []string{".env"}, []string{}, map[string]string{}, ComposeLoadOptions{})
	if err != nil {
		t.Fatal(err)
	}

	repoName := "repoName"

	stack := test.ComposeUp(ctx, t,
		test.WithYAML(cfg),
		test.WithCustomLabel(map[string]string{
			DocoCDLabels.Source.Name: repoName,
		}),
	)

	latest, err := GetLatestDeployStatus(ctx, stack.Client, resolveTestSwarmMode(ctx, t, stack.Client), repoName, stackName)
	if err != nil {
		t.Fatal(err)
	}

	for svc, want := range map[string]struct {
		Replicas uint64
	}{
		"test2": {Replicas: 2},
		"test":  {Replicas: 2},
		"nginx": {Replicas: 1},
	} {
		got := latest.DeployedStatus[Service(svc)]
		if got.Replicas != want.Replicas {
			t.Errorf("expected running tasks for service %s to be %d, got %d", svc, want.Replicas, got.Replicas)
		}

		if got.SwarmMode != "" {
			t.Errorf("expected swarm mode for service %s to be empty, got %s", svc, got.SwarmMode)
		}
	}
}

func TestGetLatestServiceSwarm(t *testing.T) {
	t.Parallel()

	dockerCli, err := CreateDockerCli(false)
	if err != nil {
		t.Fatalf("Failed to create Docker CLI: %v", err)
	}

	if !resolveTestSwarmMode(t.Context(), t, dockerCli.Client()) {
		t.Skip("Swarm mode is not enabled, skipping test")
	}

	ctx := t.Context()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "test.compose.yaml")

	cfg := `
services:
  replicas:
    image: nginx:latest
    deploy:
      replicas: 2
    environment:
      TZ: Europe/Berlin
  global:
    image: nginx:latest
    deploy:
      mode: global
    environment:
      TZ: Europe/Berlin
  replicated-job:
    image: nginx:latest
    command: 'sh -c "exit 0"'
    deploy:
      replicas: 2
      mode: replicated-job
    environment:
      TZ: Europe/Berlin
  global-job:
    image: nginx:latest
    command: 'sh -c "exit 0"'
    deploy:
      mode: global-job
    environment:
      TZ: Europe/Berlin

`
	createComposeFile(t, filePath, cfg)

	stackName := test.ConvertTestName(t.Name())

	project, err := LoadCompose(t.Context(), nil, tmpDir, tmpDir, stackName, []string{filePath}, []string{".env"}, []string{}, map[string]string{}, ComposeLoadOptions{})
	if err != nil {
		t.Fatal(err)
	}

	projectHash, err := ProjectHash(project, "")
	if err != nil {
		t.Fatalf("failed to get project hash: %v", err)
	}

	repoName := "repoName"

	deployCfg := &deploy.Config{
		Name:          stackName,
		RemoveOrphans: true,
	}

	swarmStack, opts, err := LoadSwarmStack(dockerCli, project, deployCfg, tmpDir)
	if err != nil {
		t.Fatalf("Failed to load swarm stack: %v", err)
	}

	p := webhook.ParsedPayload{
		Ref:       git.SwarmModeBranch,
		CommitSHA: plumbing.NewHash("244b6f9a5b3dc546ab3822d9c0744846f539c6ef"),
		Name:      stackName,
		FullName:  repoName,
		CloneURL:  cloneUrlTest,
		Private:   true,
	}

	err = retry.New(
		retry.Attempts(5),
		retry.Delay(2*time.Second),
		retry.Context(ctx),
	).Do(
		func() error {
			timestamp := time.Now().UTC().Format(time.RFC3339)
			addSwarmServiceLabels(swarmStack, project, deployCfg, &p, "", tmpDir, "dev", timestamp, p.CommitSHAString(), projectHash)
			addSwarmVolumeLabels(swarmStack, deployCfg, &p)
			addSwarmConfigLabels(swarmStack, deployCfg, &p, "", tmpDir, "dev", timestamp, p.CommitSHAString())
			addSwarmSecretLabels(swarmStack, deployCfg, &p, "", tmpDir, "dev", timestamp, p.CommitSHAString())

			return DeploySwarmStack(ctx, dockerCli, swarmStack, opts)
		},
	)
	if err != nil {
		t.Fatalf("Failed to deploy swarm stack: %v", err)
	}

	t.Logf("Swarm stack deployed successfully")

	dockerClient := dockerCli.Client()

	latest, err := GetLatestDeployStatus(ctx, dockerClient, resolveTestSwarmMode(ctx, t, dockerClient), repoName, stackName)
	if err != nil {
		t.Fatal(err)
	}

	for svc, want := range map[string]struct {
		Replicas uint64
		Mode     swarm.DeployMode
	}{
		"replicas": {
			Replicas: 2,
			Mode:     swarm.DeployModeReplicated,
		},
		"global": {
			Replicas: 0,
			Mode:     swarm.DeployModeGlobal,
		},
		"replicated-job": {
			Replicas: 2,
			Mode:     swarm.DeployModeReplicatedJob,
		},
		"global-job": {
			Replicas: 0,
			Mode:     swarm.DeployModeGlobalJob,
		},
	} {
		got := latest.DeployedStatus[Service(svc)]
		if got.Replicas != want.Replicas {
			t.Errorf("expected running tasks for service %s to be %d, got %d", svc, want.Replicas, got.Replicas)
		}

		if got.SwarmMode != want.Mode {
			t.Errorf("expected swarm mode for service %s to be %s, got %s", svc, want.Mode, got.SwarmMode)
		}
	}

	t.Cleanup(func() {
		ctx := context.Background()

		err = PruneStackConfigs(ctx, dockerClient, stackName, 0)
		if err != nil {
			t.Fatalf("Failed to prune stack configs: %v", err)
		} else {
			t.Logf("Stack configs pruned successfully")
		}

		err = PruneStackSecrets(ctx, dockerClient, stackName, 0)
		if err != nil {
			t.Fatalf("Failed to prune stack secrets: %v", err)
		} else {
			t.Logf("Stack secrets pruned successfully")
		}

		err = RemoveSwarmStack(ctx, dockerCli, stackName)
		if err != nil {
			t.Fatalf("Failed to remove swarm stack: %v", err)
		} else {
			t.Logf("Swarm stack removed successfully")
		}
	})
}

func TestCheckServiceMismatch(t *testing.T) {
	swarmServices := types.Services{
		"replicated": {
			Name:   "replicated",
			Scale:  new(2),
			Deploy: &types.DeployConfig{Mode: string(swarm.DeployModeReplicated)},
		},
		"replicated-job": {
			Name:   "replicated-job",
			Scale:  new(2),
			Deploy: &types.DeployConfig{Mode: string(swarm.DeployModeReplicatedJob)},
		},
		"global": {
			Name:   "global",
			Deploy: &types.DeployConfig{Mode: string(swarm.DeployModeGlobal)},
		},
		"global-job": {
			Name:   "global-job",
			Deploy: &types.DeployConfig{Mode: string(swarm.DeployModeGlobalJob)},
		},
	}

	tests := []struct {
		name            string
		deployed        map[Service]ServiceStatus
		swarmModeEnable bool
		services        types.Services
		want            []ServiceMismatch
	}{
		{
			name: "swarmMode=false, no mismatch",
			deployed: map[Service]ServiceStatus{
				"foo": {Replicas: 1},
			},
			swarmModeEnable: false,
			services: types.Services{
				"foo": {},
			},
			want: nil,
		},
		{
			name: "swarmMode=false, unnecessary service",
			deployed: map[Service]ServiceStatus{
				"foo": {Replicas: 1},
				"bar": {Replicas: 1},
			},
			swarmModeEnable: false,
			services: types.Services{
				"foo": {},
			},
			want: []ServiceMismatch{
				{
					ServiceName: "bar",
					Reasons: []ServiceMismatchReason{
						{
							Reason: ServiceMismatchReasonUnnecessary,
						},
					},
				},
			},
		},
		{
			name: "swarmMode=false, ignore deploy mode",
			deployed: map[Service]ServiceStatus{
				"foo": {Replicas: 1},
			},
			swarmModeEnable: false,
			services: types.Services{
				"foo": {
					Deploy: &types.DeployConfig{
						Mode: "global",
					},
				},
			},
			want: nil,
		},
		{
			name: "swarmMode=false, mismatch replicas for restart always",
			deployed: map[Service]ServiceStatus{
				"foo": {Replicas: 1},
			},
			swarmModeEnable: false,
			services: types.Services{
				"foo": {
					Restart: "always",
					Scale:   new(2),
				},
			},
			want: []ServiceMismatch{
				{
					ServiceName: "foo",
					Reasons: []ServiceMismatchReason{
						{
							Reason: ServiceMismatchReasonReplicas,
							Want:   2,
							Got:    uint64(1),
						},
					},
				},
			},
		},
		{
			name:            "swarmMode=false, no deployed for restart always",
			deployed:        map[Service]ServiceStatus{},
			swarmModeEnable: false,
			services: types.Services{
				"foo": {
					Restart: "always",
					Scale:   new(2),
				},
			},
			want: []ServiceMismatch{
				{
					ServiceName: "foo",
					Reasons: []ServiceMismatchReason{
						{
							Reason: ServiceMismatchReasonNotDeployed,
						},
					},
				},
			},
		},
		{
			name:            "swarmMode=false, scaled to zero for restart always may remain absent",
			deployed:        map[Service]ServiceStatus{},
			swarmModeEnable: false,
			services: types.Services{
				"foo": {
					Restart: "always",
					Scale:   new(0),
				},
			},
			want: nil,
		},
		{
			name:            "swarmMode=false, no restart policy may remain stopped",
			deployed:        map[Service]ServiceStatus{},
			swarmModeEnable: false,
			services: types.Services{
				"job": {},
			},
			want: nil,
		},
		{
			name:            "swarmMode=false, restart on-failure may remain stopped",
			deployed:        map[Service]ServiceStatus{},
			swarmModeEnable: false,
			services: types.Services{
				"job": {
					Restart: "on-failure",
				},
			},
			want: nil,
		},
		{
			name:            "swarmMode=false, restart no may remain stopped",
			deployed:        map[Service]ServiceStatus{},
			swarmModeEnable: false,
			services: types.Services{
				"job": {
					Restart: "no",
				},
			},
			want: nil,
		},
		{
			name: "swarmMode=false, restart always should stay running",
			deployed: map[Service]ServiceStatus{
				"web": {Replicas: 0},
			},
			swarmModeEnable: false,
			services: types.Services{
				"web": {
					Restart: "always",
				},
			},
			want: []ServiceMismatch{
				{
					ServiceName: "web",
					Reasons: []ServiceMismatchReason{
						{
							Reason: ServiceMismatchReasonReplicas,
							Want:   1,
							Got:    uint64(0),
						},
					},
				},
			},
		},
		{
			name: "swarmMode=false, restart unless-stopped should stay running",
			deployed: map[Service]ServiceStatus{
				"web": {Replicas: 0},
			},
			swarmModeEnable: false,
			services: types.Services{
				"web": {
					Restart: "unless-stopped",
				},
			},
			want: []ServiceMismatch{
				{
					ServiceName: "web",
					Reasons: []ServiceMismatchReason{
						{
							Reason: ServiceMismatchReasonReplicas,
							Want:   1,
							Got:    uint64(0),
						},
					},
				},
			},
		},
		{
			name: "swarmMode=true, no missing",
			deployed: map[Service]ServiceStatus{
				"foo":         {Replicas: 1, SwarmMode: swarm.DeployModeReplicated},
				"replicated":  {Replicas: 1, SwarmMode: swarm.DeployModeReplicated},
				"replicated2": {Replicas: 1, SwarmMode: swarm.DeployModeReplicated},
			},
			swarmModeEnable: true,
			services: types.Services{
				"foo": {
					Deploy: &types.DeployConfig{Mode: string(swarm.DeployModeReplicated), Replicas: new(1)},
				},
				"replicated": {
					Name: "replicated",
				},
				"replicated2": {
					Name:   "replicated2",
					Deploy: &types.DeployConfig{Replicas: new(1)},
				},
			},
			want: nil,
		},
		{
			name:            "swarmMode=true, replicated scaled to zero may remain absent",
			deployed:        map[Service]ServiceStatus{},
			swarmModeEnable: true,
			services: types.Services{
				"replicated": {
					Name:   "replicated",
					Scale:  new(0),
					Deploy: &types.DeployConfig{Mode: string(swarm.DeployModeReplicated)},
				},
			},
			want: nil,
		},
		{
			name: "swarmMode=true, unnecessary service",
			deployed: map[Service]ServiceStatus{
				"foo":        {Replicas: 1, SwarmMode: swarm.DeployModeReplicated},
				"replicated": {Replicas: 1, SwarmMode: swarm.DeployModeReplicated},
			},
			swarmModeEnable: true,
			services: types.Services{
				"foo": {
					Deploy: &types.DeployConfig{Mode: string(swarm.DeployModeReplicated), Replicas: new(1)},
				},
			},
			want: []ServiceMismatch{
				{
					ServiceName: "replicated",
					Reasons: []ServiceMismatchReason{
						{
							Reason: ServiceMismatchReasonUnnecessary,
						},
					},
				},
			},
		},
		{
			name: "swarmMode=true, ignore unnecessary scheduler ephemeral by label",
			deployed: map[Service]ServiceStatus{
				"foo": {Replicas: 1, SwarmMode: swarm.DeployModeReplicated},
				"foo-doco-job-1730000000000000000": {
					Replicas:  1,
					SwarmMode: swarm.DeployModeReplicatedJob,
					Labels: Labels{
						DocoCDJobLabels.JobEphemeral: "true",
					},
				},
			},
			swarmModeEnable: true,
			services: types.Services{
				"foo": {
					Deploy: &types.DeployConfig{Mode: string(swarm.DeployModeReplicated), Replicas: new(1)},
				},
			},
			want: nil,
		},
		{
			name: "swarmMode=true, ignore unnecessary scheduler ephemeral by execution mode",
			deployed: map[Service]ServiceStatus{
				"foo": {Replicas: 1, SwarmMode: swarm.DeployModeReplicated},
				"foo-doco-job-1730000000000000000": {
					Replicas:  1,
					SwarmMode: swarm.DeployModeReplicatedJob,
					Labels: Labels{
						DocoCDJobLabels.JobExecutionMode: string(JobExecutionModeOneOff),
					},
				},
			},
			swarmModeEnable: true,
			services: types.Services{
				"foo": {
					Deploy: &types.DeployConfig{Mode: string(swarm.DeployModeReplicated), Replicas: new(1)},
				},
			},
			want: nil,
		},
		{
			name: "swarmMode=true, no missing",
			deployed: map[Service]ServiceStatus{
				"replicated":     {Replicas: 2, SwarmMode: swarm.DeployModeReplicated},
				"replicated-job": {Replicas: 2, SwarmMode: swarm.DeployModeReplicatedJob},
				// global ignore replicas
				"global":     {Replicas: 2, SwarmMode: swarm.DeployModeGlobal},
				"global-job": {Replicas: 2, SwarmMode: swarm.DeployModeGlobalJob},
			},
			swarmModeEnable: true,
			services:        swarmServices,
			want:            nil,
		},
		{
			name: "swarmMode=true, mismatch",
			deployed: map[Service]ServiceStatus{
				"replicated":     {Replicas: 2, SwarmMode: swarm.DeployModeReplicatedJob},
				"replicated-job": {Replicas: 1, SwarmMode: swarm.DeployModeReplicatedJob},
				"global-job":     {Replicas: 2, SwarmMode: swarm.DeployModeGlobalJob},
			},
			swarmModeEnable: true,
			services:        swarmServices,
			want: []ServiceMismatch{
				{
					ServiceName: "replicated",
					Reasons: []ServiceMismatchReason{
						{
							Reason: ServiceMismatchReasonSwarmMode,
							Want:   swarm.DeployModeReplicated,
							Got:    swarm.DeployModeReplicatedJob,
						},
					},
				},
				{
					ServiceName: "replicated-job",
					Reasons: []ServiceMismatchReason{
						{
							Reason: ServiceMismatchReasonReplicas,
							Want:   2,
							Got:    uint64(1),
						},
					},
				},
				{
					ServiceName: "global",
					Reasons: []ServiceMismatchReason{
						{
							Reason: ServiceMismatchReasonNotDeployed,
						},
					},
				},
			},
		},
	}
	cmpFunc := func(a, b ServiceMismatch) int {
		return strings.Compare(a.ServiceName, b.ServiceName)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CheckServiceMismatch(tt.swarmModeEnable, tt.deployed, tt.services)
			if err != nil {
				t.Fatal(err)
			}

			slices.SortFunc(got, cmpFunc)
			slices.SortFunc(tt.want, cmpFunc)

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("CheckServiceMismatch() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestCheckServiceMismatch_AutostartDisabled(t *testing.T) {
	t.Parallel()

	for _, policy := range []struct {
		restart         string
		enforceReplicas bool
	}{
		{restart: ""},
		{restart: "no"},
		{restart: "on-failure"},
		{restart: "on-failure:3"},
		{restart: "always", enforceReplicas: true},
		{restart: "unless-stopped", enforceReplicas: true},
	} {
		for _, tt := range []struct {
			name       string
			present    bool
			replicas   uint64
			containers uint64
			scale      int
			reason     string
		}{
			{name: "stopped", present: true, containers: 1, scale: 1},
			{name: "all scaled replicas stopped", present: true, containers: 2, scale: 2},
			{name: "stopped replica missing", present: true, containers: 1, scale: 2, reason: ServiceMismatchReasonContainers},
			{name: "stopped extra container", present: true, containers: 2, scale: 1},
			{name: "running", present: true, replicas: 1, containers: 1, scale: 1},
			{name: "partially running", present: true, replicas: 1, containers: 1, scale: 2, reason: ServiceMismatchReasonReplicas},
			{name: "too many running", present: true, replicas: 2, containers: 2, scale: 1, reason: ServiceMismatchReasonReplicas},
			{name: "missing", scale: 1, reason: ServiceMismatchReasonNotDeployed},
			{name: "scaled to zero and absent"},
			{name: "scaled to zero and stopped", present: true, containers: 1},
			{name: "scaled to zero but running", present: true, replicas: 1, containers: 1, reason: ServiceMismatchReasonReplicas},
		} {
			t.Run(policy.restart+"/"+tt.name, func(t *testing.T) {
				t.Parallel()

				services := types.Services{"web": {
					Name: "web", Restart: policy.restart, Scale: new(tt.scale),
					Labels: types.Labels{DocoCDLabels.Deployment.Autostart: "false"},
				}}

				deployed := map[Service]ServiceStatus{}
				if tt.present {
					deployed["web"] = ServiceStatus{Replicas: tt.replicas, Containers: tt.containers}
				}

				got, err := CheckServiceMismatch(false, deployed, services)
				if err != nil {
					t.Fatal(err)
				}

				var want []ServiceMismatch

				if tt.reason != "" && (tt.reason != ServiceMismatchReasonReplicas || policy.enforceReplicas) {
					reason := ServiceMismatchReason{Reason: tt.reason}

					switch tt.reason {
					case ServiceMismatchReasonReplicas:
						reason.Want, reason.Got = tt.scale, tt.replicas
					case ServiceMismatchReasonContainers:
						reason.Want, reason.Got = tt.scale, tt.containers
					}

					want = []ServiceMismatch{{ServiceName: "web", Reasons: []ServiceMismatchReason{reason}}}
				}

				if !reflect.DeepEqual(got, want) {
					t.Fatalf("CheckServiceMismatch() = %#v, want %#v", got, want)
				}
			})
		}
	}
}

func TestCheckServiceMismatch_ContainerCountNeedsAutostartDisabled(t *testing.T) {
	t.Parallel()

	for _, restart := range []string{"", "no", "on-failure"} {
		t.Run(restart, func(t *testing.T) {
			t.Parallel()

			got, err := CheckServiceMismatch(false,
				map[Service]ServiceStatus{"web": {Containers: 1}},
				types.Services{"web": {Name: "web", Restart: restart, Scale: new(2)}})
			if err != nil {
				t.Fatal(err)
			}

			if len(got) != 0 {
				t.Fatalf("autostart-enabled service reported container drift: %v", got)
			}
		})
	}
}

func TestCheckServiceMismatch_AutostartContainerInventory(t *testing.T) {
	t.Parallel()

	type inventoryContainer struct {
		state container.ContainerState
		hook  string
	}

	for _, tt := range []struct {
		name       string
		containers []inventoryContainer
		scale      int
		want       []ServiceMismatchReason
	}{
		{name: "created", containers: []inventoryContainer{{state: container.StateCreated}}, scale: 1},
		{name: "exited", containers: []inventoryContainer{{state: container.StateExited}}, scale: 1},
		{name: "running", containers: []inventoryContainer{{state: container.StateRunning}}, scale: 1},
		{
			name:       "hook only",
			containers: []inventoryContainer{{state: container.StateCreated, hook: "pre_start"}},
			scale:      1,
			want:       []ServiceMismatchReason{{Reason: ServiceMismatchReasonNotDeployed}},
		},
		{
			name:       "all scaled replicas stopped",
			containers: []inventoryContainer{{state: container.StateCreated}, {state: container.StateExited}},
			scale:      2,
		},
		{
			name: "stopped replica missing",
			containers: []inventoryContainer{
				{state: container.StateExited},
				{state: container.StateCreated, hook: "pre_start"},
			},
			scale: 2,
			want:  []ServiceMismatchReason{{Reason: ServiceMismatchReasonContainers, Want: 2, Got: uint64(1)}},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			summaries := make([]container.Summary, 0, len(tt.containers))
			for _, c := range tt.containers {
				summaries = append(summaries, container.Summary{
					State: c.state,
					Labels: Labels{
						api.ProjectLabel:                  "stack",
						api.ServiceLabel:                  "web",
						api.HookLabel:                     c.hook,
						DocoCDLabels.Deployment.Autostart: "true",
					},
				})
			}

			deployed := getServiceStatusFromContainerStatus("stack", summaries)
			services := types.Services{"web": {
				Name: "web", Restart: "unless-stopped", Scale: new(tt.scale),
				Labels: types.Labels{DocoCDLabels.Deployment.Autostart: "false"},
			}}

			got, err := CheckServiceMismatch(false, deployed, services)
			if err != nil {
				t.Fatal(err)
			}

			var want []ServiceMismatch
			if tt.want != nil {
				want = []ServiceMismatch{{ServiceName: "web", Reasons: tt.want}}
			}

			if !reflect.DeepEqual(got, want) {
				t.Fatalf("inventory mismatch = %#v, want %#v", got, want)
			}
		})
	}
}

func TestCheckServiceMismatch_AutostartLabels(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name         string
		labels       types.Labels
		customLabels types.Labels
		wantMismatch bool
		wantError    bool
	}{
		{name: "default enabled", wantMismatch: true},
		{name: "enabled", labels: types.Labels{DocoCDLabels.Deployment.Autostart: "true"}, wantMismatch: true},
		{name: "disabled", labels: types.Labels{DocoCDLabels.Deployment.Autostart: "false"}},
		{name: "custom disabled", customLabels: types.Labels{DocoCDLabels.Deployment.Autostart: "false"}},
		{
			name: "custom overrides enabled", labels: types.Labels{DocoCDLabels.Deployment.Autostart: "true"},
			customLabels: types.Labels{DocoCDLabels.Deployment.Autostart: " FALSE "},
		},
		{
			name: "custom overrides disabled", labels: types.Labels{DocoCDLabels.Deployment.Autostart: "false"},
			customLabels: types.Labels{DocoCDLabels.Deployment.Autostart: "true"}, wantMismatch: true,
		},
		{
			name: "custom overrides invalid", labels: types.Labels{DocoCDLabels.Deployment.Autostart: "invalid"},
			customLabels: types.Labels{DocoCDLabels.Deployment.Autostart: "false"},
		},
		{name: "invalid", labels: types.Labels{DocoCDLabels.Deployment.Autostart: "invalid"}, wantError: true},
		{
			name: "invalid custom", labels: types.Labels{DocoCDLabels.Deployment.Autostart: "false"},
			customLabels: types.Labels{DocoCDLabels.Deployment.Autostart: "invalid"}, wantError: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			deployed := map[Service]ServiceStatus{"web": {
				Labels:     Labels{DocoCDLabels.Deployment.Autostart: "false"},
				Containers: 1,
			}}
			services := types.Services{"web": {
				Name: "web", Restart: "unless-stopped", Labels: tt.labels, CustomLabels: tt.customLabels,
			}}

			got, err := CheckServiceMismatch(false, deployed, services)
			if tt.wantError {
				if err == nil || !strings.Contains(err.Error(), "service web: invalid "+DocoCDLabels.Deployment.Autostart) {
					t.Fatalf("expected a service-specific autostart error, got %v", err)
				}

				if got != nil {
					t.Fatalf("invalid labels returned mismatches: %v", got)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			var want []ServiceMismatch
			if tt.wantMismatch {
				want = []ServiceMismatch{{ServiceName: "web", Reasons: []ServiceMismatchReason{{
					Reason: ServiceMismatchReasonReplicas, Want: 1, Got: uint64(0),
				}}}}
			}

			if !reflect.DeepEqual(got, want) {
				t.Fatalf("CheckServiceMismatch() = %#v, want %#v", got, want)
			}
		})
	}
}

func TestCheckServiceMismatch_AutostartPreservesSwarm(t *testing.T) {
	t.Parallel()

	for _, label := range []string{"false", "invalid"} {
		for _, tt := range []struct {
			name     string
			mode     swarm.DeployMode
			deployed map[Service]ServiceStatus
			reason   string
		}{
			{name: "missing", mode: swarm.DeployModeReplicated, reason: ServiceMismatchReasonNotDeployed},
			{
				name: "stopped", mode: swarm.DeployModeReplicated,
				deployed: map[Service]ServiceStatus{"web": {SwarmMode: swarm.DeployModeReplicated}}, reason: ServiceMismatchReasonReplicas,
			},
			{
				name: "job", mode: swarm.DeployModeReplicatedJob,
				deployed: map[Service]ServiceStatus{"web": {SwarmMode: swarm.DeployModeReplicatedJob}}, reason: ServiceMismatchReasonReplicas,
			},
			{
				name: "mode drift", mode: swarm.DeployModeReplicated,
				deployed: map[Service]ServiceStatus{"web": {SwarmMode: swarm.DeployModeGlobal}}, reason: ServiceMismatchReasonSwarmMode,
			},
			{
				name: "global", mode: swarm.DeployModeGlobal,
				deployed: map[Service]ServiceStatus{"web": {SwarmMode: swarm.DeployModeGlobal}},
			},
			{
				name: "global job", mode: swarm.DeployModeGlobalJob,
				deployed: map[Service]ServiceStatus{"web": {SwarmMode: swarm.DeployModeGlobalJob}},
			},
		} {
			t.Run(label+"/"+tt.name, func(t *testing.T) {
				t.Parallel()

				services := types.Services{"web": {
					Name: "web", Deploy: &types.DeployConfig{Mode: string(tt.mode)},
					Labels: types.Labels{DocoCDLabels.Deployment.Autostart: label},
				}}

				got, err := CheckServiceMismatch(true, tt.deployed, services)
				if err != nil {
					t.Fatal(err)
				}

				if tt.reason == "" {
					if len(got) != 0 {
						t.Fatalf("unexpected Swarm mismatch: %v", got)
					}
				} else if len(got) != 1 || len(got[0].Reasons) != 1 || got[0].Reasons[0].Reason != tt.reason {
					t.Fatalf("Swarm mismatches = %v, want %q", got, tt.reason)
				}
			})
		}
	}
}

func TestRepositoryLabelMatches(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		label        string
		repositories []string
		want         bool
	}{
		{name: "empty label", label: " ", repositories: []string{"github.com/owner/repo"}, want: false},
		{name: "no repositories", label: "owner/repo", want: false},
		{name: "only empty repositories", label: "owner/repo", repositories: []string{"", " "}, want: false},
		{name: "repository name", label: "github.com/owner/repo", repositories: []string{"github.com/owner/repo"}, want: true},
		{name: "full name label", label: "owner/repo", repositories: []string{"https://github.com/owner/repo.git"}, want: true},
		{name: "scp-like git url", label: "owner/repo", repositories: []string{"git@github.com:owner/repo.git"}, want: true},
		{name: "label with whitespace", label: " owner/repo ", repositories: []string{"https://github.com/owner/repo.git"}, want: true},
		{name: "second repository", label: "owner/app", repositories: []string{"github.com/owner/config", "https://github.com/owner/app.git"}, want: true},
		{name: "other repository", label: "owner/other", repositories: []string{"github.com/owner/repo"}, want: false},
		{name: "oci tag ignored", label: "ghcr.io/org/app", repositories: []string{"ghcr.io/org/app:v2"}, want: true},
		{name: "oci digest ignored", label: "org/app", repositories: []string{"ghcr.io/org/app@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}, want: true},
		{name: "other oci repository", label: "ghcr.io/org/other", repositories: []string{"ghcr.io/org/app:v2"}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := RepositoryLabelMatches(tt.label, tt.repositories...); got != tt.want {
				t.Fatalf("RepositoryLabelMatches(%q, %q) = %v, want %v", tt.label, tt.repositories, got, tt.want)
			}
		})
	}
}
