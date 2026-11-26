package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
	metricdto "github.com/prometheus/client_model/go"

	"github.com/kimdre/doco-cd/internal/common/types/clone"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/prometheus"
	"github.com/kimdre/doco-cd/internal/selfupdate"
)

type swarmSelfUpdateClient struct {
	client.APIClient
	config     *swarm.Config
	removeErr  error
	service    swarm.Service
	serviceErr error
	claims     int
}

func (c *swarmSelfUpdateClient) ConfigCreate(_ context.Context, opts client.ConfigCreateOptions) (client.ConfigCreateResult, error) {
	c.config = &swarm.Config{ID: "config-id", Spec: *clone.New(&opts.Spec)}
	c.config.Version.Index = 10

	return client.ConfigCreateResult{ID: c.config.ID}, nil
}

func (c *swarmSelfUpdateClient) ConfigInspect(context.Context, string, client.ConfigInspectOptions) (client.ConfigInspectResult, error) {
	if c.config == nil {
		return client.ConfigInspectResult{}, errdefs.ErrNotFound
	}

	return client.ConfigInspectResult{Config: *clone.New(c.config)}, nil
}

func (c *swarmSelfUpdateClient) ConfigRemove(context.Context, string, client.ConfigRemoveOptions) (client.ConfigRemoveResult, error) {
	if c.removeErr != nil {
		return client.ConfigRemoveResult{}, c.removeErr
	}

	c.config = nil

	return client.ConfigRemoveResult{}, nil
}

func (c *swarmSelfUpdateClient) ConfigUpdate(_ context.Context, _ string, opts client.ConfigUpdateOptions) (client.ConfigUpdateResult, error) {
	c.config.Spec = *clone.New(&opts.Spec)
	c.claims++

	return client.ConfigUpdateResult{}, nil
}

func (c *swarmSelfUpdateClient) ServiceInspect(context.Context, string, client.ServiceInspectOptions) (client.ServiceInspectResult, error) {
	return client.ServiceInspectResult{Service: c.service}, c.serviceErr
}

type swarmSelfUpdateCLI struct {
	command.Cli
	apiClient client.APIClient
}

func (c swarmSelfUpdateCLI) Client() client.APIClient { return c.apiClient }

func prepareTestSwarmSelfUpdate(t *testing.T) (*swarmSelfUpdateClient, *selfupdate.SwarmHandover) {
	t.Helper()
	withSwarmSelfIdentity(t, "self", "app")

	options := SelfUpdateConfig()
	options.Identity.SwarmServiceID = "service-id"
	options.Identity.SwarmTaskID = "old-task"
	ConfigureSelfUpdate(options)

	apiClient := &swarmSelfUpdateClient{}
	desired := swarm.ServiceSpec{TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: "busybox:latest"}}}
	req := runtimeDeployRequest{request: DeployRequest{
		DockerCLI: swarmSelfUpdateCLI{apiClient: apiClient}, DeployConfig: &deploy.Config{Name: "self"},
		LatestCommit: "original-commit", RepositoryName: "example.com/owner/repo",
		CommitStatus: &selfupdate.CommitStatusInfo{CommitSHA: "original-commit", Context: "original-context"},
	}}

	handover, err := prepareSwarmSelfUpdate(t.Context(), req, swarm.Service{ID: "service-id"}, &desired, []string{"worker-id", "service-id"})
	if err != nil {
		t.Fatal(err)
	}

	if desired.Labels[selfupdate.SwarmHandoverLabel] != handover.Record.ID ||
		desired.TaskTemplate.ContainerSpec.Labels[selfupdate.SwarmHandoverLabel] != "" {
		t.Fatal("handover marker was not confined to service metadata")
	}

	return apiClient, handover
}

