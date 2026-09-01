package controlplane_test

import (
	"context"
	"errors"
	"testing"

	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/commitstatus"
	"github.com/kimdre/doco-cd/internal/common/validation"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/reconciliation"
	"github.com/kimdre/doco-cd/internal/source"
	"github.com/kimdre/doco-cd/internal/stages"
)

// fakeDockerCli satisfies command.Cli for tests, returning a fakeAPIClient
// whose Info call reports a non-swarm-manager daemon so
// Deployment.Deploy's swarm refresh step succeeds without a real Docker
// daemon and without touching global swarm feature-detection state (which
// would be unsafe to mutate from parallel tests).
type fakeDockerCli struct {
	command.Cli
}

func (fakeDockerCli) Client() client.APIClient {
	return fakeAPIClient{}
}

type fakeAPIClient struct {
	client.APIClient
}

func (fakeAPIClient) Info(context.Context, client.InfoOptions) (client.SystemInfoResult, error) {
	return client.SystemInfoResult{}, nil
}

type fakeSourcePreparer struct {
	result source.Result
	err    error

	calls int
}

func (f *fakeSourcePreparer) Prepare(_ context.Context, _ source.Request) (source.Result, error) {
	f.calls++

	return f.result, f.err
}

type fakeReconciler struct {
	err error

	calls   int
	lastReq reconciliation.DeployRequest
}

func (f *fakeReconciler) Deploy(_ context.Context, req reconciliation.DeployRequest) error {
	f.calls++
	f.lastReq = req

	return f.err
}

func newTestDeployment(t *testing.T, preparer controlplane.SourcePreparer, reconciler controlplane.Reconciler) *controlplane.Deployment {
	t.Helper()

	d, err := controlplane.NewDeployment(controlplane.DeploymentDependencies{
		SourcePreparer: preparer,
		Reconciler:     reconciler,
		DockerCLI:      fakeDockerCli{},
		DataMountPoint: container.MountPoint{Type: "bind", Source: "/src", Destination: "/dst"},
	})
	if err != nil {
		t.Fatalf("NewDeployment() error = %v", err)
	}

	return d
}

func validDeploymentRequest() controlplane.DeploymentRequest {
	return controlplane.DeploymentRequest{
		Logger:     logger.New(logger.LevelCritical).Logger,
		JobTrigger: stages.JobTriggerWebhook,
		SourceType: config.SourceTypeGit,
		SourceRef:  "https://example.com/owner/repo.git",
	}
}

func TestDeploymentRequest_AllowsDefaultSourceType(t *testing.T) {
	t.Parallel()

	err := validation.Validate(controlplane.DeploymentRequest{
		Logger:     logger.New(logger.LevelCritical).Logger,
		JobTrigger: stages.JobTriggerWebhook,
		SourceRef:  "https://github.com/owner/repo.git",
	})
	if err != nil {
		t.Fatalf("DeploymentRequest with default source type must validate: %v", err)
	}
}

func TestDeploy_InvalidRequest(t *testing.T) {
	t.Parallel()

	d := newTestDeployment(t, &fakeSourcePreparer{}, &fakeReconciler{})

	err := d.Deploy(t.Context(), controlplane.DeploymentRequest{})

	var de controlplane.DeploymentError

	if !errors.As(err, &de) {
		t.Fatalf("expected DeploymentError, got %v (%T)", err, err)
	}

	if de.HTTPStatusCode != 500 {
		t.Fatalf("expected 500, got %d", de.HTTPStatusCode)
	}

	if de.Msg != "invalid deployment request" {
		t.Fatalf("unexpected message: %q", de.Msg)
	}
}

