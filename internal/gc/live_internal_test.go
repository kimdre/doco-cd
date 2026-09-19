package gc

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/common/types/set"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/source/store"
)

// liveTestCli is a minimal command.Cli fake for tests in this package: only
// Client() is ever called on a docker.ContextClientResult's Cli field by
// this package's code.
type liveTestCli struct {
	command.Cli
	apiClient client.APIClient
}

func (c liveTestCli) Client() client.APIClient { return c.apiClient }

// liveTestClient is a minimal client.APIClient fake returning canned
// containers/services for ContainerList/ServiceList.
type liveTestClient struct {
	client.APIClient
	containers []container.Summary
	services   []swarm.Service
}

func (c *liveTestClient) ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
	return client.ContainerListResult{Items: c.containers}, nil
}

func (c *liveTestClient) ServiceList(context.Context, client.ServiceListOptions) (client.ServiceListResult, error) {
	return client.ServiceListResult{Items: c.services}, nil
}

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func TestAddLiveRevisions_ComposeContainers(t *testing.T) {
	t.Parallel()

	fakeClient := &liveTestClient{
		containers: []container.Summary{
			{
				Names: []string{"/stack-web-1"},
				Labels: map[string]string{
					docker.DocoCDLabels.Source.Name:          "https://git.example.com/user/repo.git",
					docker.DocoCDLabels.Deployment.CommitSHA: "abc123",
				},
			},
			// Missing the commit SHA label - must be ignored, not crash.
			{
				Names: []string{"/stack-incomplete-1"},
				Labels: map[string]string{
					docker.DocoCDLabels.Source.Name: "user/other-repo",
				},
			},
		},
	}

	result := docker.ContextClientResult{
		Name: "", Cli: liveTestCli{apiClient: fakeClient},
	}

	live := make(map[string]set.Set[store.Revision])
	if err := addLiveRevisions(context.Background(), result, false, live, testLogger(), "", ""); err != nil {
		t.Fatalf("addLiveRevisions() error = %v", err)
	}

	repoName := docker.NormalizeRepositoryLabel("https://git.example.com/user/repo.git")

	if _, ok := live[repoName]["abc123"]; !ok {
		t.Fatalf("addLiveRevisions() live = %+v, want %q/%q", live, repoName, "abc123")
	}

	if len(live) != 1 {
		t.Fatalf("addLiveRevisions() live = %+v, want exactly one repository (incomplete label set must be ignored)", live)
	}
}

func TestAddLiveRevisions_SwarmServices(t *testing.T) {
	t.Parallel()

	fakeClient := &liveTestClient{
		services: []swarm.Service{
			{
				Spec: swarm.ServiceSpec{
					Annotations: swarm.Annotations{
						Name: "stack_web",
						Labels: map[string]string{
							docker.DocoCDLabels.Source.Name:          "user/repo",
							docker.DocoCDLabels.Deployment.CommitSHA: "def456",
						},
					},
				},
			},
		},
	}

	result := docker.ContextClientResult{
		Name: "remote", Cli: liveTestCli{apiClient: fakeClient}, SwarmMode: true,
	}

	live := make(map[string]set.Set[store.Revision])
	if err := addLiveRevisions(context.Background(), result, true, live, testLogger(), "", ""); err != nil {
		t.Fatalf("addLiveRevisions() error = %v", err)
	}

	repoName := docker.NormalizeRepositoryLabel("user/repo")
	if _, ok := live[repoName]["def456"]; !ok {
		t.Fatalf("addLiveRevisions() live = %+v, want %q/%q", live, repoName, "def456")
	}
}

func TestLiveRevisions_NilContextsReturnsEmpty(t *testing.T) {
	t.Parallel()

	live, err := LiveRevisions(context.Background(), nil, testLogger(), "", "")
	if err != nil {
		t.Fatalf("LiveRevisions(nil) error = %v", err)
	}

	if len(live) != 0 {
		t.Fatalf("LiveRevisions(nil) = %+v, want empty", live)
	}
}

func TestAddLiveRevisions_UsesArtifactWorkingDirectoryForStoreIdentity(t *testing.T) {
	t.Parallel()

	const (
		revision       = "abc123"
		configRevision = "sha256:config123"
	)

	dataMountSource := "/srv/doco-cd"
	workingDir := filepath.Join(dataMountSource, "git.example.com", "deployment-owner", "deployment-repo",
		store.ArtifactsSubdir, store.ArtifactDirName(revision), "compose")
	configWorkingDir := filepath.Join(dataMountSource, "ghcr.io", "config-owner", "config-repo",
		store.ArtifactsSubdir, store.ArtifactDirName(configRevision))

	fakeClient := &liveTestClient{
		containers: []container.Summary{{
			Names: []string{"/stack-web-1"},
			Labels: map[string]string{
				docker.DocoCDLabels.Source.Name:             "config-owner/config-repo",
				docker.DocoCDLabels.Deployment.CommitSHA:    revision,
				docker.DocoCDLabels.Deployment.WorkingDir:   workingDir,
				docker.DocoCDLabels.Source.ConfigRevision:   configRevision,
				docker.DocoCDLabels.Source.ConfigWorkingDir: configWorkingDir,
			},
		}},
	}

	result := docker.ContextClientResult{Name: "", Cli: liveTestCli{apiClient: fakeClient}}
	live := make(map[string]set.Set[store.Revision])

	if err := addLiveRevisions(context.Background(), result, false, live, testLogger(), dataMountSource, "/data"); err != nil {
		t.Fatalf("addLiveRevisions() error = %v", err)
	}

	key := "git.example.com/deployment-owner/deployment-repo"
	if !live[key].Contains(revision) {
		t.Fatalf("addLiveRevisions() live = %+v, want %q/%q", live, key, revision)
	}

	configKey := "ghcr.io/config-owner/config-repo"
	if !live[configKey].Contains(configRevision) {
		t.Fatalf("addLiveRevisions() live = %+v, want %q/%q", live, configKey, configRevision)
	}
}

func TestAddLiveRevisions_PreservesLegacyMixedConfigStore(t *testing.T) {
	t.Parallel()

	dataMount := "/data"
	deploymentWorkingDir := filepath.Join(dataMount, "github.com", "owner", "app",
		store.ArtifactsSubdir, "abc123")

	fakeClient := &liveTestClient{containers: []container.Summary{{
		Names: []string{"/mixed-source-1"},
		Labels: map[string]string{
			docker.DocoCDLabels.Source.Type:           "oci",
			docker.DocoCDLabels.Source.Name:           "owner/config",
			docker.DocoCDLabels.Source.URL:            "ghcr.io/owner/config:latest",
			docker.DocoCDLabels.Deployment.CommitSHA:  "abc123",
			docker.DocoCDLabels.Deployment.WorkingDir: deploymentWorkingDir,
		},
	}}}

	result := docker.ContextClientResult{Name: "", Cli: liveTestCli{apiClient: fakeClient}}
	live := make(map[string]set.Set[store.Revision])

	if err := addLiveRevisions(t.Context(), result, false, live, testLogger(), dataMount, dataMount); err != nil {
		t.Fatalf("addLiveRevisions() error = %v", err)
	}

	if !live["ghcr.io/owner/config"].Contains(allRevisions) {
		t.Fatalf("legacy mixed config store was not protected: %+v", live)
	}
}