func TestSwarmSelfUpdateCleanup(t *testing.T) {
	for _, name := range []string{
		"success", "failure", "handover", "cleanup failure", "uncertain shutdown",
		"unaccepted version", "later update",
		"accepted shutdown", "accepted pruning shutdown", "success during shutdown",
		"deadline during shutdown", "real failure during shutdown",
	} {
		t.Run(name, func(t *testing.T) {
			apiClient, handover := prepareTestSwarmSelfUpdate(t)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			var resultErr error

			keep := false

			switch name {
			case "failure":
				resultErr = errors.New("service failed")
			case "handover":
				resultErr = selfupdate.ErrHandover
				keep = true
			case "cleanup failure":
				apiClient.removeErr = errors.New("config removal failed")
				keep = true
			case "uncertain shutdown":
				resultErr = context.Canceled
				keep = true

				cancel()
			case "unaccepted version", "later update":
				resultErr = context.Canceled
				apiClient.service.Version.Index = handover.ServiceVersion

				apiClient.service.Spec.Labels = map[string]string{selfupdate.SwarmHandoverLabel: handover.Record.ID}
				if name == "later update" {
					apiClient.service.Version.Index++
					apiClient.service.Spec.Labels[selfupdate.SwarmHandoverLabel] = "later-handover"
				}

				keep = true

				cancel()
			case "accepted shutdown", "accepted pruning shutdown":
				resultErr = context.Canceled
				if name == "accepted pruning shutdown" {
					resultErr = fmt.Errorf("failed to prune stack configs: %w", resultErr)
				}

				apiClient.service.Version.Index = handover.ServiceVersion + 1
				apiClient.service.Spec.Labels = map[string]string{selfupdate.SwarmHandoverLabel: handover.Record.ID}
				keep = true

				cancel()
			case "success during shutdown":
				apiClient.service.Version.Index = handover.ServiceVersion + 1
				apiClient.service.Spec.Labels = map[string]string{selfupdate.SwarmHandoverLabel: handover.Record.ID}
				keep = true

				cancel()
			case "deadline during shutdown":
				resultErr = context.DeadlineExceeded
				keep = true

				cancel()
			case "real failure during shutdown":
				resultErr = errors.Join(context.Canceled, errors.New("worker failed"))
				keep = true

				cancel()
			}

			expected := resultErr
			if name == "success during shutdown" || name == "cleanup failure" ||
				name == "accepted shutdown" || name == "accepted pruning shutdown" {
				expected = selfupdate.ErrHandover
			}

			err := finishSwarmSelfUpdate(ctx, apiClient, handover, resultErr)
			if !errors.Is(err, expected) || errors.Is(err, selfupdate.ErrHandover) != errors.Is(expected, selfupdate.ErrHandover) {
				t.Fatalf("cleanup changed the outcome: %v", err)
			}

			pending, err := selfupdate.LoadSwarmHandover(t.Context(), apiClient, handover.ServiceID)
			if err != nil || (pending != nil) != keep {
				t.Fatalf("pending record = %+v, %v; want retained %v", pending, err, keep)
			}

			if name == "real failure during shutdown" && (pending.Record.State != selfupdate.StateFinalising ||
				!strings.Contains(pending.Record.Error, "worker failed")) {
				t.Fatal("shutdown erased the genuine service failure")
			}

			if name == "cleanup failure" && (pending.Record.State != selfupdate.StateFinalising || pending.OwnerTaskID != "old-task") {
				t.Fatal("cleanup failure did not preserve the result for this task")
			}

			if name == "deadline during shutdown" && (pending.Record.State != selfupdate.StateFinalising || !pending.Record.TimedOut) {
				t.Fatal("shutdown hid a deployment deadline")
			}
		})
	}
}

func TestSwarmSelfUpdateCleanupLeavesLaterHandover(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprintf("shutdown=%v", shutdown), func(t *testing.T) {
			apiClient, handover := prepareTestSwarmSelfUpdate(t)
			later := clone.New(apiClient.config)
			later.ID = "later-config-id"
			apiClient.config = later

			apiClient.removeErr = errors.New("remove response lost")
			resultErr := errors.New("original deployment failed")

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			if shutdown {
				cancel()
			}

			if err := finishSwarmSelfUpdate(ctx, apiClient, handover, resultErr); !errors.Is(err, resultErr) {
				t.Fatalf("cleanup lost the original failure: %v", err)
			}

			if apiClient.claims != 0 || apiClient.config.Spec.Labels["cd.doco.self.swarm.state"] != string(selfupdate.StateHandover) {
				t.Fatal("old deployment cleanup changed a later handover")
			}
		})
	}
}

func TestSwarmSelfUpdateCleanupCannotConfirmAcceptance(t *testing.T) {
	apiClient, handover := prepareTestSwarmSelfUpdate(t)
	apiClient.serviceErr = errors.New("manager unavailable")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := finishSwarmSelfUpdate(ctx, apiClient, handover, context.Canceled)
	if !errors.Is(err, apiClient.serviceErr) || !errors.Is(err, context.Canceled) || errors.Is(err, selfupdate.ErrHandover) {
		t.Fatalf("unknown acceptance was classified as handover: %v", err)
	}

	if apiClient.config == nil || apiClient.claims != 0 {
		t.Fatal("unknown acceptance discarded or resolved the pending record")
	}
}

