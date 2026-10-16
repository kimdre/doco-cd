package reconciliation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/config"
	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/stages"
)

func TestCleanupRetryHonorsRestoredStackAndDisabledDeletion(t *testing.T) {
	t.Parallel()

	for _, restored := range []bool{false, true} {
		t.Run(fmt.Sprintf("restored=%t", restored), func(t *testing.T) {
			t.Parallel()

			origin := deployConfig.AutoDiscoveryOrigin{
				WorkingDirectory: "services", Revision: cleanupTestRevision,
				Settings: deployConfig.AutoDiscoveryConfig{Enabled: true, Delete: true},
			}
			stack := cleanupTestStack("old", "services/old", map[string]string{
				docker.DocoCDLabels.Deployment.AutoDiscoveryConfig: docker.MarshalAutoDiscoveryConfig(origin.Settings),
			})
			cli := cleanupTestCLI{apiClient: &cleanupTestClient{containers: []container.Summary{stack}}}
			manager := newTestManagerWithDependencies(t, Dependencies{
				DockerCLI: cli,
				Contexts:  docker.NewContextRegistry(cli, docker.ContextRegistryOptions{}),
			})
			req := cleanupTestRequest()
			req.Logger = slog.New(slog.DiscardHandler)
			req.AutoDiscoveryOrigins = []deployConfig.AutoDiscoveryOrigin{origin}
			released := false
			req.RefreshCleanup = func(context.Context) (DeployRequest, func(), error) {
				fresh := req
				if restored {
					fresh.DeployConfigs = []*deployConfig.Config{cleanupTestDiscovered("old", origin)}
				} else {
					origin.Settings.Delete = false
					fresh.AutoDiscoveryOrigins = []deployConfig.AutoDiscoveryOrigin{origin}
				}

				return fresh, func() { released = true }, nil
			}
			req = seedCleanupRetry(t, manager, req)
			manager.runDueCleanupRetry(t.Context(), time.Now(), manager.cleanupObsolete)

			if !released || len(manager.cleanupRetries.pending) != 0 {
				t.Fatal("refreshed configuration did not cancel obsolete-stack removal")
			}
		})
	}
}

func TestDeployReportsCleanupErrorsInsteadOfSkipping(t *testing.T) {
	t.Parallel()

	for _, paused := range []bool{false, true} {
		t.Run(fmt.Sprintf("paused=%t", paused), func(t *testing.T) {
			t.Parallel()

			listErr := errors.New("cleanup inventory unavailable")
			cli := cleanupTestCLI{apiClient: &cleanupTestClient{listErr: map[string]error{
				docker.DocoCDLabels.Deployment.AutoDiscovery + "=true": listErr,
			}}}
			manager := newTestManagerWithDependencies(t, Dependencies{
				DockerCLI: cli,
				Contexts:  docker.NewContextRegistry(cli, docker.ContextRegistryOptions{}),
			})

			req := cleanupTestRequest()
			req.Logger = slog.New(slog.DiscardHandler)

			req.AutoDiscoveryOrigins = []deployConfig.AutoDiscoveryOrigin{{
				WorkingDirectory: ".",
				Revision:         cleanupTestRevision,
				Settings:         deployConfig.AutoDiscoveryConfig{Enabled: true, Delete: true},
			}}
			if paused {
				req.DeployConfigs = []*deployConfig.Config{{Name: "current", Pause: true}}
			}

			_, err := manager.deploy(t.Context(), req, manager.newSyncWindowGate(req, time.Now()))
			if !errors.Is(err, listErr) || errors.Is(err, stages.ErrSkipDeployment) {
				t.Fatalf("deploy error = %v, want the cleanup error without a skip sentinel", err)
			}
		})
	}
}

func TestCleanupIgnoresObservationsFromOtherReferences(t *testing.T) {
	t.Parallel()

	for _, oci := range []bool{false, true} {
		t.Run(fmt.Sprintf("oci=%t", oci), func(t *testing.T) {
			t.Parallel()

			manager := &Manager{}
			req := cleanupTestRequest()
			req.Logger = slog.New(slog.DiscardHandler)

			req.Repository.ResolvedReference = "main"
			if oci {
				req.Repository.Source = config.SourceTypeOCI
				req.Repository.ConfigSourceUrl = "oci://registry.example.com/repository:production"
			}

			req.pauseGeneration = manager.observePause(req)
			manager.cancelCleanupRetry(req)

			other := req

			other.Repository.ResolvedReference = "development"
			if oci {
				other.Repository.ConfigSourceUrl = "oci://registry.example.com/repository:development"
			}

			other.DeployConfigs = []*deployConfig.Config{{Name: "obsolete", Pause: true}}
			other.pauseGeneration = manager.observePause(other)
			manager.cancelCleanupRetry(other)

			if cleanupRetryKey(manager.currentCleanupRequest(req)) != cleanupRetryKey(req) {
				t.Fatal("cleanup reused an observation from another reference")
			}

			allowed := manager.removalPredicate(req, "", manager.newSyncWindowGate(req, time.Now()))
			if !allowed(req.Logger, "obsolete") {
				t.Fatal("another reference canceled or paused this cleanup")
			}

			current := req
			current.pauseGeneration = manager.observePause(current)
			manager.cancelCleanupRetry(current)

			if allowed(req.Logger, "obsolete") {
				t.Fatal("newer observation of the same source did not supersede cleanup")
			}
		})
	}
}