func TestDeploy_SourcePreparerError_MapsToBadRequest(t *testing.T) {
	t.Parallel()

	preparer := &fakeSourcePreparer{err: fmt400Err()}
	reconciler := &fakeReconciler{}
	d := newTestDeployment(t, preparer, reconciler)

	err := d.Deploy(t.Context(), validDeploymentRequest())

	var de controlplane.DeploymentError

	if !errors.As(err, &de) {
		t.Fatalf("expected DeploymentError, got %v (%T)", err, err)
	}

	if de.HTTPStatusCode != 400 {
		t.Fatalf("expected 400, got %d", de.HTTPStatusCode)
	}

	if de.Msg != "invalid source type" {
		t.Fatalf("unexpected message: %q", de.Msg)
	}

	// The early source-preparation failure must short-circuit the pipeline:
	// reconciliation (and any later commit-status reporting it would trigger)
	// must never run.
	if reconciler.calls != 0 {
		t.Fatalf("expected reconciler not to be called after a source preparation failure, got %d calls", reconciler.calls)
	}
}

func TestDeploy_SourcePreparerError_Mapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantMsg    string
	}{
		{
			name:       "invalid request",
			err:        errWrap(source.ErrInvalidRequest, "invalid"),
			wantStatus: 500,
			wantMsg:    "invalid deployment request",
		},
		{
			name:       "unsupported trigger",
			err:        errWrap(source.ErrUnsupportedJobTrigger, "invalid"),
			wantStatus: 400,
			wantMsg:    "unsupported job trigger",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newTestDeployment(t, &fakeSourcePreparer{err: tt.err}, &fakeReconciler{})

			err := d.Deploy(t.Context(), validDeploymentRequest())

			var de controlplane.DeploymentError
			if !errors.As(err, &de) {
				t.Fatalf("expected DeploymentError, got %v (%T)", err, err)
			}

			if de.HTTPStatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", de.HTTPStatusCode, tt.wantStatus)
			}

			if de.Msg != tt.wantMsg {
				t.Fatalf("message = %q, want %q", de.Msg, tt.wantMsg)
			}
		})
	}
}

func fmt400Err() error {
	return errWrap(source.ErrInvalidSourceType, "invalid")
}

func errWrap(sentinel error, detail string) error {
	return &wrappedTestError{sentinel: sentinel, detail: detail}
}

type wrappedTestError struct {
	sentinel error
	detail   string
}

func (e *wrappedTestError) Error() string { return e.sentinel.Error() + ": " + e.detail }
func (e *wrappedTestError) Unwrap() error { return e.sentinel }

func TestDeploy_SourcePreparerError_MapsToInternalServerError(t *testing.T) {
	t.Parallel()

	preparer := &fakeSourcePreparer{err: errWrap(source.ErrGitClone, "network unreachable")}
	reconciler := &fakeReconciler{}
	d := newTestDeployment(t, preparer, reconciler)

	err := d.Deploy(t.Context(), validDeploymentRequest())

	var de controlplane.DeploymentError

	if !errors.As(err, &de) {
		t.Fatalf("expected DeploymentError, got %v (%T)", err, err)
	}

	if de.HTTPStatusCode != 500 {
		t.Fatalf("expected 500, got %d", de.HTTPStatusCode)
	}

	if de.Msg != "failed to clone repository" {
		t.Fatalf("unexpected message: %q", de.Msg)
	}

	if reconciler.calls != 0 {
		t.Fatalf("expected reconciler not to be called after a source preparation failure, got %d calls", reconciler.calls)
	}
}

