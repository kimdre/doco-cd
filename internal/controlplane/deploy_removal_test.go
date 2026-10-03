package controlplane_test

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"

	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/reconciliation"
	"github.com/kimdre/doco-cd/internal/source"
)

// destroyingReconciler records removal requests the way the destroy stage does.
type destroyingReconciler struct {
	repoNames []string
	err       error

	gotRemovals bool
}

func (r *destroyingReconciler) Deploy(_ context.Context, req reconciliation.DeployRequest) error {
	r.gotRemovals = req.RepositoryRemovals != nil

	for _, name := range r.repoNames {
		req.RepositoryRemovals.Add(name)
	}

	return r.err
}

type fakeRepositoryRemover struct {
	released *bool

	calls             int
	repoNames         []string
	releasedAtRemoval bool
}

func (f *fakeRepositoryRemover) RemoveUnused(_ context.Context, _ *slog.Logger, repoNames []string) {
	f.calls++
	f.repoNames = repoNames
	f.releasedAtRemoval = *f.released
}

func newRemovalTestDeployment(t *testing.T, reconciler controlplane.Reconciler, remover controlplane.RepositoryRemover, released *bool) *controlplane.Deployment {
	t.Helper()

	result := source.Result{
		RepoName:      "github.com/owner/repo",
		DeployConfigs: []*deploy.Config{{Name: "stack"}},
	}.WithRelease(func() { *released = true })

	d, err := controlplane.NewDeployment(controlplane.DeploymentDependencies{
		SourcePreparer:    &fakeSourcePreparer{result: result},
		Reconciler:        reconciler,
		Contexts:          &fakeDockerContextResolver{},
		DataMountPoint:    container.MountPoint{Type: "bind", Source: "/src", Destination: "/dst"},
		RepositoryRemover: remover,
	})
	if err != nil {
		t.Fatalf("NewDeployment() error = %v", err)
	}

	return d
}

func TestDeploy_RemovesRequestedRepositoriesAfterRelease(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		deployErr error
	}{
		{name: "successful job"},
		{name: "failed job", deployErr: errors.New("another stack failed")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			released := false
			reconciler := &destroyingReconciler{
				repoNames: []string{"github.com/owner/repo", "github.com/owner/other", "github.com/owner/repo"},
				err:       tt.deployErr,
			}
			remover := &fakeRepositoryRemover{released: &released}
			d := newRemovalTestDeployment(t, reconciler, remover, &released)

			err := d.Deploy(t.Context(), validDeploymentRequest())
			if (err != nil) != (tt.deployErr != nil) {
				t.Fatalf("Deploy() error = %v, want error %v", err, tt.deployErr)
			}

			if !reconciler.gotRemovals {
				t.Fatal("reconciler got no RepositoryRemovals")
			}

			if remover.calls != 1 {
				t.Fatalf("RemoveUnused() calls = %d, want 1", remover.calls)
			}

			if !remover.releasedAtRemoval {
				t.Fatal("RemoveUnused() ran before the job released its locks")
			}

			want := []string{"github.com/owner/other", "github.com/owner/repo"}
			if !slices.Equal(remover.repoNames, want) {
				t.Fatalf("RemoveUnused() repositories = %v, want %v", remover.repoNames, want)
			}
		})
	}
}

func TestDeploy_SkipsRemovalWithoutRequests(t *testing.T) {
	t.Parallel()

	released := false
	reconciler := &destroyingReconciler{}
	remover := &fakeRepositoryRemover{released: &released}
	d := newRemovalTestDeployment(t, reconciler, remover, &released)

	if err := d.Deploy(t.Context(), validDeploymentRequest()); err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}

	if remover.calls != 0 {
		t.Fatalf("RemoveUnused() calls = %d, want 0", remover.calls)
	}

	if !released {
		t.Fatal("Deploy() did not release the source result")
	}
}

func TestDeploy_WithoutRepositoryRemoverPassesNoRemovals(t *testing.T) {
	t.Parallel()

	released := false
	reconciler := &destroyingReconciler{}
	d := newRemovalTestDeployment(t, reconciler, nil, &released)

	if err := d.Deploy(t.Context(), validDeploymentRequest()); err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}

	if reconciler.gotRemovals {
		t.Fatal("reconciler got RepositoryRemovals without a RepositoryRemover")
	}
}