func TestSwarmSelfUpdateWaitsBeforeStartingAnotherStatus(t *testing.T) {
	apiClient, handover := prepareTestSwarmSelfUpdate(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	for _, name := range []string{"self", "another-stack", "remote-context", "resolved"} {
		t.Run(name, func(t *testing.T) {
			stack, contextName := "self", ""

			switch name {
			case "another-stack":
				stack = name
			case "remote-context":
				contextName = "remote"
			case "resolved":
				if err := selfupdate.RemoveSwarmHandover(t.Context(), apiClient, *handover); err != nil {
					t.Fatal(err)
				}
			}

			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
			defer cancel()

			err := WaitForSwarmSelfUpdate(ctx, apiClient, stack, contextName, log)
			if name == "self" {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("pending self update was not awaited: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSwarmHandoverMetricsPreserveDeploymentStart(t *testing.T) {
	t.Parallel()

	labels := [3]string{t.Name(), "self", "default"}

	recovery, err := json.Marshal(SwarmRecoveryInfo{MetricsLabels: labels, MetricsStartedAt: time.Now().Add(-time.Minute)})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		prometheus.DeploymentsTotal.DeleteLabelValues(labels[:]...)
		prometheus.DeploymentDuration.DeleteLabelValues(labels[:]...)
	})

	if err := RecordSwarmHandoverMetrics(selfupdate.SwarmHandover{
		Recovery: recovery, Record: selfupdate.Record{CreatedAt: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}

	var counter, histogram metricdto.Metric
	if err := prometheus.DeploymentsTotal.WithLabelValues(labels[:]...).Write(&counter); err != nil {
		t.Fatal(err)
	}

	histogramMetric, ok := prometheus.DeploymentDuration.WithLabelValues(labels[:]...).(interface{ Write(*metricdto.Metric) error })
	if !ok {
		t.Fatal("deployment duration does not expose its metric")
	}

	if err := histogramMetric.Write(&histogram); err != nil {
		t.Fatal(err)
	}

	if counter.GetCounter().GetValue() != 1 || histogram.GetHistogram().GetSampleCount() != 1 ||
		histogram.GetHistogram().GetSampleSum() < time.Minute.Seconds() {
		t.Fatalf("handover metrics lost the deployment start: counter=%v, histogram=%v", &counter, &histogram)
	}
}

func TestSwarmHandoverRestoresDeploymentOutcome(t *testing.T) {
	t.Parallel()

	target := DeploymentTarget{Context: "remote", Repository: "example.com/owner/repo", Stack: t.Name(), ConfigSource: "config-source", ConfigTarget: "original"}
	desired := DeploymentDesiredState{Revision: "original-commit", ConfigHash: "config-hash", ProjectHash: "project-hash"}

	recovery, err := json.Marshal(SwarmRecoveryInfo{Target: target, Desired: &desired})
	if err != nil {
		t.Fatal(err)
	}

	handover := selfupdate.SwarmHandover{
		Recovery: recovery,
		Record: selfupdate.Record{Stack: target.Stack, Source: selfupdate.SourceInfo{
			RepoName: target.Repository, CommitSHA: desired.Revision, ProjectHash: desired.ProjectHash,
		}, Error: "convergence failed"},
	}

	t.Cleanup(func() {
		DeploymentFailures.Clear(target)
		deployStatusCache.Delete(getDeployStatusCacheKey(target.Repository, target.Stack))
	})

	if err := RecordSwarmHandoverOutcome(handover); err != nil {
		t.Fatal(err)
	}

	if failure, ok := DeploymentFailures.Get(target); !ok || failure.Error != handover.Record.Error || failure.CommitSHA != desired.Revision {
		t.Fatalf("genuine handover failure was not restored: %+v, %v", failure, ok)
	}

	handover.Record.Error = ""
	if err := RecordSwarmHandoverOutcome(handover); err != nil {
		t.Fatal(err)
	}

	if _, ok := DeploymentFailures.Get(target); ok {
		t.Fatal("confirmed success retained the failure marker")
	}

	if status, ok := getDeployStatusFromCache(&deployStatusCache, target.Repository, target.Stack); !ok ||
		status.CommitSHA != desired.Revision || status.ComposeHash != desired.ProjectHash {
		t.Fatalf("confirmed deployment was not cached: %+v, %v", status, ok)
	}
}