func TestDeploy_Success_AdaptsResultAndInvokesObserver(t *testing.T) {
	t.Parallel()

	deployConfigs := []*deploy.Config{
		{Name: "stack-a", Context: "ctx-a"},
		{Name: "stack-b", Context: "ctx-b"},
	}

	preparer := &fakeSourcePreparer{result: source.Result{
		SourceType:    config.SourceTypeGit,
		RepoName:      "owner/repo",
		PathInternal:  "/dst/owner/repo",
		PathExternal:  "/src/owner/repo",
		Revision:      "deadbeef",
		OCITrusted:    true,
		DeployConfigs: deployConfigs,
	}}
	reconciler := &fakeReconciler{}
	d := newTestDeployment(t, preparer, reconciler)

	var observed [][2]string

	req := validDeploymentRequest()
	req.Metadata = notification.Metadata{
		DeploymentTargetObserver: func(stack, context string) {
			observed = append(observed, [2]string{stack, context})
		},
	}

	if err := d.Deploy(t.Context(), req); err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}

	if preparer.calls != 1 {
		t.Fatalf("expected source preparer to be called once, got %d", preparer.calls)
	}

	if reconciler.calls != 1 {
		t.Fatalf("expected reconciler to be called once, got %d", reconciler.calls)
	}

	got := reconciler.lastReq.Repository

	want := stages.RepositoryData{
		Source:       config.SourceTypeGit,
		SourceUrl:    req.SourceRef,
		Name:         "owner/repo",
		PathInternal: "/dst/owner/repo",
		PathExternal: "/src/owner/repo",
		Revision:     "deadbeef",
		OCITrusted:   true,
	}
	if got != want {
		t.Fatalf("Repository = %+v, want %+v", got, want)
	}

	if len(reconciler.lastReq.DeployConfigs) != 2 {
		t.Fatalf("expected 2 deploy configs to be passed through, got %d", len(reconciler.lastReq.DeployConfigs))
	}

	wantObserved := [][2]string{{"stack-a", "ctx-a"}, {"stack-b", "ctx-b"}}
	if len(observed) != len(wantObserved) {
		t.Fatalf("observed = %v, want %v", observed, wantObserved)
	}

	for i := range wantObserved {
		if observed[i] != wantObserved[i] {
			t.Fatalf("observed[%d] = %v, want %v", i, observed[i], wantObserved[i])
		}
	}
}

func TestDeploy_ReconciliationFailure_MapsToInternalServerError(t *testing.T) {
	t.Parallel()

	preparer := &fakeSourcePreparer{result: source.Result{DeployConfigs: []*deploy.Config{{Name: "stack"}}}}
	reconciler := &fakeReconciler{err: errors.New("compose up failed")}
	d := newTestDeployment(t, preparer, reconciler)

	err := d.Deploy(t.Context(), validDeploymentRequest())

	var de controlplane.DeploymentError

	if !errors.As(err, &de) {
		t.Fatalf("expected DeploymentError, got %v (%T)", err, err)
	}

	if de.HTTPStatusCode != 500 {
		t.Fatalf("expected 500, got %d", de.HTTPStatusCode)
	}

	if de.Msg != "deployment failed" {
		t.Fatalf("unexpected message: %q", de.Msg)
	}
}

func TestDeploy_PreservesErrWebhookFilterMismatchIdentity(t *testing.T) {
	t.Parallel()

	preparer := &fakeSourcePreparer{result: source.Result{DeployConfigs: []*deploy.Config{{Name: "stack"}}}}
	reconciler := &fakeReconciler{err: stages.ErrWebhookFilterMismatch}
	d := newTestDeployment(t, preparer, reconciler)

	err := d.Deploy(t.Context(), validDeploymentRequest())

	if !errors.Is(err, stages.ErrWebhookFilterMismatch) {
		t.Fatalf("expected ErrWebhookFilterMismatch identity to be preserved, got %v", err)
	}

	if de, ok := errors.AsType[controlplane.DeploymentError](err); ok {
		t.Fatalf("expected ErrWebhookFilterMismatch not to be wrapped in a DeploymentError, got %+v", de)
	}
}

// TestCommitStatusEarlyFailure_SkipsWithoutBlockingDeployment is a sanity check that
// commitstatus.ResolveRequest (used by internal/source to report early Git
// clone/config-resolution failures before controlplane ever runs
// reconciliation) safely no-ops without credentials, so a Deployment.Deploy
// failure path never blocks on commit-status reporting.
func TestCommitStatusEarlyFailure_SkipsWithoutBlockingDeployment(t *testing.T) {
	t.Parallel()

	_, ok := commitstatus.ResolveRequest(logger.New(logger.LevelCritical).Logger, commitstatus.RequestParams{
		Enabled:     true,
		SourceIsGit: true,
		SourceURL:   "https://example.com/owner/repo.git",
		CommitSHA:   "deadbeef",
	})
	if ok {
		t.Fatal("expected ResolveRequest to skip without configured credentials")
	}
}
