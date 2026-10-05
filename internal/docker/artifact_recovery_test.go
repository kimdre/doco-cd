package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/source/store"
)

type artifactRecoveryListClient struct {
	swarmPinningTestClient
	err error
}

func (c *artifactRecoveryListClient) ServiceList(ctx context.Context, opts client.ServiceListOptions) (client.ServiceListResult, error) {
	if c.err != nil {
		return client.ServiceListResult{}, c.err
	}

	return c.swarmPinningTestClient.ServiceList(ctx, opts)
}

func TestStaleSwarmArtifactServicesRotation(t *testing.T) {
	t.Parallel()

	dest := t.TempDir()
	base := filepath.Join(dest, "repo")
	dir := filepath.Join(base, store.ArtifactsSubdir, "rev")
	if err := os.MkdirAll(dir, filesystem.PermDir); err != nil {
		t.Fatal(err)
	}

	if _, ok, err := filesystem.BirthTime(dir); err != nil || !ok {
		t.Skipf("file system does not record creation times: %v", err)
	}

	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	service := func(name, timestamp, pinned string) swarm.Service {
		return swarm.Service{Spec: swarm.ServiceSpec{
			Annotations: swarm.Annotations{Name: "stack_" + name, Labels: map[string]string{
				DocoCDLabels.Deployment.CommitSHA:       "rev",
				DocoCDLabels.Deployment.WorkingDir:      "/srv/data/repo/artifacts/rev/app",
				DocoCDLabels.Deployment.Timestamp:       timestamp,
				DocoCDLabels.Deployment.PinnedRevisions: pinned,
			}},
		}}
	}

	job := service("completed-job", past, "")
	job.Spec.Mode.ReplicatedJob = &swarm.ReplicatedJob{}
	globalJob := service("global-job", past, "")
	globalJob.Spec.Mode.GlobalJob = &swarm.GlobalJob{}
	fake := &artifactRecoveryListClient{swarmPinningTestClient: swarmPinningTestClient{services: []swarm.Service{
		service("unchanged-sibling", past, ""),
		service("healthy-sibling", future, ""),
		service("missing-pinned", future, "old"),
		job, globalJob,
	}}}
	load := ComposeLoadOptions{DataHostPath: "/srv/data", DataMountPath: dest}

	for range 2 { // A failed update or process restart must leave recovery selectable.
		got, err := staleSwarmArtifactServices(t.Context(), fake, "stack", load)
		if err != nil || !slices.Equal(got, []string{"missing-pinned", "unchanged-sibling"}) {
			t.Fatalf("staleSwarmArtifactServices() = %v, %v", got, err)
		}
	}

	fake.services[0].Spec.Labels[DocoCDLabels.Deployment.Timestamp] = future
	fake.services[2].Spec.Labels[DocoCDLabels.Deployment.PinnedRevisions] = ""
	got, err := staleSwarmArtifactServices(t.Context(), fake, "stack", load)
	if err != nil || len(got) != 0 {
		t.Fatalf("after successful recovery = %v, %v", got, err)
	}

	fake.err = errors.New("list failed")
	if _, err := staleSwarmArtifactServices(t.Context(), fake, "stack", load); !errors.Is(err, fake.err) {
		t.Fatalf("list failure = %v, want %v", err, fake.err)
	}
}
